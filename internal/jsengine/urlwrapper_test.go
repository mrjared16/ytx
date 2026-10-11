package jsengine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testRuntime returns a usable subprocess engine key ("bun" or "node") or
// skips the test when no runtime is available. Constructors accept only the
// canonical keys and internally fall back from node to the "nodejs" binary
// name, so a system with only "nodejs" in PATH is reported as "node".
func testRuntime(t *testing.T) string {
	t.Helper()
	for _, e := range []string{"bun", "node"} {
		if p, err := exec.LookPath(e); err == nil && p != "" {
			return e
		}
	}
	if p, err := exec.LookPath("nodejs"); err == nil && p != "" {
		return "node"
	}
	t.Skip("no bun/node runtime available")
	return ""
}

// urlWrapperBundle renders a prepared synthetic URL-wrapper bundle. The
// wrapper records nothing externally: every assertion is enforced inside the
// wrapper (throwing on protocol violations) and observable through the
// returned URLTransformResult values.
//
// mode controls the n getter behavior:
//   - ok:        returns n + "_transformed"
//   - unchanged: returns the original n
//   - w8:        returns a "_w8_" throttled marker
//   - errmarker: returns a value containing "error"
//   - empty:     returns ""
//   - noget:     returns an object without a getter
//   - nonobj:    returns a non-object result
//   - nosig:     omits the signature even when one was requested
//   - emptysig:  returns "" for the signature
//   - hugealr:   returns an alr value over the field size limit
//   - hugethrow: throws with an error message over the frame size limit
//   - throwoncectrl: first call throws an error dominated by control
//     characters (escaped to six bytes each by JSON.stringify); later calls
//     behave like ok
//   - multibyteframe: first call returns a sig/alr pair whose UTF-16 lengths
//     sit exactly at the field cap but whose serialized UTF-8 bytes exceed
//     the frame cap; later calls behave like ok
//   - hang:      blocks forever inside the wrapper
//   - throw:     throws before returning
func urlWrapperBundle(wrapperName, rawSig, mode string) []byte {
	return []byte(fmt.Sprintf(`
var throwOnce = true;
var multibyteOnce = true;
function urlWrapper(streamUrl, sigParam, sig) {
    var u = new URL(streamUrl);
    var hasN = u.searchParams.has('n');
    var hasSig = (typeof sig === 'string') && sig !== '';
    var nCalls = 0, sigCalls = 0, alrCalls = 0;
    var mode = %q;
    if (mode === 'hang') { while (true) {} }
    if (mode === 'throw') { throw new Error('wrapper boom'); }
    if (mode === 'hugethrow') { throw new Error(new Array(200001).join('z')); }
    if (mode === 'throwoncectrl') {
        if (throwOnce) {
            throwOnce = false;
            throw new Error(String.fromCharCode(1).repeat(60000));
        }
    }
    if (mode === 'noget') { return { n: 'ignored' }; }
    if (mode === 'nonobj') { return 42; }
    return {
        get: function (k) {
            if (k === 'n') {
                if (!hasN) { throw new Error('get(n) called without n in URL'); }
                if (++nCalls > 1) { throw new Error('get(n) called more than once'); }
                if (u.searchParams.get('keep') !== 'keepme') { throw new Error('unrelated param lost'); }
                var raw = u.searchParams.get('n');
                if (mode === 'unchanged') { return raw; }
                if (mode === 'w8') { return '_w8_throttled_'; }
                if (mode === 'errmarker') { return 'value_with_error_word'; }
                if (mode === 'empty') { return ''; }
                return raw + '_transformed';
            }
            if (k === sigParam) {
                if (!hasSig) { throw new Error('get(sigParam) called without signature'); }
                if (++sigCalls > 1) { throw new Error('get(sigParam) called more than once'); }
                if (sig !== encodeURIComponent(%q)) { throw new Error('signature not encoded exactly once'); }
                if (mode === 'nosig') { return undefined; }
                if (mode === 'emptysig') { return ''; }
                return sig;
            }
            if (k === 'alr') {
                if (++alrCalls > 1) { throw new Error('get(alr) called more than once'); }
                if (mode === 'hugealr') { return 'x'.repeat(100000); }
                if (mode === 'multibyteframe') {
                    if (multibyteOnce) {
                        multibyteOnce = false;
                        return '\u503C'.repeat(16384);
                    }
                    return 'alr_value';
                }
                return 'alr_value';
            }
            return undefined;
        }
    };
}
_exposed[%q] = urlWrapper;
`, mode, rawSig, wrapperName))
}

// badExposureBundle exposes a non-function under wrapperName.
func badExposureBundle(wrapperName string) []byte {
	return []byte(fmt.Sprintf("function unusedFn() {} _exposed[%q] = 42;\n", wrapperName))
}

func newTestURLWrapper(t *testing.T, engine string, bundle []byte, wrapperName string) *SubprocessRunner {
	t.Helper()
	runner, err := NewURLWrapperRunnerContext(context.Background(), engine, bundle, wrapperName)
	if err != nil {
		t.Fatalf("NewURLWrapperRunnerContext: %v", err)
	}
	t.Cleanup(runner.Close)
	return runner
}

