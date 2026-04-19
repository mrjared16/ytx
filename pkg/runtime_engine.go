package ytx

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/mrjared16/ytx/internal/jsengine"
)

// SetEngineType sets the global JS engine type
func SetEngineType(t jsengine.EngineType) {
	defaultRuntime.SetEngineType(t)
}

// GetEngineType returns the current global JS engine type
func GetEngineType() jsengine.EngineType {
	return defaultRuntime.GetEngineType()
}

// ParseEngineType parses a string into EngineType
func ParseEngineType(s string) (jsengine.EngineType, error) {
	return jsengine.ParseEngineType(s)
}

// GetCachedEngine returns a cached JS engine or creates a new one
func GetCachedEngine(cacheKey string, runtimeJS []byte, nFuncName string) (jsengine.JSEngine, error) {
	return defaultRuntime.GetCachedEngine(context.Background(), cacheKey, runtimeJS, nFuncName)
}

// CloseCachedEngine closes the cached engine
func CloseCachedEngine() {
	defaultRuntime.CloseCachedEngine()
}

func CachedEngineName() string {
	return defaultRuntime.CachedEngineName()
}

// NewJSEngine creates a new JS engine based on the engine type
// Tries to use pre-spawned process first for faster initialization
func NewJSEngine(engineType jsengine.EngineType, playerJS []byte, nFuncName string) (jsengine.JSEngine, error) {
	return defaultRuntime.newJSEngine(context.Background(), engineType, playerJS, nFuncName)
}

func (r *Runtime) SetEngineType(t jsengine.EngineType) {
	r.engineMu.Lock()
	defer r.engineMu.Unlock()
	r.engineType = t
}

func (r *Runtime) GetEngineType() jsengine.EngineType {
	r.engineMu.RLock()
	defer r.engineMu.RUnlock()
	return r.engineType
}

func (r *Runtime) CachedEngineName() string {
	r.engineCache.mu.Lock()
	defer r.engineCache.mu.Unlock()
	return r.engineCache.lastEngineName
}

func (r *Runtime) GetCachedEngine(ctx context.Context, cacheKey string, runtimeJS []byte, nFuncName string) (jsengine.JSEngine, error) {
	if cacheKey == "" {
		cacheKey = nFuncName
	}

	r.engineCache.mu.Lock()
	if engine, ok := r.engineCache.cachedEngines[cacheKey]; ok {
		r.engineCache.mu.Unlock()
		return engine, nil
	}
	r.engineCache.mu.Unlock()

	v, err, _ := r.engineGroup.Do(cacheKey, func() (any, error) {
		r.engineCache.mu.Lock()
		if engine, ok := r.engineCache.cachedEngines[cacheKey]; ok {
			r.engineCache.mu.Unlock()
			return engine, nil
		}
		r.engineCache.mu.Unlock()

		engine, err := r.newJSEngine(ctx, r.GetEngineType(), runtimeJS, nFuncName)
		if err != nil {
			return nil, err
		}

		r.engineCache.mu.Lock()
		r.engineCache.cachedEngines[cacheKey] = engine
		r.engineCache.lastEngineName = engine.Name()
		r.engineCache.mu.Unlock()
		return engine, nil
	})
	if err != nil {
		return nil, err
	}
	engine, _ := v.(jsengine.JSEngine)
	return engine, nil
}

func (r *Runtime) CloseCachedEngine() {
	r.engineCache.mu.Lock()
	defer r.engineCache.mu.Unlock()

	for key, engine := range r.engineCache.cachedEngines {
		if engine != nil {
			engine.Close()
		}
		delete(r.engineCache.cachedEngines, key)
	}
	r.engineCache.lastEngineName = ""
}

func (r *Runtime) CloseCachedEngineKey(cacheKey string) {
	if cacheKey == "" {
		return
	}

	r.engineCache.mu.Lock()
	defer r.engineCache.mu.Unlock()

	engine, ok := r.engineCache.cachedEngines[cacheKey]
	if !ok {
		return
	}
	if engine != nil {
		engine.Close()
	}
	delete(r.engineCache.cachedEngines, cacheKey)
	if r.engineCache.lastEngineName != "" && engine != nil && r.engineCache.lastEngineName == engine.Name() {
		r.engineCache.lastEngineName = ""
	}
}

