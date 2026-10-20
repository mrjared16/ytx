package ytx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mrjared16/ytx/internal/cache"

	"golang.org/x/sync/singleflight"
)

// errNativeVideoRequiresGuest rejects the native (guest-only) video path when
// it is handed an authenticated configuration. Music is the authenticated
// mode; running the guest visitor flight under a music auth key would share
// (and collide with) the music flight and could pollute the authenticated
// visitor caches. Rejected before any state read or network I/O.
var errNativeVideoRequiresGuest = errors.New("native video extraction is guest-only: an authenticated configuration must use music mode")

// videoVisitorFlightTimeout bounds one shared video visitor bootstrap or
// forced-refresh request so its lifetime never depends on any caller's
// cancellation or deadline. Generous enough for slow networks, but a hung
// request is released and reported within the bound.
const videoVisitorFlightTimeout = 30 * time.Second

// videoVisitorDoChan starts or joins the single shared video visitor flight
// for authKey. The flight fn runs at most once (via singleflight), in its own
// goroutine, under a bounded context derived from the initiating caller's
// context but stripped of its cancellation and deadline (values are retained).
// Canceling the initiating caller therefore can never abort the shared work or
// fail joiners. Each caller must select on its own ctx.Done() vs the returned
// channel so any caller - leader or joiner - returns promptly on cancellation.
// There is deliberately no production observation hook: callers that need to
// prove they reached the wait wrap their own context (see the tests).
func (e *Extractor) videoVisitorDoChan(ctx context.Context, authKey string, flight func(flightCtx context.Context) (any, error)) <-chan singleflight.Result {
	if ctx == nil {
		ctx = context.Background()
	}
	// The bounded context is derived inside the flight closure so joiners
	// neither create timers nor invoke the flight fn.
	return e.runtime.visitorGroup.DoChan(authKey, func() (any, error) {
		flightCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), videoVisitorFlightTimeout)
		defer stop()
		return flight(flightCtx)
	})
}

// videoVisitorFlightResult decodes one shared video visitor flight result
// under the strict flight contract. The bootstrap and forced-refresh paths
// share auth key "video", so a bootstrap that leads with a typed but empty
// VisitorCache must never be accepted as a successful forced refresh (or an
// empty bootstrap candidate): an empty shared result is always an error.
func videoVisitorFlightResult(res singleflight.Result) (string, error) {
	if res.Err != nil {
		return "", res.Err
	}
	vc, ok := res.Val.(*cache.VisitorCache)
	if !ok || vc == nil {
		return "", fmt.Errorf("video visitor flight returned no visitor data")
	}
	if vc.Data == "" {
		return "", fmt.Errorf("video visitor flight returned empty visitor data")
	}
	return vc.Data, nil
}

// fetchVisitorData gets visitorData from YouTube using WEB client (required since Jan 2025)
// and reuses shared runtime cache state across extractors.
func (e *Extractor) fetchVisitorData(videoID string) error {
	return e.fetchVisitorDataContext(context.Background(), videoID)
}