// TestURLWrapperLoadFrameValidation pins the load-path frame contract:
// only a non-empty object carrying success:true is a valid load response.
// null, {}, and {"success":false} would decode into a zero struct with an
// empty error and be mistaken for a successful load, so they are rejected.
func TestURLWrapperLoadFrameValidation(t *testing.T) {
	cases := []struct {
		name    string
		line    string // response bytes
		wantErr bool
	}{
		{name: "validSuccess", line: "{\"success\":true}\n", wantErr: false},
		{name: "jsError", line: "{\"error\":\"boom\"}\n", wantErr: true},
		{name: "nullFrame", line: "null\n", wantErr: true},
		{name: "emptyObject", line: "{}\n", wantErr: true},
		{name: "falseSuccess", line: "{\"success\":false}\n", wantErr: true},
		{name: "malformedJSON", line: "not json\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &SubprocessRunner{
				stdin:     json.NewEncoder(io.Discard),
				urlStdout: bufio.NewReader(bytes.NewBufferString(tc.line)),
				mode:      runnerModeURL,
			}
			err := runner.loadWrapperFileContext(context.Background(), "player.js")
			if tc.wantErr && err == nil {
				t.Fatalf("expected load error for frame %q, got nil", tc.line)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected load error for frame %q: %v", tc.line, err)
			}
		})
	}
}

// TestURLWrapperEmptyStreamURLRejected pins the domain contract: an empty
// stream URL must fail fast before any IPC exchange instead of reaching the
// runner (or, in a live runner, being sent to the wrapper as-is).
func TestURLWrapperEmptyStreamURLRejected(t *testing.T) {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	runner := &SubprocessRunner{
		stdin:     json.NewEncoder(io.Discard),
		urlStdout: bufio.NewReader(bytes.NewBufferString("")),
		mode:      runnerModeURL,
		callGate:  gate,
		cmd:       &exec.Cmd{},
	}
	_, err := runner.TransformURLContext(context.Background(), "", "sig", "")
	if err == nil || !strings.Contains(err.Error(), "empty stream URL") {
		t.Fatalf("expected empty-stream-URL error, got %v", err)
	}
}

func TestURLWrapperTransform(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	const rawSig = "sig%2Fpart x!~"
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, rawSig, "ok"), wrapperName)

	streamURL := "https://rr.example.com/videoplayback?n=aaabbbccc&keep=keepme&pot=xy"
	res, err := runner.TransformURLContext(context.Background(), streamURL, "sig", rawSig)
	if err != nil {
		t.Fatalf("TransformURLContext: %v", err)
	}
	if res.N != "aaabbbccc_transformed" {
		t.Errorf("N = %q, want %q", res.N, "aaabbbccc_transformed")
	}
	if res.Signature != rawSig {
		t.Errorf("Signature = %q, want %q (must be decoded exactly once)", res.Signature, rawSig)
	}
	if res.ALR != "alr_value" {
		t.Errorf("ALR = %q, want %q", res.ALR, "alr_value")
	}
}

func TestURLWrapperAbsentSigAndN(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	const rawSig = "sig%2Fpart x!~"
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, rawSig, "ok"), wrapperName)

	// No n parameter, no signature: wrapper must never see get('n') or
	// get(signatureParam) (the wrapper throws if it does). alr is always read.
	streamURL := "https://rr.example.com/videoplayback?keep=keepme&other=1"
	res, err := runner.TransformURLContext(context.Background(), streamURL, "sig", "")
	if err != nil {
		t.Fatalf("TransformURLContext: %v", err)
	}
	if res.N != "" {
		t.Errorf("N = %q, want empty", res.N)
	}
	if res.Signature != "" {
		t.Errorf("Signature = %q, want empty", res.Signature)
	}
	if res.ALR != "alr_value" {
		t.Errorf("ALR = %q, want %q", res.ALR, "alr_value")
	}
}

func TestURLWrapperEmptyOptionalNUnvalidated(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	// n param present but empty: the returned n is optional and must NOT be
	// validated (a "_w8_" value would be rejected by ValidateNTransformResult).
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "ignored", "w8"), wrapperName)

	streamURL := "https://rr.example.com/videoplayback?n=&keep=keepme"
	res, err := runner.TransformURLContext(context.Background(), streamURL, "sig", "")
	if err != nil {
		t.Fatalf("TransformURLContext: %v", err)
	}
	if res.N != "_w8_throttled_" {
		t.Errorf("N = %q, want %q (empty optional n must not be validated)", res.N, "_w8_throttled_")
	}
}

func TestURLWrapperValidateNRejectsInvalid(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	streamURL := "https://rr.example.com/videoplayback?n=aaabbbccc&keep=keepme"
	cases := []struct {
		name string
		mode string
	}{
		{name: "unchanged", mode: "unchanged"},
		{name: "throttledSentinel", mode: "w8"},
		{name: "errorMarker", mode: "errmarker"},
		{name: "empty", mode: "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "ignored", tc.mode), wrapperName)
			res, err := runner.TransformURLContext(context.Background(), streamURL, "sig", "")
			if err == nil {
				t.Fatalf("expected n validation error, got result %+v", res)
			}
		})
	}
}

