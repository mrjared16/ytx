package ytx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrjared16/ytx/internal/cache"
)

// probeRecordingBody wraps a response body and records the probe's read and
// close activity: the length of every Read buffer it requested, how many
// bytes it consumed in total, and whether Close ran. This proves the read
// stays bounded without over-constraining the number of Read calls.
type probeRecordingBody struct {
	inner    io.Reader
	readErr  error // when set, injected instead of reading inner
	bufLens  []int // length of each Read buffer the probe requested
	consumed int   // total bytes the probe consumed from this body
	closed   bool
}

func (b *probeRecordingBody) Read(p []byte) (n int, err error) {
	b.bufLens = append(b.bufLens, len(p))
	if b.readErr != nil {
		return 0, b.readErr
	}
	n, err = b.inner.Read(p)
	b.consumed += n
	return
}

func (b *probeRecordingBody) Close() error {
	b.closed = true
	return nil
}

// probeStaggeredReader returns (0, nil) once before yielding payload bytes,
// like a transport that briefly delivers no data without signaling EOF. The
// probe must tolerate that without treating it as an empty body.
type probeStaggeredReader struct {
	payload      []byte
	returnedZero bool
}

func (r *probeStaggeredReader) Read(p []byte) (n int, err error) {
	if !r.returnedZero {
		r.returnedZero = true
		return 0, nil
	}
	if len(r.payload) == 0 {
		return 0, io.EOF
	}
	p[0] = r.payload[0]
	r.payload = r.payload[1:]
	return 1, nil
}

// probeBodyReadErr is the exact cause the probe must propagate through its
// read-failure wrap chain; errors.Is against it proves the cause survived.
var probeBodyReadErr = errors.New("simulated probe body read failure")

// probeMock drives a fake http.Client for range probes: it counts requests,
// records Range headers, and wraps every response body so lifecycle state
// stays observable. By default it echoes each requested range as a matching
// Content-Range over a total of 1000 bytes.
type probeMock struct {
	status       int
	bodyText     string
	readErr      error
	stagger      bool   // first Read returns (0, nil) before yielding bodyText
	contentRange string // when echoRange is false, sent verbatim ("" = omitted)
	echoRange    bool
	requests     int
	ranges       []string
	bodies       []*probeRecordingBody
}

func (m *probeMock) client() *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				return nil, fmt.Errorf("unexpected method %s", req.Method)
			}
			m.requests++
			m.ranges = append(m.ranges, req.Header.Get("Range"))
			var inner io.Reader
			if m.stagger {
				inner = &probeStaggeredReader{payload: []byte(m.bodyText)}
			} else {
				inner = strings.NewReader(m.bodyText)
			}
			body := &probeRecordingBody{inner: inner, readErr: m.readErr}
			m.bodies = append(m.bodies, body)
			contentRange := m.contentRange
			if m.echoRange {
				start, end := int64(-1), int64(-1)
				if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
					return jsonHTTPResponse(http.StatusBadRequest, "bad range"), nil
				}
				contentRange = fmt.Sprintf("bytes %d-%d/1000", start, end)
			}
			hdr := http.Header{}
			if contentRange != "" {
				hdr.Set("Content-Range", contentRange)
			}
			return &http.Response{
				StatusCode: m.status,
				Header:     hdr,
				Body:       body,
			}, nil
		}),
	}
}

func newProbeTestExtractor(t *testing.T, m *probeMock) *Extractor {
	t.Helper()
	ext, err := NewExtractor(ModeVideo, "", WithRuntime(NewRuntime()), WithHTTPClient(m.client()))
	if err != nil {
		t.Fatalf("NewExtractor returned error: %v", err)
	}
	ext.cacheManager = nil
	return ext
}

// newBareProbeExtractor is newProbeTestExtractor without an owning mock, for
// tests that install their own http.Client directly.
func newBareProbeExtractor(t *testing.T) *Extractor {
	t.Helper()
	ext, err := NewExtractor(ModeVideo, "", WithRuntime(NewRuntime()), WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("unexpected request %s %s", req.Method, req.URL)
		}),
	}))
	if err != nil {
		t.Fatalf("NewExtractor returned error: %v", err)
	}
	ext.cacheManager = nil
	return ext
}

// assertProbeBodyBounds verifies the probe's read discipline on every
// response body: it never requests more than one byte per Read, consumes at
// most one byte in total (bounded read), and closes the body on every path.
func assertProbeBodyBounds(t *testing.T, m *probeMock) {
	t.Helper()
	if len(m.bodies) == 0 {
		t.Fatal("expected at least one probe request")
	}
	for i, b := range m.bodies {
		for j, bl := range b.bufLens {
			if bl > 1 {
				t.Fatalf("probe body %d read %d requested %d bytes, want <= 1", i, j, bl)
			}
		}
		if b.consumed > 1 {
			t.Fatalf("probe body %d consumed %d bytes, want <= 1", i, b.consumed)
		}
		if !b.closed {
			t.Fatalf("probe body %d not closed", i)
		}
	}
}