// fetchVisitorDataContext keeps the historical public signature. The shared
// flight result (the VALUE, never a leader callback) is delivered to ALL
// waiters through the singleflight. Auth-key isolation, SWR apply, and store
// semantics are unchanged. The flight is caller-bound on the initiating
// caller's context, exactly as HEAD; guest isolation from the native video
// flight comes from visitorLegacyAuthKey ("video:legacy").
func (e *Extractor) fetchVisitorDataContext(ctx context.Context, videoID string) error {
	if e.visitorData != "" {
		if !e.config.NeedsCookies || e.sessionIndex != "" || e.delegatedSID != "" {
			return nil
		}
	}

	authKey := e.visitorAuthKey()
	now := time.Now()

	e.runtime.visitorCache.RLock()
	memVisitor := &cache.VisitorCache{
		Data:         e.runtime.visitorCache.data,
		SessionIndex: e.runtime.visitorCache.sessionIndex,
		DelegatedSID: e.runtime.visitorCache.delegatedSID,
		IsAuth:       e.runtime.visitorCache.isAuth,
		AuthKey:      e.runtime.visitorCache.authKey,
		ExpiresAt:    e.runtime.visitorCache.expiry,
	}
	memUpdated := e.runtime.visitorCache.updated
	e.runtime.visitorCache.RUnlock()
	if e.visitorCacheUsable(memVisitor, authKey) {
		e.applyVisitorCache(memVisitor)
		if now.After(memVisitor.ExpiresAt) && now.Before(memUpdated.Add(visitorDataSWRGrace)) {
			e.refreshVisitorDataAsync(ctx, videoID)
		}
		return nil
	}

	if e.cacheManager != nil {
		if vc, stale, err := e.cacheManager.LoadVisitorDataAllowStale(visitorDataSWRGrace); err == nil && e.visitorCacheUsable(vc, authKey) {
			e.applyVisitorCache(vc)
			e.storeVisitorCache(vc)
			if stale {
				e.refreshVisitorDataAsync(ctx, videoID)
			}
			return nil
		}
	}

	// The shared flight returns the visitor value to every waiter: the leader
	// runs the fetch, joiners receive the same value through the singleflight
	// result — never through a leader-only callback.
	//
	// Guest and music both keep the historical caller-bound singleflight: the
	// flight runs under the initiating caller's own context, exactly as HEAD.
	// Guest isolation comes from the dedicated key (visitorLegacyAuthKey), not
	// from a detached lifetime, so a canceled legacy caller aborts its own
	// flight without ever touching the native guest flight.
	v, err, _ := e.runtime.visitorGroup.Do(e.visitorLegacyAuthKey(), func() (any, error) {
		vc, err := e.fetchFreshVisitorDataContext(ctx, videoID)
		if err != nil {
			return nil, err
		}
		e.storeVisitorCache(vc)
		return vc, nil
	})
	if err != nil {
		return err
	}
	if vc, ok := v.(*cache.VisitorCache); ok {
		e.applyVisitorCache(vc)
	}
	return nil
}

// fetchVideoVisitorCandidate returns the visitorData candidate for one video
// extraction WITHOUT committing it: nothing is applied to extractor state and
// nothing is written to the runtime or disk caches. The candidate may come
// from already-validated extractor state, the shared runtime visitor cache,
// the disk cache, or a fresh bootstrap. The value is committed only after
// the video chain validates it end-to-end, so failed runs leave every layer
// of state untouched. No stale-while-revalidate refresh is started here: an
// async refresh would commit an unvalidated fresh visitor.
//
// The native video path is explicitly GUEST-ONLY. An authenticated
// (cookie-bearing) configuration belongs to the music/audio path; running
// this flight under a music auth key would share (and collide with) the music
// visitor flight and could pollute the authenticated visitor caches. The
// configuration is therefore rejected before any state read or network I/O.
func (e *Extractor) fetchVideoVisitorCandidate(ctx context.Context, videoID string) (string, error) {
	if e.config.NeedsCookies {
		return "", errNativeVideoRequiresGuest
	}

	// Guest-only from here on, so any already-present visitor is usable as the
	// candidate without the session/delegated-SID probe the shared path needs.
	if e.visitorData != "" {
		return e.visitorData, nil
	}

	authKey := e.visitorAuthKey()

	e.runtime.visitorCache.RLock()
	memVisitor := &cache.VisitorCache{
		Data:         e.runtime.visitorCache.data,
		SessionIndex: e.runtime.visitorCache.sessionIndex,
		DelegatedSID: e.runtime.visitorCache.delegatedSID,
		IsAuth:       e.runtime.visitorCache.isAuth,
		AuthKey:      e.runtime.visitorCache.authKey,
		ExpiresAt:    e.runtime.visitorCache.expiry,
	}
	e.runtime.visitorCache.RUnlock()
	if e.visitorCacheUsable(memVisitor, authKey) {
		return memVisitor.Data, nil
	}

	if e.cacheManager != nil {
		if vc, _, err := e.cacheManager.LoadVisitorDataAllowStale(visitorDataSWRGrace); err == nil && e.visitorCacheUsable(vc, authKey) {
			return vc.Data, nil
		}
	}

	ch := e.videoVisitorDoChan(ctx, authKey, func(flightCtx context.Context) (any, error) {
		return e.fetchFreshVisitorDataContext(flightCtx, videoID)
	})
	var res singleflight.Result
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res = <-ch:
		// Prefer the caller's own cancellation even if the shared result
		// arrived first: a canceled caller must never act on a flight result.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		// Strict on the shared guest video flight: an empty result led by
		// either the bootstrap or the forced refresh is never a success.
		return videoVisitorFlightResult(res)
	}
}

func (e *Extractor) visitorAuthKey() string {
	if !e.config.NeedsCookies {
		return "video"
	}
	if e.sapisid != "" {
		return "music:" + e.sapisid
	}
	return "music"
}

