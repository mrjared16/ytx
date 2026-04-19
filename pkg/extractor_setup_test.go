package ytx

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mrjared16/ytx/internal/cache"
)

// TimingToleranceMs allows for network variance in timing comparisons.
const TimingToleranceMs = 300

const liveProbeTestsEnvVar = "RUN_LIVE_PROBE_TESTS"

// TestResult captures timing and results for a single extraction
type TestResult struct {
	Mode          string        `json:"mode"`
	POMode        bool          `json:"po_mode,omitempty"`
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
	"dQw4w9WgXcQ",
}

var bulkProbeVideoIDs = []string{
	"bvfTp68YyZ0",
	"QG3OcdUgv6Y",
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

// compareResults compares actual results against golden baseline
func compareResults(t *testing.T, actual, expected []TestResult) {
	t.Helper()
	if len(actual) != len(expected) {
		t.Fatalf("result count mismatch: got %d want %d", len(actual), len(expected))
	}

	for i, exp := range expected {
		if i >= len(actual) {
			t.Errorf("Missing result %d (%s/%s)", i, exp.Mode, exp.VideoID)
			continue
		}
		act := actual[i]

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
		if act.POMode != exp.POMode {
			t.Errorf("Result %d: po_mode mismatch: got %v, want %v", i, act.POMode, exp.POMode)
		}
		if act.NTransformed != exp.NTransformed && exp.Success {
			t.Errorf("Result %d: n_transform mismatch: got %v, want %v", i, act.NTransformed, exp.NTransformed)
		}

		if exp.Timings.TotalMs > 0 && act.Timings.TotalMs > 0 {
			upperLimit := exp.Timings.TotalMs + TimingToleranceMs
			if act.Timings.TotalMs > upperLimit {
				t.Errorf("Result %d: timing upper-bound check failed: got %dms, want <= %dms (baseline=%dms, deviation=%dms)",
					i, act.Timings.TotalMs, upperLimit, exp.Timings.TotalMs, TimingToleranceMs)
			}
		}
	}
}

func runBulkMusicProbeRegression(t *testing.T, poMode bool) {
	requireExplicitLiveProbeRun(t)

	if testing.Short() {
		t.Skip("Skipping bulk probe regression test in short mode")
	}

	home, _ := os.UserHomeDir()
	cookiePath := filepath.Join(home, ".config", "ytx", "cookies.txt")
	if _, err := os.Stat(cookiePath); os.IsNotExist(err) {
		t.Skip("Cookie file not found, skipping bulk probe regression test")
	}

	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache_bulk_probe")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("create cache dir: %v", err)
	}

	runtime := NewRuntime()
	defer runtime.CloseCachedEngine()

	opts := []ExtractorOption{
		WithRuntime(runtime),
		WithCacheManager(cache.NewCacheManagerWithDir(cacheDir)),
	}
	if poMode {
		opts = append(opts, WithPOMode(true))
	}

	ext, err := NewExtractor(ModeMusic, cookiePath, opts...)
	if err != nil {
		t.Fatalf("failed to create extractor: %v", err)
	}

	results := ext.BulkExtractOrdered(bulkProbeVideoIDs)
	if len(results) != len(bulkProbeVideoIDs) {
		t.Fatalf("result count mismatch: got %d want %d", len(results), len(bulkProbeVideoIDs))
	}

	for i, videoID := range bulkProbeVideoIDs {
		result := results[i]
		if result.ID != videoID {
			t.Fatalf("result %d id mismatch: got %s want %s", i, result.ID, videoID)
		}
		if result.Error != "" {
			t.Fatalf("bulk extraction failed for %s: %s", videoID, result.Error)
		}
		if result.URL == "" {
			t.Fatalf("bulk extraction returned empty URL for %s", videoID)
		}
		if !checkNTransformed(result.URL) {
			t.Fatalf("bulk extraction returned untransformed n param for %s", videoID)
		}

		status := probeStreamURL(result.URL)
		if !poMode && status == http.StatusForbidden {
			t.Logf("bulk extraction URL returned HTTP 403 for %s in normal mode; treating as PO-gated live behavior (hint: rerun in explicit PO mode)", videoID)
			continue
		}
		if poMode && status == http.StatusForbidden {
			t.Logf("bulk PO extraction URL returned HTTP 403 for %s; retrying once with fresh runtime/cache", videoID)
			retryResult, retryStatus, retryErr := retrySingleMusicProbe(t, cookiePath, videoID, true)
			if retryErr == nil && retryResult != nil && (retryStatus == http.StatusOK || retryStatus == http.StatusPartialContent) {
				continue
			}
			if retryErr != nil {
				t.Logf("bulk PO retry for %s failed during extraction: %v", videoID, retryErr)
			} else {
				t.Logf("bulk PO retry for %s returned HTTP %d", videoID, retryStatus)
			}
		}
		if status != http.StatusOK && status != http.StatusPartialContent {
			hint := ""
			if !poMode {
				hint = " (hint: rerun in explicit PO mode)"
			}
			t.Fatalf("bulk extraction URL not streamable for %s: got HTTP %d%s", videoID, status, hint)
		}
	}
}

