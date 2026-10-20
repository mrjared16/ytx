package ytx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mrjared16/ytx/internal/cache"

	"golang.org/x/sync/singleflight"
)

// rangeProbeResponse serves a 206 ranged response for the requested range,
// validating it semantically (0 <= start <= end < total) instead of blindly
// echoing. Out-of-range requests get 416.
func rangeProbeResponse(req *http.Request, total int64) *http.Response {
	rangeHdr := req.Header.Get("Range")
	start, end := int64(-1), int64(-1)
	if _, err := fmt.Sscanf(rangeHdr, "bytes=%d-%d", &start, &end); err != nil {
		return jsonHTTPResponse(http.StatusBadRequest, "bad range")
	}
	if start < 0 || end < start || end >= total {
		return jsonHTTPResponse(http.StatusRequestedRangeNotSatisfiable, "range out of bounds")
	}
	resp := &http.Response{
		StatusCode: http.StatusPartialContent,
		Header: http.Header{
			"Content-Range": []string{fmt.Sprintf("bytes %d-%d/%d", start, end, total)},
		},
		Body: io.NopCloser(strings.NewReader("x")),
	}
	return resp
}

type playerCall struct {
	clientName    string
	clientVersion string
	userAgent     string
	visitorID     string
	url           string
	body          string
}

type chainHarness struct {
	t            *testing.T
	mu           sync.Mutex
	playerCalls  []playerCall
	probeUAs     map[string][]string
	probeRanges  map[string][]string
	playerByName map[string]func(call int) (int, string)
	probeByHost  map[string]func(req *http.Request) *http.Response
	// bootstrapFn serves WEB guide-bootstrap POSTs (no X-YouTube-Client-Name).
	// Nil means bootstraps fail as unexpected requests.
	bootstrapFn func(call int) (int, string)
	// bootstrapReqFn, when set, takes precedence over bootstrapFn and also
	// receives the request so handlers can observe req.Context().Done().
	bootstrapReqFn func(req *http.Request, call int) (int, string)
	// bootstrapEntered receives one value per ENTERED WEB guide-bootstrap
	// request, so a test can assert a bootstrap was never even attempted (as
	// opposed to merely not having incremented the count by assertion time).
	// A nil channel is safe: the signal send sits in a default branch.
	bootstrapEntered chan bool
	playerCount      int
	bootstrapCount   int
}

func newChainHarness(t *testing.T) *chainHarness {
	t.Helper()
	return &chainHarness{
		t:            t,
		probeUAs:     map[string][]string{},
		probeRanges:  map[string][]string{},
		playerByName: map[string]func(call int) (int, string){},
		probeByHost:  map[string]func(req *http.Request) *http.Response{},
	}
}

func (h *chainHarness) client() *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPost && req.URL.Host == "www.youtube.com" && req.URL.Path == "/youtubei/v1/player" {
				body, _ := io.ReadAll(req.Body)
				name := req.Header.Get("X-YouTube-Client-Name")
				h.mu.Lock()
				if name == "" {
					// WEB guide-bootstrap (no client header): bypasses cache.
					h.bootstrapCount++
					call := h.bootstrapCount
					select {
					case h.bootstrapEntered <- true:
					default:
					}
					reqFn := h.bootstrapReqFn
					fn := h.bootstrapFn
					h.mu.Unlock()
					if reqFn != nil {
						status, respBody := reqFn(req, call)
						return jsonHTTPResponse(status, respBody), nil
					}
					if fn == nil {
						return nil, fmt.Errorf("unexpected bootstrap request")
					}
					status, respBody := fn(call)
					return jsonHTTPResponse(status, respBody), nil
				}
				h.playerCount++
				call := h.playerCount
				h.playerCalls = append(h.playerCalls, playerCall{
					clientName:    name,
					clientVersion: req.Header.Get("X-YouTube-Client-Version"),
					userAgent:     req.Header.Get("User-Agent"),
					visitorID:     req.Header.Get("X-Goog-Visitor-Id"),
					url:           req.URL.String(),
					body:          string(body),
				})
				fn := h.playerByName[name]
				h.mu.Unlock()
				if fn == nil {
					return nil, fmt.Errorf("unexpected client name %q", name)
				}
				status, respBody := fn(call)
				return jsonHTTPResponse(status, respBody), nil
			}
			if req.Method == http.MethodGet {
				h.mu.Lock()
				h.probeUAs[req.URL.String()] = append(h.probeUAs[req.URL.String()], req.Header.Get("User-Agent"))
				h.probeRanges[req.URL.String()] = append(h.probeRanges[req.URL.String()], req.Header.Get("Range"))
				fn := h.probeByHost[req.URL.Host]
				h.mu.Unlock()
				if fn == nil {
					return nil, fmt.Errorf("unexpected probe host %q", req.URL.Host)
				}
				return fn(req), nil
			}
			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}),
	}
}

func okPlayerBody(videoURL, audioURL string, withContentLength bool) string {
	return okPlayerBodyWithVisitor(videoURL, audioURL, withContentLength, "VISITOR_TEST")
}

func okPlayerBodyWithVisitor(videoURL, audioURL string, withContentLength bool, visitor string) string {
	cl := ""
	if withContentLength {
		cl = `,"contentLength":"1000"`
	}
	return `{"responseContext":{"visitorData":"` + visitor + `"},"playabilityStatus":{"status":"OK"},` +
		`"streamingData":{"adaptiveFormats":[` +
		`{"itag":137,"url":"` + videoURL + `","mimeType":"video/mp4; codecs=\"avc1.640028\"","bitrate":4000000,"width":1920,"height":1080,"quality":"hd1080"` + cl + `},` +
		`{"itag":140,"url":"` + audioURL + `","mimeType":"audio/mp4; codecs=\"mp4a.40.2\"","bitrate":128000,"quality":"tiny","audioQuality":"AUDIO_QUALITY_MEDIUM"` + cl + `}` +
		`]},"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"test title","lengthSeconds":"120","author":"test author","shortDescription":""}}`
}

func loginPlayerBody() string {
	return `{"responseContext":{"visitorData":"VISITOR_TEST"},"playabilityStatus":{"status":"LOGIN_REQUIRED","reason":"Sign in to confirm you are not a bot"},"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"t","lengthSeconds":"1","author":"a","shortDescription":""}}`
}

func loginPlayerBodyWithVisitor(visitor string) string {
	return `{"responseContext":{"visitorData":"` + visitor + `"},"playabilityStatus":{"status":"LOGIN_REQUIRED","reason":"Sign in to confirm"},"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"t","lengthSeconds":"1","author":"a","shortDescription":""}}`
}

func unplayablePlayerBody() string {
	return `{"responseContext":{"visitorData":"VISITOR_TEST"},"playabilityStatus":{"status":"ERROR","reason":"Video unavailable"},"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"t","lengthSeconds":"1","author":"a","shortDescription":""}}`
}

// trueSABRPlayerBody has adaptive entries with neither url nor signatureCipher.
func trueSABRPlayerBody() string {
	return `{"responseContext":{"visitorData":"VISITOR_TEST"},"playabilityStatus":{"status":"OK"},` +
		`"streamingData":{"adaptiveFormats":[` +
		`{"itag":137,"mimeType":"video/mp4; codecs=\"avc1.640028\"","bitrate":4000000,"width":1920,"height":1080,"quality":"hd1080"},` +
		`{"itag":140,"mimeType":"audio/mp4; codecs=\"mp4a.40.2\"","bitrate":128000,"quality":"tiny"}` +
		`]},"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"t","lengthSeconds":"1","author":"a","shortDescription":""}}`
}

// cipherOnlyPlayerBody has adaptive entries with signatureCipher but no direct URL.
func cipherOnlyPlayerBody() string {
	return `{"responseContext":{"visitorData":"VISITOR_TEST"},"playabilityStatus":{"status":"OK"},` +
		`"streamingData":{"adaptiveFormats":[` +
		`{"itag":137,"signatureCipher":"url=https%3A%2F%2Fx&s=abc&sp=sig","mimeType":"video/mp4; codecs=\"avc1.640028\"","bitrate":4000000,"width":1920,"height":1080,"quality":"hd1080"},` +
		`{"itag":140,"signatureCipher":"url=https%3A%2F%2Fx&s=abc&sp=sig","mimeType":"audio/mp4; codecs=\"mp4a.40.2\"","bitrate":128000,"quality":"tiny"}` +
		`]},"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"t","lengthSeconds":"1","author":"a","shortDescription":""}}`
}

// shadowedDirectPlayerBody ranks preferred ciphered formats first, with usable
// direct fallbacks present. Selection must not reject the direct streams.
func shadowedDirectPlayerBody() string {
	return `{"responseContext":{"visitorData":"VISITOR_TEST"},"playabilityStatus":{"status":"OK"},` +
		`"streamingData":{"adaptiveFormats":[` +
		`{"itag":137,"signatureCipher":"url=https%3A%2F%2Fx&s=abc&sp=sig","mimeType":"video/mp4; codecs=\"avc1.640028\"","bitrate":8000000,"width":1920,"height":1080,"quality":"hd1080"},` +
		`{"itag":136,"url":"https://synthetic-video.googlevideo.com/videoplayback","mimeType":"video/mp4; codecs=\"avc1.4d401f\"","bitrate":3000000,"width":1280,"height":720,"quality":"hd720","contentLength":"1000"},` +
		`{"itag":774,"signatureCipher":"url=https%3A%2F%2Fx&s=abc&sp=sig","mimeType":"audio/webm; codecs=\"opus\"","bitrate":256000,"quality":"tiny"},` +
		`{"itag":140,"url":"https://synthetic-audio.googlevideo.com/videoplayback","mimeType":"audio/mp4; codecs=\"mp4a.40.2\"","bitrate":128000,"quality":"tiny","contentLength":"1000"}` +
		`]},"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"t","lengthSeconds":"1","author":"a","shortDescription":""}}`
}

func audioOnlyPlayerBody() string {
	return `{"responseContext":{"visitorData":"VISITOR_TEST"},"playabilityStatus":{"status":"OK"},` +
		`"streamingData":{"formats":[{"itag":18,"url":"https://synthetic-muxed.googlevideo.com/videoplayback","mimeType":"video/mp4","bitrate":500000,"width":640,"height":360,"quality":"medium"}],` +
		`"adaptiveFormats":[{"itag":140,"url":"https://synthetic-audio.googlevideo.com/videoplayback","mimeType":"audio/mp4; codecs=\"mp4a.40.2\"","bitrate":128000,"quality":"tiny"}]},` +
		`"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"t","lengthSeconds":"1","author":"a","shortDescription":""}}`
}

func newVideoTestExtractor(t *testing.T, h *chainHarness) *Extractor {
	t.Helper()
	ext, err := NewExtractor(ModeVideo, "", WithRuntime(NewRuntime()), WithHTTPClient(h.client()))
	if err != nil {
		t.Fatalf("NewExtractor returned error: %v", err)
	}
	ext.cacheManager = nil
	ext.visitorData = "VISITOR_TEST"
	return ext
}

func okProbe(_ *http.Request) func(req *http.Request) *http.Response {
	return func(req *http.Request) *http.Response { return rangeProbeResponse(req, 1000) }
}

func requireExhausted(t *testing.T, err error) *VideoExhaustedError {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var exhausted *VideoExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected VideoExhaustedError, got %T: %v", err, err)
	}
	return exhausted
}

func TestVideoChainFirstSuccessStops(t *testing.T) {
	h := newChainHarness(t)
	body := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, body }
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	if h.playerCount != 1 {
		t.Fatalf("expected 1 player call, got %d", h.playerCount)
	}
	if result.Client != "VISIONOS" {
		t.Fatalf("expected client VISIONOS, got %q", result.Client)
	}
	if result.UserAgent == "" {
		t.Fatal("expected user_agent to be set")
	}
	if result.Title != "test title" || result.Author != "test author" {
		t.Fatalf("title/author not preserved: %+v", result)
	}
	if got := h.playerCalls[0]; got.clientVersion != "1.02" || got.userAgent != result.UserAgent {
		t.Fatalf("header/profile mismatch: %+v vs %q", got, result.UserAgent)
	}
}

func TestVideoChainLoginFallback(t *testing.T) {
	h := newChainHarness(t)
	ok := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, loginPlayerBody() }
	h.playerByName["28"] = func(int) (int, string) { return http.StatusOK, ok }
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	if h.playerCount != 2 {
		t.Fatalf("expected 2 player calls, got %d", h.playerCount)
	}
	if result.Client != "ANDROID_VR" {
		t.Fatalf("expected fallback client ANDROID_VR, got %q", result.Client)
	}
}