func TestURLWrapperMissingGetterAndNonObject(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	streamURL := "https://rr.example.com/videoplayback?n=aaabbbccc"

	t.Run("missingGetter", func(t *testing.T) {
		runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "ignored", "noget"), wrapperName)
		_, err := runner.TransformURLContext(context.Background(), streamURL, "sig", "")
		if err == nil || !strings.Contains(err.Error(), "lacks getter") {
			t.Fatalf("expected explicit lacks-getter error, got %v", err)
		}
	})

	t.Run("nonObject", func(t *testing.T) {
		runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "ignored", "nonobj"), wrapperName)
		_, err := runner.TransformURLContext(context.Background(), streamURL, "sig", "")
		if err == nil || !strings.Contains(err.Error(), "non-object") {
			t.Fatalf("expected explicit non-object error, got %v", err)
		}
	})
}

func TestURLWrapperInvalidExposure(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	_, err := NewURLWrapperRunnerContext(context.Background(), engine, badExposureBundle(wrapperName), wrapperName)
	if err == nil || !strings.Contains(err.Error(), "is not a function") {
		t.Fatalf("expected not-a-function load error, got %v", err)
	}
}

func TestURLWrapperRejectsNonRuntimeEngine(t *testing.T) {
	bundle := urlWrapperBundle("urlWrapper", "sig", "ok")
	for _, engine := range []string{"quickjs", "bogus", ""} {
		_, err := NewURLWrapperRunnerContext(context.Background(), engine, bundle, "urlWrapper")
		if err == nil {
			t.Errorf("engine %q: expected rejection error", engine)
		}
	}
}

func TestURLWrapperCanceledLoad(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := NewURLWrapperRunnerContext(ctx, engine, urlWrapperBundle(wrapperName, "sig", "hang"), wrapperName)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected canceled load error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("canceled load took too long: %v", elapsed)
	}
}

func TestURLWrapperCanceledBlockedCall(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "ignored", "hang"), wrapperName)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runner.TransformURLContext(ctx, "https://rr.example.com/videoplayback?n=aaabbbccc", "sig", "")
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected canceled call error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("canceled call took too long: %v", elapsed)
	}
	// Cancellation is a terminal error: the runner must report closed for
	// pkg cache eviction.
	if !runner.IsClosed() {
		t.Fatal("runner does not report closed after terminal cancellation")
	}

	// Runner is dead after cancellation: Close must not hang, and further
	// calls must fail fast with an explicit error.
	runner.Close()
	_, err = runner.TransformURLContext(context.Background(), "https://rr.example.com/videoplayback?n=aaabbbccc", "sig", "")
	if err == nil || !strings.Contains(err.Error(), "runner is closed") {
		t.Fatalf("expected runner-is-closed error, got %v", err)
	}
}

func TestURLWrapperConcurrentCloseNoDeadlock(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "ignored", "hang"), wrapperName)

	// Long context so only Close can unblock the in-flight call.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	callDone := make(chan error, 1)
	go func() {
		_, err := runner.TransformURLContext(ctx, "https://rr.example.com/videoplayback?n=aaabbbccc", "sig", "")
		callDone <- err
	}()
	time.Sleep(300 * time.Millisecond) // let the call block inside the wrapper

	closeDone := make(chan struct{})
	go func() {
		runner.Close()
		close(closeDone)
	}()

	select {
	case <-closeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Close deadlocked against blocked TransformURLContext")
	}
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("expected in-flight call to error after Close, got nil")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight call did not return after Close")
	}
}

func TestURLWrapperUniqueTempDirCleanup(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"

	a := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "sig", "ok"), wrapperName)
	b := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "sig", "ok"), wrapperName)
	if a.tmpDir == "" || b.tmpDir == "" {
		t.Fatal("expected per-run temp dirs")
	}
	if a.tmpDir == b.tmpDir {
		t.Fatalf("expected unique temp dirs, both %q", a.tmpDir)
	}
	// Save the original path: Close clears r.tmpDir, so stat the saved path.
	aTmpDir := a.tmpDir
	bTmpDir := b.tmpDir
	if _, err := os.Stat(aTmpDir); err != nil {
		t.Fatalf("temp dir missing before Close: %v", err)
	}
	a.Close()
	if _, err := os.Stat(aTmpDir); !os.IsNotExist(err) {
		t.Errorf("temp dir not removed after Close (stat err = %v)", err)
	}
	// b is closed by t.Cleanup; ensure its dir is still present beforehand.
	if _, err := os.Stat(bTmpDir); err != nil {
		t.Fatalf("b temp dir missing before cleanup: %v", err)
	}
}