// TestProbeRangeRequiresBodyByte verifies a matching 206 header set is not
// enough: the requested byte must actually arrive in the body. A one-byte
// body passes (even through a legit zero-byte no-error first read), an
// empty body fails, and a read error propagates its exact cause.
func TestProbeRangeRequiresBodyByte(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bodyText string
		readErr  error
		stagger  bool
		match    string // "" = success expected
	}{
		{"one byte body passes", "x", nil, false, ""},
		{"zero-byte no-error read before byte passes", "x", nil, true, ""},
		{"empty body fails", "", nil, false, "empty body"},
		{"read error keeps cause", "x", probeBodyReadErr, false, "simulated probe body read failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &probeMock{
				status:    http.StatusPartialContent,
				bodyText:  tc.bodyText,
				readErr:   tc.readErr,
				stagger:   tc.stagger,
				echoRange: true,
			}
			ext := newProbeTestExtractor(t, m)
			contentRange, err := ext.doRangeProbe(t.Context(), "https://synthetic-video.googlevideo.com/stream", "test-ua", 0, 0)
			if tc.match == "" {
				if err != nil {
					t.Fatalf("probe unexpectedly failed: %v", err)
				}
				if contentRange != "bytes 0-0/1000" {
					t.Fatalf("unexpected content range %q", contentRange)
				}
			} else {
				if err == nil {
					t.Fatal("expected probe failure, got success")
				}
				if tc.readErr != nil && !errors.Is(err, tc.readErr) {
					t.Fatalf("read failure cause not preserved through wrap: %v", err)
				}
				if !strings.Contains(fmt.Sprintf("%v", err), tc.match) {
					t.Fatalf("error %v does not contain %q", err, tc.match)
				}
			}
			if m.requests != 1 {
				t.Fatalf("expected 1 request, got %d", m.requests)
			}
			assertProbeBodyBounds(t, m)
		})
	}
}

// cancelingErrContext is a TEST-ONLY caller context that can inject a caller
// cancellation at ONE precise observation point: the final Err() read that
// happens after the probe round succeeded. On that read it cancels the
// UNDERLYING context (so it becomes genuinely canceled for every later reader)
// but still returns the snapshot it took, which is nil — exactly the real-world
// race, where the caller is canceled in the window between a successful probe
// round and winner publication.
//
// Nothing here counts Err() calls or depends on a call index: the phase is
// derived from the arming condition (both probe bodies closed) alone.
type cancelingErrContext struct {
	context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	armed     bool
	tripped   bool
	closeSeen int
}

func newCancelingErrContext() *cancelingErrContext {
	base, cancel := context.WithCancel(context.Background())
	return &cancelingErrContext{Context: base, cancel: cancel}
}

// bodyClosed records one probe body close and arms once BOTH probe bodies have
// been served and closed, so the probe round is provably complete and
// successful before any cancellation is injected.
func (c *cancelingErrContext) bodyClosed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeSeen++
	if c.closeSeen >= 2 && !c.armed {
		c.armed = true
	}
}

func (c *cancelingErrContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed {
		return c.Context.Err()
	}
	if !c.tripped {
		// The validator's FINAL post-probe Err() snapshot: take it, trip the
		// underlying cancellation, and hand back the snapshot it observed.
		c.tripped = true
		snapshot := c.Context.Err()
		c.cancel()
		return snapshot
	}
	// Every later reader (the winner publication gate) sees the cancellation.
	return c.Context.Err()
}

