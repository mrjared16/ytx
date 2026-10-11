package ytx

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/mrjared16/ytx/internal/jsengine"
)

// urlWrapperTestEngine returns "bun" or "node" when a subprocess runtime is
// available, otherwise skips the calling test.
func urlWrapperTestEngine(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("bun"); err == nil && p != "" {
		return "bun"
	}
	if p, err := exec.LookPath("node"); err == nil && p != "" {
		return "node"
	}
	t.Skip("no bun/node runtime available")
	return ""
}

// urlWrapperTestPlayer renders a synthetic embed-style player bundle: the
// recognized wrapper `jW` plus three decoy ALR markers (a g.zc-style object
// setter, a this.ZD spaced marker inside a 2-arg constructor, and a 2-arg
// redirector). mode controls the n getter:
//
//	ok:     returns n + "_mod"
//	noalr:  leaves the alr value unset
//	emptyn: returns "" for n
//	unchanged: returns the input n untouched (must be rejected upstream)
//	srev:   reverses the (encoded) signature before signing (exact reference)
//	throw:  throws on every call (healthy runner, failing call)
func urlWrapperTestPlayer(mode string) []byte {
	return []byte(fmt.Sprintf(`
(function(_yt_player){
var g={};
var signatureTimestamp=20726;
var MODE=%q;
g.$I=function(s,already){var _=this;_.u=new URL(s);_.m={};_.set=function(k,v){_.m[k]=String(v)};_.get=function(k){if(k==="n"&&_.u.searchParams.has("n")&&_.m.n===undefined){var raw=_.u.searchParams.get("n");if(MODE==="emptyn"){_.m.n=""}else if(MODE==="unchanged"){_.m.n=raw}else{_.m.n=raw+"_mod"}}if(k==="alr"&&MODE==="noalr"){return undefined}return _.m[k]}};
var S=[];S[21]="set";
jW=function(T,M,q){M=M===void 0?"":M;q=q===void 0?"":q;if(MODE==="throw"){throw new Error("boom")}T=new g.$I(T,!0);T.set("alr","yes");q&&(q=Qe(q),T[S[21]](M,q));return T};
Qe=function(x){if(MODE==="srev"){return x.split("").reverse().join("")}return x};
g.zc=function(T,M,q){var Q=new g.$I(M,!0);Q.get("alr")||Q.set("alr","yes");T.K&&Q.set("x",1);return Q};
Veg=function(P,O){this.ZD=new g.$I(P,!0);this.ZD.set("alr", "yes");return this};
yp=function(T,M){var q=new g.$I(T,!0);q.set("alr","yes");q.set("id","9");return q};
})(_yt_player);
`, mode))
}