func TestVideoChainTrueSABRFallback(t *testing.T) {
	h := newChainHarness(t)
	ok := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, trueSABRPlayerBody() }
	h.playerByName["28"] = func(int) (int, string) { return http.StatusOK, ok }
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	if result.Client != "ANDROID_VR" {
		t.Fatalf("expected SABR fallback to ANDROID_VR, got %q", result.Client)
	}
}

func TestVideoChainCipherOnlyFallback(t *testing.T) {
	h := newChainHarness(t)
	ok := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, cipherOnlyPlayerBody() }
	h.playerByName["28"] = func(int) (int, string) { return http.StatusOK, ok }
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	if result.Client != "ANDROID_VR" {
		t.Fatalf("expected cipher-only fallback to ANDROID_VR, got %q", result.Client)
	}
}

func TestVideoChainExhaustedKinds(t *testing.T) {
	for _, tt := range []struct {
		name string
		body func() string
		kind string
	}{
		{"sabr", trueSABRPlayerBody, VideoAttemptSABROnly},
		{"cipher", cipherOnlyPlayerBody, VideoAttemptCipherOnly},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newChainHarness(t)
			for _, name := range []string{"101", "28", "3", "5"} {
				h.playerByName[name] = func(int) (int, string) { return http.StatusOK, tt.body() }
			}
			ext := newVideoTestExtractor(t, h)
			exhausted := requireExhausted(t, func() error {
				_, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
				return err
			}())
			if len(exhausted.Attempts) != 4 {
				t.Fatalf("expected 4 attempts, got %d", len(exhausted.Attempts))
			}
			for i, a := range exhausted.Attempts {
				if a.Kind != tt.kind {
					t.Fatalf("attempt %d: expected kind %q, got %+v", i, tt.kind, a)
				}
			}
		})
	}
}

func TestVideoChainDirectNotShadowedByCipher(t *testing.T) {
	h := newChainHarness(t)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, shadowedDirectPlayerBody() }
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	if h.playerCount != 1 {
		t.Fatalf("expected direct streams accepted on first profile, got %d player calls", h.playerCount)
	}
	if result.VideoItag != 136 || result.AudioItag != 140 {
		t.Fatalf("expected direct itags 136/140, got %d/%d", result.VideoItag, result.AudioItag)
	}
}

func TestVideoChainMissingStreamFallback(t *testing.T) {
	h := newChainHarness(t)
	ok := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, audioOnlyPlayerBody() }
	h.playerByName["28"] = func(int) (int, string) { return http.StatusOK, ok }
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	if result.Client != "ANDROID_VR" {
		t.Fatalf("expected missing-video fallback to ANDROID_VR, got %q", result.Client)
	}
	if strings.Contains(result.VideoURL, "muxed") {
		t.Fatalf("muxed-only stream must not be accepted: %q", result.VideoURL)
	}
}

func TestVideoChainProbe403Fallback(t *testing.T) {
	h := newChainHarness(t)
	bad := okPlayerBody("https://synthetic-bad-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	ok := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, bad }
	h.playerByName["28"] = func(int) (int, string) { return http.StatusOK, ok }
	h.probeByHost["synthetic-bad-video.googlevideo.com"] = func(req *http.Request) *http.Response {
		return jsonHTTPResponse(http.StatusForbidden, "forbidden")
	}
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	if result.Client != "ANDROID_VR" {
		t.Fatalf("expected probe-403 fallback to ANDROID_VR, got %q", result.Client)
	}
}

func TestVideoChainProbeTotalMismatchFallback(t *testing.T) {
	h := newChainHarness(t)
	bad := okPlayerBody("https://synthetic-bad-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	ok := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, bad }
	h.playerByName["28"] = func(int) (int, string) { return http.StatusOK, ok }
	h.probeByHost["synthetic-bad-video.googlevideo.com"] = func(req *http.Request) *http.Response {
		// Correct range but wrong total: must be rejected.
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     http.Header{"Content-Range": []string{"bytes 999-999/2000"}},
			Body:       io.NopCloser(strings.NewReader("x")),
		}
	}
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	if result.Client != "ANDROID_VR" {
		t.Fatalf("expected total-mismatch fallback to ANDROID_VR, got %q", result.Client)
	}
}

func TestVideoChainDiscoveryMustBeZeroZero(t *testing.T) {
	h := newChainHarness(t)
	bad := okPlayerBody("https://synthetic-bad-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", false)
	ok := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, bad }
	h.playerByName["28"] = func(int) (int, string) { return http.StatusOK, ok }
	h.probeByHost["synthetic-bad-video.googlevideo.com"] = func(req *http.Request) *http.Response {
		// Discovery asks bytes=0-0 but server answers a wider range: reject.
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     http.Header{"Content-Range": []string{"bytes 0-1/1000"}},
			Body:       io.NopCloser(strings.NewReader("xx")),
		}
	}
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	if result.Client != "ANDROID_VR" {
		t.Fatalf("expected discovery-mismatch fallback to ANDROID_VR, got %q", result.Client)
	}
}

func TestVideoChainDiscoveryEnd403Fallback(t *testing.T) {
	h := newChainHarness(t)
	bad := okPlayerBody("https://synthetic-bad-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", false)
	ok := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, bad }
	h.playerByName["28"] = func(int) (int, string) { return http.StatusOK, ok }
	h.probeByHost["synthetic-bad-video.googlevideo.com"] = func(req *http.Request) *http.Response {
		// Byte-0 discovery succeeds, final-byte probe is forbidden.
		if req.Header.Get("Range") == "bytes=0-0" {
			return rangeProbeResponse(req, 1000)
		}
		return jsonHTTPResponse(http.StatusForbidden, "forbidden")
	}
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	if result.Client != "ANDROID_VR" {
		t.Fatalf("expected end-403 fallback to ANDROID_VR, got %q", result.Client)
	}
}

func TestVideoChainSecondaryProbe429Stops(t *testing.T) {
	h := newChainHarness(t)
	body := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, body }
	h.playerByName["28"] = func(int) (int, string) { return http.StatusOK, body }
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = func(req *http.Request) *http.Response {
		return jsonHTTPResponse(http.StatusTooManyRequests, "rate limited")
	}

	ext := newVideoTestExtractor(t, h)
	exhausted := requireExhausted(t, func() error {
		_, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
		return err
	}())
	if h.playerCount != 1 {
		t.Fatalf("expected probe-429 to stop the chain, got %d player calls", h.playerCount)
	}
	if !exhausted.RateLimited() {
		t.Fatalf("expected RateLimited, got %+v", exhausted.Attempts)
	}
	if exhausted.MostlyLogin() {
		t.Fatal("429 must never classify as AUTH_FAILED")
	}
}

func TestVideoChainProbesUseWinningUA(t *testing.T) {
	h := newChainHarness(t)
	body := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, body }
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	for url, uas := range h.probeUAs {
		if len(uas) == 0 {
			t.Fatalf("no probe recorded for %s", url)
		}
		for _, ua := range uas {
			if ua != result.UserAgent {
				t.Fatalf("probe UA mismatch for %s: got %q want %q", url, ua, result.UserAgent)
			}
		}
	}
	if len(h.probeUAs) != 2 {
		t.Fatalf("expected probes for video+audio, got %v", h.probeUAs)
	}
}

func TestVideoChainProbeRequestsExactRanges(t *testing.T) {
	h := newChainHarness(t)
	known := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, known }
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	if _, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ"); err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	// Known contentLength=1000: exactly one final-byte request per stream.
	for _, url := range []string{"https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback"} {
		want := []string{"bytes=999-999"}
		if !reflect.DeepEqual(h.probeRanges[url], want) {
			t.Fatalf("probe ranges for %s: got %v want %v", url, h.probeRanges[url], want)
		}
	}
}

func TestVideoChainProbeWithoutContentLength(t *testing.T) {
	h := newChainHarness(t)
	body := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", false)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, body }
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	if result.Client != "VISIONOS" {
		t.Fatalf("expected VISIONOS, got %q", result.Client)
	}
	// Unknown length: byte-0 discovery then final-byte probe, per stream.
	for _, url := range []string{"https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback"} {
		want := []string{"bytes=0-0", "bytes=999-999"}
		if !reflect.DeepEqual(h.probeRanges[url], want) {
			t.Fatalf("probe ranges for %s: got %v want %v", url, h.probeRanges[url], want)
		}
	}
}

func freshVisitorBody() string {
	return `{"responseContext":{"visitorData":"VISITOR_FRESH"}}`
}

// lastPlayerVisitor returns the visitor header of the most recent player call.
func (h *chainHarness) lastPlayerVisitor() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.playerCalls) == 0 {
		return ""
	}
	return h.playerCalls[len(h.playerCalls)-1].visitorID
}

func TestVideoChainIgnoresNonOKVisitorData(t *testing.T) {
	h := newChainHarness(t)
	ok := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	// Failed responses carry a visitor that must never be adopted: the second
	// profile keeps using the constant bootstrap value.
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, loginPlayerBodyWithVisitor("VISITOR_BAD") }
	h.playerByName["28"] = func(int) (int, string) { return http.StatusOK, ok }
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	if result.Client != "ANDROID_VR" {
		t.Fatalf("expected ANDROID_VR, got %q", result.Client)
	}
	if len(h.playerCalls) != 2 {
		t.Fatalf("expected 2 player calls, got %d", len(h.playerCalls))
	}
	if h.playerCalls[0].visitorID != "VISITOR_TEST" {
		t.Fatalf("expected bootstrapped visitor header, got %+v", h.playerCalls[0])
	}
	if h.playerCalls[1].visitorID != "VISITOR_TEST" {
		t.Fatalf("failed-response visitor must not be adopted, got %+v", h.playerCalls[1])
	}
	var reqBody InnertubeRequest
	if err := json.Unmarshal([]byte(h.playerCalls[1].body), &reqBody); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if reqBody.Context.Client.VisitorData != "VISITOR_TEST" {
		t.Fatalf("expected constant bootstrap visitor in body, got %q", reqBody.Context.Client.VisitorData)
	}
	if ext.visitorData != "VISITOR_TEST" {
		t.Fatalf("expected extractor visitorData unchanged, got %q", ext.visitorData)
	}
}

// TestVideoChainFailedBootstrapVisitorNotAdopted covers the bootstrap-failure
// ownership hole: when the initial visitor bootstrap fails or leaves the
// visitor empty, an HTTP-200 LOGIN_REQUIRED response carrying VISITOR_BAD
// must neither be adopted into extractor state nor persisted by the later
// winner. Every player attempt keeps the original empty visitor, and no
// forced-refresh cycle is entered because the first pass already produced a
// result.
func TestVideoChainFailedBootstrapVisitorNotAdopted(t *testing.T) {
	for _, tt := range []struct {
		name        string
		bootstrapFn func(int) (int, string)
	}{
		{"bootstrap fails", func(int) (int, string) { return http.StatusInternalServerError, "boom" }},
		{"bootstrap empty", func(int) (int, string) { return http.StatusOK, `{"responseContext":{}}` }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newChainHarness(t)
			// Failed and successful responses both carry replacement visitors;
			// neither may be adopted because the guide bootstrap is
			// authoritative per runVideoChain's invariant.
			ok := okPlayerBodyWithVisitor("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true, "VISITOR_OK_RESP")
			h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, loginPlayerBodyWithVisitor("VISITOR_BAD") }
			h.playerByName["28"] = func(int) (int, string) { return http.StatusOK, ok }
			h.bootstrapFn = tt.bootstrapFn
			h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
			h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

			ext := newVideoTestExtractor(t, h)
			ext.visitorData = "" // bootstrap leaves the visitor empty
			ext.cacheManager = cache.NewCacheManagerWithDir(t.TempDir())

			result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
			if err != nil {
				t.Fatalf("ExtractVideoContext returned error: %v", err)
			}
			if result.Client != "ANDROID_VR" {
				t.Fatalf("expected ANDROID_VR, got %q", result.Client)
			}
			if len(h.playerCalls) != 2 {
				t.Fatalf("expected 2 player calls, got %d", len(h.playerCalls))
			}
			for i, call := range h.playerCalls {
				if call.visitorID != "" {
					t.Fatalf("player call %d: expected empty visitor header, got %q", i, call.visitorID)
				}
				var reqBody InnertubeRequest
				if err := json.Unmarshal([]byte(call.body), &reqBody); err != nil {
					t.Fatalf("decode request body %d: %v", i, err)
				}
				if reqBody.Context.Client.VisitorData != "" {
					t.Fatalf("player call %d: expected empty visitor in body, got %q", i, reqBody.Context.Client.VisitorData)
				}
			}
			// The failed-response visitor must never be adopted, even though
			// the bootstrap left the extractor with an empty visitor.
			if ext.visitorData != "" {
				t.Fatalf("failed-response visitor was adopted: got %q, want empty", ext.visitorData)
			}
			if vc, _, err := ext.cacheManager.LoadVisitorDataAllowStale(0); err == nil && vc != nil && vc.Data == "VISITOR_BAD" {
				t.Fatalf("failed-response visitor was persisted: %+v", vc)
			}
			// First pass returned a result: exactly one bootstrap attempt, no
			// forced-refresh cycle.
			if h.bootstrapCount != 1 {
				t.Fatalf("expected exactly 1 bootstrap attempt, got %d", h.bootstrapCount)
			}
		})
	}
}

