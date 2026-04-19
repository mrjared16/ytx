package ytx

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrjared16/ytx/internal/cache"
	"github.com/mrjared16/ytx/internal/jsextract"
)

func TestFindSigFunctionName(t *testing.T) {
	tests := []struct {
		name      string
		js        string
		wantName  string
		wantParam int
		wantErr   bool
	}{
		{
			name:      "detects decodeURIComponent signature function with param",
			js:        `x&&(x=AbC(123,decodeURIComponent(x)))`,
			wantName:  "AbC",
			wantParam: 123,
		},
		{
			name:      "detects decodeURIComponent signature function without param",
			js:        `x&&(x=TyZ(decodeURIComponent(x)))`,
			wantName:  "TyZ",
			wantParam: 0,
		},
		{
			name:      "detects double-parenthesized self-reference decodeURIComponent pattern",
			js:        `c&&((c)=Ab(decodeURIComponent(c)));`,
			wantName:  "Ab",
			wantParam: 0,
		},
		{
			name:      "detects encodeURIComponent set pattern",
			js:        `c&&d.set("sig",encodeURIComponent(Qq(a)))`,
			wantName:  "Qq",
			wantParam: 0,
		},
		{
			name:    "rejects url wrapper candidate without direct signature match",
			js:      `function LI(url,mode,sig){var o=new URLObj(sig);o.set("alr","yes");return o;};x&&(x=LI(decodeURIComponent(x)))`,
			wantErr: true,
		},
		{
			name:    "returns error when no pattern matches",
			js:      `var x = 1; function nope(){ return x; }`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotParam, err := findSigFunctionName([]byte(tc.js))

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got name=%q param=%d", gotName, gotParam)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if gotName != tc.wantName {
				t.Fatalf("name mismatch: got %q, want %q", gotName, tc.wantName)
			}

			if gotParam != tc.wantParam {
				t.Fatalf("param mismatch: got %d, want %d", gotParam, tc.wantParam)
			}
		})
	}
}

func TestDecryptSignatureExtractsFunctionOnDemand(t *testing.T) {
	playerJS := []byte(`x&&(x=AbC(decodeURIComponent(x)));function AbC(a){a=a.split("");a.reverse();return a.join("")}`)

	cipher := &Cipher{
		sigFunctionName: "AbC",
		playerJS:        playerJS,
	}

	if cipher.jsCode != "" {
		t.Fatal("expected empty jsCode before on-demand extraction")
	}

	decrypted, err := cipher.DecryptSignature("abc")
	if err != nil {
		t.Fatalf("DecryptSignature returned error: %v", err)
	}

	if decrypted != "cba" {
		t.Fatalf("unexpected decrypted signature: got %q want %q", decrypted, "cba")
	}

	if cipher.jsCode == "" {
		t.Fatal("expected jsCode to be populated after on-demand extraction")
	}
}

func TestDecryptSignatureWrapperUsesFullPlayerJS(t *testing.T) {
	playerJS := `
function URLObj(sig){this.map={s:sig};}
URLObj.prototype.get=function(k){return this.map[k];};
URLObj.prototype.set=function(k,v){this.map[k]=v;};
URLObj.prototype.apply=function(){this.map.s=this.map.s.split('').reverse().join('');};
URLObj.prototype.update=function(){this.apply();};
function kS(url,mode,sig){var o=new URLObj(sig);o.set("alr","yes");return o;}
`

	cipher := &Cipher{
		sigFunctionName:   "kS",
		sigUsesURLWrapper: true,
		jsCode:            `function broken(`,
		playerJS:          []byte(playerJS),
	}

	decrypted, err := cipher.DecryptSignature("abc")
	if err != nil {
		t.Fatalf("DecryptSignature returned error: %v", err)
	}

	if decrypted != "cba" {
		t.Fatalf("unexpected decrypted signature: got %q want %q", decrypted, "cba")
	}
}