// TestValidateStreamEndsPublishesNothingAfterCancellation proves the validator
// re-reads the caller's context AFTER the probe round, not before: arming
// cancellation once both probes have already succeeded must make the winner
// un-publishable, and must leave extractor/runtime/disk state untouched.
//
// This is deliberately the narrow oracle: the cancel is injected at exactly the
// publication boundary, so a check placed before the probe round (or omitted)
// cannot satisfy it.
func TestValidateStreamEndsPublishesNothingAfterCancellation(t *testing.T) {
	const (
		videoURL = "https://synthetic-video.googlevideo.com/videoplayback"
		audioURL = "https://synthetic-audio.googlevideo.com/videoplayback"
	)
	ctx := newCancelingErrContext()
	t.Cleanup(ctx.cancel)

	// BOTH probe bodies arm on close, so the probe round is provably complete
	// and successful before any cancellation is injected.
	armOnClose := func(req *http.Request) *http.Response {
		resp := rangeProbeResponse(req, 1000)
		resp.Body = &armOnCloseBody{inner: resp.Body, onClose: ctx.bodyClosed}
		return resp
	}
	h := newChainHarness(t)
	h.playerByName["101"] = func(int) (int, string) {
		return http.StatusOK, okPlayerBody(videoURL, audioURL, true)
	}
	h.probeByHost["synthetic-video.googlevideo.com"] = armOnClose
	h.probeByHost["synthetic-audio.googlevideo.com"] = armOnClose

	ext := newVideoTestExtractor(t, h)
	ext.cacheManager = cache.NewCacheManagerWithDir(t.TempDir())

	result, err := ext.ExtractVideoContext(ctx, "dQw4w9WgXcQ")
	if result != nil {
		t.Fatalf("a canceled-after-probes extraction must not publish a result, got %+v", result)
	}
	exhausted := requireExhausted(t, err)
	if !errors.Is(exhausted, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", exhausted)
	}
	if !exhausted.Cancelled() {
		t.Fatalf("expected Cancelled() to classify the run, got %+v", exhausted.Attempts)
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

	// No visitor state was written at any layer.
	if ext.visitorData != "VISITOR_TEST" {
		t.Fatalf("canceled run mutated extractor visitor state: %q", ext.visitorData)
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

// armOnCloseBody runs onClose exactly once, after the underlying body has been
// closed, so the cancellation is injected strictly after the response body
// completed.
type armOnCloseBody struct {
	inner   io.ReadCloser
	onClose func()
	once    sync.Once
}

func (b *armOnCloseBody) Read(p []byte) (int, error) { return b.inner.Read(p) }

func (b *armOnCloseBody) Close() error {
	err := b.inner.Close()
	b.once.Do(b.onClose)
	return err
}

// TestProbe429StaysTerminal verifies a rate-limited response keeps its
// playerHTTPError classification regardless of body state, so the video
// chain still stops on 429.
func TestProbe429StaysTerminal(t *testing.T) {
	m := &probeMock{status: http.StatusTooManyRequests, bodyText: "", echoRange: false}
	ext := newProbeTestExtractor(t, m)
	_, err := ext.doRangeProbe(t.Context(), "https://synthetic-video.googlevideo.com/stream", "test-ua", 0, 0)
	var httpErr *playerHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected playerHTTPError classification, got %T: %v", err, err)
	}
	if httpErr.Status != http.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %d", httpErr.Status)
	}
	if m.requests != 1 {
		t.Fatalf("expected 1 request, got %d", m.requests)
	}
	assertProbeBodyBounds(t, m)
}

// TestProbeErrorResponsesUnchanged pins the pre-existing failure modes
// (non-206 status, missing Content-Range) so the body check does not shadow
// them and their messages stay actionable.
func TestProbeErrorResponsesUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		contentRange string
		echoRange    bool
		match        string
	}{
		{"non-206 rejects", http.StatusNotFound, "", true, "got HTTP 404, want 206"},
		{"missing Content-Range rejects", http.StatusPartialContent, "", false, "missing Content-Range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &probeMock{
				status:       tc.status,
				bodyText:     "x",
				contentRange: tc.contentRange,
				echoRange:    tc.echoRange,
			}
			ext := newProbeTestExtractor(t, m)
			_, err := ext.doRangeProbe(t.Context(), "https://synthetic-video.googlevideo.com/stream", "test-ua", 0, 0)
			if err == nil {
				t.Fatal("expected probe failure, got success")
			}
			if !strings.Contains(fmt.Sprintf("%v", err), tc.match) {
				t.Fatalf("error %v does not contain %q", err, tc.match)
			}
			if m.requests != 1 {
				t.Fatalf("expected 1 request, got %d", m.requests)
			}
			assertProbeBodyBounds(t, m)
		})
	}
}

