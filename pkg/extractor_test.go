package ytx

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TimingToleranceMs allows for network variance in timing comparisons
const TimingToleranceMs = 150

// PerfRegressionMinBudgetMs is the minimum extra slowdown budget allowed before
// failing strict regression checks. This keeps manual perf checks useful without
// making normal network variance fail the suite.
const PerfRegressionMinBudgetMs int64 = 250

// TestResult captures timing and results for a single extraction
type TestResult struct {
	Mode          string        `json:"mode"`
	CacheState    string        `json:"cache_state,omitempty"`
	VideoID       string        `json:"video_id"`
	Success       bool          `json:"success"`
	Error         string        `json:"error,omitempty"`
	Itag          int           `json:"itag,omitempty"`
	Bitrate       int           `json:"bitrate,omitempty"`
	QualityLabel  string        `json:"quality_label,omitempty"`
	HTTPStatus    int           `json:"http_status,omitempty"`
	NTransformed  bool          `json:"n_transformed,omitempty"`
	Timings       TimingMetrics `json:"timings"`
	TotalDuration string        `json:"total_duration"`
}

// TimingMetrics captures duration of each stage
type TimingMetrics struct {
	CipherFetch   string `json:"cipher_fetch,omitempty"`
	APICall       string `json:"api_call,omitempty"`
	NTransform    string `json:"n_transform,omitempty"`
	TotalExtract  string `json:"total_extract"`
	URLValidation string `json:"url_validation,omitempty"`
	// Profiled timings in milliseconds
	VisitorDataMs   int64 `json:"visitor_data_ms,omitempty"`
	STSWaitMs       int64 `json:"sts_wait_ms,omitempty"`
	PlayerAPIMs     int64 `json:"player_api_ms,omitempty"`
	CipherInitMs    int64 `json:"cipher_init_ms,omitempty"`
	CipherPrewarmMs int64 `json:"cipher_prewarm_ms,omitempty"`
	SigDecryptMs    int64 `json:"sig_decrypt_ms,omitempty"`
	NTransformMs    int64 `json:"n_transform_ms,omitempty"`
	OtherMs         int64 `json:"other_ms,omitempty"`
	TotalMs         int64 `json:"total_ms,omitempty"`
}

// GoldenImage represents the expected baseline for regression testing
type GoldenImage struct {
	GeneratedAt string       `json:"generated_at"`
	Version     int          `json:"version"`
	Results     []TestResult `json:"results"`
}

// Test video IDs - using popular videos that are unlikely to be deleted
var testVideoIDs = []string{
	"dQw4w9WgXcQ", // Rick Astley - Never Gonna Give You Up (very stable)
}

// goldenImagePath returns the path to the golden image file
func goldenImagePath() string {
	return filepath.Join("testdata", "golden_image.json")
}

// loadGoldenImage loads the existing golden image, if any
func loadGoldenImage() (*GoldenImage, error) {
	data, err := os.ReadFile(goldenImagePath())
	if err != nil {
		return nil, err
	}
	var golden GoldenImage
	if err := json.Unmarshal(data, &golden); err != nil {
		return nil, err
	}
	return &golden, nil
}

