package ytx

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrjared16/ytx/internal/cache"
)

// TestMusicModeWithCookies tests music mode if cookies are available
func TestMusicModeWithCookies(t *testing.T) {
	runMusicModeWithCookiesProbe(t, false)
}

func TestMusicModeWithCookiesPO(t *testing.T) {
	runMusicModeWithCookiesProbe(t, true)
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
	ext.cacheManager = cache.NewCacheManagerWithDir(cacheDir)
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
	ext.cacheManager = cache.NewCacheManagerWithDir(cacheDir)
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

func TestMusicModeExplicitPOModeInjectsTokenAndCachesIt(t *testing.T) {
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

	ext, err := NewExtractor(ModeMusic, cookieFile, WithPOMode(true))
	if err != nil {
		t.Fatalf("failed to create extractor: %v", err)
	}
	ext.cacheManager = cache.NewCacheManagerWithDir(cacheDir)
	ext.invalidateCipherCache()
	defer ext.invalidateCipherCache()

	var mu sync.Mutex
	attGetCount := 0
	playerBodyPoToken := ""

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

			case req.Method == http.MethodPost && req.URL.Host == "music.youtube.com" && req.URL.Path == "/youtubei/v1/att/get":
				mu.Lock()
				attGetCount++
				mu.Unlock()
				return jsonHTTPResponse(http.StatusOK, `{"bgChallenge":{"engagementType":"ENGAGEMENT_TYPE_UNBOUND","challengeToken":"CHALLENGE_TEST"}}`), nil

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

				responseBody := `{
"responseContext":{"visitorData":"VISITOR_TEST"},
"playabilityStatus":{"status":"OK"},
"streamingData":{"adaptiveFormats":[{"itag":141,"url":"https://stream.test/videoplayback?foo=bar","mimeType":"audio/mp4; codecs=\"mp4a.40.2\"","bitrate":256000,"quality":"tiny"}]},
"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"test","lengthSeconds":"1","author":"author","shortDescription":""}
}`
				return jsonHTTPResponse(http.StatusOK, responseBody), nil
			}

			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}),
	}

	result, err := ext.Extract("dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("first extraction failed: %v", err)
	}
	if !strings.Contains(result.URL, "pot=") {
		t.Fatalf("expected pot in URL, got %q", result.URL)
	}

	mu.Lock()
	if playerBodyPoToken == "" {
		mu.Unlock()
		t.Fatal("expected serviceIntegrityDimensions.poToken to be set")
	}
	if attGetCount != 1 {
		mu.Unlock()
		t.Fatalf("expected att/get to be called once, got %d", attGetCount)
	}
	mu.Unlock()

	_, err = ext.Extract("dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("second extraction failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if attGetCount != 1 {
		t.Fatalf("expected cached PO token reuse (att/get count stays 1), got %d", attGetCount)
	}

	CloseCachedEngine()
}