// visitorLegacyAuthKey returns the singleflight key for the legacy guest
// visitor flights (fetchVisitorDataContext and refreshVisitorDataAsync, guest
// only). Those flights commit shared cache state from inside the flight and
// stay caller-bound on the explicit legacy API, so they are deliberately
// isolated from the native guest flight key "video": a legacy leader must
// never impose its cancellation, lifetime, or commit semantics on native
// extraction-local candidate joiners, and native bounded flights must never
// hijack legacy committing work. Music/authenticated keys are unchanged (the
// music foreground path is HEAD's single caller-bound flight on the music auth
// key, exactly as before). The shared runtime/disk cache is still keyed by
// visitorAuthKey, so cache isolation and apply/store semantics are unchanged.
func (e *Extractor) visitorLegacyAuthKey() string {
	if !e.config.NeedsCookies {
		return "video:legacy"
	}
	return e.visitorAuthKey()
}

func (e *Extractor) visitorCacheUsable(vc *cache.VisitorCache, authKey string) bool {
	if vc == nil || vc.Data == "" {
		return false
	}
	if vc.AuthKey != "" && vc.AuthKey != authKey {
		return false
	}
	if e.config.NeedsCookies {
		return vc.IsAuth
	}
	return true
}

func (e *Extractor) applyVisitorCache(vc *cache.VisitorCache) {
	if vc == nil {
		return
	}
	e.visitorData = vc.Data
	e.sessionIndex = vc.SessionIndex
	e.delegatedSID = vc.DelegatedSID
}

func (e *Extractor) storeVisitorCache(vc *cache.VisitorCache) {
	if vc == nil || vc.Data == "" {
		return
	}
	e.runtime.visitorCache.Lock()
	e.runtime.visitorCache.data = vc.Data
	e.runtime.visitorCache.sessionIndex = vc.SessionIndex
	e.runtime.visitorCache.delegatedSID = vc.DelegatedSID
	e.runtime.visitorCache.isAuth = vc.IsAuth
	e.runtime.visitorCache.authKey = vc.AuthKey
	e.runtime.visitorCache.expiry = vc.ExpiresAt
	e.runtime.visitorCache.updated = time.Now()
	e.runtime.visitorCache.Unlock()

	if e.cacheManager != nil {
		if err := e.cacheManager.SaveVisitorData(vc); err != nil {
			// Best-effort persistence: keep in-memory visitor cache even if disk write fails.
		}
	}
}

func (e *Extractor) refreshVisitorDataAsync(parent context.Context, videoID string) {
	go func() {
		ctx, cancel := backgroundRefreshContext(parent, backgroundWarmupTTL)
		defer cancel()
		_, err, _ := e.runtime.visitorGroup.Do(e.visitorLegacyAuthKey(), func() (any, error) {
			vc, err := e.fetchFreshVisitorDataContext(ctx, videoID)
			if err != nil {
				return nil, err
			}
			e.storeVisitorCache(vc)
			return vc, nil
		})
		if err != nil {
			return
		}
	}()
}

func (e *Extractor) fetchFreshVisitorDataContext(ctx context.Context, videoID string) (*cache.VisitorCache, error) {
	if e.config.NeedsCookies {
		return e.fetchMusicVisitorDataContext(ctx, videoID)
	}
	return e.fetchWebVisitorDataContext(ctx, videoID)
}

func (e *Extractor) fetchWebVisitorDataContext(ctx context.Context, videoID string) (*cache.VisitorCache, error) {
	reqBody := InnertubeRequest{
		VideoID: videoID,
		Context: InnertubeContext{
			Client: InnertubeClient{
				HL:            "en",
				GL:            "US",
				ClientName:    WEBClientName,
				ClientVersion: WEBClientVersion,
				TimeZone:      "UTC",
				UTCOffset:     0,
			},
		},
		ContentCheckOK: true,
		RacyCheckOK:    true,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("%s?key=%s&prettyPrint=false", WEBAPIEndpoint, WEBAPIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", WEBUserAgent)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("visitor API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(200, len(body))])))
	}

	var playerResp PlayerResponse
	if err := json.NewDecoder(resp.Body).Decode(&playerResp); err != nil {
		return nil, err
	}

	return &cache.VisitorCache{
		Data:      playerResp.ResponseContext.VisitorData,
		IsAuth:    false,
		AuthKey:   e.visitorAuthKey(),
		ExpiresAt: time.Now().Add(visitorDataTTL),
	}, nil
}