// saveGoldenImage saves the golden image to testdata
func saveGoldenImage(golden *GoldenImage) error {
	if err := os.MkdirAll("testdata", 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(goldenImagePath(), data, 0644)
}

// withinTolerance checks if actual timing is within tolerance of expected
func withinTolerance(actual, expected, toleranceMs int64) bool {
	diff := math.Abs(float64(actual - expected))
	return diff <= float64(toleranceMs)
}

func perfRegressionBudget(expected int64) int64 {
	relativeBudget := int64(math.Ceil(float64(expected) * 0.35))
	if relativeBudget < PerfRegressionMinBudgetMs {
		return PerfRegressionMinBudgetMs
	}
	return relativeBudget
}

// compareResults compares actual results against golden baseline
func compareResults(t *testing.T, actual, expected []TestResult) {
	t.Helper()
	strictPerfRegression := os.Getenv("STRICT_REGRESSION") == "1"
	if len(actual) != len(expected) {
		t.Fatalf("result count mismatch: got %d want %d", len(actual), len(expected))
	}

	for i, exp := range expected {
		if i >= len(actual) {
			t.Errorf("Missing result %d (%s/%s)", i, exp.Mode, exp.VideoID)
			continue
		}
		act := actual[i]

		// Compare non-timing fields
		if act.Mode != exp.Mode {
			t.Errorf("Result %d: mode mismatch: got %s, want %s", i, act.Mode, exp.Mode)
		}
		if act.CacheState != exp.CacheState {
			t.Errorf("Result %d: cache_state mismatch: got %s, want %s", i, act.CacheState, exp.CacheState)
		}
		if act.VideoID != exp.VideoID {
			t.Errorf("Result %d: videoID mismatch: got %s, want %s", i, act.VideoID, exp.VideoID)
		}
		if act.Success != exp.Success {
			t.Errorf("Result %d: success mismatch: got %v, want %v (error: %s)", i, act.Success, exp.Success, act.Error)
		}
		if act.HTTPStatus != exp.HTTPStatus && exp.Success {
			t.Errorf("Result %d: HTTP status mismatch: got %d, want %d", i, act.HTTPStatus, exp.HTTPStatus)
		}
		if act.Itag != exp.Itag && exp.Success {
			t.Errorf("Result %d: itag mismatch: got %d, want %d", i, act.Itag, exp.Itag)
		}
		if act.Bitrate != exp.Bitrate && exp.Bitrate != 0 {
			t.Errorf("Result %d: bitrate mismatch: got %d, want %d", i, act.Bitrate, exp.Bitrate)
		}
		if act.QualityLabel != exp.QualityLabel && exp.QualityLabel != "" {
			t.Errorf("Result %d: quality mismatch: got %s, want %s", i, act.QualityLabel, exp.QualityLabel)
		}
		if act.NTransformed != exp.NTransformed && exp.Success {
			t.Errorf("Result %d: n_transform mismatch: got %v, want %v", i, act.NTransformed, exp.NTransformed)
		}

		// Compare timings with tolerance (only if both have profiled data)
		if exp.Timings.TotalMs > 0 && act.Timings.TotalMs > 0 {
			if !withinTolerance(act.Timings.TotalMs, exp.Timings.TotalMs, TimingToleranceMs) {
				t.Logf("Result %d: timing outside tolerance: got %dms, want %dms (tolerance: %dms)",
					i, act.Timings.TotalMs, exp.Timings.TotalMs, TimingToleranceMs)
				// Log as warning, not error - network variance is expected
			}

			if strictPerfRegression && act.Timings.TotalMs > exp.Timings.TotalMs {
				budget := perfRegressionBudget(exp.Timings.TotalMs)
				if act.Timings.TotalMs-exp.Timings.TotalMs > budget {
					t.Errorf("Result %d: significant slowdown detected: got %dms, want <= %dms (baseline=%dms, budget=%dms)",
						i, act.Timings.TotalMs, exp.Timings.TotalMs+budget, exp.Timings.TotalMs, budget)
				}
			}
		}
	}
}

// TestExtractorRegression is the main regression test
func TestExtractorRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping regression test in short mode")
	}

	// Create isolated temp directory for this test
	tmpDir := t.TempDir()

	results := make([]TestResult, 0)

	for _, videoID := range testVideoIDs {
		videoCacheDir := filepath.Join(tmpDir, "cache_video_"+videoID)
		if err := os.MkdirAll(videoCacheDir, 0755); err != nil {
			t.Fatalf("create video cache dir: %v", err)
		}
		videoRuntime := NewRuntime()
		defer videoRuntime.CloseCachedEngine()

		for _, cacheState := range []string{"cold", "warm"} {
			cacheState := cacheState
			t.Run(fmt.Sprintf("Video_%s_%s", cacheState, videoID), func(t *testing.T) {
				result := testVideoMode(t, videoCacheDir, videoID, videoRuntime, cacheState)
				results = append(results, result)
				logResult(t, result)
			})
		}

		// Test music mode if cookies are available
		musicCacheDir := filepath.Join(tmpDir, "cache_music_"+videoID)
		if err := os.MkdirAll(musicCacheDir, 0755); err != nil {
			t.Fatalf("create music cache dir: %v", err)
		}
		musicRuntime := NewRuntime()
		defer musicRuntime.CloseCachedEngine()

		for _, cacheState := range []string{"cold", "warm"} {
			cacheState := cacheState
			t.Run(fmt.Sprintf("Music_%s_%s", cacheState, videoID), func(t *testing.T) {
				home, _ := os.UserHomeDir()
				cookiePath := filepath.Join(home, ".config", "ytx", "cookies.txt")
				if _, err := os.Stat(cookiePath); os.IsNotExist(err) {
					t.Skip("Cookie file not found, skipping music mode test")
				}
				result := testMusicMode(t, musicCacheDir, videoID, cookiePath, musicRuntime, cacheState)
				results = append(results, result)
				logResult(t, result)
			})
		}
	}

	updateGolden := os.Getenv("UPDATE_GOLDEN") == "1"

	// Load existing golden image for comparison
	existingGolden, err := loadGoldenImage()
	if !updateGolden && err == nil && len(existingGolden.Results) > 0 {
		t.Log("Comparing against existing golden image...")
		compareResults(t, results, existingGolden.Results)
	} else if updateGolden {
		t.Log("UPDATE_GOLDEN=1 set, skipping golden comparison")
	} else {
		t.Log("No existing golden image found, creating new baseline")
	}

	// Create new golden image
	golden := GoldenImage{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Version:     3, // Increment when test structure changes
		Results:     results,
	}

	goldenJSON, _ := json.MarshalIndent(golden, "", "  ")
	t.Logf("\n=== Current Results ===\n%s\n", goldenJSON)

	// Save if UPDATE_GOLDEN env is set or no golden exists
	if updateGolden || err != nil {
		if saveErr := saveGoldenImage(&golden); saveErr != nil {
			t.Logf("Failed to save golden image: %v", saveErr)
		} else {
			t.Logf("Golden image saved to: %s", goldenImagePath())
		}
	} else {
		t.Log("Set UPDATE_GOLDEN=1 to update the golden image")
	}
}