func TestURLWrapperNodeFallback(t *testing.T) {
	// Requires a real node runtime to symlink as "nodejs"; skip otherwise.
	nodePath, err := exec.LookPath("node")
	if err != nil {
		if nodePath, err = exec.LookPath("nodejs"); err != nil {
			t.Skip("node runtime not available")
		}
	}
	// Resolve the real binary (PATH lookups can return version-manager shims
	// that dispatch on argv[0] and need an unrestricted PATH to work).
	out, err := exec.Command(nodePath, "-e", "console.log(process.execPath)").Output()
	if err != nil {
		t.Skipf("node runtime not runnable: %v", err)
	}
	realNode := strings.TrimSpace(string(out))

	// PATH without "node" (only a "nodejs" symlink) forces the fallback.
	binDir := t.TempDir()
	if err := os.Symlink(realNode, filepath.Join(binDir, "nodejs")); err != nil {
		t.Fatalf("symlink nodejs: %v", err)
	}
	t.Setenv("PATH", binDir)

	const wrapperName = "urlWrapper"
	const rawSig = "sig%2Fpart x!~"
	runner, err := NewURLWrapperRunnerContext(context.Background(), "node", urlWrapperBundle(wrapperName, rawSig, "ok"), wrapperName)
	if err != nil {
		t.Fatalf("NewURLWrapperRunnerContext via nodejs fallback: %v", err)
	}
	defer runner.Close()

	streamURL := "https://rr.example.com/videoplayback?n=aaabbbccc&keep=keepme"
	res, err := runner.TransformURLContext(context.Background(), streamURL, "sig", rawSig)
	if err != nil {
		t.Fatalf("TransformURLContext via nodejs fallback: %v", err)
	}
	if res.N != "aaabbbccc_transformed" || res.Signature != rawSig || res.ALR != "alr_value" {
		t.Errorf("unexpected result via nodejs fallback: %+v", res)
	}
}

func TestURLWrapperIsClosed(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "sig", "ok"), wrapperName)

	if runner.IsClosed() {
		t.Fatal("fresh runner reports closed")
	}
	if _, err := runner.TransformURLContext(context.Background(), "https://rr.example.com/videoplayback?n=aaabbbccc&keep=keepme", "sig", ""); err != nil {
		t.Fatalf("transform: %v", err)
	}
	if runner.IsClosed() {
		t.Fatal("runner reports closed after successful call")
	}
	runner.Close()
	if !runner.IsClosed() {
		t.Fatal("runner does not report closed after Close")
	}
	// Cancellation of the ACTIVE call is terminal (the subprocess is killed;
	// see TestURLWrapperCanceledBlockedCall), while a QUEUED call's cancel
	// must not poison a healthy active runner (see
	// TestURLWrapperQueuedCancelDoesNotKillActive). Both are covered by their
	// own tests; nothing further belongs here.
}

func TestURLWrapperRejectsUnaryOps(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "sig", "ok"), wrapperName)

	if _, err := runner.TransformN("aaabbbccc"); err == nil || !strings.Contains(err.Error(), "url-wrapper mode") {
		t.Errorf("TransformN: expected url-wrapper mode rejection, got %v", err)
	}
	if _, err := runner.TransformNBatch([]string{"aaabbbccc", "dddeeefff"}); err == nil || !strings.Contains(err.Error(), "url-wrapper mode") {
		t.Errorf("TransformNBatch: expected url-wrapper mode rejection, got %v", err)
	}
	if err := runner.FinishPreload(nil, "whatever"); err == nil || !strings.Contains(err.Error(), "url-wrapper mode") {
		t.Errorf("FinishPreload: expected url-wrapper mode rejection, got %v", err)
	}

	// The URL protocol still works after the rejections.
	res, err := runner.TransformURLContext(context.Background(), "https://rr.example.com/videoplayback?n=aaabbbccc&keep=keepme", "sig", "")
	if err != nil {
		t.Fatalf("TransformURLContext after unary rejections: %v", err)
	}
	if res.N != "aaabbbccc_transformed" {
		t.Errorf("N = %q, want %q", res.N, "aaabbbccc_transformed")
	}
}

func TestURLWrapperRejectsURLOpOnUnaryRunner(t *testing.T) {
	// White-box: a hand-built legacy-mode runner must reject URL-protocol
	// calls before touching any IPC endpoints (no subprocess involved).
	runner := &SubprocessRunner{mode: runnerModeUnary}
	_, err := runner.TransformURLContext(context.Background(), "https://rr.example.com/videoplayback?n=aaabbbccc", "sig", "")
	if err == nil || !strings.Contains(err.Error(), "not url-wrapper mode") {
		t.Errorf("expected not-url-wrapper-mode rejection, got %v", err)
	}
}

func TestURLWrapperQueuedCancelDoesNotKillActive(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "ignored", "hang"), wrapperName)
	streamURL := "https://rr.example.com/videoplayback?n=aaabbbccc"

	activeCtx, activeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer activeCancel()
	activeDone := make(chan error, 1)
	go func() {
		_, err := runner.TransformURLContext(activeCtx, streamURL, "sig", "")
		activeDone <- err
	}()
	time.Sleep(300 * time.Millisecond) // let the active call block inside the wrapper

	// A queued call with a short deadline must return promptly WITHOUT
	// killing the active owner's process.
	queuedCtx, queuedCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer queuedCancel()
	start := time.Now()
	_, err := runner.TransformURLContext(queuedCtx, streamURL, "sig", "")
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("queued call: expected canceled error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("queued canceled call took too long: %v", elapsed)
	}

	// The active call must still be blocked and only fail once IT is canceled.
	select {
	case err := <-activeDone:
		t.Fatalf("active call returned early (%v); queued cancel must not kill the owner's process", err)
	default:
	}
	activeCancel()
	select {
	case err := <-activeDone:
		if err == nil || !strings.Contains(err.Error(), "canceled") {
			t.Fatalf("active call: expected canceled error after its own cancel, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("active call did not return after its own cancel")
	}
}

func TestURLWrapperMissingRequiredSignature(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	streamURL := "https://rr.example.com/videoplayback?n=aaabbbccc&keep=keepme"
	cases := []struct {
		name string
		mode string
	}{
		{name: "missing", mode: "nosig"},
		{name: "empty", mode: "emptysig"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const sig = "rawsig"
			runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, sig, tc.mode), wrapperName)
			_, err := runner.TransformURLContext(context.Background(), streamURL, "sig", sig)
			if err == nil || !strings.Contains(err.Error(), "did not return signature") {
				t.Fatalf("expected did-not-return-signature error, got %v", err)
			}
		})
	}

	// Go response-boundary check: a requested signature must be present even
	// if the runner script were bypassed. An absent (unrequested) signature
	// stays optional.
	t.Run("goBoundary", func(t *testing.T) {
		runner := &SubprocessRunner{}
		if _, err := runner.finalizeURLResult(urlCallResponse{Sig: "", ALR: "alr_value"}, streamURL, "rawsig"); err == nil || !strings.Contains(err.Error(), "empty signature") {
			t.Fatalf("expected empty-signature error at boundary, got %v", err)
		}
		res, err := runner.finalizeURLResult(urlCallResponse{ALR: "alr_value"}, "https://rr.example.com/videoplayback", "")
		if err != nil {
			t.Fatalf("empty optional signature must pass, got %v", err)
		}
		if res.Signature != "" || res.ALR != "alr_value" {
			t.Errorf("unexpected result: %+v", res)
		}
	})
}