func TestDecryptSignatureWrapperUsesBuiltRuntimeBeforeRawPlayerJS(t *testing.T) {
	playerJS := `
(function(){
function URLObj(sig){this.map={s:sig};}
URLObj.prototype.get=function(k){return this.map[k];};
URLObj.prototype.set=function(k,v){this.map[k]=v;};
URLObj.prototype.apply=function(){this.map.s=this.map.s.split('').reverse().join('');};
URLObj.prototype.update=function(){this.apply();};
var kS=function(url,mode,sig){var o=new URLObj(sig);o.set("alr","yes");return o;};
})();
`

	cipher := &Cipher{
		sigFunctionName:   "kS",
		sigUsesURLWrapper: true,
		jsCode:            jsextract.BuildWrapperRuntimeJS(playerJS, "kS"),
		playerJS:          []byte(playerJS),
	}

	decrypted, err := cipher.DecryptSignature("abc")
	if err != nil {
		t.Fatalf("DecryptSignature returned error: %v", err)
	}

	if decrypted != "cba" {
		t.Fatalf("unexpected decrypted signature: got %q want %q", decrypted, "cba")
	}
}

func TestDecryptSignatureColdStartRebuildsFromCachedRawPlayerJS(t *testing.T) {
	playerJS := `
(function(){
function URLObj(sig){this.map={s:sig};}
URLObj.prototype.get=function(k){return this.map[k];};
URLObj.prototype.set=function(k,v){this.map[k]=v;};
URLObj.prototype.apply=function(){this.map.s="ABCD"+Array(101).join("x");};
var kS=function(url,mode,sig){var o=new URLObj(sig);o.set("alr","yes");o.apply();return o;};
})();
`

	tmpDir := t.TempDir()
	cm := cache.NewCacheManagerWithDir(tmpDir)
	cache := &cache.CipherCache{
		Version:            cache.CurrentCacheVersion,
		CreatedAt:          time.Now(),
		ExpiresAt:          time.Now().Add(cache.CacheTTL),
		SigFunction:        "kS",
		SigUsesURLWrapper:  true,
		JSCode:             jsextract.BuildWrapperRuntimeJS(playerJS, "kS"),
		SignatureTimestamp: 12345,
	}
	if err := cm.Save(cache); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}
	if err := cm.SavePlayerJS([]byte(playerJS)); err != nil {
		t.Fatalf("SavePlayerJS returned error: %v", err)
	}

	loaded, err := cm.Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if loaded.JSCode != "" {
		t.Fatalf("expected wrapper cache load to ignore cipher_code.bin, got len=%d", len(loaded.JSCode))
	}
	playerJSBytes, err := cm.LoadPlayerJS()
	if err != nil {
		t.Fatalf("LoadPlayerJS returned error: %v", err)
	}

	cipher := NewCipherFromCache(loaded)
	cipher.playerJS = playerJSBytes
	decrypted, err := cipher.DecryptSignature("abc")
	if err != nil {
		t.Fatalf("DecryptSignature returned error: %v", err)
	}

	if len(decrypted) != 104 {
		t.Fatalf("unexpected decrypted signature length: got %d want 104", len(decrypted))
	}
	if !strings.HasPrefix(decrypted, "ABCD") {
		t.Fatalf("unexpected decrypted signature prefix: got %q", decrypted[:4])
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "cipher_code.bin")); !os.IsNotExist(err) {
		t.Fatalf("expected wrapper cache to avoid cipher_code.bin, got err=%v", err)
	}
}

func TestDecryptSignatureWrapperUsesArrowFunctionAssignment(t *testing.T) {
	playerJS := `
(function(){
function URLObj(sig){this.map={s:sig};}
URLObj.prototype.get=function(k){return this.map[k];};
URLObj.prototype.set=function(k,v){this.map[k]=v;};
URLObj.prototype.apply=function(){this.map.s=this.map.s.split('').reverse().join('');};
URLObj.prototype.update=function(){this.apply();};
var kS = (url, mode, sig) => { var o = new URLObj(sig); o.set("alr", "yes"); return o; };
})();
`

	cipher := &Cipher{
		sigFunctionName:   "kS",
		sigUsesURLWrapper: true,
		jsCode:            jsextract.BuildWrapperRuntimeJS(playerJS, "kS"),
		playerJS:          []byte(playerJS),
	}

	decrypted, err := cipher.DecryptSignature("abc")
	if err != nil {
		t.Fatalf("DecryptSignature returned error: %v", err)
	}

	if decrypted != "cba" {
		t.Fatalf("unexpected decrypted signature: got %q want %q", decrypted, "cba")
	}
}