// testVideoMode tests video extraction with timing
func testVideoMode(t *testing.T, cacheDir, videoID string, runtime *Runtime, cacheState string) TestResult {
	result := TestResult{
		Mode:       "video",
		CacheState: cacheState,
		VideoID:    videoID,
	}

	totalStart := time.Now()

	// Video mode doesn't need cookies
	ext, err := NewExtractor(ModeVideo, "", WithRuntime(runtime), WithCacheManager(&CacheManager{cacheDir: cacheDir}))
	if err != nil {
		result.Error = fmt.Sprintf("failed to create extractor: %v", err)
		result.TotalDuration = time.Since(totalStart).String()
		return result
	}

	// Enable profiling
	ext.SetProfile(true)

	// Time the extraction
	extractStart := time.Now()
	videoResult, err := ext.ExtractVideo(videoID)
	extractDuration := time.Since(extractStart)

	result.Timings.TotalExtract = extractDuration.String()

	if err != nil {
		result.Error = fmt.Sprintf("extraction failed: %v", err)
		result.TotalDuration = time.Since(totalStart).String()
		return result
	}

	result.Success = true
	result.Itag = videoResult.VideoItag
	result.QualityLabel = fmt.Sprintf("%dx%d", videoResult.Width, videoResult.Height)

	// Copy profiled timings
	if videoResult.Timings != nil {
		result.Timings.VisitorDataMs = videoResult.Timings.VisitorDataMs
		result.Timings.PlayerAPIMs = videoResult.Timings.PlayerAPIMs
		result.Timings.TotalMs = videoResult.Timings.TotalMs
	}

	// Validate video URL works
	if videoResult.VideoURL != "" {
		validStart := time.Now()
		status := validateURL(videoResult.VideoURL)
		result.Timings.URLValidation = time.Since(validStart).String()
		result.HTTPStatus = status

		if status != 200 {
			result.Error = fmt.Sprintf("Video URL returned HTTP %d", status)
			result.Success = false
		}
	}

	result.TotalDuration = time.Since(totalStart).String()

	return result
}

// testMusicMode tests music extraction with timing and profiling
func testMusicMode(t *testing.T, cacheDir, videoID, cookiePath string, runtime *Runtime, cacheState string) TestResult {
	result := TestResult{
		Mode:       "music",
		CacheState: cacheState,
		VideoID:    videoID,
	}

	totalStart := time.Now()

	ext, err := NewExtractor(ModeMusic, cookiePath, WithRuntime(runtime), WithCacheManager(&CacheManager{cacheDir: cacheDir}))
	if err != nil {
		result.Error = fmt.Sprintf("failed to create extractor: %v", err)
		result.TotalDuration = time.Since(totalStart).String()
		return result
	}

	// Enable profiling
	ext.SetProfile(true)

	// Time the extraction
	extractStart := time.Now()
	musicResult, err := ext.Extract(videoID)
	extractDuration := time.Since(extractStart)

	result.Timings.TotalExtract = extractDuration.String()

	if err != nil {
		result.Error = fmt.Sprintf("extraction failed: %v", err)
		result.TotalDuration = time.Since(totalStart).String()
		return result
	}

	result.Success = true
	result.Itag = musicResult.Itag
	result.Bitrate = musicResult.Bitrate

	// Copy profiled timings
	if musicResult.Timings != nil {
		result.Timings.VisitorDataMs = musicResult.Timings.VisitorDataMs
		result.Timings.PlayerAPIMs = musicResult.Timings.PlayerAPIMs
		result.Timings.CipherInitMs = musicResult.Timings.CipherInitMs
		result.Timings.NTransformMs = musicResult.Timings.NTransformMs
		result.Timings.TotalMs = musicResult.Timings.TotalMs
	}

	// Check n-transform worked
	result.NTransformed = checkNTransformed(musicResult.URL)

	// Validate URL
	if musicResult.URL != "" {
		validStart := time.Now()
		status := validateURL(musicResult.URL)
		result.Timings.URLValidation = time.Since(validStart).String()
		result.HTTPStatus = status

		if status != 200 {
			result.Error = fmt.Sprintf("Music URL returned HTTP %d", status)
			result.Success = false
		}
	}

	result.TotalDuration = time.Since(totalStart).String()

	return result
}

// checkNTransformed checks if the n-parameter looks transformed
func checkNTransformed(url string) bool {
	// Parse URL and check n parameter length
	// Transformed n-params are typically 10-14 chars, untransformed are 16-20
	if idx := strings.Index(url, "&n="); idx != -1 {
		end := strings.Index(url[idx+3:], "&")
		if end == -1 {
			end = len(url) - idx - 3
		}
		nParam := url[idx+3 : idx+3+end]
		return len(nParam) <= 15
	}
	return true // No n-param means no transformation needed
}

// validateURL makes a HEAD request to check if URL is accessible
func validateURL(url string) int {
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Head(url)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	return resp.StatusCode
}

// logResult logs a test result
func logResult(t *testing.T, r TestResult) {
	status := "✓"
	if !r.Success {
		status = "✗"
	}

	t.Logf("%s [%s/%s] %s: itag=%d, http=%d, duration=%s",
		status, r.Mode, r.CacheState, r.VideoID, r.Itag, r.HTTPStatus, r.TotalDuration)

	if r.Timings.TotalMs > 0 {
		t.Logf("  Profiled: visitor=%dms, sts-wait=%dms, api=%dms, cipher-wait=%dms, cipher-prewarm=%dms, sig=%dms, n-transform=%dms, other=%dms, total=%dms",
			r.Timings.VisitorDataMs,
			r.Timings.STSWaitMs,
			r.Timings.PlayerAPIMs,
			r.Timings.CipherInitMs,
			r.Timings.CipherPrewarmMs,
			r.Timings.SigDecryptMs,
			r.Timings.NTransformMs,
			r.Timings.OtherMs,
			r.Timings.TotalMs)
	}

	if r.Error != "" {
		t.Logf("  Error: %s", r.Error)
	}
}

