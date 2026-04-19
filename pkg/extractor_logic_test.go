package ytx

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrjared16/ytx/internal/cache"
	"github.com/mrjared16/ytx/internal/jsextract"
)

// TestNTransformIsolated tests n-transform without affecting system files
func TestNTransformIsolated(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping n-transform test in short mode")
	}

	tmpDir := t.TempDir()

	cacheDir := filepath.Join(tmpDir, "cache")
	os.MkdirAll(cacheDir, 0755)

	runtime := NewRuntime()
	defer runtime.CloseCachedEngine()

	ext, err := NewExtractor(ModeVideo, "", WithRuntime(runtime))
	if err != nil {
		t.Fatalf("failed to create extractor: %v", err)
	}
	ext.cacheManager = cache.NewCacheManagerWithDir(cacheDir)

	videoID := "dQw4w9WgXcQ"

	cipherStart := time.Now()
	cipher, err := ext.getCachedCipher(videoID)
	cipherDuration := time.Since(cipherStart)
	t.Logf("Cipher fetch: %s", cipherDuration)

	if err != nil {
		t.Fatalf("failed to get cipher: %v", err)
	}

	t.Logf("N-function name: %s", cipher.NFunctionName())
	t.Logf("Player.js size: %d bytes", cipher.PlayerJSLen())

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

}

// TestCacheIsolation verifies that tests don't affect system cache
func TestCacheIsolation(t *testing.T) {

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

	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "isolated_cache")
	os.MkdirAll(cacheDir, 0755)

	cm := cache.NewCacheManagerWithDir(cacheDir)

	testCache := &cache.CipherCache{
		Version:     1,
		PlayerURL:   "/test/player.js",
		SigFunction: "testFunc",
	}
	err := cm.Save(testCache)
	if err != nil {
		t.Fatalf("failed to save to isolated cache: %v", err)
	}

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
	ext.cacheManager = cache.NewCacheManagerWithDir(filepath.Join(tmpDir, "cache"))
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

func TestPersistCipherArtifactsWritesWrapperBytecode(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	ext, err := NewExtractor(ModeVideo, "")
	if err != nil {
		t.Fatalf("failed to create extractor: %v", err)
	}
	ext.cacheManager = cache.NewCacheManagerWithDir(cacheDir)
	defer ext.invalidateCipherCache()

	playerJS := []byte(`
(function(){
function URLObj(sig){this.map={s:sig};}
URLObj.prototype.get=function(k){return this.map[k];};
URLObj.prototype.set=function(k,v){this.map[k]=v;};
URLObj.prototype.apply=function(){this.map.s=this.map.s.split('').reverse().join('');};
URLObj.prototype.update=function(){this.apply();};
var kS=function(url,mode,sig){var o=new URLObj(sig);o.set("alr","yes");return o;};
})();`)

	ext.cipher = &Cipher{
		sigFunctionName:   "kS",
		sigUsesURLWrapper: true,
		jsCode:            jsextract.BuildWrapperRuntimeJS(string(playerJS), "kS"),
		playerJS:          playerJS,
		playerURL:         "/s/player/test123/player_ias.vflset/en_US/base.js",
		playerFingerprint: computePlayerFingerprint(playerJS),
	}

	ext.persistCipherArtifacts()

	cache, err := ext.cacheManager.Load()
	if err != nil {
		t.Fatalf("failed to load persisted cache: %v", err)
	}
	if cache.WrapperBuildID != currentWrapperBytecodeBuildID() {
		t.Fatalf("unexpected wrapper build id: got %q want %q", cache.WrapperBuildID, currentWrapperBytecodeBuildID())
	}
	if cache.JSCode != "" {
		t.Fatalf("expected wrapper runtime text to stay out of cache metadata, got len=%d", len(cache.JSCode))
	}
	if _, err := ext.cacheManager.LoadWrapperRuntimeBytecode(); err != nil {
		t.Fatalf("expected wrapper bytecode artifact: %v", err)
	}
}
