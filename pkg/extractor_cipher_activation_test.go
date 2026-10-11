package ytx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrjared16/ytx/internal/cache"
	"golang.org/x/sync/singleflight"
)

const activationVideoID = "dQw4w9WgXcQ"

const activationPlayerPath = "/s/player/acttest/player_embed.vflset/en_US/base.js"

// activationModernPlayer is a synthetic URL-wrapper player with a positive STS.
const activationModernPlayer = `var signatureTimestamp=20731;
(function(_yt_player){
var g={};
g.$I=function(s,already){var _=this;_.u=new URL(s);_.m={};_.set=function(k,v){_.m[k]=String(v)};_.get=function(k){if(k==="n"&&_.u.searchParams.has("n")&&_.m.n===undefined){_.m.n=_.u.searchParams.get("n")+"_mod"}return _.m[k]}};
var S=[];S[21]="set";
jW=function(T,M,q){M=M===void 0?"":M;q=q===void 0?"":q;T=new g.$I(T,!0);T.set("alr","yes");q&&(q=Qe(q),T[S[21]](M,q));return T};
Qe=function(x){return x};
})(_yt_player);
`

// activationLegacyPlayer is a classic URLObj-wrapper player with its own
// positive STS literal.
const activationLegacyPlayer = `var signatureTimestamp=12345;
(function(){
function URLObj(sig){this.map={s:sig};}
URLObj.prototype.get=function(k){return this.map[k];};
URLObj.prototype.set=function(k,v){this.map[k]=v;};
URLObj.prototype.apply=function(){this.map.s=this.map.s.split('').reverse().join('');};
kS=function(url,mode,sig){var o=new URLObj(sig);o.set("alr","yes");o.apply();return o;};
})();`

const activationPlayerResponseBody = `{
"responseContext":{"visitorData":"VISITOR_TEST"},
"playabilityStatus":{"status":"OK"},
"streamingData":{"adaptiveFormats":[{"itag":141,"url":"https://stream.test/videoplayback?foo=bar","mimeType":"audio/mp4; codecs=\"mp4a.40.2\"","bitrate":256000,"quality":"tiny"}]},
"videoDetails":{"videoId":"dQw4w9WgXcQ","title":"test","lengthSeconds":"1","author":"author","shortDescription":""}
}`

// activationHarness drives cipher extraction against an isolated cache with
// a deterministic transport (embed page + player JS served, held on demand).
type activationHarness struct {
	t             *testing.T
	ext           *Extractor
	playerGets    int
	apiBodies     []string
	playerEntered chan struct{} // non-nil: blocking signal once the player JS GET entered the transport
	playerHold    chan struct{} // non-nil: player JS GET blocks until closed
	playerErr     int           // HTTP status served for the player JS; 0 = ok
	servedPlayer  []byte
}

func newActivationHarness(t *testing.T) *activationHarness {
	t.Helper()
	return newActivationHarnessRuntime(t, NewRuntime())
}

func newActivationHarnessRuntime(t *testing.T, runtime *Runtime) *activationHarness {
	t.Helper()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	t.Cleanup(func() { runtime.CloseCachedEngine() })

	cookieFile := filepath.Join(t.TempDir(), "cookies.txt")
	content := "# Netscape HTTP Cookie File\n.youtube.com\tTRUE\t/\tTRUE\t4102444800\tSAPISID\tTestOnlyFakeSAPISIDValue\n"
	if err := os.WriteFile(cookieFile, []byte(content), 0600); err != nil {
		t.Fatalf("write cookie file: %v", err)
	}

	ext, err := NewExtractor(ModeMusic, cookieFile,
		WithRuntime(runtime),
		WithCacheManager(cache.NewCacheManagerWithDir(cacheDir)),
	)
	if err != nil {
		t.Fatalf("NewExtractor: %v", err)
	}
	// Pre-set visitor/session so the mock never sees the watch page.
	ext.visitorData = "VISITOR_TEST"
	ext.sessionIndex = "0"

	h := &activationHarness{t: t, servedPlayer: []byte(activationModernPlayer)}
	ext.httpClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.Method == http.MethodGet && req.URL.Host == "www.youtube.com" && strings.HasPrefix(req.URL.Path, "/embed/"):
				return jsonHTTPResponse(http.StatusOK, `<!doctype html><html><script src="`+activationPlayerPath+`"></script></html>`), nil
			case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/s/player/"):
				if h.playerEntered != nil {
					select {
					case h.playerEntered <- struct{}{}:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
				}
				if h.playerHold != nil {
					select {
					case <-h.playerHold:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
				}
				h.playerGets++
				if h.playerErr != 0 {
					return jsonHTTPResponse(h.playerErr, "player fetch failed"), nil
				}
				return jsonHTTPResponse(http.StatusOK, string(h.servedPlayer)), nil
			case req.Method == http.MethodPost && req.URL.Host == "music.youtube.com" && req.URL.Path == "/youtubei/v1/player":
				body, _ := io.ReadAll(req.Body)
				h.apiBodies = append(h.apiBodies, string(body))
				return jsonHTTPResponse(http.StatusOK, activationPlayerResponseBody), nil
			}
			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}),
	}
	h.ext = ext
	return h
}

