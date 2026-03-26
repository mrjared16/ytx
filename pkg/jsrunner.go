package ytx

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

// Pre-spawned process for parallel initialization
var (
	preSpawnedRunner *SubprocessRunner
	preSpawnedMu     sync.Mutex
	preSpawnedEngine string
)

// PreSpawnJSProcess starts a JS subprocess early (before player.js is downloaded)
// This allows overlapping process startup with network I/O, saving ~100-150ms
// Call LoadPlayerJS() later to complete initialization
func PreSpawnJSProcess(engine string) error {
	preSpawnedMu.Lock()
	defer preSpawnedMu.Unlock()

	// Already spawned?
	if preSpawnedRunner != nil && preSpawnedEngine == engine {
		return nil
	}

	var jsPath string
	var err error

	switch engine {
	case "bun":
		jsPath, err = exec.LookPath("bun")
		if err != nil {
			return fmt.Errorf("bun not found in PATH")
		}
	case "node":
		jsPath, err = exec.LookPath("node")
		if err != nil {
			jsPath, err = exec.LookPath("nodejs")
			if err != nil {
				return fmt.Errorf("node not found in PATH")
			}
		}
	default:
		return fmt.Errorf("unknown subprocess engine: %s", engine)
	}

	// Write runner script to temp file (small, fast)
	tmpDir := os.TempDir()
	runnerPath := filepath.Join(tmpDir, tempRunnerFile)
	if err := os.WriteFile(runnerPath, []byte(nRunnerScript), 0644); err != nil {
		return fmt.Errorf("failed to write runner script: %w", err)
	}

	// Start process (but don't load player.js yet)
	cmd := exec.Command(jsPath, runnerPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start %s: %w", engine, err)
	}

	preSpawnedRunner = &SubprocessRunner{
		cmd:        cmd,
		stdin:      json.NewEncoder(stdin),
		stdout:     json.NewDecoder(stdout),
		engineName: engine,
	}
	preSpawnedEngine = engine

	return nil
}

// GetPreSpawnedRunner returns a pre-spawned runner if available, loading the function
// Returns nil if no pre-spawned runner exists for this engine
func GetPreSpawnedRunner(engine string, runtimeJS []byte, funcName string) (*SubprocessRunner, error) {
	preSpawnedMu.Lock()
	defer preSpawnedMu.Unlock()

	if preSpawnedRunner == nil || preSpawnedEngine != engine {
		return nil, nil // No pre-spawned runner, caller should use NewSubprocessRunner
	}

	runner := preSpawnedRunner
	preSpawnedRunner = nil // Claim the runner
	preSpawnedEngine = ""

	// Write player.js and load function
	tmpDir := os.TempDir()
	playerJSPath := filepath.Join(tmpDir, tempPlayerFile)
	if err := os.WriteFile(playerJSPath, runtimeJS, 0644); err != nil {
		runner.Close()
		return nil, fmt.Errorf("failed to write player.js: %w", err)
	}

	runner.funcName = funcName
	if err := runner.loadFunctionFromFile(playerJSPath, funcName, true); err != nil {
		runner.Close()
		return nil, err
	}

	return runner, nil
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

	msg := map[string]interface{}{
		"type":     "load_file",
		"path":     playerJSPath,
		"fun":      funcName,
		"prepared": prepared,
	}

	if err := r.stdin.Encode(msg); err != nil {
		return fmt.Errorf("failed to send load command: %w", err)
	}

	var resp map[string]interface{}
	if err := r.stdout.Decode(&resp); err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
		return fmt.Errorf("js error: %s", errMsg)
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

	msg := map[string]interface{}{
		"type": "call",
		"args": []string{n},
	}

	if err := r.stdin.Encode(msg); err != nil {
		return n, fmt.Errorf("failed to send call command: %w", err)
	}

	var result interface{}
	if err := r.stdout.Decode(&result); err != nil {
		return n, fmt.Errorf("failed to read response: %w", err)
	}

	// Check for error response
	if m, ok := result.(map[string]interface{}); ok {
		if errMsg, ok := m["error"].(string); ok && errMsg != "" {
			return n, fmt.Errorf("js error: %s", errMsg)
		}
	}

	// Result should be a string
	var transformed string
	if s, ok := result.(string); ok {
		transformed = s
	} else {
		transformed = fmt.Sprintf("%v", result)
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

	msg := map[string]interface{}{
		"type":   "batch",
		"values": nValues,
	}

	if err := r.stdin.Encode(msg); err != nil {
		return nValues, fmt.Errorf("failed to send batch command: %w", err)
	}

	var resp map[string]interface{}
	if err := r.stdout.Decode(&resp); err != nil {
		return nValues, fmt.Errorf("failed to read batch response: %w", err)
	}

	// Check for error
	if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
		return nValues, fmt.Errorf("js batch error: %s", errMsg)
	}

	// Parse results
	resultsRaw, ok := resp["results"].([]interface{})
	if !ok {
		return nValues, fmt.Errorf("invalid batch response format")
	}

	results := make([]string, len(nValues))
	for i, r := range resultsRaw {
		if i >= len(nValues) {
			break
		}
		if m, ok := r.(map[string]interface{}); ok {
			if val, ok := m["value"].(string); ok {
				// Validate each result using shared function
				validated, err := ValidateNTransformResult(nValues[i], val)
				if err != nil {
					results[i] = nValues[i] // fallback to original on validation failure
				} else {
					results[i] = validated
				}
			} else {
				results[i] = nValues[i] // fallback to original
			}
		} else {
			results[i] = nValues[i]
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