// TestProbeMediaURLPolicy pins the probe URL policy as pure validation: HTTPS
// only, no userinfo, non-empty non-IP host, a strict ".googlevideo.com"
// boundary that rejects lookalikes in BOTH directions plus the bare apex, a
// default-or-explicit-443 port, and a strictly parseable query. All hostnames
// are synthetic; no DNS resolution or network access happens here.
func TestProbeMediaURLPolicy(t *testing.T) {
	const validQuery = "?expire=1784000000&ei=abc&ip=203.0.113.7&ipbits=32&mime=video%2Fmp4&n=xyzIt&ratebypass=yes&signature=DEADBEEFCAFE"
	for _, tc := range []struct {
		name    string
		raw     string
		wantErr string // "" = must be accepted
	}{
		{"approved subdomain with signed query", "https://rr1.googlevideo.com/videoplayback" + validQuery, ""},
		{"explicit 443 allowed", "https://rr1.googlevideo.com:443/videoplayback" + validQuery, ""},
		{"no query accepted", "https://rr3.googlevideo.com/videoplayback", ""},
		{"multi-label approved subdomain", "https://a.b.c.googlevideo.com/videoplayback", ""},
		{"http scheme rejected", "http://rr1.googlevideo.com/videoplayback", "not https"},
		{"other scheme rejected", "ftp://rr1.googlevideo.com/videoplayback", "not https"},
		{"userinfo rejected", "https://user:pw@rr1.googlevideo.com/videoplayback", "userinfo"},
		{"malformed query rejected", "https://rr1.googlevideo.com/videoplayback?n=abc&keep=%ZZ", "malformed query"},
		{"suffix-prefix lookalike rejected", "https://evilgooglevideo.com/videoplayback", "not an approved media host"},
		{"prefix lookalike rejected", "https://googlevideo.com.evil.example/videoplayback", "not an approved media host"},
		{"bare apex rejected", "https://googlevideo.com/videoplayback", "not an approved media host"},
		{"empty label before suffix rejected", "https://.googlevideo.com/videoplayback", "not an approved media host"},
		{"unrelated host rejected", "https://media.example.com/videoplayback", "not an approved media host"},
		{"youtube.com rejected", "https://www.youtube.com/videoplayback", "not an approved media host"},
		{"ipv4 literal rejected", "https://192.0.2.10/videoplayback", "IP literal"},
		{"ipv6 literal rejected", "https://[2001:db8::1]/videoplayback", "IP literal"},
		{"nonstandard port rejected", "https://rr1.googlevideo.com:8443/videoplayback", "not the standard https port"},
		{"empty host rejected", "https:///videoplayback", "no host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateMediaStreamURL(tc.raw)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected %q to be accepted, got %v", tc.raw, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %q to be rejected with %q, got acceptance", tc.raw, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestProbeRejectsUnapprovedURLBeforeAnyIO proves the policy runs BEFORE the
// transport: a rejected URL must never produce a request at all.
func TestProbeRejectsUnapprovedURLBeforeAnyIO(t *testing.T) {
	for _, raw := range []string{
		"http://rr1.googlevideo.com/videoplayback",
		"https://evilgooglevideo.com/videoplayback",
		"https://googlevideo.com.evil.example/videoplayback",
		"https://user:pw@rr1.googlevideo.com/videoplayback",
		"https://rr1.googlevideo.com/videoplayback?keep=%ZZ",
		"https://rr1.googlevideo.com:8443/videoplayback",
		"https://192.0.2.10/videoplayback",
	} {
		t.Run(raw, func(t *testing.T) {
			m := &probeMock{status: http.StatusPartialContent, bodyText: "x", echoRange: true}
			ext := newProbeTestExtractor(t, m)
			if _, err := ext.doRangeProbe(t.Context(), raw, "test-ua", 0, 0); err == nil {
				t.Fatal("expected the probe to reject the URL, got success")
			}
			if m.requests != 0 {
				t.Fatalf("rejected URL must not reach the transport, got %d requests", m.requests)
			}
			if len(m.bodies) != 0 {
				t.Fatalf("rejected URL must not produce a response body, got %d", len(m.bodies))
			}
		})
	}
}

// redirectProbeMock serves a 302 whose Location is chosen per test, so redirect
// destination validation can be exercised without any real network.
type redirectProbeMock struct {
	location   string
	hops       int
	requests   int
	hopSignals []chan bool
}

func (m *redirectProbeMock) client() *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			m.requests++
			if len(m.hopSignals) >= m.requests {
				select {
				case m.hopSignals[m.requests-1] <- true:
				default:
				}
			}
			if m.hops > 1 {
				m.hops--
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{m.location}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			}
			return rangeProbeResponse(req, 1000), nil
		}),
	}
}

