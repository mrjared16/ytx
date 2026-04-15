package ytx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const visitorDataTTL = 30 * time.Minute

const (
	visitorDataSWRGrace = 2 * time.Hour
	cipherSWRGrace      = 18 * time.Hour
	backgroundWarmupTTL = 20 * time.Second
	defaultPOTokenTTL   = 10 * time.Minute
	stsWaitBudget       = 250 * time.Millisecond
)

// Extractor handles YouTube stream extraction
type Extractor struct {
	config           ClientConfig
	runtime          *Runtime
	cookies          []*http.Cookie
	httpClient       *http.Client
	sapisid          string
	cipher           *Cipher // lazy initialized, only for music mode
	cipherMu         sync.Mutex
	cacheManager     *CacheManager // file-based persistent cache
	cipherRetried    atomic.Bool   // track if we've already retried with fresh cipher
	visitorData      string        // required since Jan 2025 for all API requests
	sessionIndex     string        // X-Goog-AuthUser header
	delegatedSID     string        // X-Goog-PageId header
	profile          bool          // enable profiling
	timings          Timings       // profiling timers
	fetchSubs        bool          // whether to include subtitles in output
	subLangs         []string      // subtitle languages to fetch (nil = default, empty = all)
	maxHeight        int           // max video height (0 = no limit)
	audioFormat      AudioFormat   // preferred audio format (default: AudioFormatWebm)
	earlySTSOverride int           // STS from early cipher signal (for API overlap)
	warnings         []string
	poMode           bool // explicit PO mode; off by default
	poChallenge      *POTokenChallenge
	poPlayerToken    string
	poURLToken       string
}

// defaultSubtitleLangs are fetched when --subs is used without --sub-langs
// Users can override via --sub-langs flag
var defaultSubtitleLangs = []string{"en"}

// ExtractorOption customizes shared services for a new extractor.
type ExtractorOption func(*Extractor) error

func WithRuntime(runtime *Runtime) ExtractorOption {
	return func(e *Extractor) error {
		if runtime == nil {
			return fmt.Errorf("runtime cannot be nil")
		}
		e.runtime = runtime
		return nil
	}
}

func WithHTTPClient(client *http.Client) ExtractorOption {
	return func(e *Extractor) error {
		if client == nil {
			return fmt.Errorf("http client cannot be nil")
		}
		e.httpClient = client
		return nil
	}
}

func WithCacheManager(cacheManager *CacheManager) ExtractorOption {
	return func(e *Extractor) error {
		e.cacheManager = cacheManager
		return nil
	}
}

func WithPOMode(enabled bool) ExtractorOption {
	return func(e *Extractor) error {
		e.poMode = enabled
		return nil
	}
}

// NewExtractor creates a new extractor for the given mode
func NewExtractor(mode ClientMode, cookieFile string, opts ...ExtractorOption) (*Extractor, error) {
	config := GetClientConfig(mode)

	ext := &Extractor{
		config:  config,
		runtime: defaultRuntime,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				ForceAttemptHTTP2:   true,
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}

	for _, opt := range opts {
		if err := opt(ext); err != nil {
			return nil, err
		}
	}

	// Initialize file-based cache manager (graceful degradation if fails)
	if ext.cacheManager == nil {
		if cm, err := NewCacheManager(); err == nil {
			ext.cacheManager = cm
		}
	}

	if ext.runtime == nil {
		ext.runtime = defaultRuntime
	}

	// Only load cookies if needed (music mode)
	if config.NeedsCookies {
		if cookieFile == "" {
			return nil, fmt.Errorf("music mode requires --cookies")
		}

		cookies, err := ParseNetscapeCookieFile(cookieFile)
		if err != nil {
			return nil, fmt.Errorf("failed to parse cookies: %w", err)
		}
		ext.cookies = cookies

		// Find SAPISID
		sapisid := FindCookie(cookies, "SAPISID", "__Secure-3PAPISID")
		if sapisid == "" {
			return nil, fmt.Errorf("SAPISID cookie not found - ensure you're logged in")
		}
		ext.sapisid = sapisid

	}

	return ext, nil
}

// SetProfile enables profiling for this extractor
func (e *Extractor) SetProfile(enabled bool) {
	e.profile = enabled
}

// SetFetchSubtitles enables subtitle extraction with optional language filter.
// If langs is nil or empty, uses default language (en).
// Use []string{"all"} to fetch all available languages.
func (e *Extractor) SetFetchSubtitles(langs []string) {
	e.fetchSubs = true
	e.subLangs = langs
}

// SetMaxHeight sets the maximum video height (e.g., 1080 for 1080p).
// Set to 0 to disable limit (default behavior).
func (e *Extractor) SetMaxHeight(height int) {
	e.maxHeight = height
}

func (e *Extractor) SetPreferWebm(enabled bool) {
	if enabled {
		e.audioFormat = AudioFormatWebm
	} else {
		e.audioFormat = AudioFormatM4A
	}
}

