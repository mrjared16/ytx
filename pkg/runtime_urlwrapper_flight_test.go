package ytx

import (
	"context"
	"testing"
	"time"

	"github.com/mrjared16/ytx/internal/jsengine"
)

// legacyStubEngine is a non-subprocess JSEngine used to represent the foreign
// value a held legacy (caller-chosen-key) flight would produce. Close signals
// the shared closed channel, proving the cache is the only owner that ever
// closes the stub (value copies all share the channel reference).
type legacyStubEngine struct {
	closed chan int
}

func (e legacyStubEngine) Name() string                                 { return "legacy-stub" }
func (e legacyStubEngine) TransformN(n string) (string, error)          { return n, nil }
func (e legacyStubEngine) TransformNBatch(v []string) ([]string, error) { return v, nil }
func (e legacyStubEngine) Close()                                       { e.closed <- 1 }

// TestURLWrapperFlightKeyNamespaceCollision pins the dedicated URL-wrapper
// singleflight group: a legacy caller-chosen cache key that equals a derived
// URL flight key must not make URL creation join (and type-assert) a foreign
// legacy flight. URL creation proceeds independently while the legacy flight
// is still held, produces a valid URL-mode runner, and leaves the legacy
// flight's own value untouched for its waiters.
func TestURLWrapperFlightKeyNamespaceCollision(t *testing.T) {
	_ = urlWrapperTestEngine(t)

	runtime := NewRuntime()

	const playerURL = "/s/player/urlwtest/base.js"
	const fp = "urlwtest-fp"
	urlCacheKey := urlWrapperRunnerCacheKey(playerURL, fp, "jW")

	runtime.engineCache.mu.Lock()
	gen := runtime.engineCache.generation
	keyGen := runtime.engineCache.keyGenerations[urlCacheKey]
	runtime.engineCache.mu.Unlock()

	// A legacy caller may pass ANY string as its cache key; pick the exact
	// derived URL flight key to force the namespace collision.
	collidingKey := urlWrapperFlightKey(urlCacheKey, gen, keyGen)

	release := make(chan struct{})
	closed := make(chan int, 2)
	stub := legacyStubEngine{closed: closed}
	legacyDone := make(chan jsengine.JSEngine, 1)
	legacyCh := runtime.engineGroup.DoChan(collidingKey, func() (any, error) {
		<-release
		// Mirror the real legacy producer: publish under the caller's key
		// while holding engineCache.mu, then return that same instance. A
		// getter scheduled after release then hits the cache instead of
		// starting real creation.
		runtime.engineCache.mu.Lock()
		runtime.engineCache.cachedEngines[collidingKey] = stub
		runtime.engineCache.mu.Unlock()
		return stub, nil
	})
	go func() {
		res := <-legacyCh
		if res.Err != nil {
			t.Errorf("legacy flight: %v", res.Err)
			legacyDone <- nil
			return
		}
		engine, _ := res.Val.(jsengine.JSEngine)
		legacyDone <- engine
	}()

	// The public getter is NOT positively registered as a flight joiner —
	// only the synchronous DoChan above is. Its launch order is
	// nondeterministic: it either joins the held flight or lands after
	// release, and the producer above mirrors real legacy publication so
	// both orderings return the stub. The getter's ownership/cache
	// assertions below are outcomes, not registration proof.
	legacyGetDone := make(chan jsengine.JSEngine, 1)
	go func() {
		engine, err := runtime.GetCachedEngine(context.Background(), collidingKey, nil, "legacyFn")
		if err != nil {
			t.Errorf("legacy GetCachedEngine: %v", err)
			legacyGetDone <- nil
			return
		}
		legacyGetDone <- engine
	}()

	// URL creation with the colliding derived flight key must NOT join the
	// held legacy flight: it proceeds independently on a dedicated group.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner, err := runtime.GetURLWrapperRunner(ctx, urlWrapperTestPlayer("ok"), playerURL, fp, "jW")
	if err != nil {
		t.Fatalf("URL creation blocked on or failed from the legacy flight: %v", err)
	}
	if !runner.SupportsURLTransform() {
		t.Fatal("URL creation produced a non-URL runner")
	}
	if runner.IsClosed() {
		t.Fatal("URL runner closed immediately after creation")
	}

	// URL creation completed while the legacy flight was still held: the
	// legacy path must still be pending, untouched by the URL create.
	select {
	case engine := <-legacyGetDone:
		t.Fatalf("legacy GetCachedEngine completed before release: %v", engine)
	default:
	}

	close(release)
	if engine := <-legacyDone; engine == nil || engine.Name() != "legacy-stub" {
		t.Fatalf("legacy flight value not preserved: %v", engine)
	}
	if engine := <-legacyGetDone; engine == nil || engine.Name() != "legacy-stub" {
		t.Fatalf("legacy GetCachedEngine returned %v, want the held foreign engine", engine)
	}

	// Both cache owners unchanged: the legacy publish used the colliding
	// (generation) key, distinct from the URL cache key — neither check
	// displaced the other.
	runtime.engineCache.mu.Lock()
	cached := runtime.engineCache.cachedEngines[urlCacheKey]
	cachedStub := runtime.engineCache.cachedEngines[collidingKey]
	runtime.engineCache.mu.Unlock()
	if cached != runner {
		t.Fatal("URL runner was displaced in the cache after the legacy flight completed")
	}
	if cachedStub == nil || cachedStub.Name() != "legacy-stub" {
		t.Fatal("legacy publish did not land under the colliding key")
	}

	// Cleanup order matters: close once via the cache, then prove no other
	// owner closed the stub (exactly one Close signal).
	t.Cleanup(func() {
		runtime.CloseCachedEngine()
		select {
		case <-closed:
		default:
			t.Errorf("legacy stub was never closed")
		}
		select {
		case <-closed:
			t.Errorf("legacy stub closed more than once")
		default:
		}
	})
}