// TestProbeRedirectDestinationPolicy proves redirect destinations get the SAME
// media URL policy: an escape to a non-approved host is refused, a hop within
// approved hosts is followed, and the SHARED extractor client is never mutated.
func TestProbeRedirectDestinationPolicy(t *testing.T) {
	t.Run("escape to non-approved host refused", func(t *testing.T) {
		m := &redirectProbeMock{location: "https://evilgooglevideo.com/videoplayback", hops: 2}
		ext := newBareProbeExtractor(t)
		ext.httpClient = m.client()
		_, err := ext.doRangeProbe(t.Context(), "https://rr1.googlevideo.com/stream", "test-ua", 0, 0)
		if err == nil {
			t.Fatal("expected the redirect escape to be refused, got success")
		}
		if !strings.Contains(err.Error(), "redirect destination") {
			t.Fatalf("error %v does not report a rejected redirect destination", err)
		}
		if m.requests != 1 {
			t.Fatalf("the escape must be refused before the second hop, got %d requests", m.requests)
		}
		// The shared client must be untouched.
		if ext.httpClient.CheckRedirect != nil {
			t.Fatal("probe must not mutate the shared http.Client redirect policy")
		}
	})

	t.Run("hop within approved hosts followed", func(t *testing.T) {
		m := &redirectProbeMock{location: "https://rr2.googlevideo.com/stream", hops: 2}
		ext := newBareProbeExtractor(t)
		ext.httpClient = m.client()
		contentRange, err := ext.doRangeProbe(t.Context(), "https://rr1.googlevideo.com/stream", "test-ua", 0, 0)
		if err != nil {
			t.Fatalf("approved redirect hop should be followed: %v", err)
		}
		if contentRange == "" {
			t.Fatal("expected a Content-Range from the followed hop")
		}
		if m.requests != 2 {
			t.Fatalf("expected 2 requests (initial + approved hop), got %d", m.requests)
		}
	})

	t.Run("existing redirect policy is preserved and consulted", func(t *testing.T) {
		m := &redirectProbeMock{location: "https://rr2.googlevideo.com/stream", hops: 2}
		var consulted int
		base := m.client()
		base.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			consulted++
			return nil
		}
		ext := newBareProbeExtractor(t)
		ext.httpClient = base
		if _, err := ext.doRangeProbe(t.Context(), "https://rr1.googlevideo.com/stream", "test-ua", 0, 0); err != nil {
			t.Fatalf("approved redirect hop should be followed: %v", err)
		}
		if consulted == 0 {
			t.Fatal("the caller's existing CheckRedirect policy must still be consulted")
		}
		if base.CheckRedirect == nil {
			t.Fatal("the caller's redirect policy must remain installed on the shared client")
		}
	})

	t.Run("callback error is honoured verbatim", func(t *testing.T) {
		// The callback's own verdict must propagate unchanged — including the
		// http.ErrUseLastResponse sentinel, which is an ERROR return meaning
		// "return the redirect response itself, do not follow".
		m := &redirectProbeMock{location: "https://rr2.googlevideo.com/stream", hops: 2}
		base := m.client()
		var sawUseLastResponse bool
		base.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			sawUseLastResponse = true
			return http.ErrUseLastResponse
		}
		ext := newBareProbeExtractor(t)
		ext.httpClient = base

		_, err := ext.doRangeProbe(t.Context(), "https://rr1.googlevideo.com/stream", "test-ua", 0, 0)
		if !sawUseLastResponse {
			t.Fatal("the caller's CheckRedirect must be consulted")
		}
		if err == nil {
			t.Fatal("expected the probe to reject the unfollowed 302 response")
		}
		// http.ErrUseLastResponse means "return the redirect response itself,
		// do not follow". The probe therefore sees the 302 directly and fails
		// on the strict 206 requirement — proof the sentinel was honoured
		// instead of being overridden or treated as approval to continue.
		if !strings.Contains(err.Error(), "got HTTP 302, want 206") {
			t.Fatalf("expected the unfollowed 302 to surface as a non-206 probe failure, got %v", err)
		}
		if m.requests != 1 {
			t.Fatalf("ErrUseLastResponse must not follow the redirect, got %d requests", m.requests)
		}
	})

	t.Run("callback rewriting to unapproved host is re-validated", func(t *testing.T) {
		// The callback receives a MUTABLE request and can rewrite req.URL to
		// an unapproved destination. Validation that ran only BEFORE the
		// callback would wave this through, so the final destination must be
		// re-checked and the unapproved host must never reach the transport.
		var mu sync.Mutex
		var reached []string
		client := &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				mu.Lock()
				reached = append(reached, req.URL.Host)
				mu.Unlock()
				if req.URL.Host == "evilgooglevideo.com" {
					return rangeProbeResponse(req, 1000), nil
				}
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"https://rr2.googlevideo.com/stream"}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			}),
		}
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			req.URL, _ = url.Parse("https://evilgooglevideo.com/stream")
			return nil
		}
		ext := newBareProbeExtractor(t)
		ext.httpClient = client

		_, err := ext.doRangeProbe(t.Context(), "https://rr1.googlevideo.com/stream", "test-ua", 0, 0)
		if err == nil {
			t.Fatal("expected the rewritten unapproved destination to be rejected, got success")
		}
		if !strings.Contains(err.Error(), "redirect destination") {
			t.Fatalf("error %v does not report a rejected redirect destination", err)
		}
		mu.Lock()
		got := append([]string(nil), reached...)
		mu.Unlock()
		for _, host := range got {
			if host == "evilgooglevideo.com" {
				t.Fatalf("the unapproved host reached the transport: contacted %v", got)
			}
		}
		if len(got) != 1 || got[0] != "rr1.googlevideo.com" {
			t.Fatalf("expected only the initial approved request, contacted %v", got)
		}
	})

	t.Run("callback rewriting to approved host is allowed", func(t *testing.T) {
		// The re-validation must not be a blanket ban: a callback that rewrites
		// to another APPROVED host is legitimate and must still be followed.
		client := &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return rangeProbeResponse(req, 1000), nil
			}),
		}
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			req.URL, _ = url.Parse("https://rr5.googlevideo.com/stream")
			return nil
		}
		ext := newBareProbeExtractor(t)
		ext.httpClient = client
		if _, err := ext.doRangeProbe(t.Context(), "https://rr1.googlevideo.com/stream", "test-ua", 0, 0); err != nil {
			t.Fatalf("an approved rewrite must still be followed: %v", err)
		}
	})

	t.Run("default redirect limit applied when none set", func(t *testing.T) {
		// An endless approved-host redirect chain must terminate at the
		// standard 10-redirect limit rather than looping forever.
		var requested int
		client := &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requested++
				if requested > 50 {
					return nil, fmt.Errorf("redirect loop was not stopped")
				}
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"https://rr2.googlevideo.com/stream"}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			}),
		}
		ext := newBareProbeExtractor(t)
		ext.httpClient = client
		if _, err := ext.doRangeProbe(t.Context(), "https://rr1.googlevideo.com/stream", "test-ua", 0, 0); err == nil {
			t.Fatal("expected the default redirect limit to stop the chain")
		}
		if requested > 12 {
			t.Fatalf("default redirect limit not enforced: %d requests", requested)
		}
	})
}