func (h *activationHarness) apiCalls() int {
	h.t.Helper()
	return len(h.apiBodies)
}

// lastBodySTS reads the recorded timestamp after extraction completes.
func (h *activationHarness) lastBodySTS() int {
	if len(h.apiBodies) == 0 {
		return -1
	}
	var m struct {
		PlaybackContext struct {
			ContentPlaybackContext struct {
				SignatureTimestamp int `json:"signatureTimestamp"`
			} `json:"contentPlaybackContext"`
		} `json:"playbackContext"`
	}
	if err := json.Unmarshal([]byte(h.apiBodies[len(h.apiBodies)-1]), &m); err != nil {
		return -2
	}
	return m.PlaybackContext.ContentPlaybackContext.SignatureTimestamp
}

func activationSeedCache(t *testing.T, h *activationHarness, playerJS []byte, fp string, sts int) {
	t.Helper()
	now := time.Now()
	cc := &cache.CipherCache{
		Version:            cache.CurrentCacheVersion,
		CreatedAt:          now,
		ExpiresAt:          now.Add(cache.CacheTTL),
		PlayerURL:          activationPlayerPath,
		PlayerFingerprint:  fp,
		BaseJSPath:         activationPlayerPath,
		SigFunction:        "jW",
		SigUsesURLWrapper:  true,
		SignatureTimestamp: sts,
		WrapperBuildID:     currentWrapperBytecodeBuildID(),
		JSCode:             "",
	}
	if err := h.ext.cacheManager.Save(cc); err != nil {
		t.Fatalf("save cipher cache: %v", err)
	}
	if len(playerJS) > 0 {
		if err := h.ext.cacheManager.SavePlayerJS(playerJS); err != nil {
			t.Fatalf("save player.js: %v", err)
		}
	}
}

func TestActivationCoherentWarmCacheNoFetch(t *testing.T) {
	h := newActivationHarness(t)
	seeded := []byte(activationModernPlayer)
	activationSeedCache(t, h, seeded, computePlayerFingerprint(seeded), 20731)

	cipher, err := h.ext.getCachedCipherContext(context.Background(), activationVideoID)
	if err != nil {
		t.Fatalf("getCachedCipherContext: %v", err)
	}
	if h.playerGets != 0 {
		t.Fatalf("coherent warm cache must not fetch, got %d player GETs", h.playerGets)
	}
	if cipher.signatureTimestamp != 20731 {
		t.Fatalf("warm cipher must carry the coherent cached STS 20731, got %d", cipher.signatureTimestamp)
	}
	if !cipher.isModernURLWrapperProtocol() || cipher.urlWrapperName != "jW" {
		t.Fatalf("warm modern cipher must be hydrated from the cached bundle, proto=%v name=%q", cipher.urlWrapperProtocol, cipher.urlWrapperName)
	}
	// Source identity proof (single-load): the returned cipher's bytes are
	// EXACTLY the validated cached bundle, and its hash/STS come from the
	// SAME bytes the validator ran on. This is not a scheduling proof —
	// replacing the file after the return cannot discriminate a legacy
	// double-read — it pins the bytes/hash/STS coherence itself.
	if string(cipher.playerJS) != string(seeded) {
		t.Fatalf("returned cipher must carry the exact validated cached bundle, got %d bytes want %d", len(cipher.playerJS), len(seeded))
	}
	if cipher.playerFingerprint != computePlayerFingerprint(cipher.playerJS) {
		t.Fatalf("cipher fingerprint must be derived from its own carried bytes, fp=%q", cipher.playerFingerprint)
	}
	if findSignatureTimestamp(cipher.playerJS) != cipher.signatureTimestamp {
		t.Fatalf("cipher STS must be derived from its own carried bytes, sts=%d", cipher.signatureTimestamp)
	}
}