// TestNTransformIsolated tests n-transform without affecting system files
func TestNTransformIsolated(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping n-transform test in short mode")
	}

	tmpDir := t.TempDir()

	// Create extractor with isolated cache
	cacheDir := filepath.Join(tmpDir, "cache")
	os.MkdirAll(cacheDir, 0755)

	// Use video mode (doesn't need cookies) to get cipher
	ext, err := NewExtractor(ModeVideo, "")
	if err != nil {
		t.Fatalf("failed to create extractor: %v", err)
	}
	ext.cacheManager = &CacheManager{cacheDir: cacheDir}

	videoID := "dQw4w9WgXcQ"

	// Get cipher
	cipherStart := time.Now()
	cipher, err := ext.getCachedCipher(videoID)
	cipherDuration := time.Since(cipherStart)
	t.Logf("Cipher fetch: %s", cipherDuration)

	if err != nil {
		t.Fatalf("failed to get cipher: %v", err)
	}

	t.Logf("N-function name: %s", cipher.NFunctionName())
	t.Logf("Player.js size: %d bytes", cipher.PlayerJSLen())

	// Test n-transform
	testInputs := []string{
		"oBVQHlRh8vEpIQ",
		"ix8aPIX5ET2dOfGfRj",
		"3Uo4vnSmC1nWmM4-yi",
	}

	for _, input := range testInputs {
		transformStart := time.Now()
		output, err := cipher.TransformN(input)
		transformDuration := time.Since(transformStart)

		if err != nil {
			t.Errorf("TransformN(%s) error: %v", input, err)
			continue
		}

		transformed := output != input && len(output) < len(input)
		t.Logf("TransformN(%s) = %s (duration=%s, transformed=%v)",
			input, output, transformDuration, transformed)

		if !transformed {
			t.Errorf("Expected transformation, got same or longer output")
		}
	}

	// Cleanup
	CloseCachedEngine()
}

// TestCacheIsolation verifies that tests don't affect system cache
func TestCacheIsolation(t *testing.T) {
	// Check system cache location
	systemCacheDir, _ := os.UserCacheDir()
	systemYtxCache := filepath.Join(systemCacheDir, "ytx")

	// Record state before test
	var beforeFiles []string
	if entries, err := os.ReadDir(systemYtxCache); err == nil {
		for _, e := range entries {
			info, _ := e.Info()
			beforeFiles = append(beforeFiles, fmt.Sprintf("%s:%d", e.Name(), info.Size()))
		}
	}

	// Run a test that would normally use cache
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "isolated_cache")
	os.MkdirAll(cacheDir, 0755)

	cm := &CacheManager{cacheDir: cacheDir}

	// Write something to isolated cache
	testCache := &CipherCache{
		Version:     1,
		PlayerURL:   "/test/player.js",
		SigFunction: "testFunc",
	}
	err := cm.Save(testCache)
	if err != nil {
		t.Fatalf("failed to save to isolated cache: %v", err)
	}

	// Verify isolated cache has the file
	isolatedCachePath := cm.CachePath()
	if _, err := os.Stat(isolatedCachePath); os.IsNotExist(err) {
		t.Error("Isolated cache file was not created")
	}

	// Verify system cache was not affected
	var afterFiles []string
	if entries, err := os.ReadDir(systemYtxCache); err == nil {
		for _, e := range entries {
			info, _ := e.Info()
			afterFiles = append(afterFiles, fmt.Sprintf("%s:%d", e.Name(), info.Size()))
		}
	}

	// Compare
	if len(beforeFiles) != len(afterFiles) {
		t.Errorf("System cache was modified: before=%v, after=%v", beforeFiles, afterFiles)
	}

	t.Logf("Cache isolation verified: system cache unchanged")
}

func TestFetchVisitorDataUsesGlobalCacheForMusicMode(t *testing.T) {
	tmpDir := t.TempDir()
	cookieFile := filepath.Join(tmpDir, "cookies.txt")
	cookieData := ".youtube.com\tTRUE\t/\tTRUE\t4102444800\tSAPISID\ttest-sapisid\n"
	if err := os.WriteFile(cookieFile, []byte(cookieData), 0644); err != nil {
		t.Fatalf("failed to write cookie file: %v", err)
	}

	runtime := NewRuntime()
	ext, err := NewExtractor(ModeMusic, cookieFile, WithRuntime(runtime))
	if err != nil {
		t.Fatalf("failed to create extractor: %v", err)
	}

	runtime.visitorCache.Lock()
	runtime.visitorCache.data = "VISITOR_CACHE"
	runtime.visitorCache.sessionIndex = "7"
	runtime.visitorCache.delegatedSID = "DELEGATED_CACHE"
	runtime.visitorCache.isAuth = true
	runtime.visitorCache.authKey = "music:test-sapisid"
	runtime.visitorCache.expiry = time.Now().Add(visitorDataTTL)
	runtime.visitorCache.updated = time.Now()
	runtime.visitorCache.Unlock()
	defer func() {
		runtime.visitorCache.Lock()
		runtime.visitorCache.data = ""
		runtime.visitorCache.sessionIndex = ""
		runtime.visitorCache.delegatedSID = ""
		runtime.visitorCache.isAuth = false
		runtime.visitorCache.authKey = ""
		runtime.visitorCache.expiry = time.Time{}
		runtime.visitorCache.updated = time.Time{}
		runtime.visitorCache.Unlock()
	}()

	requests := 0
	ext.httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			return nil, fmt.Errorf("unexpected network request: %s", req.URL.String())
		}),
	}

	if err := ext.fetchVisitorData("dQw4w9WgXcQ"); err != nil {
		t.Fatalf("fetchVisitorData returned error: %v", err)
	}
	if requests != 0 {
		t.Fatalf("expected cached visitor data to avoid network, got %d requests", requests)
	}
	if ext.visitorData != "VISITOR_CACHE" {
		t.Fatalf("unexpected visitorData: got %q", ext.visitorData)
	}
	if ext.sessionIndex != "7" {
		t.Fatalf("unexpected sessionIndex: got %q", ext.sessionIndex)
	}
	if ext.delegatedSID != "DELEGATED_CACHE" {
		t.Fatalf("unexpected delegatedSID: got %q", ext.delegatedSID)
	}
}

