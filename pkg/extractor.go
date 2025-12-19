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

// Extractor handles YouTube stream extraction
type Extractor struct {
	config        ClientConfig
	cookies       []*http.Cookie
	httpClient    *http.Client
	sapisid       string
	cipher        *Cipher       // lazy initialized, only for music mode
	cacheManager  *CacheManager // file-based persistent cache
	cipherRetried atomic.Bool   // track if we've already retried with fresh cipher
}

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

// Extract gets the best audio stream URL for a video
func (e *Extractor) Extract(videoID string) (*Result, error) {
	// 1. Call innertube API
	playerResp, err := e.callPlayerAPI(videoID)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}

	// 2. Check playability
	if playerResp.PlayabilityStatus.Status != "OK" {
		return nil, fmt.Errorf("video not playable: %s - %s",
			playerResp.PlayabilityStatus.Status,
			playerResp.PlayabilityStatus.Reason)
	}

	// 3. Find best audio stream
	stream := e.findBestAudioStream(playerResp.StreamingData.AdaptiveFormats)
	if stream == nil {
		return nil, fmt.Errorf("no audio stream found")
	}

	// 4. Get stream URL (may need decryption)
	streamURL, err := e.getStreamURL(videoID, stream)
	if err != nil {
		return nil, fmt.Errorf("failed to get stream URL: %w", err)
	}

	return &Result{
		URL:      streamURL,
		Itag:     stream.Itag,
		Bitrate:  stream.Bitrate,
		MimeType: stream.MimeType,
		Title:    playerResp.VideoDetails.Title,
		Author:   playerResp.VideoDetails.Author,
	}, nil
}

// ExtractVideo gets video and audio stream URLs (for MPV playback)
func (e *Extractor) ExtractVideo(videoID string) (*VideoResult, error) {
	playerResp, err := e.callPlayerAPI(videoID)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
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

	// ANDROID_VR returns direct URLs (no cipher)
	if video.URL == "" || audio.URL == "" {
		return nil, fmt.Errorf("no direct URLs - cipher required but video mode doesn't support it")
	}

	return &VideoResult{
		VideoURL:  video.URL,
		AudioURL:  audio.URL,
		VideoItag: video.Itag,
		AudioItag: audio.Itag,
		Width:     video.Width,
		Height:    video.Height,
		Title:     playerResp.VideoDetails.Title,
		Author:    playerResp.VideoDetails.Author,
	}, nil
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

	reqBody := InnertubeRequest{
		VideoID: videoID,
		Context: InnertubeContext{
			Client: client,
		},
		ContentCheckOK: true,
		RacyCheckOK:    true,
		PlaybackContext: &PlaybackContext{
			ContentPlaybackContext: ContentPlaybackContext{
				HTML5Preference: "HTML5_PREF_WANTS",
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

	// Try to find premium itags first
	for _, targetItag := range PremiumAudioItags {
		for i := range audioFormats {
			if audioFormats[i].Itag == targetItag {
				return &audioFormats[i]
			}
		}
	}

	// Fallback: sort by bitrate and return highest
	sort.Slice(audioFormats, func(i, j int) bool {
		return audioFormats[i].Bitrate > audioFormats[j].Bitrate
	})

	return &audioFormats[0]
}

// findBestVideoStream finds the highest quality video stream
func (e *Extractor) findBestVideoStream(formats []Format) *Format {
	var videoFormats []Format
	for _, f := range formats {
		if strings.HasPrefix(f.MimeType, "video/") && f.Height > 0 {
			videoFormats = append(videoFormats, f)
		}
	}

	if len(videoFormats) == 0 {
		return nil
	}

	// Try preferred itags first
	for _, targetItag := range VideoItags {
		for i := range videoFormats {
			if videoFormats[i].Itag == targetItag {
				return &videoFormats[i]
			}
		}
	}

	// Fallback: sort by height and return highest
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

// fetchAndCacheCipher fetches a new cipher and saves to both caches
func (e *Extractor) fetchAndCacheCipher(videoID string) (*Cipher, error) {
	cipherCache.Lock()
	defer cipherCache.Unlock()

	// Double-check after acquiring write lock
	if cipherCache.cipher != nil && time.Now().Before(cipherCache.expiry) {
		return cipherCache.cipher, nil
	}

	// Fetch new cipher
	cipher, _, err := NewCipherWithURL(videoID, e.httpClient)
	if err != nil {
		return nil, err
	}

	// Save to in-memory cache
	cipherCache.cipher = cipher
	cipherCache.expiry = time.Now().Add(cacheTTL)

	// Save to file cache (ignore errors - graceful degradation)
	if e.cacheManager != nil {
		_ = e.cacheManager.Save(cipher.ToCache())
	}

	return cipher, nil
}

// invalidateCipherCache clears both in-memory and file caches
func (e *Extractor) invalidateCipherCache() {
	cipherCache.Lock()
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
		return e.unthrottle(videoID, stream.URL)
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

	// Initialize cipher if needed (using cache)
	if e.cipher == nil {
		e.cipher, err = e.getCachedCipher(videoID)
		if err != nil {
			return "", fmt.Errorf("failed to initialize cipher: %w", err)
		}
	}

	// Decrypt signature with fail-forward retry
	decryptedSig, err := e.decryptWithRetry(videoID, sig)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt signature: %w", err)
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
	return e.unthrottle(videoID, parsedURL.String())
}

// decryptWithRetry attempts decryption, retrying once with fresh cipher if it fails
func (e *Extractor) decryptWithRetry(videoID, sig string) (string, error) {
	result, err := e.cipher.DecryptSignature(sig)
	if err == nil {
		return result, nil
	}

	// Fail-forward: retry once with fresh cipher (atomic to prevent races)
	if !e.cipherRetried.Swap(true) {
		e.invalidateCipherCache()
		e.cipher = nil

		// Fetch fresh cipher
		newCipher, fetchErr := e.getCachedCipher(videoID)
		if fetchErr != nil {
			return "", fmt.Errorf("retry failed: %w", fetchErr)
		}
		e.cipher = newCipher

		// Retry decryption
		return e.cipher.DecryptSignature(sig)
	}

	return "", err
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

	// Initialize cipher if needed (using cache)
	if e.cipher == nil {
		e.cipher, err = e.getCachedCipher(videoID)
		if err != nil {
			return "", fmt.Errorf("failed to initialize cipher: %w", err)
		}
	}

	// Transform n-parameter
	transformedN, err := e.cipher.TransformN(nParam)
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
