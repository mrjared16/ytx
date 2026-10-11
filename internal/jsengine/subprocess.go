package jsengine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	_ "embed"
)

//go:embed nrunner.mjs
var nRunnerScript string

// Temp file names for subprocess runner
const (
	tempRunnerFile = "ytx-nrunner.mjs"
	tempPlayerFile = "ytx-player.js"
)

type jsCommand struct {
	Type     string   `json:"type"`
	Path     string   `json:"path,omitempty"`
	Fun      string   `json:"fun,omitempty"`
	Prepared bool     `json:"prepared,omitempty"`
	Args     []string `json:"args,omitempty"`
	Values   []string `json:"values,omitempty"`
}

type jsStatusResponse struct {
	Success bool   `json:"success,omitempty"`
	Error   string `json:"error,omitempty"`
}

type jsBatchResult struct {
	Value string `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

type jsBatchResponse struct {
	Results []jsBatchResult `json:"results,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// URLTransformResult carries the fields produced by the URL-wrapper protocol.
// Fields are optional and omitted when the wrapper did not provide them.
type URLTransformResult struct {
	Signature string `json:"signature,omitempty"`
	N         string `json:"n,omitempty"`
	ALR       string `json:"alr,omitempty"`
}

// urlCallResponse is the wire response of the url_call command. "sig" is the
// signature after the single decodeURIComponent performed in the runner.
type urlCallResponse struct {
	N     string `json:"n,omitempty"`
	Sig   string `json:"sig,omitempty"`
	ALR   string `json:"alr,omitempty"`
	Error string `json:"error,omitempty"`
}

// urlWrapperCallTimeout caps every URL-wrapper load/call unless the caller
// context carries an earlier deadline.
const urlWrapperCallTimeout = 30 * time.Second

// Response bounds for the URL-wrapper protocol. Individual fields are capped
// in nrunner.mjs; the frame cap here terminates runaway output (for example
// oversized error messages) at the wire boundary.
const (
	urlResponseMaxField = 16 * 1024
	urlResponseMaxFrame = 64 * 1024
)

// errURLFrameTooLarge marks a response line that exceeded urlResponseMaxFrame.
// After an over-long frame the stdout stream is misaligned, so the runner must
// be terminated.
var errURLFrameTooLarge = errors.New("url response frame exceeds limit")

// Runner protocol modes. A runner never mixes exchanges across modes: URL
// wrapper runners reject unary operations and vice versa, so the stdin/stdout
// IPC endpoints always have a single owning exchange at a time.
const (
	runnerModeUnary = iota
	runnerModeURL
)

// boundedRunnerContext returns a context bounded by urlWrapperCallTimeout,
// respecting an earlier deadline on parent (nil parent is treated as
// context.Background).
func boundedRunnerContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	deadline := time.Now().Add(urlWrapperCallTimeout)
	if d, ok := parent.Deadline(); ok && d.Before(deadline) {
		return context.WithDeadline(parent, d)
	}
	return context.WithTimeout(parent, urlWrapperCallTimeout)
}

// lookupRuntime resolves the executable for a subprocess engine. Only Bun and
// Node are valid; Node falls back to the "nodejs" binary name.
func lookupRuntime(engine string) (string, error) {
	switch engine {
	case "bun":
		jsPath, err := exec.LookPath("bun")
		if err != nil {
			return "", fmt.Errorf("bun not found in PATH")
		}
		return jsPath, nil
	case "node":
		jsPath, err := exec.LookPath("node")
		if err != nil {
			jsPath, err = exec.LookPath("nodejs")
			if err != nil {
				return "", fmt.Errorf("node not found in PATH")
			}
		}
		return jsPath, nil
	default:
		return "", fmt.Errorf("unknown subprocess engine: %s", engine)
	}
}

