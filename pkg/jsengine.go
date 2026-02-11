package ytx

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// EngineType represents the JS engine to use for n-parameter transformation
type EngineType string

const (
	EngineAuto    EngineType = "auto"
	EngineQuickJS EngineType = "quickjs"
	EngineBun     EngineType = "bun"
	EngineNode    EngineType = "node"
)

// JSEngine is the interface for JavaScript execution engines
type JSEngine interface {
	// TransformN transforms the n-parameter value
	TransformN(n string) (string, error)
	// TransformNBatch transforms multiple n-parameter values at once (optimization for bulk)
	TransformNBatch(nValues []string) ([]string, error)
	// Close releases resources
	Close()
	// Name returns the engine name for logging
	Name() string
}

// Global engine configuration
var (
	globalEngineType EngineType = EngineAuto
	engineMu         sync.RWMutex
)

// SetEngineType sets the global JS engine type
func SetEngineType(t EngineType) {
	engineMu.Lock()
	defer engineMu.Unlock()
	globalEngineType = t
}

// GetEngineType returns the current global JS engine type
func GetEngineType() EngineType {
	engineMu.RLock()
	defer engineMu.RUnlock()
	return globalEngineType
}

// ParseEngineType parses a string into EngineType
func ParseEngineType(s string) (EngineType, error) {
	switch s {
	case "auto", "":
		return EngineAuto, nil
	case "quickjs":
		return EngineQuickJS, nil
	case "bun":
		return EngineBun, nil
	case "node":
		return EngineNode, nil
	default:
		return EngineAuto, fmt.Errorf("unknown engine type: %s (use: auto, quickjs, bun, node)", s)
	}
}

// Global cached engine
var (
	cachedEngine   JSEngine
	cachedEngineMu sync.Mutex
)

// GetCachedEngine returns a cached JS engine or creates a new one
func GetCachedEngine(playerJS []byte, nFuncName string) (JSEngine, error) {
	cachedEngineMu.Lock()
	defer cachedEngineMu.Unlock()

	if cachedEngine != nil {
		return cachedEngine, nil
	}

	engine, err := NewJSEngine(GetEngineType(), playerJS, nFuncName)
	if err != nil {
		return nil, err
	}

	cachedEngine = engine
	return engine, nil
}

// CloseCachedEngine closes the cached engine
func CloseCachedEngine() {
	cachedEngineMu.Lock()
	defer cachedEngineMu.Unlock()

	if cachedEngine != nil {
		cachedEngine.Close()
		cachedEngine = nil
	}
}

// NewJSEngine creates a new JS engine based on the engine type
// Tries to use pre-spawned process first for faster initialization
func NewJSEngine(engineType EngineType, playerJS []byte, nFuncName string) (JSEngine, error) {
	switch engineType {
	case EngineQuickJS:
		return NewQuickJSRunner(playerJS, nFuncName)
	case EngineBun:
		// Try pre-spawned runner first
		if runner, err := GetPreSpawnedRunner("bun", playerJS, nFuncName); err != nil {
			return nil, err
		} else if runner != nil {
			return runner, nil
		}
		return NewSubprocessRunner("bun", playerJS, nFuncName)
	case EngineNode:
		// Try pre-spawned runner first
		if runner, err := GetPreSpawnedRunner("node", playerJS, nFuncName); err != nil {
			return nil, err
		} else if runner != nil {
			return runner, nil
		}
		return NewSubprocessRunner("node", playerJS, nFuncName)
	case EngineAuto:
		return newAutoEngine(playerJS, nFuncName)
	default:
		return nil, fmt.Errorf("unknown engine type: %s", engineType)
	}
}

func newAutoEngine(playerJS []byte, nFuncName string) (JSEngine, error) {
	// Try Bun first (with pre-spawn support)
	if bunPath, err := exec.LookPath("bun"); err == nil && bunPath != "" {
		// Try pre-spawned runner first
		if runner, err := GetPreSpawnedRunner("bun", playerJS, nFuncName); err != nil {
			// Pre-spawn failed, try fresh
		} else if runner != nil {
			return runner, nil
		}
		if engine, err := NewSubprocessRunner("bun", playerJS, nFuncName); err == nil {
			return engine, nil
		}
	}

	// Try Node.js (with pre-spawn support)
	if nodePath, err := exec.LookPath("node"); err == nil && nodePath != "" {
		// Try pre-spawned runner first
		if runner, err := GetPreSpawnedRunner("node", playerJS, nFuncName); err != nil {
			// Pre-spawn failed, try fresh
		} else if runner != nil {
			return runner, nil
		}
		if engine, err := NewSubprocessRunner("node", playerJS, nFuncName); err == nil {
			return engine, nil
		}
	}

	if engine, err := NewQuickJSRunner(playerJS, nFuncName); err == nil {
		return engine, nil
	}

	return nil, fmt.Errorf("no JS runtime available")
}

// ValidateNTransformResult checks if the n-transform result is valid
// Returns the validated result and an error if invalid
func ValidateNTransformResult(original, transformed string) (string, error) {
	transformed = strings.Trim(transformed, "\"")
	if transformed == "" {
		return original, fmt.Errorf("n-transform returned empty result")
	}
	if transformed == original {
		return original, fmt.Errorf("n-transform returned unchanged value")
	}
	// _w8_ indicates throttled/failed transform
	if strings.Contains(transformed, "_w8_") {
		return original, fmt.Errorf("n-transform returned throttled marker: %s", transformed)
	}
	// "error" in result indicates JS error
	if strings.Contains(transformed, "error") {
		return original, fmt.Errorf("n-transform returned error: %s", transformed)
	}
	return transformed, nil
}