func TestExtractProfileTimingsAddUpToTotal(t *testing.T) {
	tmpDir := t.TempDir()
	cookieFile := filepath.Join(tmpDir, "cookies.txt")
	cookieData := ".youtube.com\tTRUE\t/\tTRUE\t4102444800\tSAPISID\ttest-sapisid\n"
	if err := os.WriteFile(cookieFile, []byte(cookieData), 0644); err != nil {
		t.Fatalf("write cookies: %v", err)
	}

	runtime := NewRuntime()
	runtime.visitorCache.Lock()
	runtime.visitorCache.data = ""
	runtime.visitorCache.sessionIndex = ""
	runtime.visitorCache.delegatedSID = ""
	runtime.visitorCache.isAuth = false
	runtime.visitorCache.authKey = ""
	runtime.visitorCache.expiry = time.Time{}
	runtime.visitorCache.updated = time.Time{}
	runtime.visitorCache.Unlock()
	defer func() {
		runtime.visitorCache.Lock()
		runtime.visitorCache.data = ""
		runtime.visitorCache.sessionIndex = ""
		runtime.visitorCache.delegatedSID = ""
		runtime.visitorCache.isAuth = false
		runtime.visitorCache.authKey = ""
		runtime.visitorCache.expiry = time.Time{}
		runtime.visitorCache.updated = time.Time{}
		runtime.visitorCache.Unlock()
		runtime.CloseCachedEngine()
	}()

	ext, err := NewExtractor(ModeMusic, cookieFile, WithRuntime(runtime))
	if err != nil {
		t.Fatalf("NewExtractor returned error: %v", err)
	}
	ext.profile = true
	ext.cacheManager = &CacheManager{cacheDir: filepath.Join(tmpDir, "cache")}
	ext.invalidateCipherCache()
	defer ext.invalidateCipherCache()

	ext.httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Helper()
			switch {
			case req.Method == http.MethodGet && req.URL.Host == "music.youtube.com" && req.URL.Path == "/watch":
				time.Sleep(10 * time.Millisecond)
				html := `<!doctype html><html>"visitorData":"VISITOR_TEST","SESSION_INDEX":"0","DELEGATED_SESSION_ID":"DELEGATED_TEST"</html>`
				return jsonHTTPResponse(http.StatusOK, html), nil
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/watch":
				time.Sleep(10 * time.Millisecond)
				html := `<!doctype html><html><script src="/s/player/watch123/player_ias.vflset/en_US/base.js"></script></html>`
				return jsonHTTPResponse(http.StatusOK, html), nil
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/s/player/watch123/player_ias.vflset/en_US/base.js":
				time.Sleep(10 * time.Millisecond)
				playerJS := `var signatureTimestamp=12345;x&&(x=AbC(decodeURIComponent(x)));function AbC(a){a=a.split("");a.reverse();return a.join("")}`
				return jsonHTTPResponse(http.StatusOK, playerJS), nil
			case req.Method == http.MethodPost && req.URL.Host == "music.youtube.com" && req.URL.Path == "/youtubei/v1/player":
				time.Sleep(10 * time.Millisecond)
				signatureCipher := "url=https%3A%2F%2Fstream.test%2Fvideoplayback%3Ffoo%3Dbar&s=abc&sp=sig"
				responseBody := `{"responseContext":{"visitorData":"VISITOR_TEST"},"playabilityStatus":{"status":"OK"},"streamingData":{"adaptiveFormats":[{"itag":141,"signatureCipher":"` + signatureCipher + `","mimeType":"audio/mp4; codecs=\"mp4a.40.2\"","bitrate":256000,"quality":"tiny"}]},"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"test","lengthSeconds":"1","author":"author","shortDescription":""}}`
				return jsonHTTPResponse(http.StatusOK, responseBody), nil
			}

			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}),
	}

	result, err := ext.Extract("dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if result.Timings == nil {
		t.Fatal("expected timings in profiled result")
	}

	sum := result.Timings.VisitorDataMs +
		result.Timings.STSWaitMs +
		result.Timings.PlayerAPIMs +
		result.Timings.CipherInitMs +
		result.Timings.SigDecryptMs +
		result.Timings.NTransformMs +
		result.Timings.OtherMs
	if sum != result.Timings.TotalMs {
		t.Fatalf("expected profiled stages to add up to total: sum=%d total=%d timings=%+v", sum, result.Timings.TotalMs, *result.Timings)
	}
	if result.Timings.CipherPrewarmMs == 0 {
		t.Fatal("expected cipher prewarm timing to be recorded")
	}
}

// BenchmarkNTransform benchmarks the n-transform performance
func BenchmarkNTransform(b *testing.B) {
	tmpDir := b.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	os.MkdirAll(cacheDir, 0755)

	ext, err := NewExtractor(ModeVideo, "")
	if err != nil {
		b.Fatalf("failed to create extractor: %v", err)
	}
	ext.cacheManager = &CacheManager{cacheDir: cacheDir}

	// Pre-warm: get cipher once
	cipher, err := ext.getCachedCipher("dQw4w9WgXcQ")
	if err != nil {
		b.Fatalf("failed to get cipher: %v", err)
	}

	testInput := "oBVQHlRh8vEpIQ"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := cipher.TransformN(testInput)
		if err != nil {
			b.Fatal(err)
		}
	}

	b.StopTimer()
	CloseCachedEngine()
}

// BenchmarkVideoExtraction benchmarks full video extraction
func BenchmarkVideoExtraction(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping in short mode")
	}

	tmpDir := b.TempDir()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cacheDir := filepath.Join(tmpDir, fmt.Sprintf("cache_%d", i))
		os.MkdirAll(cacheDir, 0755)

		ext, _ := NewExtractor(ModeVideo, "")
		ext.cacheManager = &CacheManager{cacheDir: cacheDir}

		_, err := ext.ExtractVideo("dQw4w9WgXcQ")
		if err != nil {
			b.Fatal(err)
		}

		CloseCachedEngine()
	}
}