// PreSpawn creates a subprocess early to overlap JS compilation with network I/O
func PreSpawn(engine string, playerJS []byte, nFuncName string) (*SubprocessRunner, error) {
	var jsPath string
	var err error

	switch engine {
	case "bun":
		jsPath, err = exec.LookPath("bun")
		if err != nil {
			return nil, fmt.Errorf("bun not found in PATH")
		}
	case "node":
		jsPath, err = exec.LookPath("node")
		if err != nil {
			jsPath, err = exec.LookPath("nodejs")
			if err != nil {
				return nil, fmt.Errorf("node not found in PATH")
			}
		}
	default:
		return nil, fmt.Errorf("unknown subprocess engine: %s", engine)
	}

	tmpDir := os.TempDir()
	runnerPath := filepath.Join(tmpDir, tempRunnerFile)
	if err := os.WriteFile(runnerPath, []byte(nRunnerScript), 0644); err != nil {
		return nil, fmt.Errorf("failed to write runner script: %w", err)
	}

	cmd := exec.Command(jsPath, runnerPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start %s: %w", engine, err)
	}

	runner := &SubprocessRunner{
		cmd:        cmd,
		stdin:      json.NewEncoder(stdin),
		stdout:     json.NewDecoder(stdout),
		engineName: engine,
	}

	if len(playerJS) > 0 {
		tmpDir2 := os.TempDir()
		playerJSPath := filepath.Join(tmpDir2, tempPlayerFile)
		if err := os.WriteFile(playerJSPath, playerJS, 0644); err != nil {
			return nil, fmt.Errorf("failed to write player.js: %w", err)
		}

		runner.funcName = nFuncName
		if err := runner.stdin.Encode(jsCommand{
			Type:     "load_file",
			Path:     playerJSPath,
			Fun:      nFuncName,
			Prepared: true,
		}); err != nil {
			return nil, fmt.Errorf("failed to send preload command: %w", err)
		}
		runner.preloaded = true
	}

	return runner, nil
}

// FinishPreload completes the preloading process and consumes the ready signal
func (r *SubprocessRunner) FinishPreload(runtimeJS []byte, funcName string) error {
	if r.mode == runnerModeURL {
		return fmt.Errorf("runner is url-wrapper mode; preload not supported")
	}

	if r.preloaded {
		var resp jsStatusResponse
		if err := r.stdout.Decode(&resp); err != nil {
			r.Close()
			return fmt.Errorf("failed to read preloaded response: %w", err)
		}
		if resp.Error != "" {
			r.Close()
			return fmt.Errorf("js preload error: %s", resp.Error)
		}
		return nil
	}

	// Write player.js and load function
	tmpDir := os.TempDir()
	playerJSPath := filepath.Join(tmpDir, tempPlayerFile)
	if err := os.WriteFile(playerJSPath, runtimeJS, 0644); err != nil {
		r.Close()
		return fmt.Errorf("failed to write player.js: %w", err)
	}

	r.funcName = funcName
	if err := r.loadFunctionFromFile(playerJSPath, funcName, true); err != nil {
		r.Close()
		return err
	}

	return nil
}

// SubprocessRunner executes JavaScript functions via Bun/Node subprocess
// Implements the JSEngine interface.
//
// Legacy (unary) runners and URL-wrapper runners are mutually exclusive:
// mode pins the runner to one protocol, and cross-protocol methods are
// rejected up front so the shared stdin/stdout endpoints are never exchanged
// concurrently. URL runners additionally carry callGate (a single token that
// Close never takes, so a blocked exchange cannot deadlock Close) and
// urlStdout (the sole bounded frame reader of the URL-mode stdout).
type SubprocessRunner struct {
	cmd        *exec.Cmd
	stdin      *json.Encoder
	stdout     *json.Decoder
	mu         sync.Mutex
	mode       int
	callGate   chan struct{}
	urlStdout  *bufio.Reader
	funcName   string
	engineName string
	closed     bool
	preloaded  bool
	tmpDir     string
}

