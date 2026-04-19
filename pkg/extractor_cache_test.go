package ytx

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrjared16/ytx/internal/cache"
	"github.com/mrjared16/ytx/internal/jsengine"
	"github.com/mrjared16/ytx/internal/jsextract"
)

func TestWatchFirstRetryBaseURLUsesOriginForRelativePlayerPath(t *testing.T) {
	got := watchFirstRetryBaseURL("https://music.youtube.com", "/s/player/test123/player_ias.vflset/en_US/base.js")
	if got != "https://music.youtube.com" {
		t.Fatalf("expected origin fallback for relative player URL, got %q", got)
	}

	got = watchFirstRetryBaseURL("https://music.youtube.com", "https://www.youtube.com/s/player/test123/base.js")
	if got != "https://www.youtube.com" {
		t.Fatalf("expected absolute player URL to normalize to scheme://host, got %q", got)
	}
}

func TestStoreCipherCacheEvictsPreviousEngineKeyOnPlayerRotation(t *testing.T) {
	runtime := NewRuntime()
	runtime.SetEngineType(jsengine.EngineQuickJS)
	defer runtime.CloseCachedEngine()

	ext := &Extractor{runtime: runtime}

	runtimeJS := []byte(`function fn(n){return n.slice(1)};_exposed['fn']=fn;`)
	oldCipher := &Cipher{
		runtime:           runtime,
		nFunctionName:     "fn",
		nRuntimeJS:        runtimeJS,
		playerURL:         "/s/player/old/base.js",
		playerFingerprint: "old-fp",
	}

	if _, err := runtime.GetCachedEngine(context.Background(), oldCipher.engineCacheKey(), runtimeJS, "fn"); err != nil {
		t.Fatalf("GetCachedEngine old key failed: %v", err)
	}

	ext.storeCipherCache(oldCipher, time.Now().Add(time.Hour))

	newCipher := &Cipher{
		runtime:           runtime,
		nFunctionName:     "fn",
		nRuntimeJS:        runtimeJS,
		playerURL:         "/s/player/new/base.js",
		playerFingerprint: "new-fp",
	}
	ext.storeCipherCache(newCipher, time.Now().Add(time.Hour))

	runtime.engineCache.mu.Lock()
	_, oldExists := runtime.engineCache.cachedEngines[oldCipher.engineCacheKey()]
	runtime.engineCache.mu.Unlock()

	if oldExists {
		t.Fatal("expected old engine cache entry to be evicted after player rotation")
	}
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
	ext.cacheManager = cache.NewCacheManagerWithDir(cacheDir)
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
	ext.cacheManager = cache.NewCacheManagerWithDir(cacheDir)
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

func TestGetCachedCipherLoadsPlayerJSForWrapperCache(t *testing.T) {
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
	ext.invalidateCipherCache()
	defer ext.invalidateCipherCache()

	playerJS := `
(function(){
function URLObj(sig){this.map={s:sig};}
URLObj.prototype.get=function(k){return this.map[k];};
URLObj.prototype.set=function(k,v){this.map[k]=v;};
URLObj.prototype.apply=function(){this.map.s=this.map.s.split('').reverse().join('');};
var kS=function(url,mode,sig){var o=new URLObj(sig);o.set("alr","yes");o.apply();return o;};
})();`

	cache := &cache.CipherCache{
		Version:            cache.CurrentCacheVersion,
		CreatedAt:          time.Now(),
		ExpiresAt:          time.Now().Add(cache.CacheTTL),
		PlayerURL:          "/s/player/test123/player_ias.vflset/en_US/base.js",
		PlayerFingerprint:  computePlayerFingerprint([]byte(playerJS)),
		SigFunction:        "kS",
		SigUsesURLWrapper:  true,
		SignatureTimestamp: 12345,
		JSCode:             "stale-wrapper-runtime-should-not-load",
	}
	if err := ext.cacheManager.Save(cache); err != nil {
		t.Fatalf("failed to save wrapper cache: %v", err)
	}
	if err := ext.cacheManager.SavePlayerJS([]byte(playerJS)); err != nil {
		t.Fatalf("failed to save player.js: %v", err)
	}

	cipher, err := ext.getCachedCipher("dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("getCachedCipher returned error: %v", err)
	}
	if cipher == nil {
		t.Fatal("expected cipher")
	}
	if len(cipher.playerJS) == 0 {
		t.Fatal("expected wrapper cache load to hydrate raw player.js")
	}
	if cipher.jsCode != "" {
		t.Fatalf("expected wrapper cache load to avoid persisted wrapper runtime, got %q", cipher.jsCode)
	}

	decrypted, err := cipher.DecryptSignature("abc")
	if err != nil {
		t.Fatalf("DecryptSignature returned error: %v", err)
	}
	if decrypted != "cba" {
		t.Fatalf("unexpected decrypted signature: got %q want %q", decrypted, "cba")
	}
}

func TestGetCachedCipherLoadsWrapperBytecodeArtifact(t *testing.T) {
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
	ext.invalidateCipherCache()
	defer ext.invalidateCipherCache()

	playerJS := `
(function(){
function URLObj(sig){this.map={s:sig};}
URLObj.prototype.get=function(k){return this.map[k];};
URLObj.prototype.set=function(k,v){this.map[k]=v;};
URLObj.prototype.apply=function(){this.map.s=this.map.s.split('').reverse().join('');};
URLObj.prototype.update=function(){this.apply();};
var kS=function(url,mode,sig){var o=new URLObj(sig);o.set("alr","yes");return o;};
})();`

	wrapperRuntime := jsextract.BuildWrapperRuntimeJS(playerJS, "kS")
	wrapperBytecode, err := compileToQuickJSBytecode(wrapperRuntime)
	if err != nil {
		t.Fatalf("compileToQuickJSBytecode returned error: %v", err)
	}

	cache := &cache.CipherCache{
		Version:            cache.CurrentCacheVersion,
		CreatedAt:          time.Now(),
		ExpiresAt:          time.Now().Add(cache.CacheTTL),
		PlayerURL:          "/s/player/test123/player_ias.vflset/en_US/base.js",
		PlayerFingerprint:  computePlayerFingerprint([]byte(playerJS)),
		SigFunction:        "kS",
		SigUsesURLWrapper:  true,
		SignatureTimestamp: 12345,
		WrapperBuildID:     currentWrapperBytecodeBuildID(),
	}
	if err := ext.cacheManager.Save(cache); err != nil {
		t.Fatalf("failed to save wrapper cache: %v", err)
	}
	if err := ext.cacheManager.SavePlayerJS([]byte(playerJS)); err != nil {
		t.Fatalf("failed to save player.js: %v", err)
	}
	if err := ext.cacheManager.SaveWrapperRuntimeBytecode(wrapperBytecode); err != nil {
		t.Fatalf("failed to save wrapper bytecode: %v", err)
	}

	cipher, err := ext.getCachedCipher("dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("getCachedCipher returned error: %v", err)
	}
	if cipher == nil {
		t.Fatal("expected cipher")
	}
	if !cipher.isBytecode {
		t.Fatal("expected wrapper bytecode artifact to load as bytecode")
	}
	if cipher.jsCode == "" {
		t.Fatal("expected wrapper bytecode artifact to populate jsCode")
	}
	if len(cipher.playerJS) == 0 {
		t.Fatal("expected raw player.js to remain loaded for fallback")
	}

	decrypted, err := cipher.DecryptSignature("abc")
	if err != nil {
		t.Fatalf("DecryptSignature returned error: %v", err)
	}
	if decrypted != "cba" {
		t.Fatalf("unexpected decrypted signature: got %q want %q", decrypted, "cba")
	}
}

func TestGetCachedCipherUsesExpiredPersistentCacheWithinGrace(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}

	cm := cache.NewCacheManagerWithDir(cacheDir)
	ext, err := NewExtractor(ModeVideo, "")
	if err != nil {
		t.Fatalf("failed to create extractor: %v", err)
	}
	ext.cacheManager = cm
	ext.invalidateCipherCache()
	defer ext.invalidateCipherCache()

	expired := &cache.CipherCache{
		Version:            cache.CurrentCacheVersion,
		CreatedAt:          time.Now().Add(-2 * time.Hour),
		ExpiresAt:          time.Now().Add(-time.Minute),
		SigFunction:        "kS",
		SignatureTimestamp: 12345,
		JSCode:             `function kS(a){return a.split("").reverse().join("");}`,
	}
	if err := cm.Save(expired); err != nil {
		t.Fatalf("failed to save expired cache: %v", err)
	}

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

	cipher, err := ext.getCachedCipher("dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("getCachedCipher returned error: %v", err)
	}
	if cipher == nil {
		t.Fatal("expected cipher from stale cache")
	}
	if cm.IsValid(expired) {
		t.Fatal("expected expired persistent cache to remain strictly invalid")
	}
	if cipher.sigFunctionName != "kS" {
		t.Fatalf("unexpected cipher loaded from cache: %+v", cipher)
	}
	got, err := cipher.DecryptSignature("abc")
	if err != nil {
		t.Fatalf("DecryptSignature returned error: %v", err)
	}
	if got != "cba" {
		t.Fatalf("unexpected decrypt result: got %q want %q", got, "cba")
	}
}