// TestMusicModeWithCookies tests music mode if cookies are available
func TestMusicModeWithCookies(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	// Check if default cookie file exists
	home, _ := os.UserHomeDir()
	defaultCookiePath := filepath.Join(home, ".config", "ytx", "cookies.txt")

	if _, err := os.Stat(defaultCookiePath); os.IsNotExist(err) {
		t.Skip("Cookie file not found, skipping music mode test")
	}
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	os.MkdirAll(cacheDir, 0755)

	ext, err := NewExtractor(ModeMusic, defaultCookiePath)
	if err != nil {
		t.Fatalf("failed to create extractor: %v", err)
	}
	ext.cacheManager = &CacheManager{cacheDir: cacheDir}

	videoID := "dQw4w9WgXcQ"

	extractStart := time.Now()
	result, err := ext.Extract(videoID)
	extractDuration := time.Since(extractStart)

	if err != nil {
		t.Fatalf("extraction failed: %v", err)
	}

	t.Logf("Music extraction: itag=%d, bitrate=%d, duration=%s",
		result.Itag, result.Bitrate, extractDuration)

	// Check n-transform worked
	nTransformed := checkNTransformed(result.URL)
	t.Logf("N-parameter transformed: %v", nTransformed)

	// Validate URL
	status := validateURL(result.URL)
	t.Logf("HTTP status: %d", status)

	if status != 200 {
		t.Errorf("Expected HTTP 200, got %d", status)
	}

	// Cleanup
	CloseCachedEngine()
}

func TestMusicModeUsesDocumentedCipherPathWithoutPOT(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	cookieFile := filepath.Join(tmpDir, "cookies.txt")
	cookieData := ".youtube.com\tTRUE\t/\tTRUE\t4102444800\tSAPISID\ttest-sapisid\n"
	if err := os.WriteFile(cookieFile, []byte(cookieData), 0644); err != nil {
		t.Fatalf("failed to write cookie file: %v", err)
	}

	markerPath := filepath.Join(tmpDir, "yt-dlp-invoked")
	fakeBinDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(fakeBinDir, 0755); err != nil {
		t.Fatalf("failed to create fake bin dir: %v", err)
	}
	fakeYtDlp := filepath.Join(fakeBinDir, "yt-dlp")
	script := fmt.Sprintf("#!/bin/sh\nprintf invoked > %q\nexit 42\n", markerPath)
	if err := os.WriteFile(fakeYtDlp, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write fake yt-dlp: %v", err)
	}
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ext, err := NewExtractor(ModeMusic, cookieFile)
	if err != nil {
		t.Fatalf("failed to create extractor: %v", err)
	}
	ext.cacheManager = &CacheManager{cacheDir: cacheDir}
	ext.invalidateCipherCache()
	defer ext.invalidateCipherCache()

	var mu sync.Mutex
	playerBodyPoToken := ""
	playerBodySTS := 0
	playerEmbedCalled := false
	playerJSCalled := false

	ext.httpClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.Method == http.MethodGet && req.URL.Host == "music.youtube.com" && req.URL.Path == "/watch":
				html := `<!doctype html><html><script>
"visitorData":"VISITOR_TEST",
"SESSION_INDEX":"0",
"DELEGATED_SESSION_ID":"DELEGATED_TEST",
window.ytAtR = "{\"bgChallenge\":{\"interpreterUrl\":{\"privateDoNotAccessOrElseTrustedResourceUrlWrappedValue\":\"//challenge.test/interpreter.js\"},\"interpreterHash\":\"HASH_TEST\",\"program\":\"PROGRAM_TEST\",\"globalName\":\"BG_TEST\",\"clientExperimentsStateBlob\":\"BLOB_TEST\"}}";
</script></html>`
				return jsonHTTPResponse(http.StatusOK, html), nil

			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/embed/dQw4w9WgXcQ":
				mu.Lock()
				playerEmbedCalled = true
				mu.Unlock()
				html := `<!doctype html><html><script src="/s/player/test123/player_ias.vflset/en_US/base.js"></script></html>`
				return jsonHTTPResponse(http.StatusOK, html), nil

			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/s/player/test123/player_ias.vflset/en_US/base.js":
				mu.Lock()
				playerJSCalled = true
				mu.Unlock()
				playerJS := `var signatureTimestamp=12345;x&&(x=AbC(decodeURIComponent(x)));function AbC(a){a=a.split("");a.reverse();return a.join("")}`
				return jsonHTTPResponse(http.StatusOK, playerJS), nil

			case req.Method == http.MethodPost && req.URL.Host == "music.youtube.com" && req.URL.Path == "/youtubei/v1/player":
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				var payload map[string]any
				if err := json.Unmarshal(body, &payload); err != nil {
					return nil, err
				}

				mu.Lock()
				if sid, ok := payload["serviceIntegrityDimensions"].(map[string]any); ok {
					if token, ok := sid["poToken"].(string); ok {
						playerBodyPoToken = token
					}
				}
				if playbackContext, ok := payload["playbackContext"].(map[string]any); ok {
					if contentPlaybackContext, ok := playbackContext["contentPlaybackContext"].(map[string]any); ok {
						switch sts := contentPlaybackContext["signatureTimestamp"].(type) {
						case float64:
							playerBodySTS = int(sts)
						}
					}
				}
				mu.Unlock()

				signatureCipher := "url=https%3A%2F%2Fstream.test%2Fvideoplayback%3Ffoo%3Dbar&s=abc&sp=sig"
				responseBody := `{
"responseContext":{"visitorData":"VISITOR_TEST"},
"playabilityStatus":{"status":"OK"},
"streamingData":{"adaptiveFormats":[{"itag":141,"signatureCipher":"` + signatureCipher + `","mimeType":"audio/mp4; codecs=\"mp4a.40.2\"","bitrate":256000,"quality":"tiny"}]},
"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"test","lengthSeconds":"1","author":"author","shortDescription":""}
}`
				return jsonHTTPResponse(http.StatusOK, responseBody), nil
			}

			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}),
	}

	result, err := ext.Extract("dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("extraction failed: %v", err)
	}

	if !strings.Contains(result.URL, "sig=cba") {
		t.Fatalf("expected decrypted signature in stream URL, got %q", result.URL)
	}
	if strings.Contains(result.URL, "pot=") {
		t.Fatalf("expected documented path without pot query parameter, got %q", result.URL)
	}

	mu.Lock()
	defer mu.Unlock()
	if !playerEmbedCalled {
		t.Fatal("expected player embed page to be fetched for base.js discovery")
	}
	if !playerJSCalled {
		t.Fatal("expected player JS to be fetched for documented cipher path")
	}
	if playerBodyPoToken != "" {
		t.Fatalf("did not expect serviceIntegrityDimensions.poToken on documented path, got %q", playerBodyPoToken)
	}
	if playerBodySTS != 12345 {
		t.Fatalf("expected signatureTimestamp from base.js in player request, got %d", playerBodySTS)
	}

	if _, err := os.Stat(markerPath); err == nil {
		t.Fatal("yt-dlp fallback was invoked")
	} else if !os.IsNotExist(err) {
		t.Fatalf("failed to inspect yt-dlp marker: %v", err)
	}

	CloseCachedEngine()
}