// NewSubprocessRunner creates a new subprocess-based JS runner
func NewSubprocessRunner(engine string, runtimeJS []byte, funcName string) (*SubprocessRunner, error) {
	var jsPath string
	var err error

	switch engine {
	case "bun":
		jsPath, err = exec.LookPath("bun")
		if err != nil {
			return nil, fmt.Errorf("bun not found in PATH")
		}
	case "node":
		jsPath, err = exec.LookPath("node")
		if err != nil {
			// Try alternate names
			jsPath, err = exec.LookPath("nodejs")
			if err != nil {
				return nil, fmt.Errorf("node not found in PATH")
			}
		}
	default:
		return nil, fmt.Errorf("unknown subprocess engine: %s", engine)
	}

	// Write runner script to temp file
	tmpDir := os.TempDir()
	runnerPath := filepath.Join(tmpDir, tempRunnerFile)
	if err := os.WriteFile(runnerPath, []byte(nRunnerScript), 0644); err != nil {
		return nil, fmt.Errorf("failed to write runner script: %w", err)
	}

	// Write player.js to temp file (faster than sending 1.5MB over IPC)
	playerJSPath := filepath.Join(tmpDir, tempPlayerFile)
	if err := os.WriteFile(playerJSPath, runtimeJS, 0644); err != nil {
		return nil, fmt.Errorf("failed to write player.js: %w", err)
	}

	cmd := exec.Command(jsPath, runnerPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start %s: %w", engine, err)
	}

	runner := &SubprocessRunner{
		cmd:        cmd,
		stdin:      json.NewEncoder(stdin),
		stdout:     json.NewDecoder(stdout),
		funcName:   funcName,
		engineName: engine,
	}

	// Load the function using file path (faster than sending content)
	if err := runner.loadFunctionFromFile(playerJSPath, funcName, true); err != nil {
		runner.Close()
		return nil, err
	}

	return runner, nil
}

// loadFunctionFromFile loads a function from player.js file path
func (r *SubprocessRunner) loadFunctionFromFile(playerJSPath, funcName string, prepared bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	msg := jsCommand{Type: "load_file", Path: playerJSPath, Fun: funcName, Prepared: prepared}

	if err := r.stdin.Encode(msg); err != nil {
		return fmt.Errorf("failed to send load command: %w", err)
	}

	var resp jsStatusResponse
	if err := r.stdout.Decode(&resp); err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if resp.Error != "" {
		return fmt.Errorf("js error: %s", resp.Error)
	}

	return nil
}

// NewURLWrapperRunnerContext creates a subprocess runner for the URL-wrapper
// protocol (signature/n/alr extraction). Only Bun or Node are supported;
// QuickJS is deliberately rejected. runtimeJS must be a full prepared bundle
// that already inserts `_exposed[wrapperName] = wrapperName` in its final
// IIFE. Load is bounded by the caller context (maximum urlWrapperCallTimeout).
// The runner owns a per-run unique temp directory that is removed on Close or
// on any construction error.
func NewURLWrapperRunnerContext(ctx context.Context, engine string, runtimeJS []byte, wrapperName string) (*SubprocessRunner, error) {
	if engine != "bun" && engine != "node" {
		return nil, fmt.Errorf("url wrapper requires bun or node engine, got: %q", engine)
	}
	if wrapperName == "" {
		return nil, fmt.Errorf("url wrapper name must not be empty")
	}
	jsPath, err := lookupRuntime(engine)
	if err != nil {
		return nil, err
	}

	tmpDir, err := os.MkdirTemp("", "ytx-jsengine-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	runnerPath := filepath.Join(tmpDir, "nrunner.mjs")
	if err := os.WriteFile(runnerPath, []byte(nRunnerScript), 0644); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to write runner script: %w", err)
	}
	playerJSPath := filepath.Join(tmpDir, "player.js")
	if err := os.WriteFile(playerJSPath, runtimeJS, 0644); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to write player bundle: %w", err)
	}

	cmd := exec.Command(jsPath, runnerPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to start %s: %w", engine, err)
	}

	callGate := make(chan struct{}, 1)
	callGate <- struct{}{}

	runner := &SubprocessRunner{
		cmd:        cmd,
		stdin:      json.NewEncoder(stdin),
		stdout:     json.NewDecoder(stdout),
		urlStdout:  bufio.NewReader(stdout),
		mode:       runnerModeURL,
		callGate:   callGate,
		funcName:   wrapperName,
		engineName: engine,
		tmpDir:     tmpDir,
	}

	if err := runner.loadWrapperFileContext(ctx, playerJSPath); err != nil {
		runner.Close()
		return nil, err
	}
	return runner, nil
}