func TestDecryptSignatureWrapperSkipsDollarPrefixedFalsePositive(t *testing.T) {
	playerJS := `
(function(){
function URLObj(sig){this.map={s:sig};}
URLObj.prototype.get=function(k){return this.map[k];};
URLObj.prototype.set=function(k,v){this.map[k]=v;};
URLObj.prototype.apply=function(){this.map.s=this.map.s.split('').reverse().join('');};
URLObj.prototype.update=function(){this.apply();};
$LI=function(url,mode,sig){ return { bogus: sig }; };
LI=function(url,mode,sig){var o=new URLObj(sig);o.set("alr","yes");return o;};
})();
`

	if got := findURLTransformFunctionName([]byte(playerJS)); got != "LI" {
		t.Fatalf("unexpected wrapper name: got %q want %q", got, "LI")
	}

	cipher := &Cipher{
		sigFunctionName:   "LI",
		sigUsesURLWrapper: true,
		jsCode:            jsextract.BuildWrapperRuntimeJS(playerJS, "LI"),
		playerJS:          []byte(playerJS),
	}

	decrypted, err := cipher.DecryptSignature("abc")
	if err != nil {
		t.Fatalf("DecryptSignature returned error: %v", err)
	}

	if decrypted != "cba" {
		t.Fatalf("unexpected decrypted signature: got %q want %q", decrypted, "cba")
	}
}

func TestEnsureSignatureReadyRejectsWrapperPrecompute(t *testing.T) {
	cipher := &Cipher{
		sigFunctionName:   "LI",
		sigUsesURLWrapper: true,
		playerJS:          []byte(`function LI(url,mode,sig){return sig}`),
	}

	err := cipher.ensureSignatureReady()
	if err == nil {
		t.Fatal("expected ensureSignatureReady to reject wrapper precompute")
	}
	if !strings.Contains(err.Error(), "runtime-only") {
		t.Fatalf("unexpected error: %v", err)
	}
	if cipher.jsCode != "" {
		t.Fatalf("wrapper precompute should not set jsCode, got len=%d", len(cipher.jsCode))
	}
}

func TestTransformNContextWrapperReturnsErrorWithoutTruncationFallback(t *testing.T) {
	cipher := &Cipher{
		sigFunctionName:   "LI",
		sigUsesURLWrapper: true,
		jsCode:            `function broken(`,
		playerJS:          []byte(`function stillBroken(`),
	}

	got, err := cipher.TransformNContext(context.Background(), "abcdef")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got != "abcdef" {
		t.Fatalf("unexpected n value: got %q want %q", got, "abcdef")
	}
}

func TestToCacheKeepsWrapperSignatureArtifactsWhenNFunctionExists(t *testing.T) {
	cipher := &Cipher{
		sigFunctionName:    "LI",
		sigParam:           7,
		sigUsesURLWrapper:  true,
		isBytecode:         true,
		nFunctionName:      "nFn",
		signatureTimestamp: 12345,
		jsCode:             "",
		playerURL:          "/s/player/demo/base.js",
		playerFingerprint:  "fp-demo",
	}

	cache := cipher.ToCache()

	if cache.SigFunction != "LI" {
		t.Fatalf("expected wrapper sig function to be preserved, got %q", cache.SigFunction)
	}
	if cache.SigParam != 7 {
		t.Fatalf("expected wrapper sig param to be preserved, got %d", cache.SigParam)
	}
	if !cache.SigUsesURLWrapper {
		t.Fatal("expected wrapper flag to be preserved in cache")
	}
	if !cache.IsBytecode {
		t.Fatal("expected wrapper bytecode flag to be preserved in cache")
	}
	if cache.JSCode != "" {
		t.Fatalf("expected empty wrapper jsCode to remain empty, got len=%d", len(cache.JSCode))
	}

	if cache.NFunction != "nFn" {
		t.Fatalf("expected n function to be preserved, got %q", cache.NFunction)
	}
	if cache.SignatureTimestamp != 12345 {
		t.Fatalf("expected signature timestamp to be preserved, got %d", cache.SignatureTimestamp)
	}
	if cache.PlayerURL != "/s/player/demo/base.js" {
		t.Fatalf("expected player URL to be preserved, got %q", cache.PlayerURL)
	}
	if cache.PlayerFingerprint != "fp-demo" {
		t.Fatalf("expected player fingerprint to be preserved, got %q", cache.PlayerFingerprint)
	}
}