func TestActivationMismatchedFingerprintRefetches(t *testing.T) {
	h := newActivationHarness(t)
	activationSeedCache(t, h, []byte(activationModernPlayer), "deadbeef-not-the-js-fingerprint", 20731)

	cipher, err := h.ext.getCachedCipherContext(context.Background(), activationVideoID)
	if err != nil {
		t.Fatalf("getCachedCipherContext: %v", err)
	}
	if h.playerGets != 1 {
		t.Fatalf("expected exactly one refetch GET after fingerprint mismatch, got %d", h.playerGets)
	}
	if cipher.playerFingerprint != computePlayerFingerprint([]byte(activationModernPlayer)) || cipher.signatureTimestamp != 20731 {
		t.Fatalf("refetched cipher must carry the final source identity/STS, fp=%q sts=%d", cipher.playerFingerprint, cipher.signatureTimestamp)
	}
}

func TestActivationValidJSBadSTSMetadataRefetches(t *testing.T) {
	h := newActivationHarness(t)
	activationSeedCache(t, h, []byte(activationModernPlayer), computePlayerFingerprint([]byte(activationModernPlayer)), 9999)

	cipher, err := h.ext.getCachedCipherContext(context.Background(), activationVideoID)
	if err != nil {
		t.Fatalf("getCachedCipherContext: %v", err)
	}
	if h.playerGets != 1 {
		t.Fatalf("expected exactly one refetch GET after STS mismatch, got %d", h.playerGets)
	}
	if cipher.signatureTimestamp != 20731 {
		t.Fatalf("refetched cipher must carry the JS-derived STS 20731, got %d", cipher.signatureTimestamp)
	}
}

func TestActivationOldSampledHashMigratesByRefetch(t *testing.T) {
	h := newActivationHarness(t)
	activationSeedCache(t, h, []byte(activationModernPlayer), "0123456789abcdef", 20731)

	cipher, err := h.ext.getCachedCipherContext(context.Background(), activationVideoID)
	if err != nil {
		t.Fatalf("getCachedCipherContext: %v", err)
	}
	if h.playerGets != 1 {
		t.Fatalf("expected old sampled hash to migrate by one refetch GET, got %d", h.playerGets)
	}
	if len(cipher.PlayerFingerprint()) != 64 {
		t.Fatalf("old-format cache must migrate to the full-content identity, got %q", cipher.PlayerFingerprint())
	}
}

func TestActivationModernBundleMissingSTSLiteralRefetches(t *testing.T) {
	h := newActivationHarness(t)
	// A modern wrapper bundle WITHOUT its own STS literal: the positive
	// cached STS cannot be corroborated and must never be trusted.
	activationSeedCache(t, h, []byte(activationModernPlayerNoSTS()), computePlayerFingerprint([]byte(activationModernPlayerNoSTS())), 20731)

	cipher, err := h.ext.getCachedCipherContext(context.Background(), activationVideoID)
	if err != nil {
		t.Fatalf("getCachedCipherContext: %v", err)
	}
	if h.playerGets != 1 {
		t.Fatalf("expected exactly one refetch GET for an uncorroborated cached STS, got %d", h.playerGets)
	}
	if cipher.signatureTimestamp != 20731 {
		t.Fatalf("refetched cipher must carry the final source STS, got %d", cipher.signatureTimestamp)
	}
}

// activationModernPlayerNoSTS is the modern wrapper bundle WITHOUT any
// signatureTimestamp literal.
func activationModernPlayerNoSTS() []byte {
	return []byte(strings.Replace(activationModernPlayer, "var signatureTimestamp=20731;\n", "", 1))
}