func (e *Extractor) SetAudioFormat(format AudioFormat) {
	e.audioFormat = format
}

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
	memVisitor := &VisitorCache{
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
	if vc, ok := v.(*VisitorCache); ok {
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

func (e *Extractor) visitorCacheUsable(vc *VisitorCache, authKey string) bool {
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

func (e *Extractor) applyVisitorCache(vc *VisitorCache) {
	if vc == nil {
		return
	}
	e.visitorData = vc.Data
	e.sessionIndex = vc.SessionIndex
	e.delegatedSID = vc.DelegatedSID
}

func (e *Extractor) storeVisitorCache(vc *VisitorCache) {
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
		_ = e.cacheManager.SaveVisitorData(vc)
	}
}

func (e *Extractor) refreshVisitorDataAsync(parent context.Context, videoID string) {
	go func() {
		ctx, cancel := backgroundRefreshContext(parent, backgroundWarmupTTL)
		defer cancel()
		_, _, _ = e.runtime.visitorGroup.Do(e.visitorAuthKey(), func() (any, error) {
			vc, err := e.fetchFreshVisitorDataContext(ctx, videoID)
			if err != nil {
				return nil, err
			}
			e.storeVisitorCache(vc)
			return vc, nil
		})
	}()
}

func (e *Extractor) fetchFreshVisitorDataContext(ctx context.Context, videoID string) (*VisitorCache, error) {
	if e.config.NeedsCookies {
		return e.fetchMusicVisitorDataContext(ctx, videoID)
	}
	return e.fetchWebVisitorDataContext(ctx, videoID)
}

func (e *Extractor) fetchWebVisitorDataContext(ctx context.Context, videoID string) (*VisitorCache, error) {
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

	var playerResp PlayerResponse
	if err := json.NewDecoder(resp.Body).Decode(&playerResp); err != nil {
		return nil, err
	}

	return &VisitorCache{
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
	e.storeVisitorCache(&VisitorCache{
		Data:         e.visitorData,
		SessionIndex: e.sessionIndex,
		DelegatedSID: e.delegatedSID,
		IsAuth:       e.config.NeedsCookies,
		AuthKey:      e.visitorAuthKey(),
		ExpiresAt:    time.Now().Add(visitorDataTTL),
	})
}

// musicVisitorDataRegex extracts visitorData from music.youtube.com HTML
var (
	musicVisitorDataRegex = regexp.MustCompile(`"visitorData"\s*:\s*"([^"]+)"`)
	sessionIndexRegex     = regexp.MustCompile(`"SESSION_INDEX"\s*:\s*"([^"]+)"`)
	delegatedSIDRegex     = regexp.MustCompile(`"DELEGATED_SESSION_ID"\s*:\s*"([^"]+)"`)
	datasyncIDRegex       = regexp.MustCompile(`"DATASYNC_ID"\s*:\s*"([^"]+)"`)
	ytAtNRegex            = regexp.MustCompile(`(?s)window\s*\.\s*ytAtN\s*\(\s*(\{.+?\})\s*\)\s*;`)
	ytAtRRegex            = regexp.MustCompile(`(?s)window\s*\.\s*ytAtR\s*=\s*(['"].+?['"])\s*;`)
)

// fetchMusicVisitorData gets visitorData from music.youtube.com page.
func (e *Extractor) fetchMusicVisitorData(videoID string) error {
	vc, err := e.fetchMusicVisitorDataContext(context.Background(), videoID)
	if err != nil {
		return err
	}
	e.applyVisitorCache(vc)
	e.storeVisitorCache(vc)
	return nil
}

func (e *Extractor) fetchMusicVisitorDataContext(ctx context.Context, videoID string) (*VisitorCache, error) {
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	vc := &VisitorCache{IsAuth: true, AuthKey: e.visitorAuthKey(), ExpiresAt: time.Now().Add(visitorDataTTL)}

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

// Extract gets the best audio stream URL for a video
func (e *Extractor) Extract(videoID string) (*Result, error) {
	return e.ExtractContext(context.Background(), videoID)
}

func (e *Extractor) ExtractContext(ctx context.Context, videoID string) (*Result, error) {
	e.warnings = nil
	e.poPlayerToken = ""
	e.poURLToken = ""
	e.poChallenge = nil
	e.cipherRetried.Store(false) // Allow one retry per extraction
	var totalStart time.Time
	if e.profile {
		totalStart = time.Now()
		e.timings = Timings{} // Reset timings
	}

	// Parallel fetch: visitorData, cipher (pre-warm), and prepare for API call
	// This saves ~150-250ms by overlapping network calls
	type visitorResult struct {
		err error
	}
	type cipherResult struct {
		cipher    *Cipher
		err       error
		prewarmMs int64
	}

	visitorCh := make(chan visitorResult, 1)
	cipherCh := make(chan cipherResult, 1)
	stsCh := make(chan int, 1) // early STS for API overlap
	type poTokenResult struct {
		playerToken string
		urlToken    string
		err         error
	}
	poCh := make(chan poTokenResult, 1)

	// Fetch visitorData in parallel
	go func() {
		err := e.fetchVisitorDataContext(ctx, videoID)
		visitorCh <- visitorResult{err: err}
	}()

	// Pre-fetch cipher with early STS signaling.
	// STS is signaled as soon as player.js is downloaded so the API call can start
	// before the full cipher init (pattern detection, wrapper runtime build) completes.
	go func() {
		var prewarmStart time.Time
		if e.profile {
			prewarmStart = time.Now()
		}
		cipher, err := e.getCachedCipherContext(ctx, videoID)
		if err == nil && cipher != nil {
			// Signal STS as soon as cipher is available (even from cache)
			select {
			case stsCh <- cipher.SignatureTimestamp():
			default:
			}

			// Synchronously ensure sig runtime is ready before sending cipher result.
			// This eliminates the race where DecryptSignature hits ensureWrapperContext
			// before an async prewarm finishes, paying ~326ms bootstrap redundantly.
			if cipher.canPrecomputeSignatureRuntime() {
				_ = cipher.ensureSignatureReady()
			}
			if cipher.canPrewarmSignatureRuntime() {
				_ = cipher.Prewarm()
			}
			// N-transform engine can warm in background — not needed before decrypt.
			go func() { _ = cipher.WarmNTransformEngineContext(ctx) }()
		} else {
			select {
			case stsCh <- 0:
			default:
			}
		}

		cipherCh <- cipherResult{cipher: cipher, err: err, prewarmMs: func() int64 {
			if e.profile {
				return time.Since(prewarmStart).Milliseconds()
			}
			return 0
		}()}
	}()

	// Wait for visitorData (required for API call)
	var visitorWaitStart time.Time
	if e.profile {
		visitorWaitStart = time.Now()
	}
	visitorRes := <-visitorCh
	if e.profile {
		e.timings.VisitorDataMs = time.Since(visitorWaitStart).Milliseconds()
	}
	if visitorRes.err != nil {
		// Non-fatal, continue without it
	}

	if e.poMode {
		poCoordinator := e.newPOCoordinator()
		go func() {
			playerToken, urlToken, err := poCoordinator.acquireTokens(ctx, videoID)
			poCh <- poTokenResult{playerToken: playerToken, urlToken: urlToken, err: err}
		}()
	}

	// Wait briefly for STS (not full cipher) — allows API call to start sooner
	var stsWaitStart time.Time
	if e.profile {
		stsWaitStart = time.Now()
	}
	sts := 0
	stsReady := false
	if !e.config.NeedsCookies {
		sts = <-stsCh
		stsReady = true
	} else {
		select {
		case sts = <-stsCh:
			stsReady = true
		case <-time.After(stsWaitBudget):
			sts = 0
		}
	}
	if e.profile {
		e.timings.STSWaitMs = time.Since(stsWaitStart).Milliseconds()
	}

	if e.poMode {
		poRes := <-poCh
		e.poPlayerToken = poRes.playerToken
		e.poURLToken = poRes.urlToken
		if poRes.err != nil {
			return nil, NewPOError(POFailureTokenUnavailable, "po mode token unavailable", poRes.err)
		}
	}

	// Call innertube API with STS (cipher may still be finishing heavy init)
	var apiStart time.Time
	if e.profile {
		apiStart = time.Now()
	}
	e.earlySTSOverride = sts
	playerResp, err := e.callPlayerAPIContext(ctx, videoID)
	e.earlySTSOverride = 0
	if err != nil && !stsReady {
		select {
		case sts = <-stsCh:
			if sts > 0 {
				e.earlySTSOverride = sts
				playerResp, err = e.callPlayerAPIContext(ctx, videoID)
				e.earlySTSOverride = 0
			}
		default:
		}
	}
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	if e.profile {
		e.timings.PlayerAPIMs = time.Since(apiStart).Milliseconds()
	}

	// NOW wait for full cipher (needed for sig/n decryption)
	var cipherWaitStart time.Time
	if e.profile {
		cipherWaitStart = time.Now()
	}
	cipherRes := <-cipherCh
	if e.profile {
		e.timings.CipherInitMs = time.Since(cipherWaitStart).Milliseconds()
		e.timings.CipherPrewarmMs = cipherRes.prewarmMs
	}
	if cipherRes.err == nil {
		e.cipherMu.Lock()
		e.cipher = cipherRes.cipher
		e.cipherMu.Unlock()
	}

	// Check playability
	if playerResp.PlayabilityStatus.Status != "OK" {
		if !e.poMode && IsLikelyPORequiredFromPlayerResponse(playerResp) {
			return nil, NewPOError(
				POFailureRequired,
				fmt.Sprintf("video not playable: %s - %s", playerResp.PlayabilityStatus.Status, playerResp.PlayabilityStatus.Reason),
				nil,
			)
		}
		return nil, fmt.Errorf("video not playable: %s - %s",
			playerResp.PlayabilityStatus.Status,
			playerResp.PlayabilityStatus.Reason)
	}

	// Find best audio stream
	stream := e.findBestAudioStream(playerResp.StreamingData.AdaptiveFormats)
	if stream == nil {
		return nil, fmt.Errorf("no audio stream found")
	}

	// Get stream URL (may need decryption) - cipher already initialized
	streamURL, err := e.getStreamURLContext(ctx, videoID, stream)
	if err != nil {
		return nil, fmt.Errorf("failed to get stream URL: %w", err)
	}

	result := &Result{
		URL:      streamURL,
		Itag:     stream.Itag,
		Bitrate:  stream.Bitrate,
		MimeType: stream.MimeType,
		Title:    playerResp.VideoDetails.Title,
		Author:   playerResp.VideoDetails.Author,
		Warnings: e.warningsForResult(),
	}

	if e.profile {
		e.finalizeProfile(totalStart)
		result.Timings = &e.timings
	}

	return result, nil
}

// ExtractVideo gets video and audio stream URLs (for MPV playback)
func (e *Extractor) ExtractVideo(videoID string) (*VideoResult, error) {
	return e.ExtractVideoContext(context.Background(), videoID)
}

func (e *Extractor) ExtractVideoContext(ctx context.Context, videoID string) (*VideoResult, error) {
	e.warnings = nil
	var totalStart time.Time
	if e.profile {
		totalStart = time.Now()
		e.timings = Timings{} // Reset timings
	}

	// 0. Fetch visitorData first (required since Jan 2025)
	var visitorStart time.Time
	if e.profile {
		visitorStart = time.Now()
	}
	if err := e.fetchVisitorDataContext(ctx, videoID); err != nil {
		// Non-fatal, continue without it
	}
	if e.profile {
		e.timings.VisitorDataMs = time.Since(visitorStart).Milliseconds()
	}

	var apiStart time.Time
	if e.profile {
		apiStart = time.Now()
	}
	playerResp, err := e.callPlayerAPIContext(ctx, videoID)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	if e.profile {
		e.timings.PlayerAPIMs = time.Since(apiStart).Milliseconds()
	}

	if playerResp.PlayabilityStatus.Status != "OK" {
		if !e.poMode && IsLikelyPORequiredFromPlayerResponse(playerResp) {
			return nil, NewPOError(
				POFailureRequired,
				fmt.Sprintf("video not playable: %s - %s", playerResp.PlayabilityStatus.Status, playerResp.PlayabilityStatus.Reason),
				nil,
			)
		}
		return nil, fmt.Errorf("video not playable: %s - %s",
			playerResp.PlayabilityStatus.Status,
			playerResp.PlayabilityStatus.Reason)
	}

	// Find best video stream (1080p preferred)
	video := e.findBestVideoStream(playerResp.StreamingData.AdaptiveFormats)
	if video == nil {
		return nil, fmt.Errorf("no video stream found")
	}

	// Find best audio stream
	audio := e.findBestAudioStream(playerResp.StreamingData.AdaptiveFormats)
	if audio == nil {
		return nil, fmt.Errorf("no audio stream found")
	}

	// ANDROID_VR with visitorData returns direct URLs (no cipher)
	if video.URL == "" || audio.URL == "" {
		return nil, fmt.Errorf("no direct URLs - cipher required but video mode doesn't support it")
	}

	result := &VideoResult{
		VideoURL:  video.URL,
		AudioURL:  audio.URL,
		VideoItag: video.Itag,
		AudioItag: audio.Itag,
		Width:     video.Width,
		Height:    video.Height,
		Title:     playerResp.VideoDetails.Title,
		Author:    playerResp.VideoDetails.Author,
		Warnings:  e.warningsForResult(),
	}

	// Extract subtitles if requested (from same response - zero extra latency)
	if e.fetchSubs {
		subtitles := extractSubtitles(playerResp.Captions, e.subLangs)
		if len(subtitles) > 0 {
			result.Subtitles = subtitles
			result.SubURL = subtitles[0].URL
		}
	}

	if e.profile {
		e.finalizeProfile(totalStart)
		result.Timings = &e.timings
	}

	return result, nil
}

func (e *Extractor) finalizeProfile(totalStart time.Time) {
	e.timings.TotalMs = time.Since(totalStart).Milliseconds()
	profiled := e.timings.VisitorDataMs +
		e.timings.STSWaitMs +
		e.timings.PlayerAPIMs +
		e.timings.CipherInitMs +
		e.timings.SigDecryptMs +
		e.timings.NTransformMs
	if e.timings.TotalMs > profiled {
		e.timings.OtherMs = e.timings.TotalMs - profiled
	}
	if engineName := CachedEngineName(); engineName != "" {
		e.timings.JSEngine = engineName
	}
}

// callPlayerAPI makes the innertube player API call
func (e *Extractor) callPlayerAPI(videoID string) (*PlayerResponse, error) {
	return e.callPlayerAPIContext(context.Background(), videoID)
}

func (e *Extractor) callPlayerAPIContext(ctx context.Context, videoID string) (*PlayerResponse, error) {
	// Build request body
	client := InnertubeClient{
		HL:            "en",
		GL:            "US",
		ClientName:    e.config.Name,
		ClientVersion: e.config.Version,
		UserAgent:     e.config.UserAgent,
		TimeZone:      "UTC",
		UTCOffset:     0,
	}

	// Add device info if present (IOS/Android clients)
	if e.config.DeviceMake != "" {
		client.DeviceMake = e.config.DeviceMake
	}
	if e.config.DeviceModel != "" {
		client.DeviceModel = e.config.DeviceModel
	}
	if e.config.Platform != "" {
		client.Platform = e.config.Platform
	}
	if e.config.OSName != "" {
		client.OSName = e.config.OSName
	}
	if e.config.OSVersion != "" {
		client.OSVersion = e.config.OSVersion
	}

	// Add visitorData if we have it (required since Jan 2025)
	if e.visitorData != "" {
		client.VisitorData = e.visitorData
	}

	// Get signatureTimestamp from cipher or early override (required for premium audio)
	var sts int
	if e.earlySTSOverride > 0 {
		sts = e.earlySTSOverride
	} else if e.cipher != nil {
		sts = e.cipher.SignatureTimestamp()
	}

	reqBody := InnertubeRequest{
		VideoID: videoID,
		Context: InnertubeContext{
			Client: client,
		},
		ContentCheckOK: true,
		RacyCheckOK:    true,
		PlaybackContext: &PlaybackContext{
			ContentPlaybackContext: ContentPlaybackContext{
				HTML5Preference:    "HTML5_PREF_WANTS",
				SignatureTimestamp: sts,
			},
		},
	}
	if e.poMode && e.poPlayerToken != "" {
		reqBody.ServiceIntegrityDimensions = &ServiceIntegrityDimensions{PoToken: e.poPlayerToken}
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	// Build request
	apiURL := fmt.Sprintf("%s?key=%s&prettyPrint=false", e.config.APIEndpoint, e.config.APIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", e.config.UserAgent)
	req.Header.Set("Origin", e.config.Origin)
	req.Header.Set("Referer", e.config.Origin+"/")

	// Add client-specific headers
	for k, v := range e.config.Headers {
		req.Header.Set(k, v)
	}

	// Add authentication if needed (music mode)
	if e.config.NeedsCookies && e.sapisid != "" {
		sapisidhash := GenerateSAPISIDHASH(e.sapisid, e.config.Origin)
		req.Header.Set("Authorization", sapisidhash)
		req.Header.Set("X-Origin", e.config.Origin)
		req.Header.Set("Cookie", BuildCookieHeader(e.cookies))

		// Add session headers if available
		// NOTE: X-Goog-AuthUser is needed for logged-in state
		// NOTE: X-Goog-PageId is OMITTED - it causes premium formats to be hidden
		//       when using a Brand Account that doesn't have its own Premium subscription
		if e.sessionIndex != "" {
			req.Header.Set("X-Goog-AuthUser", e.sessionIndex)
		}
		// REMOVED: X-Goog-PageId - breaks premium audio (itag 141)
		req.Header.Set("X-Youtube-Bootstrap-Logged-In", "true")
	}

	// Add visitor ID header if available (required for authenticated requests)
	if e.visitorData != "" {
		req.Header.Set("X-Goog-Visitor-Id", e.visitorData)
	}

	// Make request
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body[:min(200, len(body))]))
	}

	// Parse response
	var playerResp PlayerResponse
	if err := json.NewDecoder(resp.Body).Decode(&playerResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// Capture visitorData for subsequent requests (required since Jan 2025)
	if playerResp.ResponseContext.VisitorData != "" && e.visitorData == "" {
		e.visitorData = playerResp.ResponseContext.VisitorData
	}

	return &playerResp, nil
}

// findBestAudioStream finds the highest quality audio stream
func (e *Extractor) findBestAudioStream(formats []Format) *Format {
	// Filter audio-only formats
	var audioFormats []Format
	for _, f := range formats {
		if strings.HasPrefix(f.MimeType, "audio/") {
			audioFormats = append(audioFormats, f)
		}
	}

	if len(audioFormats) == 0 {
		return nil
	}

	var priorityItags []int
	if itags, ok := AudioFormatPriority[e.audioFormat]; ok {
		priorityItags = itags
	} else {
		priorityItags = AudioFormatPriority[AudioFormatWebm]
	}

	selectPreferred := func(candidates []Format) *Format {
		for _, targetItag := range priorityItags {
			for i := range candidates {
				if candidates[i].Itag == targetItag {
					return &candidates[i]
				}
			}
		}

		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].Bitrate > candidates[j].Bitrate
		})
		return &candidates[0]
	}

	var directURLFormats []Format
	for _, f := range audioFormats {
		if f.URL != "" {
			directURLFormats = append(directURLFormats, f)
		}
	}
	if len(directURLFormats) > 0 {
		return selectPreferred(directURLFormats)
	}

	return selectPreferred(audioFormats)
}

// findBestVideoStream finds the highest quality video stream within maxHeight limit
func (e *Extractor) findBestVideoStream(formats []Format) *Format {
	var videoFormats []Format
	for _, f := range formats {
		if strings.HasPrefix(f.MimeType, "video/") && f.Height > 0 {
			// Apply height limit if set
			if e.maxHeight > 0 && f.Height > e.maxHeight {
				continue
			}
			videoFormats = append(videoFormats, f)
		}
	}

	if len(videoFormats) == 0 {
		return nil
	}

	// Try preferred itags first (only if they're within height limit)
	for _, targetItag := range VideoItags {
		for i := range videoFormats {
			if videoFormats[i].Itag == targetItag {
				return &videoFormats[i]
			}
		}
	}

	// Fallback: sort by height and return highest (within limit)
	sort.Slice(videoFormats, func(i, j int) bool {
		return videoFormats[i].Height > videoFormats[j].Height
	})

	return &videoFormats[0]
}

// getCachedCipher returns a cached cipher or creates a new one
// Priority: 1) in-memory cache, 2) file cache, 3) fetch fresh
func (e *Extractor) getCachedCipher(videoID string) (*Cipher, error) {
	return e.getCachedCipherContext(context.Background(), videoID)
}

func (e *Extractor) getCachedCipherContext(ctx context.Context, videoID string) (*Cipher, error) {
	now := time.Now()

	// 1. Try shared runtime cache first.
	e.runtime.cipherCache.RLock()
	memCipher := e.runtime.cipherCache.cipher
	memExpiry := e.runtime.cipherCache.expiry
	memUpdated := e.runtime.cipherCache.updated
	e.runtime.cipherCache.RUnlock()
	if memCipher != nil {
		if now.Before(memExpiry) {
			return memCipher, nil
		}
		if now.Before(memUpdated.Add(cipherSWRGrace)) {
			e.refreshCipherAsync(ctx, videoID)
			return memCipher, nil
		}
	}

	// 2. Try persistent cache.
	if e.cacheManager != nil {
		if cache, err := e.cacheManager.Load(); err == nil {
			if !e.cacheManager.IsValid(cache) {
				_ = e.cacheManager.InvalidateCipherArtifacts()
			} else {
				stale := now.After(cache.ExpiresAt)
				if !stale || now.Before(cache.ExpiresAt.Add(cipherSWRGrace)) {
					cipher := NewCipherFromCacheWithRuntime(e.runtime, cache)
					if nRuntimeJS, err := e.cacheManager.LoadNRuntimeJS(); err == nil {
						cipher.nRuntimeJS = nRuntimeJS
					}

					needPlayerJS := cache.SigUsesURLWrapper || cache.JSCode == "" || (cache.NFunction != "" && len(cipher.nRuntimeJS) == 0)
					if needPlayerJS {
						if playerJS, err := e.cacheManager.LoadPlayerJS(); err == nil {
							cipher.playerJS = playerJS
						}
					}
					if cache.SigUsesURLWrapper && cache.WrapperBuildID == currentWrapperBytecodeBuildID() {
						if wrapperBytecode, err := e.cacheManager.LoadWrapperRuntimeBytecode(); err == nil && len(wrapperBytecode) > 0 {
							cipher.jsCode = string(wrapperBytecode)
							cipher.isBytecode = true
						}
					}
					if cache.SigUsesURLWrapper && len(cipher.playerJS) == 0 {
						_ = e.cacheManager.InvalidateCipherArtifacts()
						goto fetchFreshCipher
					}

					e.storeCipherCache(cipher, cache.ExpiresAt)
					e.warmNTransformAsync(ctx, cipher)
					if stale {
						e.refreshCipherAsync(ctx, videoID)
					}
					return cipher, nil
				}
			}
		}
	}

	// 3. Fetch fresh cipher (singleflighted).
fetchFreshCipher:
	v, err, _ := e.runtime.cipherGroup.Do("shared", func() (any, error) {
		return e.fetchAndCacheCipherContext(ctx, videoID)
	})
	if err != nil {
		return nil, err
	}
	return v.(*Cipher), nil
}

func (e *Extractor) ensureCipher(videoID string) (*Cipher, error) {
	return e.ensureCipherContext(context.Background(), videoID)
}

func (e *Extractor) ensureCipherContext(ctx context.Context, videoID string) (*Cipher, error) {
	if e.cipher != nil {
		return e.cipher, nil
	}

	e.cipherMu.Lock()
	defer e.cipherMu.Unlock()

	if e.cipher != nil {
		return e.cipher, nil
	}

	cipher, err := e.getCachedCipherContext(ctx, videoID)
	if err != nil {
		return nil, err
	}
	e.cipher = cipher
	return cipher, nil
}

// fetchAndCacheCipher fetches a new cipher and saves to both caches
func (e *Extractor) fetchAndCacheCipher(videoID string) (*Cipher, error) {
	return e.fetchAndCacheCipherContext(context.Background(), videoID)
}

func (e *Extractor) fetchAndCacheCipherContext(ctx context.Context, videoID string) (*Cipher, error) {
	// Try to use cached base.js path (saves ~150ms by skipping embed page)
	var cachedPath string
	var previousCache *CipherCache
	if e.cacheManager != nil {
		if cache, err := e.cacheManager.Load(); err == nil {
			previousCache = cache
		}
		if previousCache != nil && previousCache.BaseJSPath != "" {
			cachedPath = previousCache.BaseJSPath
		}
	}

	// Fetch new cipher (will try cached path first, fallback to embed page)
	// Use config.Origin for domain reuse (music.youtube.com for music mode)
	// This saves ~50-80ms by reusing existing HTTP/2 connection instead of new TLS handshake
	cipher, playerPath, err := NewCipherWithCachedPathContext(ctx, e.runtime, videoID, e.httpClient, cachedPath, e.config.Origin)
	if err != nil {
		return nil, err
	}
	if previousCache != nil {
		if previousCache.PlayerFingerprint != "" && cipher.playerFingerprint != "" && previousCache.PlayerFingerprint != cipher.playerFingerprint {
			cipher.warnings = append(cipher.warnings,
				fmt.Sprintf("player fingerprint changed from %s to %s; cached player may have been outdated", previousCache.PlayerFingerprint, cipher.playerFingerprint))
		}
		if previousCache.BaseJSPath != "" && previousCache.BaseJSPath != playerPath {
			cipher.warnings = append(cipher.warnings,
				fmt.Sprintf("player path changed from %s to %s; cache refreshed via slow path", previousCache.BaseJSPath, playerPath))
		}
	}
	e.absorbCipherWarnings(cipher)
	if cipher.canPrecomputeSignatureRuntime() {
		if err := cipher.ensureSignatureReady(); err != nil {
			cipher.warnings = append(cipher.warnings,
				fmt.Sprintf("failed to precompute signature runtime during cache warmup: %v", err))
			e.absorbCipherWarnings(cipher)
		}
	}

	if cipher.PlayerJSFetchMs > 0 {
		e.timings.PlayerJSFetchMs = cipher.PlayerJSFetchMs
	}
	if cipher.CipherAnalyzeMs > 0 {
		e.timings.CipherAnalyzeMs = cipher.CipherAnalyzeMs
	}
	if cipher.AnalyzeDetail != nil {
		e.timings.CipherDetail = cipher.AnalyzeDetail
	}

	expiry := time.Now().Add(cacheTTL)
	e.storeCipherCache(cipher, expiry)

	// Save to file cache (ignore errors - graceful degradation)
	if e.cacheManager != nil {
		cacheData := cipher.ToCache()
		cacheData.BaseJSPath = playerPath // Store base.js path for next time
		if cipher.sigUsesURLWrapper {
			if wrapperBytecode, err := cipher.wrapperRuntimeBytecode(); err == nil && len(wrapperBytecode) > 0 {
				cacheData.WrapperBuildID = currentWrapperBytecodeBuildID()
				_ = e.cacheManager.SaveWrapperRuntimeBytecode(wrapperBytecode)
			}
		}
		_ = e.cacheManager.Save(cacheData)
		// Also save player.js (compressed) for n-transform
		if len(cipher.playerJS) > 0 {
			_ = e.cacheManager.SavePlayerJS(cipher.playerJS)
		}
		if len(cipher.nRuntimeJS) > 0 {
			_ = e.cacheManager.SaveNRuntimeJS(cipher.nRuntimeJS)
		}
	}

	e.warmNTransformAsync(ctx, cipher)

	return cipher, nil
}

// invalidateCipherCache clears both in-memory and file caches
func (e *Extractor) invalidateCipherCache() {
	e.runtime.cipherCache.Lock()
	if e.runtime.cipherCache.cipher != nil {
		e.runtime.cipherCache.cipher.Close()
	}
	e.runtime.cipherCache.cipher = nil
	e.runtime.cipherCache.expiry = time.Time{}
	e.runtime.cipherCache.updated = time.Time{}
	e.runtime.cipherCache.Unlock()

	if e.cacheManager != nil {
		_ = e.cacheManager.InvalidateCipherArtifacts()
	}
}

// getStreamURL extracts the final stream URL, decrypting if necessary
func (e *Extractor) getStreamURL(videoID string, stream *Format) (string, error) {
	return e.getStreamURLContext(context.Background(), videoID, stream)
}

func (e *Extractor) getStreamURLContext(ctx context.Context, videoID string, stream *Format) (string, error) {
	// If URL is directly available (pre-signed), use it
	if stream.URL != "" {
		streamURL, err := e.unthrottleContext(ctx, videoID, stream.URL)
		if err != nil {
			return "", err
		}
		return e.applyPOToken(streamURL), nil
	}

	// Otherwise, decrypt the signature cipher
	if stream.SignatureCipher == "" {
		return "", fmt.Errorf("no URL or signature cipher available")
	}

	// Parse the cipher parameters
	params, err := url.ParseQuery(stream.SignatureCipher)
	if err != nil {
		return "", fmt.Errorf("failed to parse signature cipher: %w", err)
	}

	baseURL := params.Get("url")
	sig := params.Get("s")
	sigParam := params.Get("sp")
	if sigParam == "" {
		sigParam = "sig"
	}
	if baseURL == "" {
		return "", fmt.Errorf("signature cipher missing base URL")
	}

	if sig == "" {
		streamURL, err := e.unthrottleContext(ctx, videoID, baseURL)
		if err != nil {
			return "", err
		}
		return e.applyPOToken(streamURL), nil
	}

	// Initialize cipher if needed (using cache)
	if _, err := e.ensureCipherContext(ctx, videoID); err != nil {
		return "", fmt.Errorf("failed to initialize cipher: %w", err)
	}

	// Decrypt signature with fail-forward retry
	var sigDecryptStart time.Time
	if e.profile {
		sigDecryptStart = time.Now()
	}
	decryptedSig, err := e.decryptWithRetryContext(ctx, videoID, sig)
	if e.profile {
		e.timings.SigDecryptMs += time.Since(sigDecryptStart).Milliseconds()
	}
	if err != nil {
		return "", fmt.Errorf("failed to decrypt signature: %w", err)
	}
	// Only persist cache if the first-try succeeded.
	// If a retry happened, the retry cipher may be from a different player variant
	// (e.g. player_ias from /watch instead of player_embed from /embed).
	// Persisting that would poison the disk cache for the next CLI invocation.
	if !e.cipherRetried.Load() {
		e.persistCipherArtifacts()
	}

	// Add decrypted signature to URL
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	query := parsedURL.Query()
	query.Set(sigParam, decryptedSig)
	parsedURL.RawQuery = query.Encode()

	// Apply n-parameter transformation (throttle bypass)
	streamURL, err := e.unthrottleContext(ctx, videoID, parsedURL.String())
	if err != nil {
		return "", err
	}

	return e.applyPOToken(streamURL), nil
}

func (e *Extractor) applyPOToken(streamURL string) string {
	if !e.poMode || (e.poPlayerToken == "" && e.poURLToken == "") {
		return streamURL
	}
	token := e.poURLToken
	if token == "" {
		token = e.poPlayerToken
	}
	parsedURL, err := url.Parse(streamURL)
	if err != nil {
		return streamURL
	}
	q := parsedURL.Query()
	if q.Get("pot") == "" {
		q.Set("pot", token)
		parsedURL.RawQuery = q.Encode()
	}
	return parsedURL.String()
}

func (e *Extractor) ensurePOTokenContext(ctx context.Context, videoID string) (string, string, error) {
	if !e.poMode {
		return "", "", nil
	}
	if videoID == "" {
		return "", "", fmt.Errorf("missing video id")
	}

	if e.cacheManager != nil {
		if cache, err := e.cacheManager.LoadPOToken(videoID, e.visitorData, e.sessionIndex); err == nil && cache.PlayerToken != "" && cache.URLToken != "" {
			return cache.PlayerToken, cache.URLToken, nil
		}
	}

	t0 := time.Now()
	challenge, err := e.loadPOTokenChallengeContext(ctx, videoID)
	if err != nil {
		return "", "", err
	}
	if e.profile {
		e.timings.POChallengeMs = time.Since(t0).Milliseconds()
	}

	e.poChallenge = challenge
	t1 := time.Now()
	playerToken, urlToken, ttl, err := e.mintPOTokenContext(ctx, challenge)
	if err != nil {
		return "", "", err
	}
	if e.profile {
		e.timings.POMintMs = time.Since(t1).Milliseconds()
	}
	if ttl <= 0 {
		ttl = defaultPOTokenTTL
	}

	if e.cacheManager != nil {
		_ = e.cacheManager.SavePOToken(&POTokenCache{
			Token:        urlToken,
			PlayerToken:  playerToken,
			URLToken:     urlToken,
			VideoID:      videoID,
			VisitorData:  e.visitorData,
			SessionIndex: e.sessionIndex,
			ExpiresAt:    time.Now().Add(ttl),
		})
	}

	return playerToken, urlToken, nil
}

func (e *Extractor) loadPOTokenChallengeContext(ctx context.Context, videoID string) (*POTokenChallenge, error) {
	if challenge := e.extractWatchPagePOTokenChallengeContext(ctx, videoID); challenge != nil {
		return challenge, nil
	}
	return e.requestPOTokenChallengeViaAttGetContext(ctx, videoID)
}

func (e *Extractor) extractWatchPagePOTokenChallengeContext(ctx context.Context, videoID string) *POTokenChallenge {
	if e.config.Name != "WEB_MUSIC" {
		return nil
	}
	watchURL := fmt.Sprintf("https://music.youtube.com/watch?v=%s", videoID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, watchURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", e.config.UserAgent)
	req.Header.Set("Cookie", BuildCookieHeader(e.cookies))
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	bgChallenge := extractBGChallengeFromWatchPage(body)
	if len(bgChallenge) == 0 {
		return nil
	}

	return &POTokenChallenge{
		Source:       "watch",
		Webpage:      append([]byte(nil), body...),
		BGChallenge:  append([]byte(nil), bgChallenge...),
		VideoID:      videoID,
		VisitorData:  e.visitorData,
		SessionIndex: e.sessionIndex,
	}
}

func (e *Extractor) requestPOTokenChallengeViaAttGetContext(ctx context.Context, videoID string) (*POTokenChallenge, error) {
	client := InnertubeClient{
		HL:            "en",
		GL:            "US",
		ClientName:    e.config.Name,
		ClientVersion: e.config.Version,
		UserAgent:     e.config.UserAgent,
		TimeZone:      "UTC",
		UTCOffset:     0,
		VisitorData:   e.visitorData,
	}

	if e.config.DeviceMake != "" {
		client.DeviceMake = e.config.DeviceMake
	}
	if e.config.DeviceModel != "" {
		client.DeviceModel = e.config.DeviceModel
	}
	if e.config.Platform != "" {
		client.Platform = e.config.Platform
	}
	if e.config.OSName != "" {
		client.OSName = e.config.OSName
	}
	if e.config.OSVersion != "" {
		client.OSVersion = e.config.OSVersion
	}

	payload := map[string]any{
		"videoId":        videoID,
		"context":        map[string]any{"client": client},
		"engagementType": "ENGAGEMENT_TYPE_UNBOUND",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	attEndpoint := strings.Replace(e.config.APIEndpoint, "/player", "/att/get", 1)
	apiURL := fmt.Sprintf("%s?key=%s&prettyPrint=false", attEndpoint, e.config.APIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", e.config.UserAgent)
	req.Header.Set("Origin", e.config.Origin)
	req.Header.Set("Referer", e.config.Origin+"/")
	for k, v := range e.config.Headers {
		req.Header.Set(k, v)
	}
	if e.config.NeedsCookies && e.sapisid != "" {
		sapisidhash := GenerateSAPISIDHASH(e.sapisid, e.config.Origin)
		req.Header.Set("Authorization", sapisidhash)
		req.Header.Set("X-Origin", e.config.Origin)
		req.Header.Set("Cookie", BuildCookieHeader(e.cookies))
		if e.sessionIndex != "" {
			req.Header.Set("X-Goog-AuthUser", e.sessionIndex)
		}
		req.Header.Set("X-Youtube-Bootstrap-Logged-In", "true")
	}
	if e.visitorData != "" {
		req.Header.Set("X-Goog-Visitor-Id", e.visitorData)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, NewPOError(POFailureAttGetChallenge, "att/get request failed", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, NewPOError(POFailureAttGetChallenge, "failed reading att/get response", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, NewPOError(POFailureAttGetChallenge, fmt.Sprintf("att/get returned %d: %s", resp.StatusCode, string(respBody[:min(200, len(respBody))])), nil)
	}

	var raw any
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, NewPOError(POFailureAttGetChallenge, "failed to parse att/get response", err)
	}

	challenge := extractBGChallenge(raw)
	if challenge == nil {
		return nil, NewPOError(POFailureAttGetChallenge, "att/get response missing bgChallenge", nil)
	}
	return &POTokenChallenge{Source: "att/get", BGChallenge: challenge.BgChallenge, Challenge: *challenge, VideoID: videoID, VisitorData: e.visitorData, SessionIndex: e.sessionIndex}, nil
}

func (e *Extractor) mintPOTokenContext(ctx context.Context, challenge *POTokenChallenge) (string, string, time.Duration, error) {
	playerToken, urlToken, ttl, err := mintPOTokenWithEngine(ctx, e.runtime.GetEngineType(), challenge)
	if err != nil {
		return "", "", 0, NewPOError(POFailureRuntimeMint, "failed to mint po token", err)
	}
	return playerToken, urlToken, ttl, nil
}

func extractBGChallengeFromWatchPage(body []byte) json.RawMessage {
	if len(body) == 0 {
		return nil
	}

	if match := ytAtRRegex.FindSubmatch(body); len(match) >= 2 {
		quoted := string(match[1])
		if strings.HasPrefix(quoted, "'") {
			quoted = `"` + strings.Trim(strings.TrimPrefix(quoted, "'"), "'") + `"`
		}
		if rawJSON, err := strconv.Unquote(quoted); err == nil {
			var container map[string]any
			if err := json.Unmarshal([]byte(rawJSON), &container); err == nil {
				if raw, ok := container["bgChallenge"]; ok {
					if encoded, err := json.Marshal(raw); err == nil {
						return encoded
					}
				}
			}
		}
	}

	if match := ytAtNRegex.FindSubmatch(body); len(match) >= 2 {
		var container map[string]any
		if err := json.Unmarshal(match[1], &container); err == nil {
			if raw, ok := container["bgChallenge"]; ok {
				if encoded, err := json.Marshal(raw); err == nil {
					return encoded
				}
			}
		}
	}

	return nil
}

func extractPOToken(v any) string {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if strings.EqualFold(k, "poToken") {
				if s, ok := child.(string); ok {
					return s
				}
			}
			if tok := extractPOToken(child); tok != "" {
				return tok
			}
		}
	case []any:
		for _, child := range t {
			if tok := extractPOToken(child); tok != "" {
				return tok
			}
		}
	}
	return ""
}

func extractBGChallenge(v any) *BotGuardChallenge {
	switch t := v.(type) {
	case map[string]any:
		if raw, ok := t["bgChallenge"]; ok {
			data, err := json.Marshal(raw)
			if err != nil {
				return nil
			}
			challenge := &BotGuardChallenge{BgChallenge: data}
			if m, ok := raw.(map[string]any); ok {
				if s, ok := m["engagementType"].(string); ok {
					challenge.EngagementType = s
				}
				if s, ok := m["challengeToken"].(string); ok {
					challenge.ChallengeToken = s
				}
			}
			return challenge
		}
		for _, child := range t {
			if challenge := extractBGChallenge(child); challenge != nil {
				return challenge
			}
		}
	case []any:
		for _, child := range t {
			if challenge := extractBGChallenge(child); challenge != nil {
				return challenge
			}
		}
	}
	return nil
}

func extractPOTokenTTL(v any) time.Duration {
	seconds := extractTTLSeconds(v)
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func extractTTLSeconds(v any) int {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if strings.EqualFold(k, "expiresInSeconds") || strings.EqualFold(k, "ttlSeconds") {
				switch vv := child.(type) {
				case float64:
					return int(vv)
				case int:
					return vv
				case string:
					n, _ := strconv.Atoi(vv)
					if n > 0 {
						return n
					}
				}
			}
			if n := extractTTLSeconds(child); n > 0 {
				return n
			}
		}
	case []any:
		for _, child := range t {
			if n := extractTTLSeconds(child); n > 0 {
				return n
			}
		}
	}
	return 0
}

func (e *Extractor) warningsForResult() []string {
	if len(e.warnings) == 0 {
		return nil
	}
	out := make([]string, len(e.warnings))
	copy(out, e.warnings)
	return out
}

func (e *Extractor) absorbCipherWarnings(cipher *Cipher) {
	if cipher == nil {
		return
	}
	for _, warning := range cipher.Warnings() {
		duplicate := false
		for _, existing := range e.warnings {
			if existing == warning {
				duplicate = true
				break
			}
		}
		if !duplicate {
			e.warnings = append(e.warnings, warning)
		}
	}
}

func (e *Extractor) persistCipherArtifacts() {
	if e.cacheManager == nil || e.cipher == nil || len(e.cipher.playerJS) == 0 || !e.cipher.canPersistSignatureRuntime() {
		return
	}

	cacheData := e.cipher.ToCache()
	cacheData.BaseJSPath = e.cipher.playerURL
	if e.cipher.sigUsesURLWrapper {
		if wrapperBytecode, err := e.cipher.wrapperRuntimeBytecode(); err == nil && len(wrapperBytecode) > 0 {
			cacheData.WrapperBuildID = currentWrapperBytecodeBuildID()
			_ = e.cacheManager.SaveWrapperRuntimeBytecode(wrapperBytecode)
		}
	}
	_ = e.cacheManager.Save(cacheData)
	_ = e.cacheManager.SavePlayerJS(e.cipher.playerJS)
	if len(e.cipher.nRuntimeJS) > 0 {
		_ = e.cacheManager.SaveNRuntimeJS(e.cipher.nRuntimeJS)
	}

	e.runtime.cipherCache.Lock()
	defer e.runtime.cipherCache.Unlock()
	if time.Now().Before(e.runtime.cipherCache.expiry) {
		e.runtime.cipherCache.cipher = e.cipher
		e.runtime.cipherCache.updated = time.Now()
	}
}

func (e *Extractor) storeCipherCache(cipher *Cipher, expiry time.Time) {
	e.runtime.cipherCache.Lock()
	e.runtime.cipherCache.cipher = cipher
	e.runtime.cipherCache.expiry = expiry
	e.runtime.cipherCache.updated = time.Now()
	e.runtime.cipherCache.Unlock()
}

func (e *Extractor) refreshCipherAsync(parent context.Context, videoID string) {
	go func() {
		ctx, cancel := backgroundRefreshContext(parent, backgroundWarmupTTL)
		defer cancel()
		_, _, _ = e.runtime.cipherGroup.Do("shared", func() (any, error) {
			return e.fetchAndCacheCipherContext(ctx, videoID)
		})
	}()
}

// warmNTransformAsync warms the n-transform JS engine in the background.
// Signature runtime prewarm is done synchronously before cipher is sent on cipherCh.
func (e *Extractor) warmNTransformAsync(parent context.Context, cipher *Cipher) {
	if cipher == nil {
		return
	}
	go func() {
		ctx, cancel := backgroundRefreshContext(parent, backgroundWarmupTTL)
		defer cancel()
		_ = cipher.WarmNTransformEngineContext(ctx)
	}()
}

// decryptWithRetry attempts decryption, retrying once with fresh cipher if it fails
func (e *Extractor) decryptWithRetry(videoID, sig string) (string, error) {
	return e.decryptWithRetryContext(context.Background(), videoID, sig)
}

func (e *Extractor) decryptWithRetryContext(ctx context.Context, videoID, sig string) (string, error) {
	result, err := e.cipher.DecryptSignature(sig)
	if err == nil {
		return result, nil
	}
	useWatchFirst := strings.Contains(err.Error(), "url wrapper") || strings.Contains(err.Error(), "wrapper function not found")

	maxRetries := 1
	if e.cipher != nil && e.cipher.sigUsesURLWrapper {
		maxRetries = 2
	}

	// Fail-forward: retry with fresh cipher when live player variants are flaky.
	e.warnings = append(e.warnings, fmt.Sprintf("sig decrypt failed (will retry): %v [fn=%s jsCodeLen=%d playerJSLen=%d]",
		err, e.cipher.sigFunctionName, len(e.cipher.jsCode), len(e.cipher.playerJS)))
	for attempt := 0; attempt < maxRetries; attempt++ {
		oldCipher := e.cipher
		e.cipherRetried.Store(true)
		e.invalidateCipherCache()
		e.cipherMu.Lock()
		e.cipher = nil
		e.cipherMu.Unlock()

		// Fetch fresh cipher
		var newCipher *Cipher
		var fetchErr error
		if useWatchFirst && attempt == 0 {
			playerBaseURL := e.config.Origin
			if oldCipher != nil && oldCipher.playerURL != "" {
				playerBaseURL = oldCipher.playerURL
			}
			newCipher, _, fetchErr = fetchPlayerJSWatchFirstCipherContext(ctx, e.runtime, videoID, e.httpClient, playerBaseURL, e.cacheManager)
		} else {
			newCipher, fetchErr = e.getCachedCipherContext(ctx, videoID)
		}
		if fetchErr != nil {
			return "", fmt.Errorf("retry failed: %w", fetchErr)
		}
		e.cipherMu.Lock()
		e.cipher = newCipher
		e.cipherMu.Unlock()

		// Retry decryption
		result, err = newCipher.DecryptSignature(sig)
		if err == nil {
			return result, nil
		}
		if attempt+1 < maxRetries {
			e.warnings = append(e.warnings, fmt.Sprintf("sig decrypt retry %d failed: %v [fn=%s jsCodeLen=%d playerJSLen=%d]",
				attempt+1, err, newCipher.sigFunctionName, len(newCipher.jsCode), len(newCipher.playerJS)))
		}
	}

	return "", err
}

// unthrottle applies the n-parameter transformation to bypass throttling
func (e *Extractor) unthrottle(videoID, streamURL string) (string, error) {
	return e.unthrottleContext(context.Background(), videoID, streamURL)
}

func (e *Extractor) unthrottleContext(ctx context.Context, videoID, streamURL string) (string, error) {
	parsedURL, err := url.Parse(streamURL)
	if err != nil {
		return "", err
	}

	query := parsedURL.Query()
	nParam := query.Get("n")
	if nParam == "" {
		// No n-parameter, return as-is
		return streamURL, nil
	}

	cipher, err := e.ensureCipherContext(ctx, videoID)
	if err != nil {
		return streamURL, nil
	}

	// Transform n-parameter
	var transformStart time.Time
	if e.profile {
		transformStart = time.Now()
	}
	transformedN, err := cipher.TransformNContext(ctx, nParam)
	if e.profile {
		e.timings.NTransformMs += time.Since(transformStart).Milliseconds()
	}
	if err != nil {
		// Log but don't fail - some videos work without n transform
		return streamURL, nil
	}

	query.Set("n", transformedN)
	parsedURL.RawQuery = query.Encode()

	return parsedURL.String(), nil
}

// Helper regex patterns (compiled once at package level)
var (
	videoIDRegex      = regexp.MustCompile(`(?:v=|/)([a-zA-Z0-9_-]{11})`)
	validVideoIDRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{11}$`)
)

// ExtractVideoID extracts the video ID from a URL or returns the ID if already valid
func ExtractVideoID(input string) string {
	input = strings.TrimSpace(input)

	// If it's already a valid video ID (11 chars, alphanumeric + _-)
	if len(input) == 11 && validVideoIDRegex.MatchString(input) {
		return input
	}

	// Try to extract from URL
	matches := videoIDRegex.FindStringSubmatch(input)
	if len(matches) >= 2 {
		return matches[1]
	}

	return input // Return as-is, let the API handle validation
}

// extractSubtitles extracts and filters subtitles from player response captions.
// langs behavior:
//   - nil or empty: use defaultSubtitleLangs
//   - contains "all": return all available languages
//   - otherwise: filter to specified languages only
func extractSubtitles(captions *CaptionsRenderer, langs []string) []Subtitle {
	if captions == nil || captions.PlayerCaptionsTracklistRenderer == nil {
		return nil
	}

	tracks := captions.PlayerCaptionsTracklistRenderer.CaptionTracks
	if len(tracks) == 0 {
		return nil
	}

	// Determine which languages to include
	fetchAll := false
	if len(langs) == 0 {
		langs = defaultSubtitleLangs
	} else {
		for _, l := range langs {
			if l == "all" {
				fetchAll = true
				break
			}
		}
	}

	// Build language set for O(1) lookup
	var langSet map[string]bool
	if !fetchAll {
		langSet = make(map[string]bool, len(langs))
		for _, l := range langs {
			langSet[l] = true
		}
	}

	subtitles := make([]Subtitle, 0, len(tracks))
	for _, track := range tracks {
		if track.BaseURL == "" {
			continue
		}

		// Filter by language if not fetching all
		if !fetchAll && !langSet[track.LanguageCode] {
			continue
		}

		// Build WebVTT URL using net/url for safety
		u, err := url.Parse(track.BaseURL)
		if err != nil {
			continue
		}
		q := u.Query()
		q.Set("fmt", "vtt")
		u.RawQuery = q.Encode()

		// Get display name
		name := track.Name.SimpleText
		if name == "" {
			name = track.LanguageCode
		}

		subtitles = append(subtitles, Subtitle{
			URL:    u.String(),
			Lang:   track.LanguageCode,
			Name:   name,
			IsAuto: track.Kind == "asr",
		})
	}

	// Sort: manual before auto, then alphabetically by language
	sort.Slice(subtitles, func(i, j int) bool {
		if subtitles[i].IsAuto != subtitles[j].IsAuto {
			return !subtitles[i].IsAuto // manual first
		}
		return subtitles[i].Lang < subtitles[j].Lang
	})

	return subtitles
}