func TestURLWrapperOversizedField(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "sig", "hugealr"), wrapperName)
	_, err := runner.TransformURLContext(context.Background(), "https://rr.example.com/videoplayback?n=aaabbbccc&keep=keepme", "sig", "")
	if err == nil || !strings.Contains(err.Error(), "field limit") {
		t.Fatalf("expected field-limit error, got %v", err)
	}
}

func TestURLWrapperOversizedErrorFrame(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "sig", "hugethrow"), wrapperName)
	start := time.Now()
	_, err := runner.TransformURLContext(context.Background(), "https://rr.example.com/videoplayback?n=aaabbbccc", "sig", "")
	if err == nil {
		t.Fatal("expected bounded error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("oversized error handling took too long: %v", elapsed)
	}
	// The oversized error message is capped by the runner script; the error
	// frame must stay bounded rather than hanging or flooding the exchange.
	if len(err.Error()) > 32*1024 {
		t.Fatalf("error message not bounded (%d bytes): %v", len(err.Error()), err)
	}
	if !strings.HasSuffix(err.Error(), "...") {
		t.Errorf("truncated error missing marker: %v", err)
	}
	// Runner survives JS-side errors; a later Close is clean.
	runner.Close()
	_, err = runner.TransformURLContext(context.Background(), "https://rr.example.com/videoplayback?n=aaabbbccc", "sig", "")
	if err == nil || !strings.Contains(err.Error(), "runner is closed") {
		t.Fatalf("expected runner-is-closed error after Close, got %v", err)
	}
}

func TestURLWrapperControlCharErrorFrameBounded(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	// First call throws an error whose message is dominated by control
	// characters: JSON.stringify escapes each one to six bytes, so a
	// length-only cap can still emit a frame over the wire limit, which would
	// terminate the runner. The serialized error frame must stay bounded and
	// the same runner must remain usable for the next (valid) call.
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "ignored", "throwoncectrl"), wrapperName)
	streamURL := "https://rr.example.com/videoplayback?n=aaabbbccc&keep=keepme"

	_, err := runner.TransformURLContext(context.Background(), streamURL, "sig", "")
	if err == nil {
		t.Fatal("expected JS error with control-char message, got nil")
	}
	if !strings.HasSuffix(err.Error(), "...") {
		t.Errorf("bounded error missing truncation marker: %v", err)
	}
	if len(err.Error()) > 16*1024 {
		t.Fatalf("error message not bounded after control-char expansion (%d bytes): %v", len(err.Error()), err)
	}
	if runner.IsClosed() {
		t.Fatal("runner reported closed after bounded JS-level error")
	}

	// The same runner serves the next valid call: a bounded JS-level error is
	// a healthy, nonterminal failure.
	res, err := runner.TransformURLContext(context.Background(), streamURL, "sig", "")
	if err != nil {
		t.Fatalf("valid call after bounded error: %v", err)
	}
	if res.N != "aaabbbccc_transformed" {
		t.Errorf("N = %q, want %q", res.N, "aaabbbccc_transformed")
	}
}

func TestURLWrapperMultibyteFrameBounded(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	// A 16384-character signature of three-byte chars is exactly at the
	// 16 KiB field cap in UTF-16 units, but serializes to ~49 KiB; together
	// with the multibyte alr the frame is ~98 KiB — over the 60 KiB frame
	// cap and the Go wire cap. The frame byte bound must surface as a
	// bounded, nonterminal JS-level error instead of shipping a frame the Go
	// side terminates.
	rawSig := strings.Repeat("\u503c", urlResponseMaxField)
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, rawSig, "multibyteframe"), wrapperName)
	streamURL := "https://rr.example.com/videoplayback?keep=keepme"

	_, err := runner.TransformURLContext(context.Background(), streamURL, "sig", rawSig)
	if err == nil {
		t.Fatal("expected bounded frame error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds frame limit") {
		t.Fatalf("expected runner-side frame limit error, got %v", err)
	}
	if runner.IsClosed() {
		t.Fatal("runner reported closed after bounded frame error")
	}

	// Same runner serves a valid call: multibyte sig retained, alr back to
	// the normal value after the first call.
	res, err := runner.TransformURLContext(context.Background(), streamURL, "sig", rawSig)
	if err != nil {
		t.Fatalf("valid call after bounded frame error: %v", err)
	}
	if res.Signature != rawSig {
		t.Errorf("Signature = %q, want multibyte signature", res.Signature)
	}
	if res.ALR != "alr_value" {
		t.Errorf("ALR = %q, want %q", res.ALR, "alr_value")
	}
}