func TestVideoChainPoisonedVisitorSelfHeals(t *testing.T) {
	h := newChainHarness(t)
	ok := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	// Poisoned cached visitor: every profile gates LOGIN_REQUIRED until the
	// forced fresh bootstrap supplies VISITOR_FRESH.
	for _, name := range []string{"101", "28", "3", "5"} {
		h.playerByName[name] = func(int) (int, string) {
			if h.lastPlayerVisitor() == "VISITOR_FRESH" {
				return http.StatusOK, ok
			}
			return http.StatusOK, loginPlayerBodyWithVisitor("VISITOR_BAD")
		}
	}
	h.bootstrapFn = func(int) (int, string) { return http.StatusOK, freshVisitorBody() }
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	ext.visitorData = "VISITOR_POISONED"

	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	if result.Client != "VISIONOS" {
		t.Fatalf("expected retry to succeed on VISIONOS, got %q", result.Client)
	}
	if h.playerCount != 5 {
		t.Fatalf("expected 4 gated calls + 1 retry call, got %d", h.playerCount)
	}
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly one forced refresh, got %d", h.bootstrapCount)
	}
	if h.playerCalls[4].visitorID != "VISITOR_FRESH" {
		t.Fatalf("expected retry with fresh visitor, got %+v", h.playerCalls[4])
	}
	if ext.visitorData != "VISITOR_FRESH" {
		t.Fatalf("expected fresh visitor applied, got %q", ext.visitorData)
	}
}

func TestVideoChainSingleForcedRefresh(t *testing.T) {
	h := newChainHarness(t)
	// Even the fresh visitor stays gated: exactly one refresh, then exhaust
	// with both passes' attempts retained.
	for _, name := range []string{"101", "28", "3", "5"} {
		h.playerByName[name] = func(int) (int, string) { return http.StatusOK, loginPlayerBodyWithVisitor("VISITOR_BAD") }
	}
	h.bootstrapFn = func(int) (int, string) { return http.StatusOK, freshVisitorBody() }

	ext := newVideoTestExtractor(t, h)
	exhausted := requireExhausted(t, func() error {
		_, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
		return err
	}())
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly one forced refresh, got %d", h.bootstrapCount)
	}
	if h.playerCount != 8 {
		t.Fatalf("expected 4 + 4 player calls, got %d", h.playerCount)
	}
	if len(exhausted.Attempts) != 8 {
		t.Fatalf("expected both passes retained, got %d attempts", len(exhausted.Attempts))
	}
	if !exhausted.MostlyLogin() {
		t.Fatal("expected MostlyLogin for all-login exhaustion")
	}
}

func TestVideoChainFailedRefreshRetainsEvidence(t *testing.T) {
	h := newChainHarness(t)
	for _, name := range []string{"101", "28", "3", "5"} {
		h.playerByName[name] = func(int) (int, string) { return http.StatusOK, loginPlayerBodyWithVisitor("VISITOR_BAD") }
	}
	h.bootstrapFn = func(int) (int, string) { return http.StatusInternalServerError, "boom" }

	ext := newVideoTestExtractor(t, h)
	ext.cacheManager = cache.NewCacheManagerWithDir(t.TempDir())
	ext.visitorData = "VISITOR_POISONED"

	exhausted := requireExhausted(t, func() error {
		_, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
		return err
	}())
	if len(exhausted.Attempts) != 4 {
		t.Fatalf("expected original 4 attempts retained, got %d", len(exhausted.Attempts))
	}
	if h.playerCount != 4 {
		t.Fatalf("expected no retry after failed refresh, got %d player calls", h.playerCount)
	}
	// Failed-response visitor must be neither adopted nor persisted.
	if ext.visitorData != "VISITOR_POISONED" {
		t.Fatalf("expected original visitor retained, got %q", ext.visitorData)
	}
	if vc, _, err := ext.cacheManager.LoadVisitorDataAllowStale(0); err == nil && vc != nil && vc.Data == "VISITOR_BAD" {
		t.Fatalf("failed-response visitor must not be persisted: %+v", vc)
	}
}

// assertVisitorCommitted verifies the exact committed visitor state across
// extractor memory, the shared runtime cache, and the on-disk cache. want == ""
// asserts that nothing was committed anywhere.
func assertVisitorCommitted(t *testing.T, ext *Extractor, want string) {
	t.Helper()
	if ext.visitorData != want {
		t.Fatalf("extractor visitorData = %q, want %q", ext.visitorData, want)
	}
	ext.runtime.visitorCache.RLock()
	rt := ext.runtime.visitorCache.data
	ext.runtime.visitorCache.RUnlock()
	if rt != want {
		t.Fatalf("runtime visitor cache = %q, want %q", rt, want)
	}
	if ext.cacheManager == nil {
		return
	}
	if want == "" {
		if vc, _, err := ext.cacheManager.LoadVisitorDataAllowStale(0); err == nil && vc != nil && vc.Data != "" {
			t.Fatalf("disk visitor cache = %q, want empty", vc.Data)
		}
		return
	}
	vc, _, err := ext.cacheManager.LoadVisitorDataAllowStale(0)
	if err != nil || vc == nil || vc.Data != want {
		got := ""
		if vc != nil {
			got = vc.Data
		}
		t.Fatalf("disk visitor cache = %q err=%v, want %q", got, err, want)
	}
}

// TestVideoChainFailedRunLeavesVisitorStateUntouched covers the bootstrap
// ownership hole: a successful fresh-bootstrap visitor is only a local
// candidate. When every profile fails stream validation, neither the
// extractor, the shared runtime cache, nor disk may adopt the bootstrap
// value: failed chains preserve the prior (empty) state.
func TestVideoChainFailedRunLeavesVisitorStateUntouched(t *testing.T) {
	h := newChainHarness(t)
	// Playable bodies whose streams fail probing: every attempt exhausts on
	// probe validation, so no winner exists to authorize a commit.
	bad := okPlayerBody("https://synthetic-bad-video.googlevideo.com/videoplayback", "https://synthetic-bad-audio.googlevideo.com/videoplayback", true)
	for _, name := range []string{"101", "28", "3", "5"} {
		h.playerByName[name] = func(int) (int, string) { return http.StatusOK, bad }
	}
	h.bootstrapFn = func(int) (int, string) { return http.StatusOK, freshVisitorBody() }
	h.probeByHost["synthetic-bad-video.googlevideo.com"] = func(req *http.Request) *http.Response {
		return jsonHTTPResponse(http.StatusForbidden, "forbidden")
	}
	h.probeByHost["synthetic-bad-audio.googlevideo.com"] = func(req *http.Request) *http.Response {
		return jsonHTTPResponse(http.StatusForbidden, "forbidden")
	}

	ext := newVideoTestExtractor(t, h)
	ext.visitorData = "" // nothing committed yet; bootstrap must stay local
	ext.cacheManager = cache.NewCacheManagerWithDir(t.TempDir())

	exhausted := requireExhausted(t, func() error {
		_, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
		return err
	}())
	if h.playerCount != 4 {
		t.Fatalf("expected 4 probe-failing attempts, got %d", h.playerCount)
	}
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 bootstrap attempt, got %d", h.bootstrapCount)
	}
	if len(exhausted.Attempts) != 4 {
		t.Fatalf("expected 4 attempts retained, got %d", len(exhausted.Attempts))
	}
	assertVisitorCommitted(t, ext, "")
}

// TestVideoChainFailedRetryLeavesVisitorStateUntouched covers the forced
// refresh ownership hole: a successful forced refresh supplies a fresh
// candidate for retry only. When the retry exhausts, the previously
// committed visitor must survive unchanged in extractor memory, the runtime
// cache, and on disk.
func TestVideoChainFailedRetryLeavesVisitorStateUntouched(t *testing.T) {
	h := newChainHarness(t)
	// First pass with the committed visitor, then the retry with the fresh
	// refresh candidate: both exhaust on LOGIN_REQUIRED.
	for _, name := range []string{"101", "28", "3", "5"} {
		h.playerByName[name] = func(int) (int, string) { return http.StatusOK, loginPlayerBodyWithVisitor("VISITOR_BAD") }
	}
	h.bootstrapFn = func(int) (int, string) { return http.StatusOK, freshVisitorBody() }

	ext := newVideoTestExtractor(t, h)
	ext.cacheManager = cache.NewCacheManagerWithDir(t.TempDir())
	// Prior committed state: a previously validated visitor everywhere.
	ext.visitorData = "VISITOR_POISONED"
	if err := ext.cacheManager.SaveVisitorData(&cache.VisitorCache{
		Data:      "VISITOR_POISONED",
		ExpiresAt: time.Now().Add(visitorDataTTL),
	}); err != nil {
		t.Fatalf("seed disk cache: %v", err)
	}
	ext.runtime.visitorCache.Lock()
	ext.runtime.visitorCache.data = "VISITOR_POISONED"
	ext.runtime.visitorCache.expiry = time.Now().Add(visitorDataTTL)
	ext.runtime.visitorCache.updated = time.Now()
	ext.runtime.visitorCache.Unlock()

	exhausted := requireExhausted(t, func() error {
		_, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
		return err
	}())
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly one forced refresh, got %d", h.bootstrapCount)
	}
	if h.playerCount != 8 {
		t.Fatalf("expected 4 + 4 player calls, got %d", h.playerCount)
	}
	if len(exhausted.Attempts) != 8 {
		t.Fatalf("expected both passes retained, got %d attempts", len(exhausted.Attempts))
	}
	assertVisitorCommitted(t, ext, "VISITOR_POISONED")
}

// TestVideoChainSWRDoesNotCommitUnvalidatedVisitor covers the async
// stale-while-revalidate hole: when the runtime cache holds a
// usable-but-expired guest visitor inside the grace window, the video path
// may reuse it as a candidate but must not fire a background refresh that
// commits a fresh unvalidated visitor over extractor/runtime/disk state.
func TestVideoChainSWRDoesNotCommitUnvalidatedVisitor(t *testing.T) {
	// The whole scenario runs inside a synctest bubble. That matters because
	// the property under test is the ABSENCE of a background goroutine: a
	// forbidden refresh would be an extra goroutine racing the assertions. In
	// the bubble, synctest.Wait() is a real quiescence barrier — it returns
	// only when every bubble goroutine is durably blocked, so any refresh
	// goroutine has either run to completion (and mutated state, which the
	// assertions then see) or is parked with nothing further to do. No sleep,
	// no timing guess, and no production observation hook are needed.
	synctest.Test(t, func(t *testing.T) {
		h := newChainHarness(t)
		bootstrapEntered := make(chan bool, 8)
		h.bootstrapEntered = bootstrapEntered
		bad := okPlayerBody("https://synthetic-bad-video.googlevideo.com/videoplayback", "https://synthetic-bad-audio.googlevideo.com/videoplayback", true)
		for _, name := range []string{"101", "28", "3", "5"} {
			h.playerByName[name] = func(int) (int, string) { return http.StatusOK, bad }
		}
		// The fake bootstrap is deliberately IMMEDIATE: if the video path ever
		// starts a refresh, the request completes right away rather than
		// hanging, so a forbidden refresh is observed as state pollution
		// instead of a stuck test.
		h.bootstrapFn = func(int) (int, string) { return http.StatusOK, freshVisitorBody() }
		h.probeByHost["synthetic-bad-video.googlevideo.com"] = func(req *http.Request) *http.Response {
			return jsonHTTPResponse(http.StatusForbidden, "forbidden")
		}
		h.probeByHost["synthetic-bad-audio.googlevideo.com"] = func(req *http.Request) *http.Response {
			return jsonHTTPResponse(http.StatusForbidden, "forbidden")
		}

		ext := newVideoTestExtractor(t, h)
		ext.visitorData = "" // discarded candidate: never committed this run
		ext.cacheManager = cache.NewCacheManagerWithDir(t.TempDir())
		ext.runtime.visitorCache.Lock()
		ext.runtime.visitorCache.data = "VISITOR_STALE"
		ext.runtime.visitorCache.expiry = time.Now().Add(-time.Minute) // expired, within SWR grace
		ext.runtime.visitorCache.updated = time.Now()
		ext.runtime.visitorCache.Unlock()

		exhausted := requireExhausted(t, func() error {
			_, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
			return err
		}())
		if h.playerCount != 4 {
			t.Fatalf("expected 4 probe-failing attempts, got %d", h.playerCount)
		}
		if len(exhausted.Attempts) != 4 {
			t.Fatalf("expected 4 attempts retained, got %d", len(exhausted.Attempts))
		}

		// Quiescence barrier: every bubble goroutine (including any forbidden
		// background refresh) is now durably blocked, so the bootstrap/cache
		// inspections below cannot race one.
		synctest.Wait()

		if h.bootstrapCount != 0 {
			t.Fatalf("video SWR must not issue a fresh visitor bootstrap, got %d", h.bootstrapCount)
		}
		select {
		case <-bootstrapEntered:
			t.Fatal("video SWR must not enter a fresh visitor bootstrap at all")
		default:
		}
		// Each state layer survives unchanged: extractor memory was never applied
		// (prior state empty), the runtime cache still holds the stale candidate,
		// and the disk cache was never written.
		if ext.visitorData != "" {
			t.Fatalf("failed run applied stale candidate to extractor: got %q, want empty", ext.visitorData)
		}
		ext.runtime.visitorCache.RLock()
		rtData := ext.runtime.visitorCache.data
		ext.runtime.visitorCache.RUnlock()
		if rtData != "VISITOR_STALE" {
			t.Fatalf("runtime cache changed by failed run: got %q, want VISITOR_STALE", rtData)
		}
		if vc, _, err := ext.cacheManager.LoadVisitorDataAllowStale(0); err == nil && vc != nil && vc.Data != "" {
			t.Fatalf("failed run wrote disk cache: %+v", vc)
		}
	})
}