// TestProbe429ClassifiedBeforeBodyRead proves a rate-limited response is
// classified from its status alone, BEFORE the body is read: a 429 whose body
// would block forever or fail must still return immediately as a 429, and the
// body must never be read at all.
func TestProbe429ClassifiedBeforeBodyRead(t *testing.T) {
	body := newBlockingProbeBody()
	// Unconditional unblock: if the probe wrongly reached the body and parked,
	// cleanup releases it so no read goroutine outlives the test.
	t.Cleanup(body.release)
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{},
				Body:       body,
			}, nil
		}),
	}
	ext := newBareProbeExtractor(t)
	ext.httpClient = client

	done := make(chan error, 1)
	go func() {
		_, err := ext.doRangeProbe(t.Context(), "https://rr1.googlevideo.com/stream", "test-ua", 0, 0)
		done <- err
	}()
	select {
	case err := <-done:
		var httpErr *playerHTTPError
		if !errors.As(err, &httpErr) {
			t.Fatalf("expected playerHTTPError, got %T: %v", err, err)
		}
		if httpErr.Status != http.StatusTooManyRequests {
			t.Fatalf("expected status 429, got %d", httpErr.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("429 classification waited on the body read")
	}
	select {
	case <-body.entered:
		t.Fatal("429 response body must not be read")
	default:
	}
	if !body.closed {
		t.Fatal("429 response body must still be closed")
	}
}

// blockingProbeBody signals entry into Read and then blocks — modelling a
// response that declares a status but never delivers its body — until release
// is closed. The release channel is what keeps the test leak-free: if the probe
// ever DID reach the body (the defect this test guards against), the goroutine
// parks here and cleanup unblocks it, so no read goroutine outlives the test.
type blockingProbeBody struct {
	entered   chan bool
	releaseCh chan struct{}
	once      sync.Once
	closed    bool
}

func newBlockingProbeBody() *blockingProbeBody {
	return &blockingProbeBody{entered: make(chan bool, 1), releaseCh: make(chan struct{})}
}

// release unblocks a parked Read. Idempotent, so cleanup may always call it.
func (b *blockingProbeBody) release() { b.once.Do(func() { close(b.releaseCh) }) }

func (b *blockingProbeBody) Read(p []byte) (int, error) {
	select {
	case b.entered <- true:
	default:
	}
	<-b.releaseCh
	return 0, io.EOF
}

func (b *blockingProbeBody) Close() error {
	b.closed = true
	return nil
}

// gatedProbeMock releases each probe host through its own gate channel, so a
// test controls which probe outcome is produced first without any sleep.
type gatedProbeMock struct {
	gates   map[string]chan struct{}
	served  map[string]chan bool
	byHost  map[string]func(req *http.Request) *http.Response
	hostFor map[string]string // stream URL -> host key
}

func (m *gatedProbeMock) client() *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			key := m.hostFor[req.URL.String()]
			<-m.gates[key]
			select {
			case m.served[key] <- true:
			default:
			}
			return m.byHost[key](req), nil
		}),
	}
}

