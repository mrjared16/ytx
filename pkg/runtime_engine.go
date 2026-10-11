package ytx

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

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
	closing := make([]jsengine.JSEngine, 0, len(r.engineCache.cachedEngines))
	for key, engine := range r.engineCache.cachedEngines {
		if engine != nil {
			closing = append(closing, engine)
		}
		delete(r.engineCache.cachedEngines, key)
	}
	r.engineCache.lastEngineName = ""
	// Wholesale epoch: pending URL-wrapper creations registered before this
	// close can never publish; post-close callers derive new flight keys.
	r.engineCache.generation++
	r.engineCache.keyGenerations = make(map[string]int64)
	r.engineCache.mu.Unlock()

	for _, engine := range closing {
		engine.Close()
	}
}

func (r *Runtime) CloseCachedEngineKey(cacheKey string) {
	if cacheKey == "" {
		return
	}

	r.engineCache.mu.Lock()
	var closing jsengine.JSEngine
	if engine, ok := r.engineCache.cachedEngines[cacheKey]; ok {
		// Close via the JSEngine interface: cache keys are shared with the
		// legacy engine pool, which may hold non-subprocess engines.
		closing = engine
		delete(r.engineCache.cachedEngines, cacheKey)
		if r.engineCache.lastEngineName != "" && r.engineCache.lastEngineName == engine.Name() {
			r.engineCache.lastEngineName = ""
		}
	}
	// Advance the per-key epoch even when nothing is cached: a pending
	// URL-wrapper creation for this key must not publish after the close.
	if n, ok := r.engineCache.keyGenerations[cacheKey]; ok {
		r.engineCache.keyGenerations[cacheKey] = n + 1
	} else {
		r.engineCache.keyGenerations[cacheKey] = 1
	}
	r.engineCache.mu.Unlock()

	if closing != nil {
		closing.Close()
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

	// Modern URL-wrapper protocol players disable legacy pre-spawn outright
	// (see DiscardPreSpawnedRunner); never start an idle unconsumed runner.
	if r.preSpawn.disabled {
		return nil
	}

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

// urlWrapperRunnerCacheKey derives the runtime cache key for the modern
// URL-wrapper protocol. The key always covers protocol + wrapper name +
// playerURL + playerFingerprint so same-URL/different-player caches cannot
// collide.
func urlWrapperRunnerCacheKey(playerURL, playerFingerprint, wrapperName string) string {
	return "urlw|" + wrapperName + "|" + playerURL + "|" + playerFingerprint
}

// urlWrapperCreateTimeout bounds the detached creation goroutine.
const urlWrapperCreateTimeout = 30 * time.Second

// GetURLWrapperRunner returns a (cached) subprocess runner for the modern
// URL-wrapper protocol.
//
// The live cache recheck and the flight registration are atomic under
// engineCache.mu: a concurrent creator can only publish while holding that
// lock, so no interleaving can orphan a flight or overwrite a live owner.
// The flight itself uses a DEDICATED singleflight group (urlGroup) — never
// the legacy engineGroup, whose caller-chosen keys are arbitrary strings that
// could collide with a derived flight key — keyed by the protocol cache
// identity plus the wholesale and per-key generations:
// the first caller spawns one bounded, detached creation callback (raw
// source exposure happens there, once), every caller waits on its own
// channel under its own context, and a canceled caller leaves promptly
// without harming anyone. On completion the callback re-checks the
// generations under the cache lock: a retired flight (cache closed while
// creating) closes its runner and reports an error, and a duplicate against
// a live owner is closed while the owner is reused. A cached runner that has
// been terminated (IsClosed) is evicted (and closed, same instance) then
// recreated.
func (r *Runtime) GetURLWrapperRunner(ctx context.Context, playerJS []byte, playerURL, playerFingerprint, wrapperName string) (*jsengine.SubprocessRunner, error) {
	if wrapperName == "" {
		return nil, fmt.Errorf("url wrapper name must not be empty")
	}
	// Already canceled: leave promptly without registering any creation.
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	cacheKey := urlWrapperRunnerCacheKey(playerURL, playerFingerprint, wrapperName)

	// Live recheck + flight registration, atomic under engineCache.mu.
	r.engineCache.mu.Lock()
	var retire jsengine.JSEngine
	if engine, ok := r.engineCache.cachedEngines[cacheKey]; ok {
		if runner, isSub := engine.(*jsengine.SubprocessRunner); isSub {
			if runner.IsClosed() {
				// Terminated cached runner: evict it, close the same instance
				// outside the lock, and create a replacement below.
				delete(r.engineCache.cachedEngines, cacheKey)
				retire = runner
			} else if !runner.SupportsURLTransform() {
				// A unary-mode subprocess runner (e.g. a caller-chosen
				// legacy key collision) is parked at the derived URL-wrapper
				// key and cannot serve the protocol. Retire the same
				// instance outside the lock and build a URL-mode replacement
				// below.
				delete(r.engineCache.cachedEngines, cacheKey)
				retire = runner
			} else {
				r.engineCache.mu.Unlock()
				return runner, nil
			}
		} else {
			// A non-subprocess engine (e.g. a caller-chosen legacy key
			// collision) is parked at the derived URL-wrapper key and cannot
			// serve the protocol. Retire the same instance outside the lock
			// and rebuild a subprocess runner below — never a panic on the
			// type assertion.
			delete(r.engineCache.cachedEngines, cacheKey)
			retire = engine
		}
	}

	gen := r.engineCache.generation
	var keyGen int64
	if n, ok := r.engineCache.keyGenerations[cacheKey]; ok {
		keyGen = n
	}
	flightKey := urlWrapperFlightKey(cacheKey, gen, keyGen)
	ch := r.urlGroup.DoChan(flightKey, func() (any, error) {
		return r.urlWrapperCreate(cacheKey, gen, keyGen, playerJS, wrapperName)
	})
	r.engineCache.mu.Unlock()

	if retire != nil {
		retire.Close()
	}

	waitParent := ctx
	if waitParent == nil {
		waitParent = context.Background()
	}
	waitCtx, waitCancel := context.WithTimeout(waitParent, urlWrapperCreateTimeout)
	defer waitCancel()

	select {
	case <-waitCtx.Done():
		return nil, waitCtx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*jsengine.SubprocessRunner), nil
	}
}

// urlWrapperFlightKey identifies one URL-wrapper creation flight: the
// protocol cache key plus the wholesale and per-key generations it was
// registered under. A post-close caller always derives a NEW flight key, so
// it never joins — and is never consumed by — an older pending flight.
func urlWrapperFlightKey(cacheKey string, wholesale, perKey int64) string {
	return fmt.Sprintf("%s|g%d.k%d", cacheKey, wholesale, perKey)
}

// urlWrapperCreate runs as the singleflight callback: it is detached from
// any caller context (bounded by urlWrapperCreateTimeout), prepares the
// exposure from the RAW player source, spawns the subprocess runner, and
// publishes it only if its flight is still current.
func (r *Runtime) urlWrapperCreate(cacheKey string, gen, keyGen int64, playerJS []byte, wrapperName string) (any, error) {
	bound, cancel := context.WithTimeout(context.Background(), urlWrapperCreateTimeout)
	defer cancel()

	// Test barriers (private, off by default): deterministic control of the
	// creator/waiter interleavings in tests, bounded so a test that forgets
	// to open the gate cannot hang production logic indefinitely.
	if r.urlWrapperTestSignal != nil {
		r.urlWrapperTestSignal <- 1
	}
	if r.urlWrapperTestGate != nil {
		select {
		case <-r.urlWrapperTestGate:
		case <-time.After(urlWrapperCreateTimeout):
		}
	}

	// Production-path source preparation: the caller always supplies the RAW
	// player bundle; the exposure is inserted here, exactly once, before the
	// subprocess is constructed.
	exposed, err := exposeURLWrapperSource(playerJS, wrapperName)
	if err != nil {
		return nil, err
	}
	runner, err := r.newURLWrapperRunner(bound, []byte(exposed), wrapperName)
	if err != nil {
		return nil, err
	}

	r.engineCache.mu.Lock()
	if r.engineCache.generation != gen {
		// The wholesale cache was closed while this runner was being
		// created: never publish into the closed cache; retire the runner.
		r.engineCache.mu.Unlock()
		runner.Close()
		return nil, fmt.Errorf("url wrapper runner creation aborted: engine cache was closed")
	}
	var keyGenNow int64
	if n, ok := r.engineCache.keyGenerations[cacheKey]; ok {
		keyGenNow = n
	}
	if keyGenNow != keyGen {
		r.engineCache.mu.Unlock()
		runner.Close()
		return nil, fmt.Errorf("url wrapper runner creation aborted: %q was closed while creating", cacheKey)
	}
	var retire jsengine.JSEngine
	if engine, ok := r.engineCache.cachedEngines[cacheKey]; ok {
		if existing, isSub := engine.(*jsengine.SubprocessRunner); isSub && !existing.IsClosed() && existing.SupportsURLTransform() {
			// Another live URL-mode owner already published for this flight:
			// never overwrite it; close the duplicate and reuse the owner.
			r.engineCache.mu.Unlock()
			runner.Close()
			return existing, nil
		}
		// A runner that cannot serve the protocol — a unary-mode subprocess,
		// a terminated runner, or a non-subprocess engine — arrived at the
		// URL-wrapper key while this flight was creating (caller-chosen key
		// collision): it cannot serve the protocol. Retire the same instance
		// outside the lock and publish the URL-mode runner below — never a
		// panic on the type assertion.
		retire = engine
		delete(r.engineCache.cachedEngines, cacheKey)
	}
	r.engineCache.cachedEngines[cacheKey] = runner
	r.engineCache.lastEngineName = runner.Name()
	r.engineCache.mu.Unlock()
	if retire != nil {
		retire.Close()
	}
	return runner, nil
}

// EvictURLWrapperRunner removes the exact cached runner instance (identity
// checked) from the URL-wrapper cache and closes that same instance outside
// the cache lock. Used after a terminal call failure so the next call
// rebuilds a fresh subprocess (and its unique temp directory is released).
func (r *Runtime) EvictURLWrapperRunner(cacheKey string, runner *jsengine.SubprocessRunner) {
	r.engineCache.mu.Lock()
	var closing *jsengine.SubprocessRunner
	if engine, ok := r.engineCache.cachedEngines[cacheKey]; ok && engine == runner {
		delete(r.engineCache.cachedEngines, cacheKey)
		closing = runner
	}
	r.engineCache.mu.Unlock()
	if closing != nil {
		closing.Close()
	}
}

// DiscardPreSpawnedRunner releases any parked legacy pre-spawned subprocess
// and prevents future legacy pre-spawns on this runtime (modern URL-wrapper
// players never consume the legacy runner, so it must not stay idle).
func (r *Runtime) DiscardPreSpawnedRunner() {
	r.preSpawn.mu.Lock()
	defer r.preSpawn.mu.Unlock()
	r.preSpawn.disabled = true
	if r.preSpawn.runner != nil {
		r.preSpawn.runner.Close()
		r.preSpawn.runner = nil
	}
	r.preSpawn.engine = ""
}

// newURLWrapperRunner creates a URL-wrapper subprocess runner for the current
// engine type. Auto resolves Bun then Node; explicit bun/node are respected;
// QuickJS is rejected outright for the URL-wrapper protocol.
func (r *Runtime) newURLWrapperRunner(ctx context.Context, playerJS []byte, wrapperName string) (*jsengine.SubprocessRunner, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	switch r.GetEngineType() {
	case jsengine.EngineBun:
		return jsengine.NewURLWrapperRunnerContext(ctx, "bun", playerJS, wrapperName)
	case jsengine.EngineNode:
		return jsengine.NewURLWrapperRunnerContext(ctx, "node", playerJS, wrapperName)
	case jsengine.EngineQuickJS:
		return nil, fmt.Errorf("url wrapper protocol requires a bun/node runtime; native QuickJS is not supported for this mode")
	case jsengine.EngineAuto:
		var bunErr error
		var nodeErr error
		if runner, err := jsengine.NewURLWrapperRunnerContext(ctx, "bun", playerJS, wrapperName); err == nil {
			return runner, nil
		} else {
			bunErr = err
		}
		if runner, err := jsengine.NewURLWrapperRunnerContext(ctx, "node", playerJS, wrapperName); err == nil {
			return runner, nil
		} else {
			nodeErr = err
		}
		if strings.Contains(bunErr.Error(), "not found") && strings.Contains(nodeErr.Error(), "not found") {
			return nil, fmt.Errorf("url wrapper protocol requires bun or node, neither is available")
		}
		return nil, fmt.Errorf("url wrapper runner failed (bun: %v; node: %v)", bunErr, nodeErr)
	default:
		return nil, fmt.Errorf("unknown engine type for url wrapper protocol: %s", r.GetEngineType())
	}
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