// TestVideoChainWinnerCommitsValidatedVisitor proves that a successful run
// commits the exact visitor candidate its winning chain validated, across
// every candidate source, and that a mid-run state change (like a concurrent
// extraction or SWR result substituting e.visitorData) is never committed in
// place of the validated candidate. Subtest 1: the committed fast-path
// candidate wins and resists substitution at probe time. Subtest 2: a fresh
// bootstrap candidate is committed only after validation. Subtest 3: the
// forced-refresh candidate is committed after retry validation.
func TestVideoChainWinnerCommitsValidatedVisitor(t *testing.T) {
	t.Run("committed fast path resists substitution", func(t *testing.T) {
		h := newChainHarness(t)
		body := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
		h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, body }

		ext := newVideoTestExtractor(t, h)
		ext.cacheManager = cache.NewCacheManagerWithDir(t.TempDir())
		// Subvert shared extractor state from inside the winning chain's own
		// probe: the commit must still persist the exact validated candidate.
		h.probeByHost["synthetic-video.googlevideo.com"] = func(req *http.Request) *http.Response {
			ext.visitorData = "VISITOR_EVIL"
			return rangeProbeResponse(req, 1000)
		}
		h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

		if _, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ"); err != nil {
			t.Fatalf("ExtractVideoContext returned error: %v", err)
		}
		if h.playerCount != 1 {
			t.Fatalf("expected 1 player call, got %d", h.playerCount)
		}
		assertVisitorCommitted(t, ext, "VISITOR_TEST")
	})

	t.Run("fresh bootstrap candidate", func(t *testing.T) {
		h := newChainHarness(t)
		body := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
		h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, body }
		h.bootstrapFn = func(int) (int, string) { return http.StatusOK, freshVisitorBody() }
		h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
		h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

		ext := newVideoTestExtractor(t, h)
		ext.visitorData = ""
		ext.cacheManager = cache.NewCacheManagerWithDir(t.TempDir())

		result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
		if err != nil {
			t.Fatalf("ExtractVideoContext returned error: %v", err)
		}
		if result.Client != "VISIONOS" {
			t.Fatalf("expected VISIONOS, got %q", result.Client)
		}
		if h.bootstrapCount != 1 {
			t.Fatalf("expected exactly 1 bootstrap attempt, got %d", h.bootstrapCount)
		}
		if h.playerCalls[0].visitorID != "VISITOR_FRESH" {
			t.Fatalf("chain must run with the bootstrap candidate, got %+v", h.playerCalls[0])
		}
		assertVisitorCommitted(t, ext, "VISITOR_FRESH")
	})

	t.Run("forced refresh candidate", func(t *testing.T) {
		h := newChainHarness(t)
		okBody := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
		for _, name := range []string{"101", "28", "3", "5"} {
			h.playerByName[name] = func(int) (int, string) {
				if h.lastPlayerVisitor() == "VISITOR_FRESH" {
					return http.StatusOK, okBody
				}
				return http.StatusOK, loginPlayerBodyWithVisitor("VISITOR_BAD")
			}
		}
		h.bootstrapFn = func(int) (int, string) { return http.StatusOK, freshVisitorBody() }
		h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
		h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

		ext := newVideoTestExtractor(t, h)
		ext.visitorData = "VISITOR_POISONED"
		ext.cacheManager = cache.NewCacheManagerWithDir(t.TempDir())

		result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
		if err != nil {
			t.Fatalf("ExtractVideoContext returned error: %v", err)
		}
		if result.Client != "VISIONOS" {
			t.Fatalf("expected retry to succeed on VISIONOS, got %q", result.Client)
		}
		if h.playerCount != 5 {
			t.Fatalf("expected 4 gated calls + 1 retry call, got %d", h.playerCount)
		}
		assertVisitorCommitted(t, ext, "VISITOR_FRESH")
	})
}

func TestVideoChainRefreshCancellationIdentity(t *testing.T) {
	h := newChainHarness(t)
	for _, name := range []string{"101", "28", "3", "5"} {
		h.playerByName[name] = func(int) (int, string) { return http.StatusOK, loginPlayerBody() }
	}

	ext := newVideoTestExtractor(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	h.bootstrapFn = func(int) (int, string) {
		// Cancel while the forced refresh is being issued.
		cancel()
		return http.StatusOK, freshVisitorBody()
	}

	exhausted := requireExhausted(t, func() error {
		_, err := ext.ExtractVideoContext(ctx, "dQw4w9WgXcQ")
		return err
	}())
	// The refresh runs under a cancelled context: no retry, identity kept.
	if h.playerCount != 4 {
		t.Fatalf("expected first pass only, got %d player calls", h.playerCount)
	}
	if !errors.Is(exhausted, context.Canceled) {
		t.Fatalf("expected errors.Is(err, context.Canceled), got %v", exhausted)
	}
	// CLI categorization checks Cancelled()/RateLimited() before MostlyLogin,
	// so a cancelled chain never classifies as AUTH_FAILED.
	if !exhausted.Cancelled() {
		t.Fatal("expected Cancelled() for cancelled refresh")
	}
}

func TestVideoChainStopsOn429(t *testing.T) {
	h := newChainHarness(t)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusTooManyRequests, "rate limited" }

	ext := newVideoTestExtractor(t, h)
	exhausted := requireExhausted(t, func() error {
		_, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
		return err
	}())
	if h.playerCount != 1 {
		t.Fatalf("expected chain to stop after 429, got %d calls", h.playerCount)
	}
	if len(exhausted.Attempts) != 1 || exhausted.Attempts[0].HTTPStatus != 429 {
		t.Fatalf("expected single 429 attempt, got %+v", exhausted.Attempts)
	}
	if exhausted.MostlyLogin() {
		t.Fatal("429 must never classify as AUTH_FAILED")
	}
}

func TestVideoChainCancellationIdentity(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		h := newChainHarness(t)
		h.playerByName["101"] = func(int) (int, string) {
			return http.StatusOK, okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
		}
		ext := newVideoTestExtractor(t, h)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		exhausted := requireExhausted(t, func() error {
			_, err := ext.ExtractVideoContext(ctx, "dQw4w9WgXcQ")
			return err
		}())
		if !errors.Is(exhausted, context.Canceled) {
			t.Fatalf("expected errors.Is(err, context.Canceled), got %v", exhausted)
		}
		if h.playerCount != 0 {
			t.Fatalf("expected no player calls after cancel, got %d", h.playerCount)
		}
		if exhausted.MostlyLogin() {
			t.Fatal("cancellation must never classify as AUTH_FAILED")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		h := newChainHarness(t)
		h.playerByName["101"] = func(int) (int, string) {
			return http.StatusOK, okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
		}
		ext := newVideoTestExtractor(t, h)
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		exhausted := requireExhausted(t, func() error {
			_, err := ext.ExtractVideoContext(ctx, "dQw4w9WgXcQ")
			return err
		}())
		if !errors.Is(exhausted, context.DeadlineExceeded) {
			t.Fatalf("expected errors.Is(err, context.DeadlineExceeded), got %v", exhausted)
		}
	})
}

func TestVideoExhaustedClassification(t *testing.T) {
	newAttempts := func(kinds ...VideoAttemptError) *VideoExhaustedError {
		return &VideoExhaustedError{Attempts: kinds}
	}
	login := VideoAttemptError{Client: "X", Kind: VideoAttemptPlayability, Status: "LOGIN_REQUIRED"}
	unplay := VideoAttemptError{Client: "X", Kind: VideoAttemptPlayability, Status: "ERROR", Reason: "Video unavailable"}
	probe := VideoAttemptError{Client: "X", Kind: VideoAttemptProbe}

	if !newAttempts(login, login, login, login).MostlyLogin() {
		t.Fatal("all-login must be MostlyLogin")
	}
	if !newAttempts(login, login, login, probe).MostlyLogin() {
		t.Fatal("3/4 login must be MostlyLogin")
	}
	// Strict majority: a 2-2 tie is not MostlyLogin.
	if newAttempts(login, login, unplay, unplay).MostlyLogin() {
		t.Fatal("2-2 tie must not be MostlyLogin")
	}
	if newAttempts(login, login, unplay, unplay).MostlyUnplayable() {
		t.Fatal("2-2 tie must not be MostlyUnplayable")
	}
	if !newAttempts(unplay, unplay, unplay, login).MostlyUnplayable() {
		t.Fatal("3/4 unplayable must be MostlyUnplayable")
	}
	if newAttempts(login, login, unplay, unplay).Cancelled() {
		t.Fatal("tie must not be Cancelled")
	}
	if !(&VideoExhaustedError{Cause: context.Canceled}).Cancelled() {
		t.Fatal("Cause=Canceled must be Cancelled")
	}
	rate := VideoAttemptError{Client: "X", Kind: VideoAttemptHTTP, HTTPStatus: http.StatusTooManyRequests}
	if !newAttempts(login, rate).RateLimited() {
		t.Fatal("expected RateLimited")
	}
	if newAttempts(login, rate).MostlyLogin() {
		t.Fatal("login+429 tie must not be MostlyLogin")
	}
}

func TestVideoChainExhaustedAggregate(t *testing.T) {
	h := newChainHarness(t)
	for _, name := range []string{"101", "28", "3", "5"} {
		h.playerByName[name] = func(int) (int, string) { return http.StatusOK, loginPlayerBody() }
	}

	ext := newVideoTestExtractor(t, h)
	exhausted := requireExhausted(t, func() error {
		_, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
		return err
	}())
	if len(exhausted.Attempts) != 4 {
		t.Fatalf("expected 4 attempts retained, got %d", len(exhausted.Attempts))
	}
	for i, a := range exhausted.Attempts {
		if a.Client == "" || a.Kind == "" || a.Status != "LOGIN_REQUIRED" {
			t.Fatalf("attempt %d missing client/kind/status: %+v", i, a)
		}
	}
	if !exhausted.MostlyLogin() {
		t.Fatal("expected MostlyLogin to be true for all-login exhaustion")
	}
}

func TestVideoChainUnplayableExhausted(t *testing.T) {
	h := newChainHarness(t)
	for _, name := range []string{"101", "28", "3", "5"} {
		h.playerByName[name] = func(int) (int, string) { return http.StatusOK, unplayablePlayerBody() }
	}
	ext := newVideoTestExtractor(t, h)
	exhausted := requireExhausted(t, func() error {
		_, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
		return err
	}())
	if !exhausted.MostlyUnplayable() {
		t.Fatalf("expected MostlyUnplayable, got %+v", exhausted.Attempts)
	}
	if exhausted.MostlyLogin() {
		t.Fatal("unplayable must not classify as AUTH_FAILED")
	}
}

