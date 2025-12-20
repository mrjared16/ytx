package ytx

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TimingToleranceMs allows for network variance in timing comparisons
const TimingToleranceMs = 150

// TestResult captures timing and results for a single extraction
type TestResult struct {
	Mode          string        `json:"mode"`
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
	VisitorDataMs int64 `json:"visitor_data_ms,omitempty"`
	PlayerAPIMs   int64 `json:"player_api_ms,omitempty"`
	CipherInitMs  int64 `json:"cipher_init_ms,omitempty"`
	NTransformMs  int64 `json:"n_transform_ms,omitempty"`
	TotalMs       int64 `json:"total_ms,omitempty"`
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

// compareResults compares actual results against golden baseline
func compareResults(t *testing.T, actual, expected []TestResult) {
	t.Helper()

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
		if act.VideoID != exp.VideoID {
			t.Errorf("Result %d: videoID mismatch: got %s, want %s", i, act.VideoID, exp.VideoID)
		}
		if act.Success != exp.Success {
			t.Errorf("Result %d: success mismatch: got %v, want %v (error: %s)", i, act.Success, exp.Success, act.Error)
		}
		if act.HTTPStatus != exp.HTTPStatus && exp.Success {
			t.Errorf("Result %d: HTTP status mismatch: got %d, want %d", i, act.HTTPStatus, exp.HTTPStatus)
		}

		// Compare timings with tolerance (only if both have profiled data)
		if exp.Timings.TotalMs > 0 && act.Timings.TotalMs > 0 {
			if !withinTolerance(act.Timings.TotalMs, exp.Timings.TotalMs, TimingToleranceMs) {
				t.Logf("Result %d: timing outside tolerance: got %dms, want %dms (tolerance: %dms)",
					i, act.Timings.TotalMs, exp.Timings.TotalMs, TimingToleranceMs)
				// Log as warning, not error - network variance is expected
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
		// Test video mode (doesn't need cookies)
		t.Run(fmt.Sprintf("Video_%s", videoID), func(t *testing.T) {
			result := testVideoMode(t, tmpDir, videoID)
			results = append(results, result)
			logResult(t, result)
		})

		// Test music mode if cookies are available
		t.Run(fmt.Sprintf("Music_%s", videoID), func(t *testing.T) {
			home, _ := os.UserHomeDir()
			cookiePath := filepath.Join(home, ".config", "ytx", "cookies.txt")
			if _, err := os.Stat(cookiePath); os.IsNotExist(err) {
				t.Skip("Cookie file not found, skipping music mode test")
			}
			result := testMusicMode(t, tmpDir, videoID, cookiePath)
			results = append(results, result)
			logResult(t, result)
		})
	}

	// Load existing golden image for comparison
	existingGolden, err := loadGoldenImage()
	if err == nil && len(existingGolden.Results) > 0 {
		t.Log("Comparing against existing golden image...")
		compareResults(t, results, existingGolden.Results)
	} else {
		t.Log("No existing golden image found, creating new baseline")
	}

	// Create new golden image
	golden := GoldenImage{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Version:     2, // Increment when test structure changes
		Results:     results,
	}

	goldenJSON, _ := json.MarshalIndent(golden, "", "  ")
	t.Logf("\n=== Current Results ===\n%s\n", goldenJSON)

	// Save if UPDATE_GOLDEN env is set or no golden exists
	if os.Getenv("UPDATE_GOLDEN") == "1" || err != nil {
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
func testVideoMode(t *testing.T, tmpDir, videoID string) TestResult {
	result := TestResult{
		Mode:    "video",
		VideoID: videoID,
	}

	totalStart := time.Now()

	// Create extractor with isolated cache
	cacheDir := filepath.Join(tmpDir, "cache_video_"+videoID)
	os.MkdirAll(cacheDir, 0755)

	// Video mode doesn't need cookies
	ext, err := NewExtractor(ModeVideo, "")
	if err != nil {
		result.Error = fmt.Sprintf("failed to create extractor: %v", err)
		result.TotalDuration = time.Since(totalStart).String()
		return result
	}

	// Override cache manager to use temp dir
	ext.cacheManager = &CacheManager{cacheDir: cacheDir}

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
func testMusicMode(t *testing.T, tmpDir, videoID, cookiePath string) TestResult {
	result := TestResult{
		Mode:    "music",
		VideoID: videoID,
	}

	totalStart := time.Now()

	// Create extractor with isolated cache
	cacheDir := filepath.Join(tmpDir, "cache_music_"+videoID)
	os.MkdirAll(cacheDir, 0755)

	ext, err := NewExtractor(ModeMusic, cookiePath)
	if err != nil {
		result.Error = fmt.Sprintf("failed to create extractor: %v", err)
		result.TotalDuration = time.Since(totalStart).String()
		return result
	}

	// Override cache manager to use temp dir
	ext.cacheManager = &CacheManager{cacheDir: cacheDir}

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
		CloseCachedEngine()
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

	// Cleanup
	CloseCachedEngine()

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

	t.Logf("%s [%s] %s: itag=%d, http=%d, duration=%s",
		status, r.Mode, r.VideoID, r.Itag, r.HTTPStatus, r.TotalDuration)

	if r.Timings.TotalMs > 0 {
		t.Logf("  Profiled: visitor=%dms, api=%dms, cipher=%dms, n-transform=%dms, total=%dms",
			r.Timings.VisitorDataMs, r.Timings.PlayerAPIMs, r.Timings.CipherInitMs, r.Timings.NTransformMs, r.Timings.TotalMs)
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
