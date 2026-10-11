package ytx

import (
	"context"
	"sync"
	"time"

	"github.com/mrjared16/ytx/internal/jsengine"

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
	cachedEngines  map[string]jsengine.JSEngine
	lastEngineName string
	// generation is the wholesale engine-cache epoch, bumped by
	// CloseCachedEngine. keyGenerations tracks per-key epochs, bumped by
	// CloseCachedEngineKey even when no entry is cached, so a pending
	// URL-wrapper creation can never publish into a closed cache.
	generation     int64
	keyGenerations map[string]int64
}

type runtimePreSpawn struct {
	mu     sync.Mutex
	runner *jsengine.SubprocessRunner
	engine string
	// disabled prevents any further legacy pre-spawned runners: the modern
	// URL-wrapper protocol creates its own subprocess and must never leave
	// an idle, unconsumed legacy runner behind.
	disabled bool
}

// Runtime owns the process-wide shared services that are intentionally reused
// across extractors for speed: caches, JS engine pool, pre-spawned runner, and
// singleflight guards for cold-path work.
type Runtime struct {
	engineMu   sync.RWMutex
	engineType jsengine.EngineType

	cipherCache  *runtimeCipherCache
	visitorCache *runtimeVisitorCache
	engineCache  *runtimeEngineCache
	preSpawn     *runtimePreSpawn

	// Test-only URL-wrapper creation barriers (nil in production). When set,
	// the creation callback signals urlWrapperTestSignal, then blocks on
	// urlWrapperTestGate (bounded by urlWrapperCreateTimeout) before spawning
	// the subprocess. Used to control creator/waiter interleavings in tests.
	urlWrapperTestSignal chan int
	urlWrapperTestGate   chan int

	visitorGroup singleflight.Group
	cipherGroup  singleflight.Group
	engineGroup  singleflight.Group
	// urlGroup guards ONLY URL-wrapper creation flights. Legacy
	// GetCachedEngine keys are caller-chosen arbitrary strings and share
	// engineGroup, so a legacy key equal to a derived URL flight key must
	// never make URL creation join a foreign flight.
	urlGroup singleflight.Group
}

// NewRuntime creates an isolated shared runtime. Extractors use a package-level
// singleton by default, but tests and advanced callers can inject their own.
func NewRuntime() *Runtime {
	return &Runtime{
		engineType:   jsengine.EngineAuto,
		cipherCache:  &runtimeCipherCache{},
		visitorCache: &runtimeVisitorCache{},
		engineCache: &runtimeEngineCache{
			cachedEngines:  make(map[string]jsengine.JSEngine),
			keyGenerations: make(map[string]int64),
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