// loadWrapperFileContext sends a prepared load_file command and waits for the
// response under a bounded context. On cancellation the subprocess is killed
// and reaped; the exchange goroutine is drained before returning.
func (r *SubprocessRunner) loadWrapperFileContext(ctx context.Context, playerJSPath string) error {
	bound, cancel := boundedRunnerContext(ctx)
	defer cancel()

	msg := jsCommand{Type: "load_file", Path: playerJSPath, Fun: r.funcName, Prepared: true}

	type outcome struct {
		resp jsStatusResponse
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		if err := r.stdin.Encode(msg); err != nil {
			ch <- outcome{err: fmt.Errorf("failed to send load command: %w", err)}
			return
		}
		raw, err := r.readURLResponse()
		if err != nil {
			ch <- outcome{err: fmt.Errorf("failed to read load response: %w", err)}
			return
		}
		var resp jsStatusResponse
		if err := decodeWireObject(raw, &resp); err != nil {
			ch <- outcome{err: fmt.Errorf("failed to decode load response: %w", err)}
			return
		}
		ch <- outcome{resp: resp}
	}()

	select {
	case <-bound.Done():
		r.mu.Lock()
		r.terminateLocked()
		r.mu.Unlock()
		<-ch
		return fmt.Errorf("failed to load url wrapper: %w", bound.Err())
	case o := <-ch:
		if o.err != nil {
			return o.err
		}
		if o.resp.Error != "" {
			return fmt.Errorf("js load error: %s", o.resp.Error)
		}
		// A load is only a success when the runner confirms it: an object
		// without success:true (or with success:false) is a malformed load
		// frame, never a silent success.
		if !o.resp.Success {
			return fmt.Errorf("invalid load response: success not confirmed")
		}
		return nil
	}
}