// siblingCancelMock reproduces the ORIGINAL defect ordering deterministically.
// The first (video) transport returns an error that WRAPS
// context.DeadlineExceeded — a terminal probe failure, which makes
// validateStreamEnds cancel the shared probe context. The second (audio)
// transport BLOCKS until it observes that request-context cancellation, which
// is positive proof the first terminal result was actually consumed, and only
// then returns its ALREADY-OBSERVED 429 response.
//
// This is the ordering the old code lost: the terminal first result returned
// early and drained the sibling into a discarded slot, so the 429 vanished. The
// caller context is never canceled, so the 429 must still win classification.
type siblingCancelMock struct {
	videoObserved  chan bool
	audioObserved  chan bool
	audioSawCancel chan bool
}

func (m *siblingCancelMock) client() *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			// Terminal first result: wraps the local probe deadline.
			if req.URL.Host == "synthetic-video.googlevideo.com" {
				close(m.videoObserved)
				return nil, fmt.Errorf("probe request failed: %w", context.DeadlineExceeded)
			}
			// Sibling: block until the shared probe context is canceled, which
			// only happens because the first terminal result was consumed.
			select {
			case <-req.Context().Done():
			case <-time.After(30 * time.Second):
				return nil, fmt.Errorf("sibling probe was never canceled")
			}
			close(m.audioSawCancel)
			close(m.audioObserved)
			// The 429 was already determined before the wait; it is the
			// outcome this probe reports.
			return jsonHTTPResponse(http.StatusTooManyRequests, "rate limited"), nil
		}),
	}
}

// TestValidateStreamEndsKeepsSibling429AfterTerminalFirst is the deterministic
// counterexample for the original defect: a TERMINAL first result (a wrapped
// local deadline) must NOT discard the sibling's already-observed 429.
//
// Proof chain, with no sleep and no production hook:
//   - the audio transport only proceeds after req.Context() is canceled, and
//     that cancel comes solely from the validator consuming the video probe's
//     terminal result (the caller context is never canceled);
//   - so reaching the assertions at all proves the first terminal was consumed;
//   - the 429 is then asserted to survive, with the caller context still live.
func TestValidateStreamEndsKeepsSibling429AfterTerminalFirst(t *testing.T) {
	const (
		videoURL = "https://synthetic-video.googlevideo.com/videoplayback"
		audioURL = "https://synthetic-audio.googlevideo.com/videoplayback"
	)
	m := &siblingCancelMock{
		videoObserved:  make(chan bool),
		audioObserved:  make(chan bool),
		audioSawCancel: make(chan bool),
	}
	ext := newBareProbeExtractor(t)
	ext.httpClient = m.client()

	// The CALLER context is never canceled, so nothing here can be blamed on
	// caller cancellation: only the observed 429 can explain the result.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	profile := videoClientChain[0]
	err := ext.validateStreamEnds(ctx, profile,
		&Format{URL: videoURL, ContentLength: "1000"},
		&Format{URL: audioURL, ContentLength: "1000"})

	if ctx.Err() != nil {
		t.Fatalf("caller context must stay live, got %v", ctx.Err())
	}
	if !isRateLimitedProbeError(err) {
		t.Fatalf("expected the sibling's 429 to survive the terminal first result, got %v", err)
	}
	// Both transports demonstrably ran; the sibling really did observe the
	// cancel triggered by consuming the first terminal result.
	for name, ch := range map[string]<-chan bool{
		"video terminal observed": m.videoObserved,
		"sibling saw cancel":      m.audioSawCancel,
		"sibling answered":        m.audioObserved,
	} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never happened", name)
		}
	}
}