func (r *Runtime) newJSEngine(ctx context.Context, engineType jsengine.EngineType, playerJS []byte, nFuncName string) (jsengine.JSEngine, error) {
	switch engineType {
	case jsengine.EngineQuickJS:
		return jsengine.NewQuickJSRunner(playerJS, nFuncName)
	case jsengine.EngineBun:
		// Try pre-spawned runner first
		if runner, err := r.GetPreSpawnedRunner(ctx, "bun", playerJS, nFuncName); err != nil {
			return nil, err
		} else if runner != nil {
			return runner, nil
		}
		return jsengine.NewSubprocessRunner("bun", playerJS, nFuncName)
	case jsengine.EngineNode:
		// Try pre-spawned runner first
		if runner, err := r.GetPreSpawnedRunner(ctx, "node", playerJS, nFuncName); err != nil {
			return nil, err
		} else if runner != nil {
			return runner, nil
		}
		return jsengine.NewSubprocessRunner("node", playerJS, nFuncName)
	case jsengine.EngineAuto:
		return r.newAutoEngine(ctx, playerJS, nFuncName)
	default:
		return nil, fmt.Errorf("unknown engine type: %s", engineType)
	}
}

func (r *Runtime) newAutoEngine(ctx context.Context, playerJS []byte, nFuncName string) (jsengine.JSEngine, error) {
	// Try Bun first (with pre-spawn support)
	if bunPath, err := exec.LookPath("bun"); err == nil && bunPath != "" {
		// Try pre-spawned runner first
		if runner, err := r.GetPreSpawnedRunner(ctx, "bun", playerJS, nFuncName); err != nil {
			// Pre-spawn failed, try fresh
		} else if runner != nil {
			return runner, nil
		}
		if engine, err := jsengine.NewSubprocessRunner("bun", playerJS, nFuncName); err == nil {
			return engine, nil
		}
	}

	// Try Node.js (with pre-spawn support)
	if nodePath, err := exec.LookPath("node"); err == nil && nodePath != "" {
		// Try pre-spawned runner first
		if runner, err := r.GetPreSpawnedRunner(ctx, "node", playerJS, nFuncName); err != nil {
			// Pre-spawn failed, try fresh
		} else if runner != nil {
			return runner, nil
		}
		if engine, err := jsengine.NewSubprocessRunner("node", playerJS, nFuncName); err == nil {
			return engine, nil
		}
	}

	if engine, err := jsengine.NewQuickJSRunner(playerJS, nFuncName); err == nil {
		return engine, nil
	}

	return nil, fmt.Errorf("no JS runtime available")
}

// PreSpawnJSProcess starts a JS subprocess early and pre-loads the 2.6MB player.js immediately
// This allows overlapping the massive 200ms JS JIT-compilation with the network I/O
func PreSpawnJSProcess(engine string, playerJS []byte, nFuncName string) error {
	return defaultRuntime.PreSpawnJSProcess(context.Background(), engine, playerJS, nFuncName)
}

func (r *Runtime) PreSpawnJSProcess(ctx context.Context, engine string, playerJS []byte, nFuncName string) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}

	r.preSpawn.mu.Lock()
	defer r.preSpawn.mu.Unlock()

	// Already spawned?
	if r.preSpawn.runner != nil && r.preSpawn.engine == engine {
		return nil
	}

	runner, err := jsengine.PreSpawn(engine, playerJS, nFuncName)
	if err != nil {
		return err
	}

	r.preSpawn.runner = runner
	r.preSpawn.engine = engine
	return nil
}

// GetPreSpawnedRunner returns a pre-spawned runner if available, loading the function
// Returns nil if no pre-spawned runner exists for this engine
func GetPreSpawnedRunner(engine string, runtimeJS []byte, funcName string) (*jsengine.SubprocessRunner, error) {
	return defaultRuntime.GetPreSpawnedRunner(context.Background(), engine, runtimeJS, funcName)
}

func (r *Runtime) GetPreSpawnedRunner(ctx context.Context, engine string, runtimeJS []byte, funcName string) (*jsengine.SubprocessRunner, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}

	r.preSpawn.mu.Lock()
	defer r.preSpawn.mu.Unlock()

	if r.preSpawn.runner == nil || r.preSpawn.engine != engine {
		return nil, nil // No pre-spawned runner, caller should use NewSubprocessRunner
	}

	runner := r.preSpawn.runner
	r.preSpawn.runner = nil // Claim the runner
	r.preSpawn.engine = ""

	if err := runner.FinishPreload(runtimeJS, funcName); err != nil {
		return nil, err
	}

	return runner, nil
}