// TransformURLContext applies the URL-wrapper protocol to a stream URL. The
// loaded wrapper is invoked as wrapper(streamURL, signatureParam, sig), where
// sig is encodeURIComponent'd exactly once, or undefined when signature is
// empty. get('n') is only read when the input URL carries an n parameter,
// get(signatureParam) only when a signature was passed (a requested signature
// that the wrapper fails to return is an explicit error), and get('alr') is
// always read. The returned signature has been decoded exactly once. When the
// input URL has a non-empty n, the returned n is validated with
// ValidateNTransformResult; an empty optional n is never validated.
//
// The context is bounded (maximum urlWrapperCallTimeout) before queueing, so
// a queued caller cancels promptly without touching the active owner's
// process. Cancellation of the active owner kills the subprocess promptly and
// returns an explicit error. A malformed input URL query is rejected before
// the exchange (a healthy, nonterminal failure). Close never takes callGate,
// so a blocked
// exchange cannot deadlock a concurrent Close (the exchange goroutine is
// drained before this returns). Responses are read through a bounded frame
// reader; any transport/protocol failure (broken pipe, child exit, malformed
// or oversized frame) terminates the runner, while valid wire frames carrying
// JS-level or validation errors leave the process healthy.
func (r *SubprocessRunner) TransformURLContext(ctx context.Context, streamURL, signatureParam, signature string) (URLTransformResult, error) {
	var res URLTransformResult

	if r.mode != runnerModeURL {
		return res, fmt.Errorf("runner is not url-wrapper mode")
	}
	if streamURL == "" {
		return res, fmt.Errorf("empty stream URL")
	}

	// Reject malformed query strings before any IPC exchange: dropping such
	// pairs silently (u.Query ignores ParseQuery errors) could hide an n
	// parameter from the validation in finalizeURLResult, letting an
	// untransformed n through. Client-side rejection is a healthy,
	// nonterminal failure — the process is never touched.
	if _, _, err := urlQueryParam(streamURL, "n"); err != nil {
		return res, err
	}

	// Bound BEFORE queueing: a queued caller must be cancellable instead of
	// waiting on the gate uncancelably.
	bound, cancel := boundedRunnerContext(ctx)
	defer cancel()

	// Acquire the single-exchange token. Close never takes callGate, so a
	// blocked exchange below cannot deadlock a concurrent Close. A canceled
	// queued caller returns here without killing the active owner's process.
	select {
	case <-r.callGate:
		defer func() { r.callGate <- struct{}{} }()
	case <-bound.Done():
		return res, fmt.Errorf("url transform canceled: %w", bound.Err())
	}

	// Re-check state after acquiring: the runner may have been closed or the
	// context may have fired while we were queued.
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return res, fmt.Errorf("runner is closed")
	}
	if err := bound.Err(); err != nil {
		return res, fmt.Errorf("url transform canceled: %w", err)
	}

	msg := jsCommand{Type: "url_call", Path: streamURL, Fun: signatureParam, Values: []string{signature}}

	type outcome struct {
		resp urlCallResponse
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		if err := r.stdin.Encode(msg); err != nil {
			ch <- outcome{err: fmt.Errorf("failed to send url call command: %w", err)}
			return
		}
		raw, err := r.readURLResponse()
		if err != nil {
			ch <- outcome{err: fmt.Errorf("failed to read url call response: %w", err)}
			return
		}
		var resp urlCallResponse
		if err := decodeWireObject(raw, &resp); err != nil {
			ch <- outcome{err: fmt.Errorf("failed to decode url call response: %w", err)}
			return
		}
		ch <- outcome{resp: resp}
	}()

	select {
	case <-bound.Done():
		r.mu.Lock()
		r.terminateLocked()
		r.mu.Unlock()
		<-ch
		return res, fmt.Errorf("url transform canceled: %w", bound.Err())
	case o := <-ch:
		if o.err != nil {
			// Transport/protocol failures — broken pipe, child exit (EOF),
			// malformed JSON, missing frame, oversized frame — leave the IPC
			// stream unusable. Terminate and reap the subprocess so the
			// runner reports closed and cannot serve further calls.
			r.mu.Lock()
			r.terminateLocked()
			r.mu.Unlock()
			return res, o.err
		}
		if o.resp.Error != "" {
			// Valid wire frame carrying a JS-level error: the process is
			// healthy and remains usable for later calls.
			return res, fmt.Errorf("js url error: %s", o.resp.Error)
		}
		if o.resp.N == "" && o.resp.Sig == "" && o.resp.ALR == "" {
			// A frame whose recognized result fields (n, sig, alr) are all
			// absent or empty carries no data the wrapper could have
			// produced: a protocol failure, never a silent empty success.
			// Static message — no frame or URL content is echoed.
			r.mu.Lock()
			r.terminateLocked()
			r.mu.Unlock()
			return res, fmt.Errorf("url call response carries no result fields")
		}
		return r.finalizeURLResult(o.resp, streamURL, signature)
	}
}