func TestMusicModeDoesNotRequireChallengeOrPOTOnDocumentedPath(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	cookieFile := filepath.Join(tmpDir, "cookies.txt")
	cookieData := ".youtube.com\tTRUE\t/\tTRUE\t4102444800\tSAPISID\ttest-sapisid\n"
	if err := os.WriteFile(cookieFile, []byte(cookieData), 0644); err != nil {
		t.Fatalf("failed to write cookie file: %v", err)
	}

	ext, err := NewExtractor(ModeMusic, cookieFile)
	if err != nil {
		t.Fatalf("failed to create extractor: %v", err)
	}
	ext.cacheManager = &CacheManager{cacheDir: cacheDir}
	ext.invalidateCipherCache()
	defer ext.invalidateCipherCache()

	var mu sync.Mutex
	attGetCalled := false
	playerBodyPoToken := ""
	playerEmbedCalled := false
	playerJSCalled := false

	ext.httpClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.Method == http.MethodGet && req.URL.Host == "music.youtube.com" && req.URL.Path == "/watch":
				html := `<!doctype html><html><script>
"visitorData":"VISITOR_TEST",
"SESSION_INDEX":"0",
"DELEGATED_SESSION_ID":"DELEGATED_TEST"
</script></html>`
				return jsonHTTPResponse(http.StatusOK, html), nil

			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/embed/dQw4w9WgXcQ":
				mu.Lock()
				playerEmbedCalled = true
				mu.Unlock()
				html := `<!doctype html><html><script src="/s/player/test123/player_ias.vflset/en_US/base.js"></script></html>`
				return jsonHTTPResponse(http.StatusOK, html), nil

			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/s/player/test123/player_ias.vflset/en_US/base.js":
				mu.Lock()
				playerJSCalled = true
				mu.Unlock()
				playerJS := `var signatureTimestamp=12345;x&&(x=AbC(decodeURIComponent(x)));function AbC(a){a=a.split("");a.reverse();return a.join("")}`
				return jsonHTTPResponse(http.StatusOK, playerJS), nil

			case req.Method == http.MethodPost && req.URL.Host == "music.youtube.com" && req.URL.Path == "/youtubei/v1/att/get":
				mu.Lock()
				attGetCalled = true
				mu.Unlock()
				return nil, fmt.Errorf("unexpected att/get request on documented path")

			case req.Method == http.MethodPost && req.URL.Host == "music.youtube.com" && req.URL.Path == "/youtubei/v1/player":
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				var payload map[string]any
				if err := json.Unmarshal(body, &payload); err != nil {
					return nil, err
				}

				mu.Lock()
				if sid, ok := payload["serviceIntegrityDimensions"].(map[string]any); ok {
					if token, ok := sid["poToken"].(string); ok {
						playerBodyPoToken = token
					}
				}
				mu.Unlock()

				signatureCipher := "url=https%3A%2F%2Fstream.test%2Fvideoplayback%3Ffoo%3Dbar&s=abc&sp=sig"
				responseBody := `{
"responseContext":{"visitorData":"VISITOR_TEST"},
"playabilityStatus":{"status":"OK"},
"streamingData":{"adaptiveFormats":[{"itag":141,"signatureCipher":"` + signatureCipher + `","mimeType":"audio/mp4; codecs=\"mp4a.40.2\"","bitrate":256000,"quality":"tiny"}]},
"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"test","lengthSeconds":"1","author":"author","shortDescription":""}
}`
				return jsonHTTPResponse(http.StatusOK, responseBody), nil
			}

			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}),
	}

	result, err := ext.Extract("dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("extraction failed: %v", err)
	}

	if !strings.Contains(result.URL, "sig=cba") {
		t.Fatalf("expected decrypted signature in stream URL, got %q", result.URL)
	}
	if strings.Contains(result.URL, "pot=") {
		t.Fatalf("expected no pot query parameter on documented path, got %q", result.URL)
	}

	mu.Lock()
	defer mu.Unlock()
	if !attGetCalled {
		// The transport turns this into a hard failure, but keep the state check explicit.
	} else {
		t.Fatal("did not expect /att/get fallback when challenge is absent on documented path")
	}
	if !playerEmbedCalled {
		t.Fatal("expected player embed page fetch for base.js discovery")
	}
	if !playerJSCalled {
		t.Fatal("expected player JS fetch for documented cipher path")
	}
	if playerBodyPoToken != "" {
		t.Fatalf("did not expect serviceIntegrityDimensions.poToken on documented path, got %q", playerBodyPoToken)
	}

	CloseCachedEngine()
}

