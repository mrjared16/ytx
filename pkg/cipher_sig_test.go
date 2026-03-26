package ytx

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
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
			name:      "detects encodeURIComponent set pattern",
			js:        `c&&d.set("sig",encodeURIComponent(Qq(a)))`,
			wantName:  "Qq",
			wantParam: 0,
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
var kS=function(url,mode,sig){var o=new URLObj(sig);o.set("alr","yes");return o;};
})();
`

	cipher := &Cipher{
		sigFunctionName:   "kS",
		sigUsesURLWrapper: true,
		jsCode:            buildWrapperRuntimeJS(playerJS, "kS"),
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