func TestURLWrapperMalformedQueryRejected(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "ignored", "ok"), wrapperName)

	// A malformed percent-escape must surface as an explicit error, never be
	// silently dropped: dropping the pair would hide the n parameter from
	// validation and let an untransformed n through.
	for _, tc := range []struct {
		name string
		url  string
	}{
		{name: "malformedN", url: "https://rr.example.com/videoplayback?n=%ZZ&keep=keepme"},
		{name: "malformedOtherPair", url: "https://rr.example.com/videoplayback?n=aaabbbccc&keep=keepme&pot=%ZZ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runner.TransformURLContext(context.Background(), tc.url, "sig", "")
			if err == nil {
				t.Fatal("expected explicit malformed-query error, got nil")
			}
			if runner.IsClosed() {
				t.Fatal("runner reported closed after client-side query validation failure")
			}
		})
	}

	// Validation runs before the IPC exchange: the same runner still serves
	// valid calls afterwards.
	res, err := runner.TransformURLContext(context.Background(), "https://rr.example.com/videoplayback?n=aaabbbccc&keep=keepme", "sig", "")
	if err != nil {
		t.Fatalf("valid call after rejected malformed queries: %v", err)
	}
	if res.N != "aaabbbccc_transformed" {
		t.Errorf("N = %q, want %q", res.N, "aaabbbccc_transformed")
	}
}

// TestURLQueryParam pins urlQueryParam semantics: first-value-wins for
// repeated keys, valid percent-encoding untouched by the raw URL (it is read,
// never rewritten), and every malformed pair an explicit error — nothing is
// silently discarded.
func TestURLQueryParam(t *testing.T) {
	cases := []struct {
		name    string
		rawURL  string
		key     string
		want    string
		wantOK  bool
		wantErr bool
	}{
		{name: "valid", rawURL: "https://rr.example.com/videoplayback?n=aaabbbccc&keep=keepme", key: "n", want: "aaabbbccc", wantOK: true},
		{name: "missing", rawURL: "https://rr.example.com/videoplayback?keep=keepme", key: "n"},
		{name: "multipleValuesFirstWins", rawURL: "https://rr.example.com/videoplayback?n=first&n=second", key: "n", want: "first", wantOK: true},
		{name: "validPercentEncodingReadOnly", rawURL: "https://rr.example.com/videoplayback?n=aaabbbccc&pot=ab%2Fcd%20x", key: "n", want: "aaabbbccc", wantOK: true},
		{name: "malformedEscapeInN", rawURL: "https://rr.example.com/videoplayback?n=%ZZ&keep=keepme", key: "n", wantErr: true},
		{name: "malformedEscapeInOtherPair", rawURL: "https://rr.example.com/videoplayback?n=aaabbbccc&pot=%ZZ", key: "n", wantErr: true},
		{name: "unparsableURL", rawURL: "://bad", key: "n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := urlQueryParam(tc.rawURL, tc.key)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("urlQueryParam(%q): expected error, got %q ok=%v", tc.rawURL, got, ok)
				}
				return
			}
			if err != nil {
				t.Fatalf("urlQueryParam(%q): unexpected error: %v", tc.rawURL, err)
			}
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("urlQueryParam(%q) = %q, %v; want %q, %v", tc.rawURL, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestURLWrapperJSErrorKeepsRunnerHealthy(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "sig", "throw"), wrapperName)
	streamURL := "https://rr.example.com/videoplayback?n=aaabbbccc"

	// A valid wire frame carrying a JS-level error is not a transport
	// failure: the process stays healthy and usable.
	for i := 0; i < 2; i++ {
		_, err := runner.TransformURLContext(context.Background(), streamURL, "sig", "")
		if err == nil || !strings.Contains(err.Error(), "wrapper boom") {
			t.Fatalf("call %d: expected JS error, got %v", i, err)
		}
		if runner.IsClosed() {
			t.Fatalf("call %d: runner reported closed after JS-level error", i)
		}
	}

	// Same for a validation error: unusable candidate, healthy process.
	valRunner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "sig", "w8"), wrapperName)
	_, err := valRunner.TransformURLContext(context.Background(), streamURL, "sig", "")
	if err == nil {
		t.Fatal("expected n validation error")
	}
	if valRunner.IsClosed() {
		t.Fatal("runner reported closed after validation error")
	}
}