func TestActivationLegacyCacheCompat(t *testing.T) {
	h := newActivationHarness(t)
	h.servedPlayer = []byte(activationLegacyPlayer)
	activationSeedCache(t, h, []byte(activationLegacyPlayer), computePlayerFingerprint([]byte(activationLegacyPlayer)), 12345)

	cipher, err := h.ext.getCachedCipherContext(context.Background(), activationVideoID)
	if err != nil {
		t.Fatalf("getCachedCipherContext: %v", err)
	}
	if h.playerGets != 0 {
		t.Fatalf("coherent legacy warm cache must not fetch, got %d player GETs", h.playerGets)
	}
	if cipher.isModernURLWrapperProtocol() {
		t.Fatalf("legacy wrapper cache must stay on the legacy path, proto=%v", cipher.urlWrapperProtocol)
	}
	if cipher.signatureTimestamp != 12345 {
		t.Fatalf("legacy cache must keep its corroborated STS 12345, got %d", cipher.signatureTimestamp)
	}
}

func TestActivationStrictMalformedQueryErrorsBeforeAnyFetch(t *testing.T) {
	h := newActivationHarness(t)
	stream := &Format{
		URL:      "https://stream.test/videoplayback?n=abc&keep=%ZZ",
		Itag:     141,
		Bitrate:  256000,
		MimeType: "audio/mp4; codecs=\"mp4a.40.2\"",
		Quality:  "tiny",
	}
	_, err := h.ext.getStreamURLContext(context.Background(), activationVideoID, stream)
	if err == nil {
		t.Fatal("malformed query pair must be an explicit error")
	}
	if !strings.Contains(err.Error(), "malformed query") {
		t.Fatalf("expected the sanitized malformed-query error, got %v", err)
	}
	if h.playerGets != 0 || h.apiCalls() != 0 {
		t.Fatalf("no fetch or POST may happen before the query error, gets=%d posts=%d", h.playerGets, h.apiCalls())
	}
}

func TestActivationStrictMalformedQueryRejectsPartialParse(t *testing.T) {
	h := newActivationHarness(t)
	// A valid n alongside a malformed keep pair: the n transformation must
	// never run on a partially parsed URL.
	stream := &Format{
		URL:      "https://stream.test/videoplayback?n=abc&keep=%ZZ",
		Itag:     141,
		Bitrate:  256000,
		MimeType: "audio/mp4; codecs=\"mp4a.40.2\"",
		Quality:  "tiny",
	}
	if _, err := h.ext.unthrottleContext(context.Background(), activationVideoID, stream.URL); err == nil {
		t.Fatal("unthrottle must reject the malformed pair before any cipher work")
	}
	if h.playerGets != 0 || h.apiCalls() != 0 {
		t.Fatalf("no cipher fetch or POST may happen before the query error, gets=%d posts=%d", h.playerGets, h.apiCalls())
	}
}