// finalizeURLResult applies signature/n validation to the wire response. A
// requested (non-empty input) signature must be present; an empty optional
// signature is left untouched. Whenever the input URL carries a non-empty n,
// the returned n is validated with ValidateNTransformResult: unchanged,
// empty, throttled-marker and error-marker results are all rejected. An empty
// optional n is never validated. A malformed input query is an explicit error
// (accept-path boundary guard; TransformURLContext rejects it earlier).
func (r *SubprocessRunner) finalizeURLResult(resp urlCallResponse, streamURL, signature string) (URLTransformResult, error) {
	res := URLTransformResult{Signature: resp.Sig, N: resp.N, ALR: resp.ALR}

	if signature != "" && resp.Sig == "" {
		return res, fmt.Errorf("url wrapper returned empty signature for requested signature parameter")
	}

	nParam, ok, err := urlQueryParam(streamURL, "n")
	if err != nil {
		return res, err
	}
	if !ok || nParam == "" {
		return res, nil
	}
	if resp.N == "" {
		return res, fmt.Errorf("url wrapper returned empty n for input n parameter")
	}
	validated, err := ValidateNTransformResult(nParam, resp.N)
	if err != nil {
		return res, err
	}
	res.N = validated
	return res, nil
}

// decodeWireObject unmarshals a protocol frame that must be a non-empty JSON
// object. null and {} both unmarshal into zero structs without error and
// would otherwise be mistaken for empty successes; the runner always emits at
// least one field, so either is a malformed frame.
func decodeWireObject(raw []byte, dst any) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if len(m) == 0 {
		return errors.New("empty or null frame")
	}
	return json.Unmarshal(raw, dst)
}

// readURLResponse reads a single newline-terminated JSON frame from the
// URL-mode stdout with a bounded frame size (urlResponseMaxFrame). It is the
// only reader of r.urlStdout; legacy unary paths use r.stdout (json.Decoder)
// on separate runners, so the two never mix on one pipe.
func (r *SubprocessRunner) readURLResponse() ([]byte, error) {
	var line []byte
	for {
		frag, err := r.urlStdout.ReadSlice('\n')
		if len(frag) > 0 {
			if len(line)+len(frag) > urlResponseMaxFrame {
				return nil, errURLFrameTooLarge
			}
			line = append(line, frag...)
		}
		if err == nil {
			return line, nil
		}
		if err == bufio.ErrBufferFull {
			if len(line) >= urlResponseMaxFrame {
				// Max frame bytes accumulated with no terminator: the frame
				// can no longer fit, and the next ReadSlice could block
				// forever waiting for a '\n' that cannot arrive in bounds.
				return nil, errURLFrameTooLarge
			}
			continue // accumulate the next fragment
		}
		if err == io.EOF {
			// Process died mid-frame; hand back what was read (if anything)
			// so the caller observes an explicit decode failure.
			return line, err
		}
		return nil, err
	}
}

// urlQueryParam returns the first query value for key in rawURL. It reports
// an error when the URL is unparsable or its query contains any malformed
// pair (url.ParseQuery returns the first decoding error). Malformed pairs are
// never silently discarded: dropping them could hide an n parameter from
// downstream validation and let an untransformed n through.
func urlQueryParam(rawURL, key string) (string, bool, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false, fmt.Errorf("invalid stream URL: %w", err)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", false, fmt.Errorf("malformed query in stream URL: %w", err)
	}
	if !q.Has(key) {
		return "", false, nil
	}
	return q.Get(key), true, nil
}

// TransformN transforms the n-parameter value (implements JSEngine interface)
func (r *SubprocessRunner) TransformN(n string) (string, error) {
	if r.mode == runnerModeURL {
		return n, fmt.Errorf("runner is url-wrapper mode; n-transform not supported")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return n, fmt.Errorf("runner is closed")
	}

	msg := jsCommand{Type: "call", Args: []string{n}}

	if err := r.stdin.Encode(msg); err != nil {
		return n, fmt.Errorf("failed to send call command: %w", err)
	}

	var raw json.RawMessage
	if err := r.stdout.Decode(&raw); err != nil {
		return n, fmt.Errorf("failed to read response: %w", err)
	}

	var errResp jsStatusResponse
	if err := json.Unmarshal(raw, &errResp); err == nil && errResp.Error != "" {
		return n, fmt.Errorf("js error: %s", errResp.Error)
	}

	var transformed string
	if err := json.Unmarshal(raw, &transformed); err != nil {
		var generic any
		if err := json.Unmarshal(raw, &generic); err != nil {
			return n, fmt.Errorf("failed to decode JS result: %w", err)
		}
		transformed = fmt.Sprintf("%v", generic)
	}

	// Validate result using shared function
	return ValidateNTransformResult(n, transformed)
}