func TestURLWrapperFindCandidatesES6DefaultParamVariants(t *testing.T) {
	const wrap = `%s=function(K,q%s,N%s){K=new g.jD(K,!0);K.set("alr","yes");N&&(N=Jn(25,2163,Wq(2,3394,N)),K[F[14]](q,QE(6,223,N)));return K}`
	cases := []struct {
		name string
		js   string
		want string
	}{
		{"double-quoted-empty", fmt.Sprintf(wrap, "u8", `=""`, `=""`), "u8"},
		{"single-quoted-empty", fmt.Sprintf(wrap, "u8", `=''`, `=''`), "u8"},
		{"void-zero", fmt.Sprintf(wrap, "u8", "=void 0", "=void 0"), "u8"},
		{"boolean-literals", fmt.Sprintf(wrap, "u8", "=true", "=!0"), "u8"},
		{"plain-params", fmt.Sprintf(wrap, "u8", "", ""), "u8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findURLWrapperCandidate([]byte(tc.js)); got != tc.want {
				t.Fatalf("candidate = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("expression-default-bounded-unsupported", func(t *testing.T) {
		js := fmt.Sprintf(wrap, "u8", `=""`, `=Jn(25,2163,Wq(2,3394,N))`)
		if got := findURLWrapperCandidate([]byte(js)); got != "" {
			t.Fatalf("expression-default header must stay unsupported (bounded matcher), got %q", got)
		}
	})
	t.Run("distinct-default-wrappers-ambiguous", func(t *testing.T) {
		js := fmt.Sprintf(wrap, "u8", `=""`, `=""`) + ";" + fmt.Sprintf(wrap, "az", "=0", "=0")
		if st := urlWrapperDiscover([]byte(js)).status; st != urlWrapperDiscoveryAmbiguous {
			t.Fatalf("expected ambiguous, got %v", st)
		}
	})
}

func TestURLWrapperFindCandidatesEmbedStyle(t *testing.T) {
	candidates := findURLWrapperCandidates(urlWrapperTestPlayer("ok"))
	if len(candidates) != 1 {
		t.Fatalf("expected exactly one candidate, got %#v", candidates)
	}
	if candidates[0] != "jW" {
		t.Fatalf("unexpected wrapper name: got %q want %q", candidates[0], "jW")
	}
}

func TestURLWrapperFindCandidatesWatchStyle(t *testing.T) {
	js := []byte(`
(function(_yt_player){
var g={};
pm=function(S,x,Q){x=x===void 0?"":x;Q=Q===void 0?"":Q;S=new g.jJ(S,!0);S.set("alr","yes");Q&&(Q=wh(9,7481,r5(6,5833,Q)),S[SE[22]](x,ZU(15,2967,Q)));return S};
wh=function(a,b,c){return c};
g.h6=function(S,x,Q){var I=new g.jJ(x,!0);I.get("alr")||I.set("alr","yes");S.U&&I.set("x",1);return I};
Veg=function(P,O){this.ZD=new g.jJ(P,!0);this.ZD.set("alr", "yes");return this};
oSz=function(S,x){var Q=new g.jJ(S,!0);Q.set("alr","yes");Q.set("id","7");return Q};
})(_yt_player);
`)
	candidates := findURLWrapperCandidates(js)
	if len(candidates) != 1 {
		t.Fatalf("expected exactly one candidate, got %#v", candidates)
	}
	if candidates[0] != "pm" {
		t.Fatalf("unexpected wrapper name: got %q want %q", candidates[0], "pm")
	}
}

func TestURLWrapperFindCandidatesRenamedArgsAndSingleQuotes(t *testing.T) {
	js := []byte(`
(function(_yt_player){
var g={};
qz=function(a,b,c){c=c===void 0?"":c;a=new g.zz(a,!0);a.set('alr','yes');c&&(c=hh(c),a[K1[3]](b,c));return a};
hh=function(x){return x};
g.zc=function(a,b,c){var Q=new g.zz(b,!0);Q.get('alr')||Q.set('alr','yes');a.K&&Q.set('x',1);return Q};
Veg=function(P,O){this.ZD=new g.zz(P,!0);this.ZD.set('alr', 'yes');return this};
yp=function(a,b){var q=new g.zz(a,!0);q.set('alr','yes');q.set('id','9');return q};
})(_yt_player);
`)
	candidates := findURLWrapperCandidates(js)
	if len(candidates) != 1 {
		t.Fatalf("expected exactly one candidate, got %#v", candidates)
	}
	if candidates[0] != "qz" {
		t.Fatalf("unexpected wrapper name: got %q want %q", candidates[0], "qz")
	}
}

func TestURLWrapperFindCandidatesRejectsDecoyOnly(t *testing.T) {
	js := []byte(`
(function(_yt_player){
var g={};
g.zc=function(T,M,q){var Q=new g.$I(M,!0);Q.get("alr")||Q.set("alr","yes");T.K&&Q.set("x",1);return Q};
Veg=function(P,O){this.ZD=new g.$I(P,!0);this.ZD.set("alr", "yes");return this};
yp=function(T,M){var q=new g.$I(T,!0);q.set("alr","yes");q.set("id","9");return q};
})(_yt_player);
`)
	if got := findURLWrapperCandidates(js); got != nil {
		t.Fatalf("expected no candidates from decoys, got %#v", got)
	}
}

func TestURLWrapperFindCandidatesRejectsAmbiguous(t *testing.T) {
	js := []byte(`
(function(_yt_player){
var g={};
jW=function(T,M,q){M=M===void 0?"":M;q=q===void 0?"":q;T=new g.$I(T,!0);T.set("alr","yes");q&&(q=Qe(q),T[S[21]](M,q));return T};
Qe=function(x){return x};
var signatureTimestamp=20726;
xx=function(a,b,c){c=c===void 0?"":c;a=new g.zz(a,!0);a.set('alr','yes');c&&(c=HH(c),a[K1[3]](b,c));return a};
HH=function(x){return x};
g.zc=function(T,M,q){var Q=new g.$I(M,!0);Q.get("alr")||Q.set("alr","yes");T.K&&Q.set("x",1);return Q};
})(_yt_player);
`)
	if got := findURLWrapperCandidates(js); got != nil {
		t.Fatalf("expected ambiguity to be rejected, got %#v", got)
	}
}

func TestURLWrapperFindCandidatesRejectsMalformed(t *testing.T) {
	// Not rebuilt as new g.<class>(first, true).
	noNewG := []byte(fmt.Sprintf(`
(function(_yt_player){
var g={};
jW=function(T,M,q){M=M===void 0?"":M;q=q===void 0?"":q;T=new EX(T,!0);T.set("alr","yes");q&&(q=Qe(q),T[S[21]](M,q));return T};
Qe=function(x){return x};
g.zc=function(T,M,q){var Q=new EX(M,!0);Q.get("alr")||Q.set("alr","yes");T.K&&Q.set("x",1);return Q};
Veg=function(P,O){this.ZD=new EX(P,!0);this.ZD.set("alr", "yes");return this};
yp=function(T,M){var q=new EX(T,!0);q.set("alr","yes");q.set("id","9");return q};
})(_yt_player);
`))
	if got := findURLWrapperCandidates(noNewG); got != nil {
		t.Fatalf("expected malformed wrapper (no g.class rebuild) to be rejected, got %#v", got)
	}

	// Missing return of the same object.
	noReturn := []byte(`
(function(_yt_player){
var g={};
jW=function(T,M,q){M=M===void 0?"":M;q=q===void 0?"":q;T=new g.$I(T,!0);T.set("alr","yes");q&&(q=Qe(q),T[S[21]](M,q))};
Qe=function(x){return x};
g.zc=function(T,M,q){var Q=new g.$I(M,!0);Q.get("alr")||Q.set("alr","yes");T.K&&Q.set("x",1);return Q};
Veg=function(P,O){this.ZD=new g.$I(P,!0);this.ZD.set("alr", "yes");return this};
yp=function(T,M){var q=new g.$I(T,!0);q.set("alr","yes");q.set("id","9");return q};
})(_yt_player);
`)
	if got := findURLWrapperCandidates(noReturn); got != nil {
		t.Fatalf("expected malformed wrapper (no return) to be rejected, got %#v", got)
	}

	// Arity is not exactly 3.
	arity := []byte(`
(function(_yt_player){
var g={};
jW=function(T,M,q,Z){M=M===void 0?"":M;q=q===void 0?"":q;Z=Z===void 0?"":Z;T=new g.$I(T,!0);T.set("alr","yes");q&&(q=Qe(q),T[S[21]](M,q));return T};
Qe=function(x){return x};
g.zc=function(T,M,q){var Q=new g.$I(M,!0);Q.get("alr")||Q.set("alr","yes");T.K&&Q.set("x",1);return Q};
Veg=function(P,O){this.ZD=new g.$I(P,!0);this.ZD.set("alr", "yes");return this};
yp=function(T,M){var q=new g.$I(T,!0);q.set("alr","yes");q.set("id","9");return q};
})(_yt_player);
`)
	if got := findURLWrapperCandidates(arity); got != nil {
		t.Fatalf("expected malformed wrapper (arity 4) to be rejected, got %#v", got)
	}
}

func TestURLWrapperFindCandidatesSingleMarkerWrapper(t *testing.T) {
	// A valid wrapper does not need decoys: a single marker suffices.
	js := []byte(`um=function(R,w,p){p=p===void 0?"":p;R=new g.qJ(R,!0);R.set("alr","yes");p&&(p=X9(p),R[K1[0]](w,p));return R};`)
	candidates := findURLWrapperCandidates(js)
	if len(candidates) != 1 {
		t.Fatalf("expected single-marker wrapper to be recognized, got %#v", candidates)
	}
	if candidates[0] != "um" {
		t.Fatalf("unexpected wrapper name: got %q want %q", candidates[0], "um")
	}
}

func TestURLWrapperDiscoverDistinguishesStates(t *testing.T) {
	found := urlWrapperDiscover(urlWrapperTestPlayer("ok"))
	if found.status != urlWrapperDiscoveryFound || found.name != "jW" {
		t.Fatalf("expected found jW, got status=%d name=%q", found.status, found.name)
	}

	absent := []byte(`
(function(_yt_player){
var g={};
g.zc=function(T,M,q){var Q=new g.$I(M,!0);Q.get("alr")||Q.set("alr","yes");T.K&&Q.set("x",1);return Q};
yp=function(T,M){var q=new g.$I(T,!0);q.set("alr","yes");q.set("id","9");return q};
})();
`)
	if got := urlWrapperDiscover(absent); got.status != urlWrapperDiscoveryAbsent {
		t.Fatalf("expected absent, got status=%d name=%q", got.status, got.name)
	}

	amb := []byte(`
(function(_yt_player){
var g={};
jW=function(T,M,q){M=M===void 0?"":M;q=q===void 0?"":q;T=new g.$I(T,!0);T.set("alr","yes");q&&(q=Qe(q),T[S[21]](M,q));return T};
Qe=function(x){return x};
var signatureTimestamp=20726;
xx=function(a,b,c){c=c===void 0?"":c;a=new g.zz(a,!0);a.set('alr','yes');c&&(c=HH(c),a[K1[3]](b,c));return a};
HH=function(x){return x};
g.zc=function(T,M,q){var Q=new g.$I(M,!0);Q.get("alr")||Q.set("alr","yes");T.K&&Q.set("x",1);return Q};
})(_yt_player);
`)
	if got := urlWrapperDiscover(amb); got.status != urlWrapperDiscoveryAmbiguous {
		t.Fatalf("expected ambiguous, got status=%d name=%q", got.status, got.name)
	}
}

func TestURLWrapperTruncatedOrMalformedTailNoPanic(t *testing.T) {
	// The enclosing close brace lies beyond the tail scan window: the body
	// is incomplete and must be rejected, never reported, and never panic.
	var sb strings.Builder
	for i := 0; i < 2500; i++ {
		sb.WriteString(" ")
	}
	pad := sb.String()
	longBody := []byte(`um=function(R,w,p){p=p===void 0?"":p;R=new g.qJ(R,!0);R.set("alr","yes");p&&(p=X9(p),` + pad + `R[K1[0]](w,p));return R};`)
	if got := findURLWrapperCandidates(longBody); got != nil {
		t.Fatalf("expected truncated body to be rejected, got %#v", got)
	}

	// Unterminated string literal in the tail: must be rejected without any
	// out-of-range access.
	unterminated := []byte(`um=function(R,w,p){p=p===void 0?"":p;R=new g.qJ(R,!0);R.set("alr","yes");p&&("` + `abc;return R};`)
	if got := findURLWrapperCandidates(unterminated); got != nil {
		t.Fatalf("expected unterminated string to be rejected, got %#v", got)
	}

	// Unterminated quote at the very end of the source.
	atEnd := []byte(`um=function(R,w,p){R=new g.qJ(R,!0);R.set("alr","yes");p&&("`)
	if got := findURLWrapperCandidates(atEnd); got != nil {
		t.Fatalf("expected trailing unterminated quote to be rejected, got %#v", got)
	}
}

func TestExposeURLWrapperSourceInsertsExposure(t *testing.T) {
	raw := `var _yt_player=globalThis;(function(){var jW=function(T,M,q){return T};})()` + "   \n\n" + `})(_yt_player);`
	exposed, err := exposeURLWrapperSource([]byte(raw), "jW")
	if err != nil {
		t.Fatalf("exposeURLWrapperSource: %v", err)
	}
	const suffix = `})(_yt_player);`
	if !testHasSuffix(exposed, suffix) {
		t.Fatalf("exposed source lost the IIFE suffix: %q", exposed)
	}
	if !testHasSuffix(exposed, `_exposed['jW']=jW;`+suffix) {
		t.Fatalf("exposure not inserted immediately before the suffix: %q", exposed)
	}
	if exposed[len(exposed)-1] != ';' {
		t.Fatalf("trailing whitespace was not trimmed: %q", exposed)
	}
}

// testHasSuffix is a tiny test-local suffix check.
func testHasSuffix(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix
}

func TestExposeURLWrapperSourceRejectsBadInput(t *testing.T) {
	if _, err := exposeURLWrapperSource([]byte(`var x=1;`), "jW"); err == nil {
		t.Fatal("expected missing IIFE suffix to be rejected")
	}
	if _, err := exposeURLWrapperSource([]byte(`
(function(){var jW=function(T,M,q){return T};})()`+`})(_yt_player);`), `bad"name`); err == nil {
		t.Fatal("expected invalid wrapper name to be rejected")
	}
	if _, err := exposeURLWrapperSource(nil, "jW"); err == nil {
		t.Fatal("expected empty player source to be rejected")
	}
}

func TestURLWrapperRunnerCacheKeyIsolation(t *testing.T) {
	a := urlWrapperRunnerCacheKey("/s/player/a/base.js", "fp-a", "jW")
	b := urlWrapperRunnerCacheKey("/s/player/a/base.js", "fp-a", "pm")
	c := urlWrapperRunnerCacheKey("/s/player/b/base.js", "fp-a", "jW")
	d := urlWrapperRunnerCacheKey("", "fp-a", "jW")
	f := urlWrapperRunnerCacheKey("/s/player/a/base.js", "fp-b", "jW")
	if a == b || a == c || a == d || a == f {
		t.Fatalf("expected distinct cache keys (incl. same URL, different fingerprint), got %q %q %q %q %q", a, b, c, d, f)
	}
	if a != urlWrapperRunnerCacheKey("/s/player/a/base.js", "fp-a", "jW") {
		t.Fatalf("expected stable cache key, got %q", a)
	}
}

// testCachedEngineHas checks whether the runtime engine cache holds a key.
func testCachedEngineHas(runtime *Runtime, key string) bool {
	runtime.engineCache.mu.Lock()
	defer runtime.engineCache.mu.Unlock()
	if _, ok := runtime.engineCache.cachedEngines[key]; ok {
		return true
	}
	return false
}

func TestURLWrapperClosedRunnerSelfHeals(t *testing.T) {
	_ = urlWrapperTestEngine(t)
	runtime := NewRuntime()
	t.Cleanup(func() { runtime.CloseCachedEngine() })
	raw := urlWrapperTestPlayer("ok")
	const playerURL = "/s/player/urlwtest/base.js"
	const fp = "urlwtest-fp"
	key := urlWrapperRunnerCacheKey(playerURL, fp, "jW")

	cipher := &Cipher{
		runtime:            runtime,
		sigFunctionName:    "jW",
		sigUsesURLWrapper:  true,
		urlWrapperProtocol: true,
		urlWrapperName:     "jW",
		playerJS:           raw,
		playerURL:          playerURL,
		playerFingerprint:  fp,
	}

	r1, err := runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
	if err != nil {
		t.Fatalf("GetURLWrapperRunner: %v", err)
	}

	// Terminal failure: the runner is closed while still cached.
	r1.Close()
	if !r1.IsClosed() {
		t.Fatal("expected closed runner")
	}
	if !testCachedEngineHas(runtime, key) {
		t.Fatal("precondition: closed runner must still be cached")
	}

	// The next call detects the closed cached instance, evicts it, and
	// rebuilds a fresh runner — the call succeeds instead of reusing a dead
	// subprocess.
	res, err := cipher.TransformURLContext(context.Background(), "https://rr.example.com/videoplayback?n=abc123", "sig", "")
	if err != nil {
		t.Fatalf("expected self-healing call to succeed: %v", err)
	}
	if res.N != "abc123_mod" {
		t.Fatalf("unexpected n: got %q want %q", res.N, "abc123_mod")
	}

	r2, err := runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
	if err != nil {
		t.Fatalf("GetURLWrapperRunner: %v", err)
	}
	if r2 == nil || r2 == r1 || r2.IsClosed() {
		t.Fatal("expected a fresh live runner to replace the closed one")
	}
}

func TestURLWrapperHealthyRunnerNotEvictedOnCallError(t *testing.T) {
	_ = urlWrapperTestEngine(t)
	runtime := NewRuntime()
	t.Cleanup(func() { runtime.CloseCachedEngine() })
	raw := urlWrapperTestPlayer("throw")
	const playerURL = "/s/player/urlwtest/base.js"
	const fp = "urlwtest-fp"
	key := urlWrapperRunnerCacheKey(playerURL, fp, "jW")

	cipher := &Cipher{
		runtime:            runtime,
		sigFunctionName:    "jW",
		sigUsesURLWrapper:  true,
		urlWrapperProtocol: true,
		urlWrapperName:     "jW",
		playerJS:           raw,
		playerURL:          playerURL,
		playerFingerprint:  fp,
	}

	r1, err := runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
	if err != nil {
		t.Fatalf("GetURLWrapperRunner: %v", err)
	}

	_, err = cipher.TransformURLContext(context.Background(), "https://rr.example.com/videoplayback?n=abc123", "sig", "")
	if err == nil {
		t.Fatal("expected wrapper call to fail")
	}
	if !testCachedEngineHas(runtime, key) {
		t.Fatal("expected the healthy runner to stay cached after a call error")
	}
	r2, err := runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
	if err != nil {
		t.Fatalf("GetURLWrapperRunner: %v", err)
	}
	if r1 != r2 {
		t.Fatal("expected the same healthy runner to be reused")
	}
}

func TestURLWrapperCanceledCallerLeavesNoCreation(t *testing.T) {
	_ = urlWrapperTestEngine(t)
	runtime := NewRuntime()
	t.Cleanup(func() { runtime.CloseCachedEngine() })
	raw := urlWrapperTestPlayer("ok")
	const playerURL = "/s/player/canceled/base.js"
	const fp = "fp-c"
	key := urlWrapperRunnerCacheKey(playerURL, fp, "jW")

	// A canceled caller — the would-be creator — leaves promptly and must
	// not register any creation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner, err := runtime.GetURLWrapperRunner(ctx, raw, playerURL, fp, "jW")
	if err == nil || runner != nil {
		t.Fatalf("expected canceled caller to return an error immediately, got runner=%v err=%v", runner, err)
	}
	if testCachedEngineHas(runtime, key) {
		t.Fatal("canceled caller must not spawn a creation")
	}

	// The next healthy caller starts a fresh flight and succeeds (a canceled
	// "leader" must never contaminate later creators).
	runner, err = runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
	if err != nil {
		t.Fatalf("healthy caller after canceled leader: %v", err)
	}
	if runner == nil || runner.IsClosed() {
		t.Fatal("expected a live runner from the healthy caller")
	}
	if !testCachedEngineHas(runtime, key) {
		t.Fatal("expected the healthy caller's creation to be published")
	}
}

// urlWrapperBarrierRuntime wires the private creation gate/signal hooks and
// returns the (signal, gate) channels for deterministic interleaving tests.
func urlWrapperBarrierRuntime(t *testing.T, runtime *Runtime) (chan int, chan int) {
	t.Helper()
	signal := make(chan int, 4)
	gate := make(chan int, 4)
	runtime.urlWrapperTestSignal = signal
	runtime.urlWrapperTestGate = gate
	return signal, gate
}

func TestURLWrapperPostCloseNewFlightBeforeOldCompletes(t *testing.T) {
	_ = urlWrapperTestEngine(t)
	runtime := NewRuntime()
	t.Cleanup(func() { runtime.CloseCachedEngine() })
	raw := urlWrapperTestPlayer("ok")
	signal, gate := urlWrapperBarrierRuntime(t, runtime)
	const playerURL = "/s/player/barrier2/base.js"
	const fp = "fp-b2"
	key := urlWrapperRunnerCacheKey(playerURL, fp, "jW")

	type cres struct {
		runner *jsengine.SubprocessRunner
		err    error
	}

	// Flight 1: pre-close caller held at the creation gate.
	chA := make(chan cres, 1)
	go func() {
		runner, err := runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
		chA <- cres{runner: runner, err: err}
	}()
	<-signal // flight 1 creation reached the gate

	// Close the key (nothing cached yet): flight 1 becomes retired.
	runtime.CloseCachedEngineKey(key)

	// Flight 2: post-close caller must derive a NEW flight key and start its
	// own creation instead of joining the old, now-retired flight.
	chC := make(chan cres, 1)
	go func() {
		runner, err := runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
		chC <- cres{runner: runner, err: err}
	}()
	<-signal // flight 2 creation reached the gate

	// Release both creations: old flight retires (its runner is closed, its
	// caller gets an explicit error); new flight publishes its runner.
	gate <- 1
	gate <- 1

	a := <-chA
	c := <-chC
	if a.err == nil || a.runner != nil {
		t.Fatalf("expected the old flight to retire with an explicit error, got runner=%v err=%v", a.runner, a.err)
	}
	if c.err != nil || c.runner == nil {
		t.Fatalf("expected the new flight to publish a runner: %v", c.err)
	}
	if c.runner.IsClosed() {
		t.Fatal("published runner must be live")
	}
	if !testCachedEngineHas(runtime, key) {
		t.Fatal("expected exactly one live owner in the cache")
	}
	if out, err := c.runner.TransformURLContext(context.Background(), "https://rr.example.com/videoplayback?n=abc123", "sig", ""); err != nil {
		t.Fatalf("published runner must be usable: %v", err)
	} else if out.N != "abc123_mod" {
		t.Fatalf("unexpected n from published runner: got %q want %q", out.N, "abc123_mod")
	}
}

func TestURLWrapperLateCallerAfterPublishReusesRunner(t *testing.T) {
	_ = urlWrapperTestEngine(t)
	runtime := NewRuntime()
	t.Cleanup(func() { runtime.CloseCachedEngine() })
	raw := urlWrapperTestPlayer("ok")
	signal, gate := urlWrapperBarrierRuntime(t, runtime)
	const playerURL = "/s/player/barrier3/base.js"
	const fp = "fp-b3"
	key := urlWrapperRunnerCacheKey(playerURL, fp, "jW")

	type cres struct {
		runner *jsengine.SubprocessRunner
		err    error
	}
	ch := make(chan cres, 1)
	go func() {
		runner, err := runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
		ch <- cres{runner: runner, err: err}
	}()
	<-signal
	gate <- 1 // let the creation publish
	a := <-ch
	if a.err != nil || a.runner == nil {
		t.Fatalf("first caller failed: %v", a.err)
	}

	// A caller arriving AFTER publication must hit the live cache entry
	// (atomic recheck) and reuse the same runner — never a duplicate.
	b, err := runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
	if err != nil {
		t.Fatalf("late caller: %v", err)
	}
	if b == nil || b != a.runner || b.IsClosed() {
		t.Fatal("expected the late caller to reuse the live runner instance")
	}
	if !testCachedEngineHas(runtime, key) {
		t.Fatal("expected the single published owner to stay cached")
	}
}

func TestURLWrapperEvictClosesSameInstance(t *testing.T) {
	_ = urlWrapperTestEngine(t)
	runtime := NewRuntime()
	t.Cleanup(func() { runtime.CloseCachedEngine() })
	raw := urlWrapperTestPlayer("ok")
	const playerURL = "/s/player/evict/base.js"
	const fp = "fp-e"
	key := urlWrapperRunnerCacheKey(playerURL, fp, "jW")

	r1, err := runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
	if err != nil {
		t.Fatalf("GetURLWrapperRunner: %v", err)
	}
	if r1.IsClosed() {
		t.Fatal("runner must start live")
	}
	if !testCachedEngineHas(runtime, key) {
		t.Fatal("precondition: runner cached")
	}

	// Eviction closes the SAME instance (releasing its unique temp dir) and
	// removes exactly that entry.
	runtime.EvictURLWrapperRunner(key, r1)
	if !r1.IsClosed() {
		t.Fatal("evicted same instance must be closed")
	}
	if testCachedEngineHas(runtime, key) {
		t.Fatal("evicted instance must leave the cache")
	}

	// A later eviction of the STALE instance must not remove a replacement.
	r2, err := runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
	if err != nil {
		t.Fatalf("replacement: %v", err)
	}
	if r2 == nil || r2 == r1 || r2.IsClosed() {
		t.Fatal("expected a fresh live replacement")
	}
	runtime.EvictURLWrapperRunner(key, r1) // stale instance
	if !testCachedEngineHas(runtime, key) {
		t.Fatal("evicting a stale instance must preserve the replacement")
	}
	runtime.EvictURLWrapperRunner(key, r2)
	if testCachedEngineHas(runtime, key) {
		t.Fatal("evicting the current instance must remove it")
	}
}

func TestURLWrapperConcurrentCallersShareRunner(t *testing.T) {
	_ = urlWrapperTestEngine(t)
	runtime := NewRuntime()
	t.Cleanup(func() { runtime.CloseCachedEngine() })
	raw := urlWrapperTestPlayer("ok")
	const playerURL = "/s/player/conc/base.js"
	const fp = "fp-c2"

	type rr struct {
		runner *jsengine.SubprocessRunner
		err    error
	}
	ch := make(chan rr, 2)
	for i := 0; i < 2; i++ {
		go func() {
			runner, err := runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
			ch <- rr{runner: runner, err: err}
		}()
	}

	a := <-ch
	b := <-ch
	if a.err != nil || b.err != nil {
		t.Fatalf("concurrent GetURLWrapperRunner failed: %v / %v", a.err, b.err)
	}
	if a.runner == nil || b.runner == nil {
		t.Fatal("expected both concurrent callers to receive a runner")
	}
	if a.runner != b.runner {
		t.Fatal("expected concurrent callers to share the same runner instance")
	}

	c, err := runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
	if err != nil {
		t.Fatalf("third call failed: %v", err)
	}
	if c == nil || c != a.runner {
		t.Fatal("expected the third call to hit the cache")
	}
}

func TestURLWrapperCloseThenGetPositiveSync(t *testing.T) {
	_ = urlWrapperTestEngine(t)
	runtime := NewRuntime()
	t.Cleanup(func() { runtime.CloseCachedEngine() })
	raw := urlWrapperTestPlayer("ok")
	const playerURL = "/s/player/sync/base.js"
	const fp = "fp-s"

	r1, err := runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
	if err != nil {
		t.Fatalf("GetURLWrapperRunner: %v", err)
	}
	runtime.CloseCachedEngine()
	if !r1.IsClosed() {
		t.Fatal("expected CloseCachedEngine to terminate the runner")
	}

	r2, err := runtime.GetURLWrapperRunner(context.Background(), raw, playerURL, fp, "jW")
	if err != nil {
		t.Fatalf("second GetURLWrapperRunner after close: %v", err)
	}
	if r2 == nil || r2.IsClosed() || r2 == r1 {
		t.Fatal("expected a fresh live runner after the cache was closed")
	}
}
