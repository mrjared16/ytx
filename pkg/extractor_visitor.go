package ytx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mrjared16/ytx/internal/cache"
)

// fetchVisitorData gets visitorData from YouTube using WEB client (required since Jan 2025)
// and reuses shared runtime cache state across extractors.
func (e *Extractor) fetchVisitorData(videoID string) error {
	return e.fetchVisitorDataContext(context.Background(), videoID)
}

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

	v, err, _ := e.runtime.visitorGroup.Do(authKey, func() (any, error) {
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

func (e *Extractor) visitorAuthKey() string {
	if !e.config.NeedsCookies {
		return "video"
	}
	if e.sapisid != "" {
		return "music:" + e.sapisid
	}
	return "music"
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
		_, err, _ := e.runtime.visitorGroup.Do(e.visitorAuthKey(), func() (any, error) {
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

func (e *Extractor) cacheVisitorData() {
	if e.visitorData == "" {
		return
	}
	e.storeVisitorCache(&cache.VisitorCache{
		Data:         e.visitorData,
		SessionIndex: e.sessionIndex,
		DelegatedSID: e.delegatedSID,
		IsAuth:       e.config.NeedsCookies,
		AuthKey:      e.visitorAuthKey(),
		ExpiresAt:    time.Now().Add(visitorDataTTL),
	})
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