// TransformNBatch transforms multiple n-parameter values at once (optimization for bulk)
func (r *SubprocessRunner) TransformNBatch(nValues []string) ([]string, error) {
	if r.mode == runnerModeURL {
		return nValues, fmt.Errorf("runner is url-wrapper mode; n-transform not supported")
	}

	if len(nValues) == 0 {
		return nValues, nil
	}

	// Single value? Use regular path
	if len(nValues) == 1 {
		result, err := r.TransformN(nValues[0])
		return []string{result}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nValues, fmt.Errorf("runner is closed")
	}

	msg := jsCommand{Type: "batch", Values: nValues}

	if err := r.stdin.Encode(msg); err != nil {
		return nValues, fmt.Errorf("failed to send batch command: %w", err)
	}

	var resp jsBatchResponse
	if err := r.stdout.Decode(&resp); err != nil {
		return nValues, fmt.Errorf("failed to read batch response: %w", err)
	}

	// Check for error
	if resp.Error != "" {
		return nValues, fmt.Errorf("js batch error: %s", resp.Error)
	}

	if resp.Results == nil {
		return nValues, fmt.Errorf("invalid batch response format")
	}

	results := make([]string, len(nValues))
	for i, item := range resp.Results {
		if i >= len(nValues) {
			break
		}
		if item.Error != "" || item.Value == "" {
			results[i] = nValues[i]
			continue
		}
		validated, err := ValidateNTransformResult(nValues[i], item.Value)
		if err != nil {
			results[i] = nValues[i]
		} else {
			results[i] = validated
		}
	}

	return results, nil
}

// Close terminates the JS process (implements JSEngine interface)
func (r *SubprocessRunner) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.closed {
		r.closed = true
		if r.cmd.Process != nil {
			r.cmd.Process.Kill()
		}
		r.cmd.Wait()
	}
	if r.tmpDir != "" {
		os.RemoveAll(r.tmpDir)
		r.tmpDir = ""
	}
}

// terminateLocked kills the subprocess and reaps it, marking the runner
// closed so a later Close is a no-op (single Wait guarantee). Caller must
// hold r.mu.
func (r *SubprocessRunner) terminateLocked() {
	if r.closed {
		return
	}
	r.closed = true
	if r.cmd.Process != nil {
		r.cmd.Process.Kill()
	}
	r.cmd.Wait()
}

// IsClosed reports whether the runner has been terminated — explicit Close,
// cancellation, or a terminal protocol failure. It is synchronized under mu
// and safe for concurrent use; pkg relies on it for cache liveness checks and
// terminal-error eviction.
func (r *SubprocessRunner) IsClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// SupportsURLTransform reports whether this runner was constructed for the
// modern URL-wrapper protocol. The protocol mode is immutable: it is pinned
// at construction (NewSubprocessRunner vs NewURLWrapperRunnerContext) and
// never changes for the life of the runner, so this read-only capability is
// safe for concurrent use without locking. pkg relies on it to reject
// unary-mode runners parked at URL-wrapper cache keys.
func (r *SubprocessRunner) SupportsURLTransform() bool {
	return r.mode == runnerModeURL
}

// Name returns the engine name (implements JSEngine interface)
func (r *SubprocessRunner) Name() string {
	return r.engineName
}