func TestToCacheDropsWrapperArtifactsWithoutNFunction(t *testing.T) {
	cipher := &Cipher{
		sigFunctionName:    "LI",
		sigParam:           7,
		sigUsesURLWrapper:  true,
		isBytecode:         true,
		nFunctionName:      "",
		signatureTimestamp: 12345,
		jsCode:             "wrapper-runtime",
		playerURL:          "/s/player/demo/base.js",
		playerFingerprint:  "fp-demo",
	}

	cache := cipher.ToCache()

	if cache.SigFunction != "LI" {
		t.Fatalf("expected wrapper sig function to be preserved, got %q", cache.SigFunction)
	}
	if cache.SigParam != 7 {
		t.Fatalf("expected wrapper sig param to be preserved, got %d", cache.SigParam)
	}
	if !cache.SigUsesURLWrapper {
		t.Fatal("expected wrapper flag to be preserved")
	}
	if !cache.IsBytecode {
		t.Fatal("expected bytecode flag to be preserved")
	}
	if cache.JSCode != "" {
		t.Fatalf("expected wrapper runtime to be omitted from cache, got %q", cache.JSCode)
	}
	if cache.NFunction != "" {
		t.Fatalf("expected n function to remain empty, got %q", cache.NFunction)
	}
}

func TestToCacheDropsValidatedWrapperSignatureArtifacts(t *testing.T) {
	cipher := &Cipher{
		sigFunctionName:    "LI",
		sigParam:           7,
		sigUsesURLWrapper:  true,
		isBytecode:         false,
		nFunctionName:      "nFn",
		signatureTimestamp: 12345,
		jsCode:             "wrapper-runtime",
		playerURL:          "/s/player/demo/base.js",
		playerFingerprint:  "fp-demo",
	}

	cache := cipher.ToCache()

	if cache.SigFunction != "LI" {
		t.Fatalf("expected validated wrapper sig function to persist, got %q", cache.SigFunction)
	}
	if cache.SigParam != 7 {
		t.Fatalf("expected validated wrapper sig param to persist, got %d", cache.SigParam)
	}
	if !cache.SigUsesURLWrapper {
		t.Fatal("expected validated wrapper flag to persist in cache")
	}
	if cache.JSCode != "" {
		t.Fatalf("expected validated wrapper jsCode to be omitted, got %q", cache.JSCode)
	}
}

func TestFetchPlayerJSPrefersEmbedPageBeforeWatch(t *testing.T) {
	var mu sync.Mutex
	watchCalled := false
	embedCalled := false

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/embed/dQw4w9WgXcQ":
				mu.Lock()
				embedCalled = true
				mu.Unlock()
				html := `<!doctype html><html><script src="/s/player/embed123/player_ias.vflset/en_US/base.js"></script></html>`
				return jsonHTTPResponse(http.StatusOK, html), nil
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/watch":
				mu.Lock()
				watchCalled = true
				mu.Unlock()
				return nil, fmt.Errorf("watch page should not be fetched when embed succeeds")
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/s/player/embed123/player_ias.vflset/en_US/base.js":
				playerJS := `var signatureTimestamp=12345;x&&(x=AbC(decodeURIComponent(x)));function AbC(a){a=a.split("");a.reverse();return a.join("")}`
				return jsonHTTPResponse(http.StatusOK, playerJS), nil
			}

			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}),
	}

	playerPath, playerJS, err := fetchPlayerJS("dQw4w9WgXcQ", client, PlayerJSURLBase)
	if err != nil {
		t.Fatalf("fetchPlayerJS returned error: %v", err)
	}

	if playerPath != "/s/player/embed123/player_ias.vflset/en_US/base.js" {
		t.Fatalf("unexpected player path: got %q", playerPath)
	}
	if len(playerJS) == 0 {
		t.Fatal("expected player JS bytes")
	}

	mu.Lock()
	defer mu.Unlock()
	if !embedCalled {
		t.Fatal("expected embed page to be fetched")
	}
	if watchCalled {
		t.Fatal("did not expect watch page fetch when embed discovery succeeded")
	}
}