func TestExtractPersistsExtractedSigArtifactAndFingerprint(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	cookieFile := filepath.Join(tmpDir, "cookies.txt")
	cookieData := ".youtube.com\tTRUE\t/\tTRUE\t4102444800\tSAPISID\ttest-sapisid\n"
	if err := os.WriteFile(cookieFile, []byte(cookieData), 0644); err != nil {
		t.Fatalf("failed to write cookie file: %v", err)
	}

	ext, err := NewExtractor(ModeMusic, cookieFile)
	if err != nil {
		t.Fatalf("failed to create extractor: %v", err)
	}
	ext.cacheManager = &CacheManager{cacheDir: cacheDir}
	ext.invalidateCipherCache()
	defer ext.invalidateCipherCache()

	ext.httpClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.Method == http.MethodGet && req.URL.Host == "music.youtube.com" && req.URL.Path == "/watch":
				html := `<!doctype html><html><script>
"visitorData":"VISITOR_TEST",
"SESSION_INDEX":"0",
"DELEGATED_SESSION_ID":"DELEGATED_TEST"
</script></html>`
				return jsonHTTPResponse(http.StatusOK, html), nil
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/embed/dQw4w9WgXcQ":
				html := `<!doctype html><html><script src="/s/player/test123/player_ias.vflset/en_US/base.js"></script></html>`
				return jsonHTTPResponse(http.StatusOK, html), nil
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/s/player/test123/player_ias.vflset/en_US/base.js":
				playerJS := `var signatureTimestamp=12345;x&&(x=AbC(decodeURIComponent(x)));function AbC(a){a=a.split("");a.reverse();return a.join("")}`
				return jsonHTTPResponse(http.StatusOK, playerJS), nil
			case req.Method == http.MethodPost && req.URL.Host == "music.youtube.com" && req.URL.Path == "/youtubei/v1/player":
				signatureCipher := "url=https%3A%2F%2Fstream.test%2Fvideoplayback%3Ffoo%3Dbar&s=abc&sp=sig"
				responseBody := `{
"responseContext":{"visitorData":"VISITOR_TEST"},
"playabilityStatus":{"status":"OK"},
"streamingData":{"adaptiveFormats":[{"itag":141,"signatureCipher":"` + signatureCipher + `","mimeType":"audio/mp4; codecs=\"mp4a.40.2\"","bitrate":256000,"quality":"tiny"}]},
"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"test","lengthSeconds":"1","author":"author","shortDescription":""}
}`
				return jsonHTTPResponse(http.StatusOK, responseBody), nil
			}

			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}),
	}

	result, err := ext.Extract("dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("extraction failed: %v", err)
	}
	if !strings.Contains(result.URL, "sig=cba") {
		t.Fatalf("expected decrypted signature in stream URL, got %q", result.URL)
	}

	cache, err := ext.cacheManager.Load()
	if err != nil {
		t.Fatalf("failed to load persisted cache: %v", err)
	}
	if cache.JSCode == "" {
		t.Fatal("expected extracted signature JS to be persisted after decrypt")
	}
	if cache.PlayerFingerprint == "" {
		t.Fatal("expected player fingerprint to be persisted")
	}
	if ext.cipher == nil || cache.PlayerFingerprint != ext.cipher.PlayerFingerprint() {
		t.Fatalf("expected persisted fingerprint to match live cipher fingerprint, got cache=%q cipher=%q", cache.PlayerFingerprint, ext.cipher.PlayerFingerprint())
	}

	CloseCachedEngine()
}

func TestGetCachedCipherPersistsPreparedSigArtifactOnInitialFetch(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	ext, err := NewExtractor(ModeVideo, "")
	if err != nil {
		t.Fatalf("failed to create extractor: %v", err)
	}
	ext.cacheManager = &CacheManager{cacheDir: cacheDir}
	ext.invalidateCipherCache()
	defer ext.invalidateCipherCache()

	ext.httpClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/embed/dQw4w9WgXcQ":
				html := `<!doctype html><html><script src="/s/player/test123/player_ias.vflset/en_US/base.js"></script></html>`
				return jsonHTTPResponse(http.StatusOK, html), nil
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/s/player/test123/player_ias.vflset/en_US/base.js":
				playerJS := `var signatureTimestamp=12345;x&&(x=AbC(decodeURIComponent(x)));function AbC(a){a=a.split("");a.reverse();return a.join("")}`
				return jsonHTTPResponse(http.StatusOK, playerJS), nil
			}

			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}),
	}

	if _, err := ext.getCachedCipher("dQw4w9WgXcQ"); err != nil {
		t.Fatalf("getCachedCipher returned error: %v", err)
	}

	cache, err := ext.cacheManager.Load()
	if err != nil {
		t.Fatalf("failed to load persisted cache: %v", err)
	}
	if cache.JSCode == "" {
		t.Fatal("expected prepared signature JS to be persisted on initial fetch")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