func TestURLWrapperTransportEOFIsTerminal(t *testing.T) {
	engine := testRuntime(t)
	const wrapperName = "urlWrapper"
	runner := newTestURLWrapper(t, engine, urlWrapperBundle(wrapperName, "ignored", "hang"), wrapperName)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	callDone := make(chan error, 1)
	go func() {
		_, err := runner.TransformURLContext(ctx, "https://rr.example.com/videoplayback?n=aaabbbccc", "sig", "")
		callDone <- err
	}()
	time.Sleep(300 * time.Millisecond) // let the call block inside the wrapper

	// Simulate child exit: killing the process mid-call must surface as a
	// transport EOF AND terminate/reap the runner (IsClosed true, reaped).
	if err := runner.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("expected transport error after child exit, got nil")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight call did not return after child exit")
	}
	if !runner.IsClosed() {
		t.Fatal("runner does not report closed after transport EOF")
	}
	if runner.cmd.ProcessState == nil {
		t.Fatalf("child process was not reaped (ProcessState missing)")
	}
	reaped := runner.cmd.ProcessState.Exited()
	if !reaped {
		if ws, ok := runner.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			reaped = true
		}
	}
	if !reaped {
		t.Fatalf("child process was not reaped (ProcessState = %+v)", runner.cmd.ProcessState)
	}
	// Close is a bounded no-op afterwards.
	done := make(chan struct{})
	go func() { runner.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung after terminal transport error")
	}
	_, err := runner.TransformURLContext(context.Background(), "https://rr.example.com/videoplayback?n=aaabbbccc", "sig", "")
	if err == nil || !strings.Contains(err.Error(), "runner is closed") {
		t.Fatalf("expected runner-is-closed error, got %v", err)
	}
}

func TestURLWrapperMalformedFrameIsTerminal(t *testing.T) {
	// White-box: a wire frame that is not a valid protocol response must
	// terminate the runner (no real subprocess involved). null and {} are
	// valid JSON but not protocol responses: they would decode into a zero
	// struct and pass as an empty success, so they are malformed frames too.
	cases := []struct {
		name string
		line string // response bytes ("" = missing frame)
	}{
		{name: "malformedJSON", line: "not json at all\n"},
		{name: "missingFrame", line: ""},
		{name: "nullFrame", line: "null\n"},
		{name: "emptyObjectFrame", line: "{}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := make(chan struct{}, 1)
			gate <- struct{}{}
			runner := &SubprocessRunner{
				stdin:     json.NewEncoder(io.Discard),
				urlStdout: bufio.NewReader(bytes.NewBufferString(tc.line)),
				mode:      runnerModeURL,
				callGate:  gate,
				cmd:       &exec.Cmd{},
			}
			_, err := runner.TransformURLContext(context.Background(), "https://rr.example.com/videoplayback?n=aaabbbccc", "sig", "rawsig")
			if err == nil {
				t.Fatalf("expected transport error, got nil")
			}
			if !runner.IsClosed() {
				t.Fatalf("runner not closed after %s (err %v)", tc.name, err)
			}
		})
	}
}

// TestURLWrapperCallFrameNoResultFieldsTerminal pins the call-path contract:
// a non-empty object frame whose recognized result fields (n, sig, alr) are
// ALL absent or empty carries no wrapper-produced data and is a terminal
// protocol failure — not a silent empty success. Under this input (no n, no
// requested signature) the empty result cannot be masked by query parsing or
// signature validation. A recognized alr-only frame is a healthy success, and
// a JS error frame stays healthy and reusable.
func TestURLWrapperCallFrameNoResultFieldsTerminal(t *testing.T) {
	streamURL := "https://rr.example.com/videoplayback?keep=keepme" // no n, no signature

	for _, tc := range []struct {
		name string
		line string
	}{
		{name: "unrelatedFields", line: "{\"unrelated\":true}\n"},
		{name: "recognizedEmptyStrings", line: "{\"n\":\"\",\"sig\":\"\",\"alr\":\"\"}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := make(chan struct{}, 1)
			gate <- struct{}{}
			runner := &SubprocessRunner{
				stdin:     json.NewEncoder(io.Discard),
				urlStdout: bufio.NewReader(bytes.NewBufferString(tc.line)),
				mode:      runnerModeURL,
				callGate:  gate,
				cmd:       &exec.Cmd{},
			}
			_, err := runner.TransformURLContext(context.Background(), streamURL, "sig", "")
			if err == nil {
				t.Fatalf("expected terminal protocol error for %q, got nil", tc.line)
			}
			if !runner.IsClosed() {
				t.Fatalf("runner not closed after no-result-fields frame %q", tc.line)
			}
		})
	}

	// Recognized alr-only frame: healthy success.
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	okRunner := &SubprocessRunner{
		stdin:     json.NewEncoder(io.Discard),
		urlStdout: bufio.NewReader(bytes.NewBufferString("{\"alr\":\"alr_value\"}\n")),
		mode:      runnerModeURL,
		callGate:  gate,
		cmd:       &exec.Cmd{},
	}
	res, err := okRunner.TransformURLContext(context.Background(), streamURL, "sig", "")
	if err != nil {
		t.Fatalf("alr-only frame: unexpected error %v", err)
	}
	if res.ALR != "alr_value" {
		t.Errorf("ALR = %q, want %q", res.ALR, "alr_value")
	}
	if okRunner.IsClosed() {
		t.Fatal("runner closed after valid alr-only frame")
	}

	// JS error frame: healthy, nonterminal, reusable for a next valid call.
	gate2 := make(chan struct{}, 1)
	gate2 <- struct{}{}
	errRunner := &SubprocessRunner{
		stdin:     json.NewEncoder(io.Discard),
		urlStdout: bufio.NewReader(bytes.NewBufferString("{\"error\":\"boom\"}\n{\"alr\":\"alr_value\"}\n")),
		mode:      runnerModeURL,
		callGate:  gate2,
		cmd:       &exec.Cmd{},
	}
	_, err = errRunner.TransformURLContext(context.Background(), streamURL, "sig", "")
	if err == nil || !strings.Contains(err.Error(), "js url error: boom") {
		t.Fatalf("expected js url error, got %v", err)
	}
	if errRunner.IsClosed() {
		t.Fatal("runner closed after JS-level error frame")
	}
	res2, err := errRunner.TransformURLContext(context.Background(), streamURL, "sig", "")
	if err != nil {
		t.Fatalf("valid call after JS-level error: %v", err)
	}
	if res2.ALR != "alr_value" {
		t.Errorf("ALR = %q, want %q", res2.ALR, "alr_value")
	}
}