// TestValidateStreamEndsKeepsBothOutcomes proves validateStreamEnds retains BOTH
// probe outcomes and classifies them order-independently: any observed HTTP 429
// outranks an ordinary sibling failure no matter which probe reported first,
// and the caller's own cancellation outranks a 429. Release order is controlled
// by per-host gates and each served signal, never a sleep.
func TestValidateStreamEndsKeepsBothOutcomes(t *testing.T) {
	const (
		videoURL = "https://synthetic-video.googlevideo.com/videoplayback"
		audioURL = "https://synthetic-audio.googlevideo.com/videoplayback"
	)
	profile := videoClientChain[0]
	videoFmt := &Format{URL: videoURL, ContentLength: "1000"}
	audioFmt := &Format{URL: audioURL, ContentLength: "1000"}

	rateLimited := func(req *http.Request) *http.Response {
		return jsonHTTPResponse(http.StatusTooManyRequests, "rate limited")
	}
	forbidden := func(req *http.Request) *http.Response {
		return jsonHTTPResponse(http.StatusForbidden, "forbidden")
	}

	run := func(t *testing.T, firstSecond bool, cancelBefore bool) error {
		t.Helper()
		// Release ORDER, not which host carries which response, is what varies
		// across the two subtests: the 429 host is released first in one and
		// second in the other, so both delivery orders are genuinely exercised.
		// (An earlier version swapped the host RESPONSES while releasing
		// video-first both times, which silently tested one order twice.)
		rateLimitedHost, otherHost := "video", "audio"
		if firstSecond {
			rateLimitedHost, otherHost = "audio", "video"
		}
		m := &gatedProbeMock{
			gates:   map[string]chan struct{}{"video": make(chan struct{}), "audio": make(chan struct{})},
			served:  map[string]chan bool{"video": make(chan bool, 1), "audio": make(chan bool, 1)},
			byHost:  map[string]func(*http.Request) *http.Response{"video": forbidden, "audio": forbidden},
			hostFor: map[string]string{videoURL: "video", audioURL: "audio"},
		}
		m.byHost[rateLimitedHost] = rateLimited
		m.byHost[otherHost] = forbidden
		ext := newBareProbeExtractor(t)
		ext.httpClient = m.client()

		// Unconditional gate release: if an early Fatal skipped a gate, the
		// parked probe goroutine must still be released so it cannot outlive
		// the test.
		gatesReleased := false
		releaseAll := func() {
			if gatesReleased {
				return
			}
			gatesReleased = true
			for _, g := range m.gates {
				select {
				case <-g:
				default:
					close(g)
				}
			}
		}
		t.Cleanup(releaseAll)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if cancelBefore {
			cancel()
		}

		done := make(chan error, 1)
		go func() {
			done <- ext.validateStreamEnds(ctx, profile, videoFmt, audioFmt)
		}()

		for _, host := range []string{rateLimitedHost, otherHost} {
			select {
			case <-m.gates[host]:
			default:
				close(m.gates[host])
			}
			// Positive proof this host's transport actually ran before the next
			// one is released — a served signal, not a sleep.
			select {
			case <-m.served[host]:
			case <-time.After(5 * time.Second):
				t.Fatalf("probe host %s was never served", host)
			}
		}
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("validateStreamEnds did not return; a probe outcome was dropped")
			return nil
		}
	}

	t.Run("429 delivered first still wins", func(t *testing.T) {
		err := run(t, true, false)
		if !isRateLimitedProbeError(err) {
			t.Fatalf("expected the observed 429 to be returned, got %v", err)
		}
	})

	t.Run("429 delivered second still wins", func(t *testing.T) {
		err := run(t, false, false)
		if !isRateLimitedProbeError(err) {
			t.Fatalf("expected the observed 429 to be returned, got %v", err)
		}
	})

	t.Run("caller cancellation outranks 429", func(t *testing.T) {
		err := run(t, false, true)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected the caller's context.Canceled, got %v", err)
		}
	})
}

// TestProbeStreamEndTransportRanges verifies the request counts and Range
// headers for both probe shapes: one direct request when the content length
// is known, a 0-0 discovery request followed by the near-end request when it
// is not.
func TestProbeStreamEndTransportRanges(t *testing.T) {
	for _, tc := range []struct {
		name          string
		contentLength string
		wantRanges    []string
	}{
		{"known length probes final byte once", "1000", []string{"bytes=999-999"}},
		{"unknown length discovers then probes final byte", "", []string{"bytes=0-0", "bytes=999-999"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &probeMock{status: http.StatusPartialContent, bodyText: "x", echoRange: true}
			ext := newProbeTestExtractor(t, m)
			if err := ext.probeStreamEnd(t.Context(), "https://synthetic-video.googlevideo.com/stream", "test-ua", tc.contentLength); err != nil {
				t.Fatalf("probeStreamEnd failed: %v", err)
			}
			if len(m.ranges) != len(tc.wantRanges) {
				t.Fatalf("unexpected ranges: %+v", m.ranges)
			}
			for i, want := range tc.wantRanges {
				if m.ranges[i] != want {
					t.Fatalf("range %d: got %q want %q", i, m.ranges[i], want)
				}
			}
			if m.requests != len(tc.wantRanges) {
				t.Fatalf("expected %d requests, got %d", len(tc.wantRanges), m.requests)
			}
			assertProbeBodyBounds(t, m)
		})
	}
}