// forceRefreshVideoVisitorCandidate fetches a fresh guest WEB visitorData
// bypassing both memory and disk caches and returns it as a retry candidate,
// WITHOUT persisting or applying it (self-healing a poisoned guest cache).
// Video-only: it refuses authenticated configs so music behavior is
// untouched. Concurrent refreshes are deduped through the shared singleflight
// group keyed by the video auth key. The caller commits the candidate only if
// the retry chain validates it end-to-end.
func (e *Extractor) forceRefreshVideoVisitorCandidate(ctx context.Context, videoID string) (string, error) {
	if e.config.NeedsCookies {
		return "", errNativeVideoRequiresGuest
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ch := e.videoVisitorDoChan(ctx, e.visitorAuthKey(), func(flightCtx context.Context) (any, error) {
		vc, err := e.fetchWebVisitorDataContext(flightCtx, videoID)
		if err != nil {
			return nil, err
		}
		// Keep a typed-but-empty result out of the shared flight even when
		// this caller leads: the common result contract rejects it anyway.
		if vc == nil || vc.Data == "" {
			return nil, fmt.Errorf("video visitor refresh returned empty visitor data")
		}
		return vc, nil
	})
	var res singleflight.Result
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res = <-ch:
		// Prefer the caller's own cancellation even if the shared result
		// arrived first: a canceled caller must never act on a flight result.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		// Guest-only path: always strict against an empty shared result.
		return videoVisitorFlightResult(res)
	}
}

// commitVideoVisitor applies and persists the EXACT visitor candidate that a
// validated video chain won with. Using the parameter rather than the shared
// e.visitorData field prevents a concurrent extraction (or an unvalidated SWR
// result) from substituting a different value into this run's commit.
func (e *Extractor) commitVideoVisitor(visitorData string) {
	if visitorData == "" {
		return
	}
	vc := &cache.VisitorCache{
		Data:         visitorData,
		SessionIndex: e.sessionIndex,
		DelegatedSID: e.delegatedSID,
		IsAuth:       e.config.NeedsCookies,
		AuthKey:      e.visitorAuthKey(),
		ExpiresAt:    time.Now().Add(visitorDataTTL),
	}
	e.applyVisitorCache(vc)
	e.storeVisitorCache(vc)
}

var (
	musicVisitorDataRegex = regexp.MustCompile(`"visitorData"\s*:\s*"([^"]+)"`)
	sessionIndexRegex     = regexp.MustCompile(`"SESSION_INDEX"\s*:\s*"([^"]+)"`)
	delegatedSIDRegex     = regexp.MustCompile(`"DELEGATED_SESSION_ID"\s*:\s*"([^"]+)"`)
	datasyncIDRegex       = regexp.MustCompile(`"DATASYNC_ID"\s*:\s*"([^"]+)"`)
	ytAtNRegex            = regexp.MustCompile(`(?s)window\s*\.\s*ytAtN\s*\(\s*(\{.+?\})\s*\)\s*;`)
	ytAtRRegex            = regexp.MustCompile(`(?s)window\s*\.\s*ytAtR\s*=\s*(['"].+?['"])\s*;`)
)

func (e *Extractor) fetchMusicVisitorDataContext(ctx context.Context, videoID string) (*cache.VisitorCache, error) {
	watchURL := fmt.Sprintf("https://music.youtube.com/watch?v=%s", videoID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, watchURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", e.config.UserAgent)
	req.Header.Set("Cookie", BuildCookieHeader(e.cookies))

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("music watch returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(200, len(body))])))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	vc := &cache.VisitorCache{IsAuth: true, AuthKey: e.visitorAuthKey(), ExpiresAt: time.Now().Add(visitorDataTTL)}

	matches := musicVisitorDataRegex.FindSubmatch(body)
	if len(matches) >= 2 {
		vc.Data = string(matches[1])
	}

	matches = sessionIndexRegex.FindSubmatch(body)
	if len(matches) >= 2 {
		vc.SessionIndex = string(matches[1])
	}

	matches = delegatedSIDRegex.FindSubmatch(body)
	if len(matches) >= 2 {
		vc.DelegatedSID = string(matches[1])
	} else {
		matches = datasyncIDRegex.FindSubmatch(body)
		if len(matches) >= 2 {
			parts := strings.Split(string(matches[1]), "||")
			if len(parts) >= 2 && parts[0] != "" {
				vc.DelegatedSID = parts[0]
			}
		}
	}

	return vc, nil
}