func TestReadURLResponseBounds(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		r := &SubprocessRunner{urlStdout: bufio.NewReader(bytes.NewBufferString("{\"n\":\"x\"}\n"))}
		line, err := r.readURLResponse()
		if err != nil {
			t.Fatalf("readURLResponse: %v", err)
		}
		if string(line) != "{\"n\":\"x\"}\n" {
			t.Errorf("line = %q", string(line))
		}
	})

	t.Run("multiFragmentAccumulation", func(t *testing.T) {
		// A line larger than the default bufio buffer must accumulate across
		// fragments up to the frame cap.
		payload := strings.Repeat("a", 10*1024)
		r := &SubprocessRunner{urlStdout: bufio.NewReader(bytes.NewBufferString(`{"alr":"` + payload + `"}` + "\n"))}
		line, err := r.readURLResponse()
		if err != nil {
			t.Fatalf("readURLResponse: %v", err)
		}
		if len(line) != len(`{"alr":"`)+len(payload)+len(`"}`)+1 {
			t.Errorf("unexpected accumulated line length %d", len(line))
		}
	})

	t.Run("frameOverflow", func(t *testing.T) {
		huge := strings.Repeat("z", urlResponseMaxFrame+1024)
		r := &SubprocessRunner{urlStdout: bufio.NewReader(bytes.NewBufferString(huge + "\n"))}
		_, err := r.readURLResponse()
		if !errors.Is(err, errURLFrameTooLarge) {
			t.Fatalf("expected errURLFrameTooLarge, got %v", err)
		}
	})

	t.Run("maxFrameNoNewline", func(t *testing.T) {
		// Exactly maxFrame bytes with no terminator: the reader must error
		// instead of blocking forever on the next fragment.
		frame := strings.Repeat("z", urlResponseMaxFrame)
		r := &SubprocessRunner{urlStdout: bufio.NewReader(bytes.NewBufferString(frame))}
		_, err := r.readURLResponse()
		if !errors.Is(err, errURLFrameTooLarge) {
			t.Fatalf("expected errURLFrameTooLarge, got %v", err)
		}
	})

	t.Run("eof", func(t *testing.T) {
		r := &SubprocessRunner{urlStdout: bufio.NewReader(bytes.NewBufferString("no newline here"))}
		if _, err := r.readURLResponse(); err != io.EOF {
			t.Fatalf("expected io.EOF, got %v", err)
		}
	})
}

// TestSubprocessRunnerSupportsURLTransformImmutable pins the read-only mode
// capability accessor: pinned at construction, unchanged by calls, unchanged
// by Close — the property pkg relies on to reject unary-mode runners parked
// at URL-wrapper cache keys.
func TestSubprocessRunnerSupportsURLTransformImmutable(t *testing.T) {
	engine := testRuntime(t)

	unary, err := NewSubprocessRunner(engine, []byte(`var _exposed={};_exposed['seedfn']=function(n){return n+"_x";};`), "seedfn")
	if err != nil {
		t.Fatalf("NewSubprocessRunner: %v", err)
	}
	t.Cleanup(unary.Close)
	if unary.SupportsURLTransform() {
		t.Fatal("unary runner must not report URL-wrapper capability")
	}
	if _, err := unary.TransformN("a"); err != nil {
		t.Fatalf("unary TransformN: %v", err)
	}
	if unary.SupportsURLTransform() {
		t.Fatal("URL-wrapper capability must not change after a unary call")
	}
	unary.Close()
	if unary.SupportsURLTransform() {
		t.Fatal("URL-wrapper capability must not change after Close")
	}

	urlw := newTestURLWrapper(t, engine, urlWrapperBundle("jW", "raw", "ok"), "jW")
	if !urlw.SupportsURLTransform() {
		t.Fatal("url-wrapper runner must report URL-wrapper capability")
	}
	if _, err := urlw.TransformURLContext(context.Background(), "https://x/y?v=1&n=nv&keep=keepme", "sp", "raw"); err != nil {
		t.Fatalf("TransformURLContext: %v", err)
	}
	if !urlw.SupportsURLTransform() {
		t.Fatal("URL-wrapper capability must not change after a URL call")
	}
	urlw.Close()
	if !urlw.SupportsURLTransform() {
		t.Fatal("URL-wrapper capability must not change after Close")
	}
}
