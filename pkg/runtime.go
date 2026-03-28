package ytx

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type runtimeCipherCache struct {
	sync.RWMutex
	cipher  *Cipher
	expiry  time.Time
	updated time.Time
}

type runtimeVisitorCache struct {
	sync.RWMutex
	data         string
	sessionIndex string
	delegatedSID string
	isAuth       bool
	authKey      string
	expiry       time.Time
	updated      time.Time
}

type runtimeEngineCache struct {
	mu             sync.Mutex
	cachedEngines  map[string]JSEngine
	lastEngineName string
}

type runtimePreSpawn struct {
	mu     sync.Mutex
	runner *SubprocessRunner
	engine string
}

// Runtime owns the process-wide shared services that are intentionally reused
// across extractors for speed: caches, JS engine pool, pre-spawned runner, and
// singleflight guards for cold-path work.
type Runtime struct {
	engineMu   sync.RWMutex
	engineType EngineType

	cipherCache  *runtimeCipherCache
	visitorCache *runtimeVisitorCache
	engineCache  *runtimeEngineCache
	preSpawn     *runtimePreSpawn

	visitorGroup singleflight.Group
	cipherGroup  singleflight.Group
	engineGroup  singleflight.Group
}

// NewRuntime creates an isolated shared runtime. Extractors use a package-level
// singleton by default, but tests and advanced callers can inject their own.
func NewRuntime() *Runtime {
	return &Runtime{
		engineType:   EngineAuto,
		cipherCache:  &runtimeCipherCache{},
		visitorCache: &runtimeVisitorCache{},
		engineCache: &runtimeEngineCache{
			cachedEngines: make(map[string]JSEngine),
		},
		preSpawn: &runtimePreSpawn{},
	}
}

var defaultRuntime = NewRuntime()

func backgroundRefreshContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}
