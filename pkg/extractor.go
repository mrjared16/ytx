package ytx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Global cipher cache (shared across Extractor instances for bulk operations)
// This provides in-memory caching within a single process run
var cipherCache struct {
	sync.RWMutex
	cipher *Cipher
	expiry time.Time
}

// Global visitorData cache (valid for ~30 minutes)
var visitorDataCache struct {
	sync.RWMutex
	data         string
	sessionIndex string
	delegatedSID string
	isAuth       bool
	expiry       time.Time
}

const visitorDataTTL = 30 * time.Minute

// Extractor handles YouTube stream extraction
type Extractor struct {
	config           ClientConfig
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
}

// defaultSubtitleLangs are fetched when --subs is used without --sub-langs
// Users can override via --sub-langs flag
var defaultSubtitleLangs = []string{"en"}

// NewExtractor creates a new extractor for the given mode
func NewExtractor(mode ClientMode, cookieFile string) (*Extractor, error) {
	config := GetClientConfig(mode)

	ext := &Extractor{
		config: config,
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

	// Initialize file-based cache manager (graceful degradation if fails)
	if cm, err := NewCacheManager(); err == nil {
		ext.cacheManager = cm
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
// Uses a global cache to avoid redundant fetches across bulk operations
func (e *Extractor) fetchVisitorData(videoID string) error {
	// Check instance cache first
	if e.visitorData != "" {
		if !e.config.NeedsCookies || e.sessionIndex != "" || e.delegatedSID != "" {
			return nil
		}
	}

	// Check global cache (for bulk operations and cross-extractor reuse)
	visitorDataCache.RLock()
	if visitorDataCache.data != "" && time.Now().Before(visitorDataCache.expiry) {
		e.visitorData = visitorDataCache.data
		e.sessionIndex = visitorDataCache.sessionIndex
		e.delegatedSID = visitorDataCache.delegatedSID
		isAuth := visitorDataCache.isAuth
		visitorDataCache.RUnlock()
		if !e.config.NeedsCookies || isAuth {
			return nil
		}
	} else {
		visitorDataCache.RUnlock()
	}

	if e.config.NeedsCookies {
		if err := e.fetchMusicVisitorData(videoID); err != nil {
			return err
		}
		e.cacheVisitorData()
		return nil
	}

	// Use WEB client to get visitorData - this is critical!
	// ANDROID_VR returns LOGIN_REQUIRED without visitorData,
	// but WEB client returns visitorData that then works with ANDROID_VR
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
		return err
	}

	apiURL := fmt.Sprintf("%s?key=%s&prettyPrint=false", WEBAPIEndpoint, WEBAPIKey)
	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(jsonBody))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", WEBUserAgent)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var playerResp PlayerResponse
	if err := json.NewDecoder(resp.Body).Decode(&playerResp); err != nil {
		return err
	}

	if playerResp.ResponseContext.VisitorData != "" {
		e.visitorData = playerResp.ResponseContext.VisitorData
		e.cacheVisitorData()
	}

	return nil
}

// cacheVisitorData stores visitorData in the global cache
func (e *Extractor) cacheVisitorData() {
	if e.visitorData == "" {
		return
	}
	visitorDataCache.Lock()
	visitorDataCache.data = e.visitorData
	visitorDataCache.sessionIndex = e.sessionIndex
	visitorDataCache.delegatedSID = e.delegatedSID
	visitorDataCache.isAuth = e.config.NeedsCookies
	visitorDataCache.expiry = time.Now().Add(visitorDataTTL)
	visitorDataCache.Unlock()
}

// musicVisitorDataRegex extracts visitorData from music.youtube.com HTML
var (
	musicVisitorDataRegex = regexp.MustCompile(`"visitorData"\s*:\s*"([^"]+)"`)
	sessionIndexRegex     = regexp.MustCompile(`"SESSION_INDEX"\s*:\s*"([^"]+)"`)
	delegatedSIDRegex     = regexp.MustCompile(`"DELEGATED_SESSION_ID"\s*:\s*"([^"]+)"`)
	datasyncIDRegex       = regexp.MustCompile(`"DATASYNC_ID"\s*:\s*"([^"]+)"`)
)

// fetchMusicVisitorData gets visitorData from music.youtube.com page
func (e *Extractor) fetchMusicVisitorData(videoID string) error {
	watchURL := fmt.Sprintf("https://music.youtube.com/watch?v=%s", videoID)
	req, err := http.NewRequest("GET", watchURL, nil)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", e.config.UserAgent)
	req.Header.Set("Cookie", BuildCookieHeader(e.cookies))

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	matches := musicVisitorDataRegex.FindSubmatch(body)
	if len(matches) >= 2 {
		e.visitorData = string(matches[1])
	}

	// Extract Session Index (X-Goog-AuthUser)
	matches = sessionIndexRegex.FindSubmatch(body)
	if len(matches) >= 2 {
		e.sessionIndex = string(matches[1])
	}

	// Extract Delegated Session ID (X-Goog-PageId)
	// Try DELEGATED_SESSION_ID first
	matches = delegatedSIDRegex.FindSubmatch(body)
	if len(matches) >= 2 {
		e.delegatedSID = string(matches[1])
	} else {
		// Fallback: try parsing DATASYNC_ID (format: delegated||user)
		matches = datasyncIDRegex.FindSubmatch(body)
		if len(matches) >= 2 {
			parts := strings.Split(string(matches[1]), "||")
			if len(parts) >= 2 && parts[0] != "" {
				e.delegatedSID = parts[0]
			}
		}
	}

	return nil
}

// Extract gets the best audio stream URL for a video
func (e *Extractor) Extract(videoID string) (*Result, error) {
	e.warnings = nil
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

	// Fetch visitorData in parallel
	go func() {
		err := e.fetchVisitorData(videoID)
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
		cipher, err := e.getCachedCipher(videoID)
		prewarmMs := int64(0)
		if e.profile {
			prewarmMs = time.Since(prewarmStart).Milliseconds()
		}
		if err == nil && cipher != nil {
			// Signal STS as soon as cipher is available (even from cache)
			select {
			case stsCh <- cipher.SignatureTimestamp():
			default:
			}

			// (Removed QuickJS pre-warm here to avoid CGO OS-thread affinity corruption)
			// ensureSignatureReady() populates jsCode (lazy init)
			_ = cipher.ensureSignatureReady()
		} else {
			select {
			case stsCh <- 0:
			default:
			}
		}
		cipherCh <- cipherResult{cipher: cipher, err: err, prewarmMs: prewarmMs}
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

	// Wait for STS only (not full cipher) — allows API call to start sooner
	var stsWaitStart time.Time
	if e.profile {
		stsWaitStart = time.Now()
	}
	sts := <-stsCh
	if e.profile {
		e.timings.STSWaitMs = time.Since(stsWaitStart).Milliseconds()
	}

	// Call innertube API with STS (cipher may still be finishing heavy init)
	var apiStart time.Time
	if e.profile {
		apiStart = time.Now()
	}
	e.earlySTSOverride = sts
	playerResp, err := e.callPlayerAPI(videoID)
	e.earlySTSOverride = 0
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
	streamURL, err := e.getStreamURL(videoID, stream)
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
	if err := e.fetchVisitorData(videoID); err != nil {
		// Non-fatal, continue without it
	}
	if e.profile {
		e.timings.VisitorDataMs = time.Since(visitorStart).Milliseconds()
	}

	var apiStart time.Time
	if e.profile {
		apiStart = time.Now()
	}
	playerResp, err := e.callPlayerAPI(videoID)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	if e.profile {
		e.timings.PlayerAPIMs = time.Since(apiStart).Milliseconds()
	}

	if playerResp.PlayabilityStatus.Status != "OK" {
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

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	// Build request
	apiURL := fmt.Sprintf("%s?key=%s&prettyPrint=false", e.config.APIEndpoint, e.config.APIKey)
	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(jsonBody))
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
	// 1. Try in-memory cache first (fast path for bulk operations)
	cipherCache.RLock()
	if cipherCache.cipher != nil && time.Now().Before(cipherCache.expiry) {
		c := cipherCache.cipher
		cipherCache.RUnlock()
		return c, nil
	}
	cipherCache.RUnlock()

	// 2. Try file cache
	if e.cacheManager != nil {
		if cache, err := e.cacheManager.Load(); err == nil && e.cacheManager.IsValid(cache) {
			cipher := NewCipherFromCache(cache)
			// Load player.js from separate cache file
			if playerJS, err := e.cacheManager.LoadPlayerJS(); err == nil {
				cipher.playerJS = playerJS
			}
			// Also populate in-memory cache
			cipherCache.Lock()
			cipherCache.cipher = cipher
			cipherCache.expiry = cache.ExpiresAt
			cipherCache.Unlock()
			return cipher, nil
		}
	}

	// 3. Fetch fresh cipher
	return e.fetchAndCacheCipher(videoID)
}

func (e *Extractor) ensureCipher(videoID string) (*Cipher, error) {
	if e.cipher != nil {
		return e.cipher, nil
	}

	e.cipherMu.Lock()
	defer e.cipherMu.Unlock()

	if e.cipher != nil {
		return e.cipher, nil
	}

	cipher, err := e.getCachedCipher(videoID)
	if err != nil {
		return nil, err
	}
	e.cipher = cipher
	return cipher, nil
}

// fetchAndCacheCipher fetches a new cipher and saves to both caches
func (e *Extractor) fetchAndCacheCipher(videoID string) (*Cipher, error) {
	cipherCache.Lock()
	defer cipherCache.Unlock()

	// Double-check after acquiring write lock
	if cipherCache.cipher != nil && time.Now().Before(cipherCache.expiry) {
		return cipherCache.cipher, nil
	}

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
	cipher, playerPath, err := NewCipherWithCachedPath(videoID, e.httpClient, cachedPath, e.config.Origin)
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
	if cipher.sigFunctionName != "" {
		if err := cipher.ensureSignatureReady(); err != nil {
			cipher.warnings = append(cipher.warnings,
				fmt.Sprintf("failed to precompute signature runtime during cache warmup: %v", err))
			e.absorbCipherWarnings(cipher)
		}
	}

	// Save to in-memory cache
	cipherCache.cipher = cipher
	cipherCache.expiry = time.Now().Add(cacheTTL)

	// Save to file cache (ignore errors - graceful degradation)
	if e.cacheManager != nil {
		cacheData := cipher.ToCache()
		cacheData.BaseJSPath = playerPath // Store base.js path for next time
		_ = e.cacheManager.Save(cacheData)
		// Also save player.js (compressed) for n-transform
		if len(cipher.playerJS) > 0 {
			_ = e.cacheManager.SavePlayerJS(cipher.playerJS)
		}
	}

	return cipher, nil
}

// invalidateCipherCache clears both in-memory and file caches
func (e *Extractor) invalidateCipherCache() {
	cipherCache.Lock()
	if cipherCache.cipher != nil {
		cipherCache.cipher.Close()
	}
	cipherCache.cipher = nil
	cipherCache.expiry = time.Time{}
	cipherCache.Unlock()

	if e.cacheManager != nil {
		_ = e.cacheManager.Invalidate()
	}
}

// getStreamURL extracts the final stream URL, decrypting if necessary
func (e *Extractor) getStreamURL(videoID string, stream *Format) (string, error) {
	// If URL is directly available (pre-signed), use it
	if stream.URL != "" {
		streamURL, err := e.unthrottle(videoID, stream.URL)
		if err != nil {
			return "", err
		}
		return streamURL, nil
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
		streamURL, err := e.unthrottle(videoID, baseURL)
		if err != nil {
			return "", err
		}
		return streamURL, nil
	}

	// Initialize cipher if needed (using cache)
	if _, err := e.ensureCipher(videoID); err != nil {
		return "", fmt.Errorf("failed to initialize cipher: %w", err)
	}

	// Decrypt signature with fail-forward retry
	var sigDecryptStart time.Time
	if e.profile {
		sigDecryptStart = time.Now()
	}
	decryptedSig, err := e.decryptWithRetry(videoID, sig)
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
	streamURL, err := e.unthrottle(videoID, parsedURL.String())
	if err != nil {
		return "", err
	}

	return streamURL, nil
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
	if e.cacheManager == nil || e.cipher == nil || len(e.cipher.playerJS) == 0 || e.cipher.jsCode == "" {
		return
	}
	cacheData := e.cipher.ToCache()
	cacheData.BaseJSPath = e.cipher.playerURL
	_ = e.cacheManager.Save(cacheData)
	_ = e.cacheManager.SavePlayerJS(e.cipher.playerJS)
	cipherCache.Lock()
	defer cipherCache.Unlock()
	if time.Now().Before(cipherCache.expiry) {
		cipherCache.cipher = e.cipher
	}
}

// decryptWithRetry attempts decryption, retrying once with fresh cipher if it fails
func (e *Extractor) decryptWithRetry(videoID, sig string) (string, error) {
	result, err := e.cipher.DecryptSignature(sig)
	if err == nil {
		return result, nil
	}

	// Fail-forward: retry once with fresh cipher (atomic to prevent races)
	e.warnings = append(e.warnings, fmt.Sprintf("sig decrypt failed (will retry): %v [fn=%s jsCodeLen=%d playerJSLen=%d]",
		err, e.cipher.sigFunctionName, len(e.cipher.jsCode), len(e.cipher.playerJS)))
	if !e.cipherRetried.Swap(true) {
		e.invalidateCipherCache()
		e.cipherMu.Lock()
		e.cipher = nil
		e.cipherMu.Unlock()

		// Fetch fresh cipher
		newCipher, fetchErr := e.getCachedCipher(videoID)
		if fetchErr != nil {
			return "", fmt.Errorf("retry failed: %w", fetchErr)
		}
		e.cipherMu.Lock()
		e.cipher = newCipher
		e.cipherMu.Unlock()

		// Retry decryption
		return newCipher.DecryptSignature(sig)
	}

	return result, nil
}

// unthrottle applies the n-parameter transformation to bypass throttling
func (e *Extractor) unthrottle(videoID, streamURL string) (string, error) {
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

	cipher, err := e.ensureCipher(videoID)
	if err != nil {
		return streamURL, nil
	}

	// Transform n-parameter
	var transformStart time.Time
	if e.profile {
		transformStart = time.Now()
	}
	transformedN, err := cipher.TransformN(nParam)
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
