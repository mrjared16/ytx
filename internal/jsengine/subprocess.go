package jsengine

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

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
// Implements the JSEngine interface
type SubprocessRunner struct {
	cmd        *exec.Cmd
	stdin      *json.Encoder
	stdout     *json.Decoder
	mu         sync.Mutex
	funcName   string
	engineName string
	closed     bool
	preloaded  bool
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

// TransformN transforms the n-parameter value (implements JSEngine interface)
func (r *SubprocessRunner) TransformN(n string) (string, error) {
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

	if r.closed {
		return
	}
	r.closed = true

	if r.cmd.Process != nil {
		r.cmd.Process.Kill()
	}
	r.cmd.Wait()
}

// Name returns the engine name (implements JSEngine interface)
func (r *SubprocessRunner) Name() string {
	return r.engineName
}