func TestActivationModernStreamExactOutput(t *testing.T) {
	_ = urlWrapperTestEngine(t)
	h := newActivationHarness(t)
	modern := []byte(activationModernPlayer)
	activationSeedCache(t, h, modern, computePlayerFingerprint(modern), 20731)
	cipher, err := h.ext.getCachedCipherContext(context.Background(), activationVideoID)
	if err != nil {
		t.Fatalf("getCachedCipherContext: %v", err)
	}
	if err := cipher.WarmURLTransformContext(context.Background()); err != nil {
		t.Fatalf("WarmURLTransformContext: %v", err)
	}

	baseURL := "https://rr.example.com/videoplayback?n=abc123&keep=a%20b&keep=c&pot=xy"
	got, err := h.ext.getStreamURLModernContext(context.Background(), cipher, baseURL, "sig", "")
	if err != nil {
		t.Fatalf("getStreamURLModernContext: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("malformed result URL: %v (%q)", err, got)
	}
	q := u.Query()
	if q.Get("n") != "abc123_mod" {
		t.Fatalf("unexpected n: got %q want %q", q.Get("n"), "abc123_mod")
	}
	keeps := q["keep"]
	if q.Get("keep") != "a b" || len(keeps) != 2 || keeps[1] != "c" {
		t.Fatalf("unrelated/valid-escaped/repeated params lost: %q", got)
	}
	if q.Get("alr") != "yes" {
		t.Fatalf("expected alr=yes propagation, got %q", got)
	}
}

func TestActivationHeldPlayerFetchNoPOSTThenFinalSTS(t *testing.T) {
	h := newActivationHarness(t)
	h.playerEntered = make(chan struct{})
	h.playerHold = make(chan struct{})
	var holdOnce sync.Once
	releaseHoldOnce := func() { holdOnce.Do(func() { close(h.playerHold) }) }
	defer releaseHoldOnce()

	type res struct {
		r   *Result
		err error
	}
	ch := make(chan res, 1)
	go func() {
		r, err := h.ext.Extract(activationVideoID)
		ch <- res{r: r, err: err}
	}()

	select {
	case <-h.playerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("player JS GET must enter the transport")
	}
	// The music path must never POST while the current player's STS is
	// unknown: the positive-STS wait binds the API call.
	if h.apiCalls() != 0 {
		t.Fatalf("no player API POST expected while the player JS is held, got %d", h.apiCalls())
	}
	releaseHoldOnce()

	select {
	case out := <-ch:
		if out.err != nil {
			t.Fatalf("extract failed: %v", out.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("extraction hung after the player JS was released")
	}
	if h.apiCalls() != 1 {
		t.Fatalf("expected exactly one player API POST with the final STS, got %d", h.apiCalls())
	}
	// Evidence beyond a POST count: the accepted request must carry the
	// synthetic player's OWN positive signatureTimestamp literal (20731) —
	// never a zero or a mismatched source.
	if sts := h.lastBodySTS(); sts != 20731 {
		t.Fatalf("player API POST must carry the final source STS 20731, got %d", sts)
	}
}

func TestActivationPlayerFetchFailNoPOST(t *testing.T) {
	h := newActivationHarness(t)
	h.playerErr = http.StatusInternalServerError

	_, err := h.ext.Extract(activationVideoID)
	if err == nil {
		t.Fatal("expected the cipher fetch failure to fail extraction")
	}
	if h.apiCalls() != 0 {
		t.Fatalf("no player API POST expected after a cipher fetch failure, got %d", h.apiCalls())
	}
}

func TestActivationCancelDuringCipherWaitPrompt(t *testing.T) {
	h := newActivationHarness(t)
	h.playerEntered = make(chan struct{})
	h.playerHold = make(chan struct{})
	var holdOnce sync.Once
	releaseHoldOnce := func() { holdOnce.Do(func() { close(h.playerHold) }) }
	defer releaseHoldOnce()

	var joinCh <-chan singleflight.Result
	joinConsumed := false

	ctx, cancel := context.WithCancel(context.Background())
	type res struct {
		r   *Result
		err error
	}
	ch := make(chan res, 1)
	go func() {
		r, err := h.ext.ExtractContext(ctx, activationVideoID)
		ch <- res{r: r, err: err}
	}()

	select {
	case <-h.playerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("player JS GET must enter the transport")
	}
	// Positive flight registration while the GET is held: this DoChan
	// attaches to the SAME cipher flight (never a substitute creator).
	// Releasing and awaiting the registered flight below makes the detached
	// fetch's shared-cache write complete BEFORE TempDir cleanup.
	joinCh = h.ext.runtime.cipherGroup.DoChan("shared", func() (any, error) { return nil, nil })
	t.Cleanup(func() {
		releaseHoldOnce()
		if joinCh != nil && !joinConsumed {
			select {
			case <-joinCh:
			case <-time.After(10 * time.Second):
				t.Errorf("held cipher flight did not complete before TempDir cleanup")
			}
		}
	})

	cancel()

	select {
	case out := <-ch:
		if !errors.Is(out.err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", out.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation must be prompt")
	}
	if h.apiCalls() != 0 {
		t.Fatalf("no player API POST expected after cancellation, got %d", h.apiCalls())
	}

	// Await the REGISTERED flight: the canceled extractor returning never
	// means the detached fetch stopped. Release once and wait for the real
	// completion before this test's TempDir removal.
	releaseHoldOnce()
	select {
	case <-joinCh:
		joinConsumed = true
	case <-time.After(10 * time.Second):
		t.Fatal("held cipher flight did not complete after release")
	}
}

// activationModernPlayerSRev is the modern wrapper with a REVERSING sign
// helper, so the exact signature output is the reversed input (alnum input:
// encodeURIComponent is identity and the runner decodes exactly once).
func activationModernPlayerSRev() []byte {
	return []byte(strings.Replace(activationModernPlayer, "Qe=function(x){return x};", "Qe=function(x){return x.split(\"\").reverse().join(\"\")};", 1))
}

func activationLongSig(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	for i := 0; i < 300; i++ {
		sb.WriteString("Af5Hk9LpQ2zXwBvR1")
	}
	return sb.String()[:300]
}

// TestActivationGetStreamURLSignatureOnlyModernExactly pins the modern
// getStreamURLContext contract for a SIGNATURE-ONLY Format (no direct URL):
// the strict SignatureCipher parse drives the modern branch with the decoded
// url/s/sp, and the output carries the exact n transform, the exact reversed
// signature under sp, alr=yes, and the preserved unrelated parameter — with
// zero fetches (the cipher is already installed).
func TestActivationGetStreamURLSignatureOnlyModernExactly(t *testing.T) {
	_ = urlWrapperTestEngine(t)
	h := newActivationHarness(t)
	playerJS := activationModernPlayerSRev()
	seedPath := "/s/player/acttest/player_embed.vflset/en_US/base.js"
	cipher := &Cipher{
		runtime:            h.ext.runtime,
		sigFunctionName:    "jW",
		sigUsesURLWrapper:  true,
		urlWrapperProtocol: true,
		urlWrapperName:     "jW",
		playerJS:           playerJS,
		playerURL:          seedPath,
		playerFingerprint:  computePlayerFingerprint(playerJS),
		signatureTimestamp: 20731,
	}
	h.ext.cipherMu.Lock()
	h.ext.cipher = cipher
	h.ext.cipherMu.Unlock()
	if err := cipher.WarmURLTransformContext(context.Background()); err != nil {
		t.Fatalf("WarmURLTransformContext: %v", err)
	}

	sig := activationLongSig(t)
	stream := &Format{
		SignatureCipher: "url=https%3A%2F%2Frr.example.com%2Fvideoplayback%3Fn%3Dabc123%26keep%3Dkeepme&s=" + sig + "&sp=sig",
		Itag:            141,
		Bitrate:         256000,
		MimeType:        "audio/mp4; codecs=\"mp4a.40.2\"",
		Quality:         "tiny",
	}
	got, err := h.ext.getStreamURLContext(context.Background(), activationVideoID, stream)
	if err != nil {
		t.Fatalf("getStreamURLContext: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("malformed result URL: %v (%q)", err, got)
	}
	q := u.Query()
	if q.Get("n") != "abc123_mod" {
		t.Fatalf("unexpected n: got %q want %q", q.Get("n"), "abc123_mod")
	}
	if q.Get("keep") != "keepme" {
		t.Fatalf("unrelated param lost: %q", got)
	}
	if q.Get("alr") != "yes" {
		t.Fatalf("expected alr=yes propagation, got %q", got)
	}
	refBytes := make([]byte, len(sig))
	for i := 0; i < len(sig); i++ {
		refBytes[i] = sig[len(sig)-1-i]
	}
	if gotSig := q.Get("sig"); gotSig != string(refBytes) {
		t.Fatalf("exact signature under sp mismatch: got %d bytes want %d (reversed reference)", len(gotSig), len(sig))
	}
	if h.playerGets != 0 || h.apiCalls() != 0 {
		t.Fatalf("installed modern cipher must not fetch or POST, gets=%d posts=%d", h.playerGets, h.apiCalls())
	}
}

// TestActivationSharedFetchNeverWritesExtractorState pins that the DETACHED
// shared cipher flight does not write warnings or timings onto ANY
// extractor: A leads the flight (player JS held), A cancels; B (a second
// extractor on the same runtime) receives the completed cipher. The flight
// finisher mutates neither A's nor B's warnings/timings — diagnostics ride
// on the returned cipher and are published by the consuming context only.

func TestActivationSharedFetchNeverWritesExtractorState(t *testing.T) {
	runtime := NewRuntime()
	hA := newActivationHarnessRuntime(t, runtime)
	hB := newActivationHarnessRuntime(t, runtime)
	hA.playerEntered = make(chan struct{})
	hA.playerHold = make(chan struct{})
	var holdOnce sync.Once
	releaseHoldOnce := func() { holdOnce.Do(func() { close(hA.playerHold) }) }
	defer releaseHoldOnce()

	type res struct {
		c   *Cipher
		err error
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	chA := make(chan res, 1)
	go func() {
		c, err := hA.ext.getCachedCipherContext(ctxA, activationVideoID)
		chA <- res{c: c, err: err}
	}()
	select {
	case <-hA.playerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("shared fetch must be in flight with the player JS GET held")
	}

	// Waiter B (same runtime) joins the held flight.
	ctxB, cancelB := context.WithCancel(context.Background())
	chB := make(chan res, 1)
	go func() {
		c, err := hB.ext.getCachedCipherContext(ctxB, activationVideoID)
		chB <- res{c: c, err: err}
	}()
	// Positive flight registration for the held flight: this DoChan attaches
	// to the SAME singleflight (never a second creator). B's individual
	// getter attachment is not otherwise observable without a production
	// hook; the shared result below is the waiters' common completion point.
	observer := runtime.cipherGroup.DoChan("shared", func() (any, error) { return nil, nil })

	// The creator's cancellation stays prompt: it never aborts the shared
	// work the waiter consumes.
	cancelA()
	select {
	case a := <-chA:
		if !errors.Is(a.err, context.Canceled) {
			t.Fatalf("expected caller A to leave with context.Canceled, got %v", a.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("caller A cancellation must be prompt")
	}

	// Release the GET: the shared flight completes; B receives the cached
	// cipher and the observer receives the same flight result. Both awaited
	// before this test returns, so no TempDir cleanup races the flight.
	releaseHoldOnce()
	select {
	case <-observer:
	case <-time.After(10 * time.Second):
		t.Fatal("shared flight did not complete before B received")
	}
	select {
	case b := <-chB:
		if b.err != nil || b.c == nil {
			t.Fatalf("waiter B must receive the completed cipher, err=%v", b.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiter B did not receive the completed cipher")
	}
	_ = cancelB

	// The detached finisher (A's creator path) ran the whole time and still
	// must not have written warnings or timings onto either extractor:
	// diagnostics ride on the returned cipher, published by the consumer.
	if len(hA.ext.warnings) != 0 || len(hB.ext.warnings) != 0 {
		t.Fatalf("shared flight must not write extractor warnings: a=%v b=%v", hA.ext.warnings, hB.ext.warnings)
	}
	if hA.ext.timings.PlayerJSFetchMs != 0 || hB.ext.timings.PlayerJSFetchMs != 0 {
		t.Fatalf("shared flight must not write extractor timings: a=%d b=%d", hA.ext.timings.PlayerJSFetchMs, hB.ext.timings.PlayerJSFetchMs)
	}
}

// TestActivationRefreshCanceledParentStillSchedules pins blocker 4: a
// refresh whose PARENT context was already canceled must still register the
// shared fetch (the caller-local wait context strips the parent's
// cancellation before registration), publish the fresh cipher to cache, and
// complete the registered flight before this test returns.
func TestActivationRefreshCanceledParentStillSchedules(t *testing.T) {
	h := newActivationHarness(t)
	h.playerEntered = make(chan struct{})
	h.playerHold = make(chan struct{})
	var holdOnce sync.Once
	releaseHoldOnce := func() { holdOnce.Do(func() { close(h.playerHold) }) }
	defer releaseHoldOnce()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h.ext.refreshCipherAsync(ctx, activationVideoID)

	select {
	case <-h.playerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled-parent refresh must still enter the shared fetch")
	}
	// observer attaches while the GET is held; release then await completion.
	joinCh := h.ext.runtime.cipherGroup.DoChan("shared", func() (any, error) { return nil, nil })
	releaseHoldOnce()
	select {
	case joined := <-joinCh:
		if joined.Err != nil {
			t.Fatalf("scheduled refresh flight failed: %v", joined.Err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("scheduled refresh flight did not complete")
	}
	// The fresh cipher was published: the shared in-memory cipher cache is
	// populated and the on-disk metadata carries the fetched identity.
	if h.ext.runtime.cipherCache.cipher == nil {
		t.Fatal("refresh must populate the shared in-memory cipher cache")
	}
	cache, err := h.ext.cacheManager.Load()
	if err != nil {
		t.Fatalf("refresh must persist the fresh cipher metadata: %v", err)
	}
	if cache.PlayerFingerprint != computePlayerFingerprint([]byte(activationModernPlayer)) {
		t.Fatalf("refresh must persist the fetched player identity, fp=%q", cache.PlayerFingerprint)
	}
}

// TestActivationSharedFetchCallerCancelLeavesSharedWork pins the
// caller-cancel-safe shared flight: A leads (GET held), a synchronized
// DoChan observer attaches to the SAME flight (never a second creator), A
// cancels promptly, the hold is released, and the single flight completes
// and publishes before this test returns. The cleanup is the bounded release
// guard — it never launches a substitute flight when no entry was seen and
// never double-receives the already-consumed flight result.
func TestActivationSharedFetchCallerCancelLeavesSharedWork(t *testing.T) {
	h := newActivationHarness(t)
	h.playerEntered = make(chan struct{})
	h.playerHold = make(chan struct{})
	var holdOnce sync.Once
	releaseHoldOnce := func() { holdOnce.Do(func() { close(h.playerHold) }) }
	defer releaseHoldOnce()

	var joinCh <-chan singleflight.Result
	joinConsumed := false

	type res struct {
		c   *Cipher
		err error
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	chA := make(chan res, 1)
	go func() {
		c, err := h.ext.getCachedCipherContext(ctxA, activationVideoID)
		chA <- res{c: c, err: err}
	}()

	select {
	case <-h.playerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("shared fetch must be in flight with the player JS GET held")
	}

	// Positive shared-flight registration: while the GET is held, the runtime
	// singleflight holds EXACTLY ONE flight for the shared key; this
	// synchronous DoChan attaches a waiter to that same flight (never a
	// second creator). The joined result also awaits real shared completion
	// before any TempDir cleanup.
	joinCh = h.ext.runtime.cipherGroup.DoChan("shared", func() (any, error) { return nil, nil })
	t.Cleanup(func() {
		releaseHoldOnce()
		if joinCh != nil && !joinConsumed {
			select {
			case <-joinCh:
			case <-time.After(10 * time.Second):
				t.Errorf("shared flight did not complete before TempDir cleanup")
			}
		}
	})

	cancelA()
	select {
	case a := <-chA:
		if !errors.Is(a.err, context.Canceled) {
			t.Fatalf("expected caller A to leave with context.Canceled, got %v", a.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("caller A cancellation must be prompt")
	}

	releaseHoldOnce()
	select {
	case joined := <-joinCh:
		joinConsumed = true
		if joined.Err != nil {
			t.Fatalf("shared flight must complete after the caller left: %v", joined.Err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shared flight did not complete after the hold was released")
	}
	if h.playerGets != 1 {
		t.Fatalf("exactly ONE shared player fetch expected, got %d", h.playerGets)
	}
}

// TestActivationMainConsumerAbsorbsCipherWarningsOnce checks that creation
// warnings stay on the cipher until the main extraction goroutine consumes it.
func TestActivationMainConsumerAbsorbsCipherWarningsOnce(t *testing.T) {
	h := newActivationHarness(t)
	// The cache metadata describes a DIFFERENT fingerprint than the served
	// player: a direct fresh fetch (as the shared flight runs it) detects the
	// change and records the creation warning on the returned cipher.
	activationSeedCache(t, h, []byte(activationModernPlayer), "deadbeef-not-the-js-fingerprint", 20731)

	cipher, err := h.ext.fetchAndCacheCipher(activationVideoID)
	if err != nil {
		t.Fatalf("fetchAndCacheCipher: %v", err)
	}
	found := false
	for _, w := range cipher.warnings {
		if strings.Contains(w, "player fingerprint changed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("creation warning must ride on the returned cipher, got %v", cipher.warnings)
	}
	// The flight must not publish onto the extractor itself.
	if len(h.ext.warnings) != 0 {
		t.Fatalf("flight must not write extractor warnings, got %v", h.ext.warnings)
	}

	// The warned cipher is now in the shared in-memory cache: the main
	// consumer of the next Extract absorbs it exactly once.
	res, err := h.ext.Extract(activationVideoID)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	if res == nil || res.URL == "" {
		t.Fatal("expected a result URL")
	}
	if h.apiCalls() != 1 {
		t.Fatalf("expected exactly one player API POST, got %d", h.apiCalls())
	}
	count := 0
	for _, w := range h.ext.warnings {
		if strings.Contains(w, "player fingerprint changed") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("main consumer must absorb the fingerprint-change warning exactly once, got %d in %v", count, h.ext.warnings)
	}
}