func TestFetchPlayerJSPrefersEmbedVariantWithinPage(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/embed/dQw4w9WgXcQ":
				html := `<!doctype html><html><script src="/s/player/embed123/player_ias.vflset/en_US/base.js"></script><script src="/s/player/embed123/player_embed.vflset/en_US/base.js"></script></html>`
				return jsonHTTPResponse(http.StatusOK, html), nil
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/s/player/embed123/player_embed.vflset/en_US/base.js":
				playerJS := `var signatureTimestamp=12345;x&&(x=AbC(decodeURIComponent(x)));function AbC(a){a=a.split("");a.reverse();return a.join("")}`
				return jsonHTTPResponse(http.StatusOK, playerJS), nil
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/watch":
				return nil, fmt.Errorf("watch page should not be fetched when embed succeeds")
			}

			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}),
	}

	playerPath, playerJS, err := fetchPlayerJS("dQw4w9WgXcQ", client, PlayerJSURLBase)
	if err != nil {
		t.Fatalf("fetchPlayerJS returned error: %v", err)
	}

	if playerPath != "/s/player/embed123/player_embed.vflset/en_US/base.js" {
		t.Fatalf("unexpected player path: got %q", playerPath)
	}
	if len(playerJS) == 0 {
		t.Fatal("expected player JS bytes")
	}
}

func TestFetchPlayerJSFallsBackToWatchWhenEmbedLacksPlayerPath(t *testing.T) {
	var mu sync.Mutex
	watchCalled := false
	embedCalled := false

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/embed/dQw4w9WgXcQ":
				mu.Lock()
				embedCalled = true
				mu.Unlock()
				return jsonHTTPResponse(http.StatusOK, `<!doctype html><html>no player here</html>`), nil
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/watch":
				mu.Lock()
				watchCalled = true
				mu.Unlock()
				html := `<!doctype html><html><script src="/s/player/watch123/player_ias.vflset/vi_VN/base.js"></script></html>`
				return jsonHTTPResponse(http.StatusOK, html), nil
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/s/player/watch123/player_ias.vflset/vi_VN/base.js":
				playerJS := `var signatureTimestamp=12345;x&&(x=AbC(decodeURIComponent(x)));function AbC(a){a=a.split("");a.reverse();return a.join("")}`
				return jsonHTTPResponse(http.StatusOK, playerJS), nil
			}

			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}),
	}

	playerPath, playerJS, err := fetchPlayerJS("dQw4w9WgXcQ", client, PlayerJSURLBase)
	if err != nil {
		t.Fatalf("fetchPlayerJS returned error: %v", err)
	}

	if playerPath != "/s/player/watch123/player_ias.vflset/vi_VN/base.js" {
		t.Fatalf("unexpected player path: got %q", playerPath)
	}
	if len(playerJS) == 0 {
		t.Fatal("expected player JS bytes")
	}

	mu.Lock()
	defer mu.Unlock()
	if !embedCalled {
		t.Fatal("expected embed page to be tried first")
	}
	if !watchCalled {
		t.Fatal("expected watch page fallback when embed page lacked player path")
	}
}

func TestNewCipherWithCachedPathWarnsAndFingerprintsOnCachedPathFallback(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/s/player/stale/player_ias/base.js":
				return jsonHTTPResponse(http.StatusNotFound, `missing`), nil
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/embed/dQw4w9WgXcQ":
				html := `<!doctype html><html><script src="/s/player/fresh123/player_ias.vflset/en_US/base.js"></script></html>`
				return jsonHTTPResponse(http.StatusOK, html), nil
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && req.URL.Path == "/s/player/fresh123/player_ias.vflset/en_US/base.js":
				playerJS := `var signatureTimestamp=12345;x&&(x=AbC(decodeURIComponent(x)));function AbC(a){a=a.split("");a.reverse();return a.join("")}`
				return jsonHTTPResponse(http.StatusOK, playerJS), nil
			}

			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}),
	}

	cipher, playerPath, err := NewCipherWithCachedPath("dQw4w9WgXcQ", client, "/s/player/stale/player_ias/base.js", "")
	if err != nil {
		t.Fatalf("NewCipherWithCachedPath returned error: %v", err)
	}

	if playerPath != "/s/player/fresh123/player_ias.vflset/en_US/base.js" {
		t.Fatalf("unexpected player path: got %q", playerPath)
	}
	if cipher.PlayerFingerprint() == "" {
		t.Fatal("expected quick player fingerprint to be populated")
	}
	warnings := cipher.Warnings()
	if len(warnings) == 0 {
		t.Fatal("expected cached-path fallback warning")
	}
	if !strings.Contains(warnings[0], "cached player path") {
		t.Fatalf("unexpected warning: %q", warnings[0])
	}
}