func retrySingleMusicProbe(t *testing.T, cookiePath, videoID string, poMode bool) (*Result, int, error) {
	t.Helper()
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, 0, err
	}

	runtime := NewRuntime()
	defer runtime.CloseCachedEngine()

	opts := []ExtractorOption{WithRuntime(runtime), WithCacheManager(cache.NewCacheManagerWithDir(cacheDir))}
	if poMode {
		opts = append(opts, WithPOMode(true))
	}

	ext, err := NewExtractor(ModeMusic, cookiePath, opts...)
	if err != nil {
		return nil, 0, err
	}

	result, err := ext.Extract(videoID)
	if err != nil {
		return nil, 0, err
	}

	return result, probeStreamURL(result.URL), nil
}

// testVideoMode tests video extraction with timing
func testVideoMode(t *testing.T, cacheDir, videoID string, runtime *Runtime, cacheState string) TestResult {
	result := TestResult{
		Mode:       "video",
		CacheState: cacheState,
		VideoID:    videoID,
	}

	totalStart := time.Now()

	ext, err := NewExtractor(ModeVideo, "", WithRuntime(runtime), WithCacheManager(cache.NewCacheManagerWithDir(cacheDir)))
	if err != nil {
		result.Error = fmt.Sprintf("failed to create extractor: %v", err)
		result.TotalDuration = time.Since(totalStart).String()
		return result
	}

	ext.SetProfile(true)

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

	if videoResult.Timings != nil {
		result.Timings.VisitorDataMs = videoResult.Timings.VisitorDataMs
		result.Timings.PlayerAPIMs = videoResult.Timings.PlayerAPIMs
		result.Timings.TotalMs = videoResult.Timings.TotalMs
	}

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
func testMusicMode(t *testing.T, cacheDir, videoID, cookiePath string, runtime *Runtime, cacheState string, poMode bool) TestResult {
	result := TestResult{
		Mode:       "music",
		POMode:     poMode,
		CacheState: cacheState,
		VideoID:    videoID,
	}

	totalStart := time.Now()

	opts := []ExtractorOption{WithRuntime(runtime), WithCacheManager(cache.NewCacheManagerWithDir(cacheDir))}
	if poMode {
		opts = append(opts, WithPOMode(true))
	}
	ext, err := NewExtractor(ModeMusic, cookiePath, opts...)
	if err != nil {
		result.Error = fmt.Sprintf("failed to create extractor: %v", err)
		result.TotalDuration = time.Since(totalStart).String()
		return result
	}

	ext.SetProfile(true)

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

	if musicResult.Timings != nil {
		result.Timings.VisitorDataMs = musicResult.Timings.VisitorDataMs
		result.Timings.PlayerAPIMs = musicResult.Timings.PlayerAPIMs
		result.Timings.CipherInitMs = musicResult.Timings.CipherInitMs
		result.Timings.NTransformMs = musicResult.Timings.NTransformMs
		result.Timings.TotalMs = musicResult.Timings.TotalMs
	}

	result.NTransformed = checkNTransformed(musicResult.URL)

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

	if idx := strings.Index(url, "&n="); idx != -1 {
		end := strings.Index(url[idx+3:], "&")
		if end == -1 {
			end = len(url) - idx - 3
		}
		nParam := url[idx+3 : idx+3+end]
		return len(nParam) <= 15
	}
	return true
}

// validateURL checks if URL is accessible.
// It follows redirects and falls back to a tiny ranged GET when HEAD is not accepted.
func validateURL(url string) int {
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Head(url)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	status := resp.StatusCode
	if status == http.StatusPartialContent {
		return http.StatusOK
	}

	if status != http.StatusMethodNotAllowed && status != http.StatusForbidden {
		return status
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return status
	}
	req.Header.Set("Range", "bytes=0-0")

	resp, err = client.Do(req)
	if err != nil {
		return status
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusPartialContent {
		return http.StatusOK
	}

	return resp.StatusCode
}

func requireExplicitLiveProbeRun(t *testing.T) {
	t.Helper()
	if os.Getenv(liveProbeTestsEnvVar) != "1" {
		t.Skip("Skipping live probe test; run via make target or set RUN_LIVE_PROBE_TESTS=1")
	}

}

func probeStreamURL(url string) int {
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if total := resp.Header.Get("Content-Range"); strings.HasPrefix(total, "bytes 0-0/") {
		if size, err := strconv.ParseInt(strings.TrimPrefix(total, "bytes 0-0/"), 10, 64); err == nil && size > 1 {
			nearEndReq, err := http.NewRequest(http.MethodGet, url, nil)
			if err == nil {
				nearEndReq.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", size-1, size-1))
				nearEndReq.Header.Set("User-Agent", "Mozilla/5.0")
				if nearEndResp, err := client.Do(nearEndReq); err == nil {
					defer nearEndResp.Body.Close()
					_, _ = io.Copy(io.Discard, nearEndResp.Body)
					return nearEndResp.StatusCode
				}
			}
		}
	}

	return resp.StatusCode
}

// logResult logs a test result
func logResult(t *testing.T, r TestResult) {
	status := "✓"
	if !r.Success {
		status = "✗"
	}

	mode := r.Mode
	if r.POMode {
		mode += "+po"
	}
	t.Logf("%s [%s/%s] %s: itag=%d, http=%d, duration=%s",
		status, mode, r.CacheState, r.VideoID, r.Itag, r.HTTPStatus, r.TotalDuration)

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

// BenchmarkNTransform benchmarks the n-transform performance
func BenchmarkNTransform(b *testing.B) {
	tmpDir := b.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	os.MkdirAll(cacheDir, 0755)

	ext, err := NewExtractor(ModeVideo, "")
	if err != nil {
		b.Fatalf("failed to create extractor: %v", err)
	}
	ext.cacheManager = cache.NewCacheManagerWithDir(cacheDir)

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
		ext.cacheManager = cache.NewCacheManagerWithDir(cacheDir)

		_, err := ext.ExtractVideo("dQw4w9WgXcQ")
		if err != nil {
			b.Fatal(err)
		}

		CloseCachedEngine()
	}
}

func runMusicModeWithCookiesProbe(t *testing.T, poMode bool) {
	requireExplicitLiveProbeRun(t)

	if testing.Short() {
		t.Skip("Skipping in short mode")
	}

	home, _ := os.UserHomeDir()
	defaultCookiePath := filepath.Join(home, ".config", "ytx", "cookies.txt")

	if _, err := os.Stat(defaultCookiePath); os.IsNotExist(err) {
		t.Skip("Cookie file not found, skipping music mode test")
	}
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	runtime := NewRuntime()
	defer runtime.CloseCachedEngine()

	opts := []ExtractorOption{WithRuntime(runtime), WithCacheManager(cache.NewCacheManagerWithDir(cacheDir))}
	if poMode {
		opts = append(opts, WithPOMode(true))
	}

	ext, err := NewExtractor(ModeMusic, defaultCookiePath, opts...)
	if err != nil {
		t.Fatalf("failed to create extractor: %v", err)
	}

	videoID := "dQw4w9WgXcQ"

	extractStart := time.Now()
	result, err := ext.Extract(videoID)
	extractDuration := time.Since(extractStart)

	if err != nil {
		t.Fatalf("extraction failed: %v", err)
	}

	t.Logf("Music extraction: itag=%d, bitrate=%d, duration=%s",
		result.Itag, result.Bitrate, extractDuration)

	nTransformed := checkNTransformed(result.URL)
	t.Logf("N-parameter transformed: %v", nTransformed)

	status := probeStreamURL(result.URL)
	t.Logf("HTTP status: %d", status)
	if !poMode && status == http.StatusForbidden {
		t.Log("normal-mode live probe returned HTTP 403; treating as PO-gated live behavior (hint: rerun in explicit PO mode)")
		return
	}
	if poMode && status == http.StatusForbidden {
		t.Log("PO-mode live probe returned HTTP 403; retrying once with fresh runtime/cache")
		retryResult, retryStatus, retryErr := retrySingleMusicProbe(t, defaultCookiePath, videoID, true)
		if retryErr == nil && retryResult != nil && (retryStatus == http.StatusOK || retryStatus == http.StatusPartialContent) {
			return
		}
		if retryErr != nil {
			t.Logf("PO-mode retry extraction failed: %v", retryErr)
		} else {
			t.Logf("PO-mode retry returned HTTP %d", retryStatus)
		}
	}

	if status != http.StatusOK && status != http.StatusPartialContent {
		hint := ""
		if !poMode {
			hint = " (hint: rerun in explicit PO mode)"
		}
		t.Errorf("Expected HTTP 200, got %d%s", status, hint)
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