func TestVideoResultJSONIncludesClient(t *testing.T) {
	h := newChainHarness(t)
	body := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, body }
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	got := string(payload)
	for _, want := range []string{`"client":"VISIONOS"`, `"user_agent":`, `"video_url":"https://synthetic-video.googlevideo.com/videoplayback"`, `"audio_url":"https://synthetic-audio.googlevideo.com/videoplayback"`, `"title":"test title"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected JSON to contain %q, got %s", want, got)
		}
	}
}

func TestVideoChainProfileDrivesRequest(t *testing.T) {
	h := newChainHarness(t)
	body := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, body }
	h.probeByHost["synthetic-video.googlevideo.com"] = okProbe(nil)
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	before := ext.config
	if _, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ"); err != nil {
		t.Fatalf("ExtractVideoContext returned error: %v", err)
	}
	if !reflect.DeepEqual(ext.config, before) {
		t.Fatalf("extractor config was mutated: %+v vs %+v", ext.config, before)
	}
	call := h.playerCalls[0]
	if call.clientName != "101" || call.clientVersion != "1.02" {
		t.Fatalf("client headers not driven by profile: %+v", call)
	}
	var reqBody InnertubeRequest
	if err := json.Unmarshal([]byte(call.body), &reqBody); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	c := reqBody.Context.Client
	if c.ClientName != "VISIONOS" || c.ClientVersion != "1.02" || c.OSName != "visionOS" || c.DeviceModel != "RealityDevice17,1" {
		t.Fatalf("JSON context not driven by profile: %+v", c)
	}
	if c.UserAgent != call.userAgent {
		t.Fatalf("body UA %q != header UA %q", c.UserAgent, call.userAgent)
	}
	if c.VisitorData != "VISITOR_TEST" {
		t.Fatalf("visitor data not propagated: %q", c.VisitorData)
	}
}

// TestVideoProfilesTable drives every profile directly through
// callPlayerAPIWithProfile and verifies the deliberate ?key= choice, UA,
// numeric client header, version, and device/SDK context fields.
func TestVideoProfilesTable(t *testing.T) {
	chain := append([]videoClientProfile(nil), videoClientChain...)
	if len(chain) != 4 {
		t.Fatalf("expected 4 profiles, got %d", len(chain))
	}
	wantOrder := []string{"VISIONOS", "ANDROID_VR", "ANDROID", "IOS"}
	for i, name := range wantOrder {
		if chain[i].Name != name {
			t.Fatalf("position %d: expected %q, got %q", i, name, chain[i].Name)
		}
	}

	for _, profile := range chain {
		t.Run(profile.Name, func(t *testing.T) {
			var gotReq *http.Request
			var gotBody string
			ext, err := NewExtractor(ModeVideo, "", WithRuntime(NewRuntime()), WithHTTPClient(&http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					raw, _ := io.ReadAll(req.Body)
					gotReq = req
					gotBody = string(raw)
					return jsonHTTPResponse(http.StatusOK, okPlayerBody("https://synthetic-video.googlevideo.com/", "https://synthetic-audio.googlevideo.com/", true)), nil
				}),
			}))
			if err != nil {
				t.Fatalf("NewExtractor returned error: %v", err)
			}
			ext.cacheManager = nil

			if _, err := ext.callPlayerAPIWithProfile(t.Context(), "dQw4w9WgXcQ", profile, "VISITOR_TBL"); err != nil {
				t.Fatalf("callPlayerAPIWithProfile returned error: %v", err)
			}

			// Deliberate ?key= choice: per-family keys are required by the
			// youtubei player endpoint (live-verified with VISIONOS).
			if got := gotReq.URL.Query().Get("key"); got != profile.APIKey || got == "" {
				t.Fatalf("query key = %q, want profile key %q", got, profile.APIKey)
			}
			if got := gotReq.Header.Get("User-Agent"); got != profile.UserAgent {
				t.Fatalf("UA header = %q, want %q", got, profile.UserAgent)
			}
			if got := gotReq.Header.Get("X-YouTube-Client-Name"); got != strconv.Itoa(profile.ClientID) {
				t.Fatalf("client name header = %q, want %d", got, profile.ClientID)
			}
			if got := gotReq.Header.Get("X-YouTube-Client-Version"); got != profile.Version {
				t.Fatalf("client version header = %q, want %q", got, profile.Version)
			}
			if got := gotReq.Header.Get("X-Goog-Visitor-Id"); got != "VISITOR_TBL" {
				t.Fatalf("visitor header = %q", got)
			}

			var reqBody InnertubeRequest
			if err := json.Unmarshal([]byte(gotBody), &reqBody); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			c := reqBody.Context.Client
			if c.ClientName != profile.Name || c.ClientVersion != profile.Version || c.UserAgent != profile.UserAgent {
				t.Fatalf("body identity mismatch: %+v vs %+v", c, profile)
			}
			if c.OSName != profile.OSName || c.OSVersion != profile.OSVersion {
				t.Fatalf("body OS mismatch: %+v vs %+v", c, profile)
			}
			if c.DeviceMake != profile.DeviceMake || c.DeviceModel != profile.DeviceModel || c.Platform != profile.Platform {
				t.Fatalf("body device mismatch: %+v vs %+v", c, profile)
			}
			if c.AndroidSDKVersion != profile.SDKVersion {
				t.Fatalf("body sdk = %d, want %d", c.AndroidSDKVersion, profile.SDKVersion)
			}
			if c.VisitorData != "VISITOR_TBL" {
				t.Fatalf("body visitor = %q", c.VisitorData)
			}
		})
	}
}

// cancelOnReadBody wraps a response body and runs onRead exactly once, from
// inside the probe's Read call, before yielding the inner bytes. It models a
// caller that is canceled while the probe is still consuming the final byte,
// so the probe itself can still SUCCEED with a valid byte.
type cancelOnReadBody struct {
	inner    io.ReadCloser
	onRead   func()
	once     sync.Once
	returned bool
}

func (b *cancelOnReadBody) Read(p []byte) (int, error) {
	b.once.Do(b.onRead)
	return b.inner.Read(p)
}

func (b *cancelOnReadBody) Close() error { return b.inner.Close() }

// TestVideoChainCancellationAfterSuccessfulProbesNeverPublishes proves that a
// caller canceled during a probe round never publishes a winner.
// The cancellation is injected from inside the video probe's body read, where
// the probe still returns its requested byte.
//
// This is deliberately a PROPERTY test, not an isolated check: depending on
// scheduling, the sibling audio probe may either also succeed or observe the
// cancellation and fail. BOTH interleavings must still yield no result, no
// visitor commit, and no extractor/runtime/disk visitor state change, always
// under the caller's own cancellation identity. The deterministic, isolated
// form of the same guarantee — cancellation landing strictly AFTER both probe
// bodies closed, with the probe round provably complete — is
// TestValidateStreamEndsPublishesNothingAfterCancellation.
func TestVideoChainCancellationAfterSuccessfulProbesNeverPublishes(t *testing.T) {
	h := newChainHarness(t)
	body := okPlayerBody("https://synthetic-video.googlevideo.com/videoplayback", "https://synthetic-audio.googlevideo.com/videoplayback", true)
	h.playerByName["101"] = func(int) (int, string) { return http.StatusOK, body }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var cancelOnce sync.Once
	h.probeByHost["synthetic-video.googlevideo.com"] = func(req *http.Request) *http.Response {
		resp := rangeProbeResponse(req, 1000)
		resp.Body = &cancelOnReadBody{inner: resp.Body, onRead: func() { cancelOnce.Do(cancel) }}
		return resp
	}
	h.probeByHost["synthetic-audio.googlevideo.com"] = okProbe(nil)

	ext := newVideoTestExtractor(t, h)
	ext.cacheManager = cache.NewCacheManagerWithDir(t.TempDir())

	result, err := ext.ExtractVideoContext(ctx, "dQw4w9WgXcQ")
	if result != nil {
		t.Fatalf("canceled extraction must not publish a winner, got %+v", result)
	}
	exhausted := requireExhausted(t, err)
	if !errors.Is(exhausted, context.Canceled) {
		t.Fatalf("expected errors.Is(err, context.Canceled), got %v", exhausted)
	}
	if !exhausted.Cancelled() {
		t.Fatalf("expected Cancelled() to classify the run, got %+v", exhausted.Attempts)
	}
	// The chain must terminate immediately after the canceled winning attempt:
	// no further client attempts, and the canceled attempt is retained.
	if h.playerCount != 1 {
		t.Fatalf("chain must stop after the cancellation, got %d player calls", h.playerCount)
	}
	cancelled := 0
	for _, a := range exhausted.Attempts {
		if a.Kind == VideoAttemptCancelled {
			cancelled++
		}
	}
	if cancelled != 1 {
		t.Fatalf("expected exactly 1 canceled attempt, got %d in %+v", cancelled, exhausted.Attempts)
	}

	// Visitor state is untouched at every layer: the extractor never applied
	// the candidate, the runtime cache was never written, the disk cache was
	// never written.
	if ext.visitorData != "VISITOR_TEST" {
		t.Fatalf("canceled run mutated extractor visitor state: got %q", ext.visitorData)
	}
	ext.runtime.visitorCache.RLock()
	rtData := ext.runtime.visitorCache.data
	rtKey := ext.runtime.visitorCache.authKey
	ext.runtime.visitorCache.RUnlock()
	if rtData != "" || rtKey != "" {
		t.Fatalf("canceled run polluted the runtime visitor cache: data=%q key=%q", rtData, rtKey)
	}
	if vc, _, lerr := ext.cacheManager.LoadVisitorDataAllowStale(0); lerr == nil && vc != nil && vc.Data != "" {
		t.Fatalf("canceled run wrote the disk visitor cache: %+v", vc)
	}
}

// TestVideoNativePathRejectsAuthenticatedConfig proves the native video path is
// explicitly GUEST-ONLY: an authenticated (cookie-bearing) configuration is
// rejected by the public entry point AND by the candidate helper, clearly and
// BEFORE any state read or network I/O. Because nothing is registered, the
// native call cannot join a music visitor flight, cannot lead one, and cannot
// pollute any extractor/runtime/disk visitor state.
func TestVideoNativePathRejectsAuthenticatedConfig(t *testing.T) {
	h := newChainHarness(t)
	ext := newVideoTestExtractor(t, h)
	ext.cacheManager = cache.NewCacheManagerWithDir(t.TempDir())
	ext.config = GetClientConfig(ModeMusic)
	ext.visitorData = "" // force the candidate path, not the early return

	// 1. Candidate helper rejects.
	visitor, err := ext.fetchVideoVisitorCandidate(t.Context(), "dQw4w9WgXcQ")
	if !errors.Is(err, errNativeVideoRequiresGuest) {
		t.Fatalf("fetchVideoVisitorCandidate: got (%q, %v), want errNativeVideoRequiresGuest", visitor, err)
	}
	if visitor != "" {
		t.Fatalf("rejected candidate must be empty, got %q", visitor)
	}

	// 2. Forced refresh rejects.
	if visitor, err := ext.forceRefreshVideoVisitorCandidate(t.Context(), "dQw4w9WgXcQ"); !errors.Is(err, errNativeVideoRequiresGuest) || visitor != "" {
		t.Fatalf("forceRefreshVideoVisitorCandidate: got (%q, %v), want errNativeVideoRequiresGuest", visitor, err)
	}

	// 3. The public entry point rejects, so no chain ever starts.
	result, err := ext.ExtractVideoContext(t.Context(), "dQw4w9WgXcQ")
	if !errors.Is(err, errNativeVideoRequiresGuest) {
		t.Fatalf("ExtractVideoContext: got (%+v, %v), want errNativeVideoRequiresGuest", result, err)
	}
	if result != nil {
		t.Fatalf("rejected video extraction must return no result, got %+v", result)
	}

	// 4. No flight was registered and no I/O happened: nothing could have
	// joined or polluted a music flight, and no visitor state was touched.
	if h.playerCount != 0 || h.bootstrapCount != 0 {
		t.Fatalf("rejected native path performed I/O: player=%d bootstrap=%d", h.playerCount, h.bootstrapCount)
	}
	if ext.visitorData != "" {
		t.Fatalf("rejected native path applied visitor state: %q", ext.visitorData)
	}
	ext.runtime.visitorCache.RLock()
	rtData := ext.runtime.visitorCache.data
	rtAuth := ext.runtime.visitorCache.isAuth
	rtKey := ext.runtime.visitorCache.authKey
	ext.runtime.visitorCache.RUnlock()
	if rtData != "" || rtAuth || rtKey != "" {
		t.Fatalf("rejected native path polluted the runtime visitor cache: data=%q auth=%v key=%q", rtData, rtAuth, rtKey)
	}
	if vc, _, lerr := ext.cacheManager.LoadVisitorDataAllowStale(0); lerr == nil && vc != nil && vc.Data != "" {
		t.Fatalf("rejected native path wrote the disk visitor cache: %+v", vc)
	}
}

// TestGetClientConfigVideoRestoresAndroidVRSingleShot pins the restored public
// ModeVideo config to the historical ANDROID_VR single-shot identity, while the
// native chain keeps its own explicit profile table. A config regression here
// silently changes every audio/video consumer that reads GetClientConfig.
func TestGetClientConfigVideoRestoresAndroidVRSingleShot(t *testing.T) {
	cfg := GetClientConfig(ModeVideo)
	if cfg.Name != AndroidVRClientName {
		t.Fatalf("ModeVideo name = %q, want %q", cfg.Name, AndroidVRClientName)
	}
	if cfg.Version != AndroidVRClientVersion {
		t.Fatalf("ModeVideo version = %q, want %q", cfg.Version, AndroidVRClientVersion)
	}
	if cfg.APIEndpoint != AndroidVRAPIEndpoint || cfg.APIKey != AndroidVRAPIKey {
		t.Fatalf("ModeVideo endpoint/key = %q/%q, want %q/%q", cfg.APIEndpoint, cfg.APIKey, AndroidVRAPIEndpoint, AndroidVRAPIKey)
	}
	if cfg.UserAgent != AndroidVRUserAgent {
		t.Fatalf("ModeVideo UA = %q, want %q", cfg.UserAgent, AndroidVRUserAgent)
	}
	if cfg.NeedsCookies || cfg.NeedsCipher {
		t.Fatalf("ModeVideo must stay guest and cipher-free, got cookies=%v cipher=%v", cfg.NeedsCookies, cfg.NeedsCipher)
	}
	if got := cfg.Headers["X-Youtube-Client-Name"]; got != "28" {
		t.Fatalf("ModeVideo X-Youtube-Client-Name = %q, want 28", got)
	}
	if got := cfg.Headers["X-Youtube-Client-Version"]; got != AndroidVRClientVersion {
		t.Fatalf("ModeVideo X-Youtube-Client-Version = %q, want %q", got, AndroidVRClientVersion)
	}

	// The native chain is unaffected and remains explicit + ordered.
	wantOrder := []string{"VISIONOS", "ANDROID_VR", "ANDROID", "IOS"}
	if len(videoClientChain) != len(wantOrder) {
		t.Fatalf("native chain length = %d, want %d", len(videoClientChain), len(wantOrder))
	}
	for i, name := range wantOrder {
		if videoClientChain[i].Name != name {
			t.Fatalf("native chain position %d = %q, want %q", i, videoClientChain[i].Name, name)
		}
	}
}

// TestAudioPlayerRequestIdentityIsPinned drives the AUDIO (music) player POST
// through a deterministic recording transport and asserts the exact wire
// identity - client name header, client version header, User-Agent, and the
// in-body clientName/clientVersion. This is a request-identity assertion, not a
// timing observation, and it pins WEB_REMIX music so restoring the ModeVideo
// config can never quietly repoint the audio path.
func TestAudioPlayerRequestIdentityIsPinned(t *testing.T) {
	var mu sync.Mutex
	var got *http.Request
	var gotBody []byte

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				return nil, fmt.Errorf("unexpected method %s", req.Method)
			}
			body, _ := io.ReadAll(req.Body)
			mu.Lock()
			got = req
			gotBody = body
			mu.Unlock()
			return jsonHTTPResponse(http.StatusOK, audioOnlyPlayerBody()), nil
		}),
	}

	ext := &Extractor{
		config:     GetClientConfig(ModeMusic),
		runtime:    NewRuntime(),
		httpClient: client,
	}
	ext.cacheManager = nil
	ext.visitorData = "VISITOR_AUDIO"

	if _, err := ext.callPlayerAPIContext(t.Context(), "dQw4w9WgXcQ"); err != nil {
		t.Fatalf("callPlayerAPIContext returned error: %v", err)
	}

	mu.Lock()
	req, body := got, gotBody
	mu.Unlock()
	if req == nil {
		t.Fatal("no audio player request was recorded")
	}

	// The music endpoint and key are WEB_REMIX's, never the video ANDROID_VR
	// ones: this is the assertion that fails if the two configs get crossed.
	if req.URL.Host != "music.youtube.com" || req.URL.Path != "/youtubei/v1/player" {
		t.Fatalf("audio player endpoint = %s, want music.youtube.com/youtubei/v1/player", req.URL)
	}
	if key := req.URL.Query().Get("key"); key != ClientKey {
		t.Fatalf("audio player key = %q, want %q", key, ClientKey)
	}
	if req.Header.Get("X-YouTube-Client-Name") != "67" {
		t.Fatalf("X-YouTube-Client-Name = %q, want 67 (WEB_REMIX)", req.Header.Get("X-YouTube-Client-Name"))
	}
	if req.Header.Get("X-YouTube-Client-Version") != ClientVersion {
		t.Fatalf("X-YouTube-Client-Version = %q, want %q", req.Header.Get("X-YouTube-Client-Version"), ClientVersion)
	}
	if req.Header.Get("User-Agent") != UserAgent {
		t.Fatalf("User-Agent = %q, want %q", req.Header.Get("User-Agent"), UserAgent)
	}
	if req.Header.Get("X-Goog-Visitor-Id") != "VISITOR_AUDIO" {
		t.Fatalf("X-Goog-Visitor-Id = %q, want VISITOR_AUDIO", req.Header.Get("X-Goog-Visitor-Id"))
	}

	var decoded InnertubeRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("player body is not valid JSON: %v", err)
	}
	if decoded.Context.Client.ClientName != ClientName {
		t.Fatalf("body clientName = %q, want %q (WEB_REMIX)", decoded.Context.Client.ClientName, ClientName)
	}
	if decoded.Context.Client.ClientVersion != ClientVersion {
		t.Fatalf("body clientVersion = %q, want %q", decoded.Context.Client.ClientVersion, ClientVersion)
	}
	if decoded.Context.Client.UserAgent != UserAgent {
		t.Fatalf("body userAgent = %q, want %q", decoded.Context.Client.UserAgent, UserAgent)
	}
	if decoded.Context.Client.VisitorData != "VISITOR_AUDIO" {
		t.Fatalf("body visitorData = %q, want VISITOR_AUDIO", decoded.Context.Client.VisitorData)
	}
}

func TestParseContentRange(t *testing.T) {
	s, e, total, ok := parseContentRange("bytes 999-999/1000")
	if !ok || s != 999 || e != 999 || total != 1000 {
		t.Fatalf("unexpected parse: %d %d %d %v", s, e, total, ok)
	}
	for _, bad := range []string{"garbage", "bytes */1000", "bytes 0-0/*", "bytes 0-/1000", "bytes 0-0", "200 OK"} {
		if _, _, _, ok := parseContentRange(bad); ok {
			t.Fatalf("expected %q to fail parsing", bad)
		}
	}
}

// visitorCallResult carries one video visitor candidate call outcome.
type visitorCallResult struct {
	visitor string
	err     error
}

// runVisitorCall runs fn in a fresh goroutine and returns a buffered channel
// that receives the outcome, letting tests race caller completion against
// cancellations without sleeps.
func runVisitorCall(fn func() (string, error)) <-chan visitorCallResult {
	ch := make(chan visitorCallResult, 1)
	go func() {
		visitor, err := fn()
		ch <- visitorCallResult{visitor: visitor, err: err}
	}()
	return ch
}

// waitVisitorCall waits up to timeout for a visitor call to complete. It is a
// bounded failure guard only: on the fixed code paths the caller returns
// immediately, so a false return means a caller stayed blocked (a regression).
func waitVisitorCall(ch <-chan visitorCallResult, timeout time.Duration) (visitorCallResult, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res, true
	case <-timer.C:
		return visitorCallResult{}, false
	}
}

// blockingBootstrapFn returns a WEB bootstrap handler that signals entry and
// then blocks until release fires, the request context is canceled, or an
// escape timer fires, before serving the given body. Selecting on
// req.Context().Done() is essential: if the shared flight inherited the
// initiating caller's cancellation, the HTTP request would abort here and a
// non-canceled joiner would fail, letting the tests detect the defect.
func blockingBootstrapFn(entered chan<- bool, release <-chan struct{}, body string) func(req *http.Request, call int) (int, string) {
	return func(req *http.Request, _ int) (int, string) {
		select {
		case entered <- true:
		default:
		}
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case <-release:
			return http.StatusOK, body
		case <-req.Context().Done():
			return http.StatusInternalServerError, "bootstrap canceled by request context"
		case <-timer.C:
			return http.StatusInternalServerError, "blocked bootstrap timed out"
		}
	}
}

// waitObservedContext is a TEST-ONLY context wrapper that reports the moment a
// caller reaches its wait. Done() closes its OWN waiting channel exactly once
// (sync.Once) and then returns the underlying Done channel, so cancellation
// still behaves identically. Because Go evaluates every select channel operand
// exactly once on entering the select, wrapping the FINAL caller context
// (after context.WithCancel) turns the production
// `select { case <-ctx.Done(): ... case res = <-ch: }` into a positive
// registration signal: receiving the signal proves the caller already
// registered with (led or joined) the shared flight, with no production hook
// and no sleep.
type waitObservedContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (w *waitObservedContext) Done() <-chan struct{} {
	w.once.Do(func() { close(w.waiting) })
	return w.Context.Done()
}

// waitObserver fans several waitObservedContext signals into one channel and
// guarantees every relay goroutine it started exits, so no test can leak one.
type waitObserver struct {
	out    chan struct{}
	stop   chan struct{}
	stopMu sync.Once
}

func newWaitObserver(n int) *waitObserver {
	return &waitObserver{out: make(chan struct{}, n), stop: make(chan struct{})}
}

// wrap returns a caller context that signals o when it reaches its wait.
func (o *waitObserver) wrap(ctx context.Context) context.Context {
	w := &waitObservedContext{Context: ctx, waiting: make(chan struct{})}
	go func() {
		select {
		case <-w.waiting:
			select {
			case o.out <- struct{}{}:
			default:
			}
		case <-o.stop:
		}
	}()
	return w
}

// close releases every relay goroutine. Safe to call from t.Cleanup.
func (o *waitObserver) close() { o.stopMu.Do(func() { close(o.stop) }) }

// waitVisitorRegistrations waits until n callers have reached their wait on the
// shared visitor flight, proved by their own wrapped contexts. Receiving n
// signals deterministically guarantees all n have attached regardless of
// goroutine scheduling. The timer is a failure guard only.
func waitVisitorRegistrations(t *testing.T, observed <-chan struct{}, n int) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	seen := 0
	for seen < n {
		select {
		case <-observed:
			seen++
		case <-timer.C:
			t.Fatalf("expected %d callers to reach the visitor flight wait, saw %d", n, seen)
		}
	}
}

// heldCallSet launches visitor callers and tracks which ones the test body
// already AWAITED SUCCESSFULLY. Only a successful receive may mark a caller
// consumed: a timed-out await leaves the caller goroutine live, so cleanup must
// still join it or it can outlive the test's TempDir removal.
type heldCallSet struct {
	t        *testing.T
	calls    []<-chan visitorCallResult
	consumed map[<-chan visitorCallResult]bool
}

func newHeldCallSet(t *testing.T) *heldCallSet {
	t.Helper()
	return &heldCallSet{t: t, consumed: map[<-chan visitorCallResult]bool{}}
}

func (s *heldCallSet) launch(fn func() (string, error)) <-chan visitorCallResult {
	ch := runVisitorCall(fn)
	s.calls = append(s.calls, ch)
	return ch
}

// launchErr is launch for an error-only caller (legacy fetchVisitorDataContext).
func (s *heldCallSet) launchErr(fn func() error) <-chan visitorCallResult {
	ch := runVisitorDataCall(fn)
	s.calls = append(s.calls, ch)
	return ch
}

func (s *heldCallSet) await(ch <-chan visitorCallResult, timeout time.Duration) (visitorCallResult, bool) {
	res, ok := waitVisitorCall(ch, timeout)
	if ok {
		// Only a real receive consumed this caller.
		s.consumed[ch] = true
	}
	return res, ok
}

// joinOutstanding waits for every caller the test body did not successfully
// await, so no flight goroutine can outlive the test's TempDir removal. A
// timeout here is a real failure (a leaked flight), never a silent pass.
func (s *heldCallSet) joinOutstanding() {
	for _, ch := range s.calls {
		if ch == nil || s.consumed[ch] {
			continue
		}
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-ch:
			s.consumed[ch] = true
		case <-timer.C:
			s.t.Errorf("cleanup could not join an outstanding visitor caller within 10s; a flight goroutine would outlive TempDir removal")
		}
		timer.Stop()
	}
}

// heldRelease is an idempotently-closable release signal for a held bootstrap.
// Cleanup must be able to release the held request on EVERY path — including an
// early t.Fatal that skipped the explicit release — and a plain channel send can
// be LOST (unbuffered with no receiver yet, or already drained). close() is
// idempotent under sync.Once, so releasing twice is harmless.
type heldRelease struct {
	ch   chan struct{}
	once sync.Once
}

func newHeldRelease() *heldRelease { return &heldRelease{ch: make(chan struct{})} }

func (r *heldRelease) release() { r.once.Do(func() { close(r.ch) }) }

// TestVideoVisitorBootstrapWaiterCancelStaysBlocked proves that when two video
// callers share one blocked fresh-bootstrap request and the non-initiating
// waiter is canceled, the waiter returns promptly with its own cancellation
// identity while the shared flight stays blocked for the surviving caller, and
// exactly one underlying request ran.
func TestVideoVisitorBootstrapWaiterCancelStaysBlocked(t *testing.T) {
	calls := newHeldCallSet(t)
	h := newChainHarness(t)
	entered := make(chan bool, 1)
	release := newHeldRelease()
	h.bootstrapReqFn = blockingBootstrapFn(entered, release.ch, freshVisitorBody())
	ext := newVideoTestExtractor(t, h)
	ext.visitorData = "" // force a fresh bootstrap flight

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	obs := newWaitObserver(2)
	t.Cleanup(obs.close)

	var callA, callB <-chan visitorCallResult
	// Unconditional cleanup: release the held flight and join every caller so
	// no in-flight bootstrap can outlive this test's TempDir removal, even on
	// an early t.Fatal.
	t.Cleanup(func() {
		release.release()
		calls.joinOutstanding()
	})

	callA = calls.launch(func() (string, error) {
		return ext.fetchVideoVisitorCandidate(obs.wrap(ctxA), "dQw4w9WgXcQ")
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap request never started")
	}

	// The waiter joins the blocked flight; positively prove both callers have
	// registered (each wrapped context reached its wait) before canceling.
	callB = calls.launch(func() (string, error) {
		return ext.fetchVideoVisitorCandidate(obs.wrap(ctxB), "dQw4w9WgXcQ")
	})
	waitVisitorRegistrations(t, obs.out, 2)
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 bootstrap request while blocked, got %d", h.bootstrapCount)
	}

	// Cancel the waiter: it returns promptly with its own cancellation.
	cancelB()
	resB, ok := calls.await(callB, 5*time.Second)
	if !ok {
		t.Fatal("canceled waiter did not return promptly")
	}
	if !errors.Is(resB.err, context.Canceled) {
		t.Fatalf("canceled waiter returned err %v, want context.Canceled", resB.err)
	}
	// The shared flight is untouched: still blocked, still one request, and
	// the surviving caller has not completed.
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 bootstrap request, got %d", h.bootstrapCount)
	}
	select {
	case <-callA:
		t.Fatal("surviving caller completed while the flight is still blocked")
	default:
	}

	// Release the flight: the surviving caller succeeds with the fresh
	// candidate and no second request ever fired.
	release.release()
	resA, ok := calls.await(callA, 5*time.Second)
	if !ok {
		t.Fatal("surviving caller did not complete after release")
	}
	if resA.err != nil {
		t.Fatalf("surviving caller returned error: %v", resA.err)
	}
	if resA.visitor != "VISITOR_FRESH" {
		t.Fatalf("surviving caller got visitor %q, want VISITOR_FRESH", resA.visitor)
	}
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 bootstrap request, got %d", h.bootstrapCount)
	}
}

// TestVideoVisitorBootstrapLeaderCancelJoinerSucceeds proves that canceling
// the caller that initiated the shared fresh-bootstrap flight releases that
// caller without canceling the shared work, so a non-canceled joiner still
// succeeds from the same single underlying request. The transport selects on
// the request context, so if the flight inherited the leader's cancellation
// the HTTP request would abort and the joiner would fail instead.
func TestVideoVisitorBootstrapLeaderCancelJoinerSucceeds(t *testing.T) {
	calls := newHeldCallSet(t)
	h := newChainHarness(t)
	entered := make(chan bool, 1)
	release := newHeldRelease()
	h.bootstrapReqFn = blockingBootstrapFn(entered, release.ch, freshVisitorBody())
	ext := newVideoTestExtractor(t, h)
	ext.visitorData = "" // force a fresh bootstrap flight

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()

	obs := newWaitObserver(2)
	t.Cleanup(obs.close)

	var callA, callB <-chan visitorCallResult
	t.Cleanup(func() {
		release.release()
		calls.joinOutstanding()
	})

	callA = calls.launch(func() (string, error) {
		return ext.fetchVideoVisitorCandidate(obs.wrap(ctxA), "dQw4w9WgXcQ")
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap request never started")
	}

	// A joiner arrives while the flight is blocked; prove it registered before
	// canceling the initiating caller.
	callB = calls.launch(func() (string, error) {
		return ext.fetchVideoVisitorCandidate(obs.wrap(context.Background()), "dQw4w9WgXcQ")
	})
	waitVisitorRegistrations(t, obs.out, 2)
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 bootstrap request, got %d", h.bootstrapCount)
	}

	// Cancel the flight initiator: it must return promptly with its own
	// cancellation while the shared work (and its request context) survives.
	cancelA()
	resA, ok := calls.await(callA, 5*time.Second)
	if !ok {
		t.Fatal("flight initiator did not return promptly after cancel")
	}
	if !errors.Is(resA.err, context.Canceled) {
		t.Fatalf("flight initiator returned err %v, want context.Canceled", resA.err)
	}
	select {
	case <-callB:
		t.Fatal("joiner completed while the flight is still blocked")
	default:
	}
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 bootstrap request, got %d", h.bootstrapCount)
	}

	// Release the flight: the non-canceled joiner succeeds with the fresh
	// candidate from the same single request.
	release.release()
	resB, ok := calls.await(callB, 5*time.Second)
	if !ok {
		t.Fatal("joiner did not complete after release")
	}
	if resB.err != nil {
		t.Fatalf("joiner returned error: %v", resB.err)
	}
	if resB.visitor != "VISITOR_FRESH" {
		t.Fatalf("joiner got visitor %q, want VISITOR_FRESH", resB.visitor)
	}
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 bootstrap request, got %d", h.bootstrapCount)
	}
}

// TestVideoVisitorRefreshWaiterCancelStaysBlocked is the forced-refresh
// analogue of TestVideoVisitorBootstrapWaiterCancelStaysBlocked: a canceled
// waiter returns promptly with its own cancellation while the shared forced
// refresh stays blocked, from exactly one underlying refresh request.
func TestVideoVisitorRefreshWaiterCancelStaysBlocked(t *testing.T) {
	calls := newHeldCallSet(t)
	h := newChainHarness(t)
	entered := make(chan bool, 1)
	release := newHeldRelease()
	h.bootstrapReqFn = blockingBootstrapFn(entered, release.ch, freshVisitorBody())
	ext := newVideoTestExtractor(t, h)

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	obs := newWaitObserver(2)
	t.Cleanup(obs.close)

	var callA, callB <-chan visitorCallResult
	t.Cleanup(func() {
		release.release()
		calls.joinOutstanding()
	})

	callA = calls.launch(func() (string, error) {
		return ext.forceRefreshVideoVisitorCandidate(obs.wrap(ctxA), "dQw4w9WgXcQ")
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh request never started")
	}

	callB = calls.launch(func() (string, error) {
		return ext.forceRefreshVideoVisitorCandidate(obs.wrap(ctxB), "dQw4w9WgXcQ")
	})
	waitVisitorRegistrations(t, obs.out, 2)
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 refresh request while blocked, got %d", h.bootstrapCount)
	}

	cancelB()
	resB, ok := calls.await(callB, 5*time.Second)
	if !ok {
		t.Fatal("canceled waiter did not return promptly")
	}
	if !errors.Is(resB.err, context.Canceled) {
		t.Fatalf("canceled waiter returned err %v, want context.Canceled", resB.err)
	}
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 refresh request, got %d", h.bootstrapCount)
	}
	select {
	case <-callA:
		t.Fatal("surviving caller completed while the flight is still blocked")
	default:
	}

	release.release()
	resA, ok := calls.await(callA, 5*time.Second)
	if !ok {
		t.Fatal("surviving caller did not complete after release")
	}
	if resA.err != nil {
		t.Fatalf("surviving caller returned error: %v", resA.err)
	}
	if resA.visitor != "VISITOR_FRESH" {
		t.Fatalf("surviving caller got visitor %q, want VISITOR_FRESH", resA.visitor)
	}
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 refresh request, got %d", h.bootstrapCount)
	}
}

// TestVideoVisitorRefreshLeaderCancelJoinerSucceeds is the forced-refresh
// analogue of TestVideoVisitorBootstrapLeaderCancelJoinerSucceeds: canceling
// the refresh initiator must not fail the non-canceled joiner, with the
// transport aborting on request-context cancellation if the shared work ever
// inherited the leader's cancellation.
func TestVideoVisitorRefreshLeaderCancelJoinerSucceeds(t *testing.T) {
	calls := newHeldCallSet(t)
	h := newChainHarness(t)
	entered := make(chan bool, 1)
	release := newHeldRelease()
	h.bootstrapReqFn = blockingBootstrapFn(entered, release.ch, freshVisitorBody())
	ext := newVideoTestExtractor(t, h)

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()

	obs := newWaitObserver(2)
	t.Cleanup(obs.close)

	var callA, callB <-chan visitorCallResult
	t.Cleanup(func() {
		release.release()
		calls.joinOutstanding()
	})

	callA = calls.launch(func() (string, error) {
		return ext.forceRefreshVideoVisitorCandidate(obs.wrap(ctxA), "dQw4w9WgXcQ")
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh request never started")
	}

	callB = calls.launch(func() (string, error) {
		return ext.forceRefreshVideoVisitorCandidate(obs.wrap(context.Background()), "dQw4w9WgXcQ")
	})
	waitVisitorRegistrations(t, obs.out, 2)
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 refresh request, got %d", h.bootstrapCount)
	}

	cancelA()
	resA, ok := calls.await(callA, 5*time.Second)
	if !ok {
		t.Fatal("flight initiator did not return promptly after cancel")
	}
	if !errors.Is(resA.err, context.Canceled) {
		t.Fatalf("flight initiator returned err %v, want context.Canceled", resA.err)
	}
	select {
	case <-callB:
		t.Fatal("joiner completed while the flight is still blocked")
	default:
	}
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 refresh request, got %d", h.bootstrapCount)
	}

	release.release()
	resB, ok := calls.await(callB, 5*time.Second)
	if !ok {
		t.Fatal("joiner did not complete after release")
	}
	if resB.err != nil {
		t.Fatalf("joiner returned error: %v", resB.err)
	}
	if resB.visitor != "VISITOR_FRESH" {
		t.Fatalf("joiner got visitor %q, want VISITOR_FRESH", resB.visitor)
	}
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 refresh request, got %d", h.bootstrapCount)
	}
}

// TestVideoVisitorMixedBootstrapEmptyRefreshRejected proves the shared flight
// result contract: when a fresh bootstrap LEADS the shared "video" flight and
// returns a typed but empty VisitorCache, the joining forced-refresh caller
// must fail with an error rather than accept ("", nil) as a successful
// refresh (which would consume the one-refresh bound with an empty visitor).
func TestVideoVisitorMixedBootstrapEmptyRefreshRejected(t *testing.T) {
	calls := newHeldCallSet(t)
	h := newChainHarness(t)
	entered := make(chan bool, 1)
	release := newHeldRelease()
	h.bootstrapReqFn = blockingBootstrapFn(entered, release.ch, `{"responseContext":{}}`)
	ext := newVideoTestExtractor(t, h)
	ext.visitorData = "" // force a fresh bootstrap flight

	obs := newWaitObserver(2)
	t.Cleanup(obs.close)

	var callA, callB <-chan visitorCallResult
	t.Cleanup(func() {
		release.release()
		calls.joinOutstanding()
	})

	callA = calls.launch(func() (string, error) {
		return ext.fetchVideoVisitorCandidate(obs.wrap(context.Background()), "dQw4w9WgXcQ")
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap request never started")
	}

	// The forced refresh joins the bootstrap-led flight: same auth key, no
	// second request.
	callB = calls.launch(func() (string, error) {
		return ext.forceRefreshVideoVisitorCandidate(obs.wrap(context.Background()), "dQw4w9WgXcQ")
	})
	waitVisitorRegistrations(t, obs.out, 2)
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 request while blocked, got %d", h.bootstrapCount)
	}

	release.release()
	resA, ok := calls.await(callA, 5*time.Second)
	if !ok {
		t.Fatal("bootstrap caller did not complete after release")
	}
	resB, ok := calls.await(callB, 5*time.Second)
	if !ok {
		t.Fatal("refresh caller did not complete after release")
	}
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 request, got %d", h.bootstrapCount)
	}
	// Contract: an empty shared result is rejected by both callers; the
	// forced refresh in particular must never report an empty success.
	if resA.err == nil {
		t.Fatalf("bootstrap caller accepted empty visitor data as success")
	}
	if resB.err == nil {
		t.Fatalf("refresh caller accepted empty visitor data as success, got visitor %q", resB.visitor)
	}
}

// runVisitorDataCall runs an error-only visitor-data call in a fresh
// goroutine, reusing the visitorCallResult shape so waitVisitorCall can
// observe completion.
func runVisitorDataCall(fn func() error) <-chan visitorCallResult {
	ch := make(chan visitorCallResult, 1)
	go func() {
		ch <- visitorCallResult{err: fn()}
	}()
	return ch
}

// TestVideoVisitorLegacyGuestIsolatedFromNativeBootstrap proves the legacy
// guest bootstrap (fetchVisitorDataContext, caller-bound and committing) never
// shares the native guest flight key: while the legacy bootstrap is blocked,
// a native bootstrap overlaps on its own request; canceling the legacy caller
// aborts only the legacy flight (via its request context) and the native
// caller still gets its valid candidate. Exactly two requests total.
func TestVideoVisitorLegacyGuestIsolatedFromNativeBootstrap(t *testing.T) {
	calls := newHeldCallSet(t)
	h := newChainHarness(t)
	entered := make(chan bool, 2)
	release := newHeldRelease()
	h.bootstrapReqFn = blockingBootstrapFn(entered, release.ch, freshVisitorBody())
	ext := newVideoTestExtractor(t, h)
	ext.visitorData = "" // native bootstrap must take the flight path

	legacyCtx, cancelLegacy := context.WithCancel(context.Background())
	defer cancelLegacy()

	var legacyRes, nativeRes <-chan visitorCallResult
	t.Cleanup(func() {
		release.release()
		calls.joinOutstanding()
	})

	// 1. Legacy guest bootstrap begins and blocks.
	legacyRes = calls.launchErr(func() error { return ext.fetchVisitorDataContext(legacyCtx, "dQw4w9WgXcQ") })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("legacy bootstrap request never started")
	}

	// 2. Native bootstrap overlaps: isolation means it starts its own request.
	nativeRes = calls.launch(func() (string, error) { return ext.fetchVideoVisitorCandidate(context.Background(), "dQw4w9WgXcQ") })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("native bootstrap request never started (legacy flight collision?)")
	}
	if h.bootstrapCount != 2 {
		t.Fatalf("expected 2 requests (legacy + native isolated), got %d", h.bootstrapCount)
	}

	// 3. Cancel the legacy caller: its own flight aborts via its request
	// context while the native flight stays blocked and untouched.
	cancelLegacy()
	resLegacy, ok := calls.await(legacyRes, 5*time.Second)
	if !ok {
		t.Fatal("legacy caller did not return promptly after cancel")
	}
	if resLegacy.err == nil {
		t.Fatal("legacy caller succeeded despite its flight being canceled")
	}
	select {
	case <-nativeRes:
		t.Fatal("native caller completed while its flight is still blocked")
	default:
	}

	// 4. Release: the native caller gets its valid candidate; the legacy
	// flight is already gone, so exactly two requests ran in total.
	release.release()
	resNative, ok := calls.await(nativeRes, 5*time.Second)
	if !ok {
		t.Fatal("native caller did not complete after release")
	}
	if resNative.err != nil {
		t.Fatalf("native caller returned error: %v", resNative.err)
	}
	if resNative.visitor != "VISITOR_FRESH" {
		t.Fatalf("native caller got visitor %q, want VISITOR_FRESH", resNative.visitor)
	}
	if h.bootstrapCount != 2 {
		t.Fatalf("expected exactly 2 requests, got %d", h.bootstrapCount)
	}
}

// TestVideoVisitorLegacyGuestIsolatedFromNativeRefresh is the forced-refresh
// analogue: a blocked legacy guest bootstrap overlaps a native forced-refresh
// flight; canceling the legacy caller does not fail the native refresh, which
// still produces its valid candidate from its own single request.
func TestVideoVisitorLegacyGuestIsolatedFromNativeRefresh(t *testing.T) {
	calls := newHeldCallSet(t)
	h := newChainHarness(t)
	entered := make(chan bool, 2)
	release := newHeldRelease()
	h.bootstrapReqFn = blockingBootstrapFn(entered, release.ch, freshVisitorBody())
	ext := newVideoTestExtractor(t, h)
	ext.visitorData = "" // legacy bootstrap must take the flight path

	legacyCtx, cancelLegacy := context.WithCancel(context.Background())
	defer cancelLegacy()

	var legacyRes, nativeRes <-chan visitorCallResult
	t.Cleanup(func() {
		release.release()
		calls.joinOutstanding()
	})

	// 1. Legacy guest bootstrap begins and blocks.
	legacyRes = calls.launchErr(func() error { return ext.fetchVisitorDataContext(legacyCtx, "dQw4w9WgXcQ") })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("legacy bootstrap request never started")
	}

	// 2. Native forced refresh overlaps on its own flight.
	nativeRes = calls.launch(func() (string, error) {
		return ext.forceRefreshVideoVisitorCandidate(context.Background(), "dQw4w9WgXcQ")
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("native refresh request never started (legacy flight collision?)")
	}
	if h.bootstrapCount != 2 {
		t.Fatalf("expected 2 requests (legacy + native isolated), got %d", h.bootstrapCount)
	}

	// 3. Cancel the legacy caller: native refresh must not be failed by it.
	cancelLegacy()
	resLegacy, ok := calls.await(legacyRes, 5*time.Second)
	if !ok {
		t.Fatal("legacy caller did not return promptly after cancel")
	}
	if resLegacy.err == nil {
		t.Fatal("legacy caller succeeded despite its flight being canceled")
	}
	select {
	case <-nativeRes:
		t.Fatal("native refresh completed while its flight is still blocked")
	default:
	}

	// 4. Release: the native refresh wins its valid candidate.
	release.release()
	resNative, ok := calls.await(nativeRes, 5*time.Second)
	if !ok {
		t.Fatal("native refresh did not complete after release")
	}
	if resNative.err != nil {
		t.Fatalf("native refresh returned error: %v", resNative.err)
	}
	if resNative.visitor != "VISITOR_FRESH" {
		t.Fatalf("native refresh got visitor %q, want VISITOR_FRESH", resNative.visitor)
	}
	if h.bootstrapCount != 2 {
		t.Fatalf("expected exactly 2 requests, got %d", h.bootstrapCount)
	}
}

// TestVideoVisitorLegacyAsyncRefreshIsolatedFromNative drives the detached
// legacy SWR refresh (refreshVisitorDataAsync, which commits on success)
// overlapping a native bootstrap: each runs its own request on its own key,
// and the native caller gets its valid candidate independently.
func TestVideoVisitorLegacyAsyncRefreshIsolatedFromNative(t *testing.T) {
	calls := newHeldCallSet(t)
	h := newChainHarness(t)
	entered := make(chan bool, 2)
	release := newHeldRelease()
	h.bootstrapReqFn = blockingBootstrapFn(entered, release.ch, freshVisitorBody())
	ext := newVideoTestExtractor(t, h)
	ext.visitorData = "" // native bootstrap must take the flight path

	// The detached legacy refresh is not caller-joined, so join its
	// singleflight key explicitly: DoChan completes only after that flight
	// finishes, giving a real completion barrier instead of a sleep. The join
	// is registered AFTER step 1, so it attaches to the in-flight refresh
	// rather than leading it. legacyJoined keeps cleanup from re-reading the
	// (single-value) join channel on an early t.Fatal.
	var legacyDone <-chan singleflight.Result
	legacyJoined := false
	t.Cleanup(func() {
		release.release()
		calls.joinOutstanding()
		if legacyDone == nil || legacyJoined {
			return
		}
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-legacyDone:
		case <-timer.C:
		}
		timer.Stop()
	})

	// 1. The detached legacy SWR refresh begins and blocks.
	ext.refreshVisitorDataAsync(context.Background(), "dQw4w9WgXcQ")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("async legacy refresh request never started")
	}
	legacyDone = ext.runtime.visitorGroup.DoChan(ext.visitorLegacyAuthKey(), func() (any, error) { return nil, nil })

	// 2. Native bootstrap overlaps on its own flight.
	nativeRes := calls.launch(func() (string, error) { return ext.fetchVideoVisitorCandidate(context.Background(), "dQw4w9WgXcQ") })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("native bootstrap request never started (async refresh collision?)")
	}
	if h.bootstrapCount != 2 {
		t.Fatalf("expected 2 requests (async legacy + native isolated), got %d", h.bootstrapCount)
	}

	// 3. Release both: each flight completes independently with its own
	// response; the native caller gets its valid candidate.
	release.release()
	resNative, ok := calls.await(nativeRes, 5*time.Second)
	if !ok {
		t.Fatal("native caller did not complete after release")
	}
	if resNative.err != nil {
		t.Fatalf("native caller returned error: %v", resNative.err)
	}
	if resNative.visitor != "VISITOR_FRESH" {
		t.Fatalf("native caller got visitor %q, want VISITOR_FRESH", resNative.visitor)
	}
	if h.bootstrapCount != 2 {
		t.Fatalf("expected exactly 2 requests, got %d", h.bootstrapCount)
	}

	// 4. Explicit join: the detached legacy refresh flight has completed before
	// this test's TempDir removal.
	timer := time.NewTimer(10 * time.Second)
	select {
	case <-legacyDone:
		legacyJoined = true
	case <-timer.C:
		t.Fatal("detached legacy refresh flight did not complete before cleanup")
	}
	timer.Stop()
}

// TestVideoVisitorMixedRefreshLeadsBootstrapJoins covers the opposite leader
// ordering of the shared flight: the forced refresh leads with a valid fresh
// visitor and the bootstrap caller joins the flight, both callers receiving
// the same candidate from exactly one underlying request.
func TestVideoVisitorMixedRefreshLeadsBootstrapJoins(t *testing.T) {
	calls := newHeldCallSet(t)
	h := newChainHarness(t)
	entered := make(chan bool, 1)
	release := newHeldRelease()
	h.bootstrapReqFn = blockingBootstrapFn(entered, release.ch, freshVisitorBody())
	ext := newVideoTestExtractor(t, h)
	ext.visitorData = "" // force a fresh bootstrap flight for the joiner

	obs := newWaitObserver(2)
	t.Cleanup(obs.close)

	var callA, callB <-chan visitorCallResult
	t.Cleanup(func() {
		release.release()
		calls.joinOutstanding()
	})

	callA = calls.launch(func() (string, error) {
		return ext.forceRefreshVideoVisitorCandidate(obs.wrap(context.Background()), "dQw4w9WgXcQ")
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh request never started")
	}

	callB = calls.launch(func() (string, error) {
		return ext.fetchVideoVisitorCandidate(obs.wrap(context.Background()), "dQw4w9WgXcQ")
	})
	waitVisitorRegistrations(t, obs.out, 2)
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 request while blocked, got %d", h.bootstrapCount)
	}

	release.release()
	resA, ok := calls.await(callA, 5*time.Second)
	if !ok {
		t.Fatal("refresh caller did not complete after release")
	}
	resB, ok := calls.await(callB, 5*time.Second)
	if !ok {
		t.Fatal("bootstrap joiner did not complete after release")
	}
	if h.bootstrapCount != 1 {
		t.Fatalf("expected exactly 1 request, got %d", h.bootstrapCount)
	}
	if resA.err != nil || resA.visitor != "VISITOR_FRESH" {
		t.Fatalf("refresh caller got visitor %q err %v, want VISITOR_FRESH", resA.visitor, resA.err)
	}
	if resB.err != nil {
		t.Fatalf("bootstrap joiner returned error: %v", resB.err)
	}
	if resB.visitor != "VISITOR_FRESH" {
		t.Fatalf("bootstrap joiner got visitor %q, want VISITOR_FRESH", resB.visitor)
	}
}
