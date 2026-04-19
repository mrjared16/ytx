package ytx

import (
	"context"
	"testing"

	"github.com/mrjared16/ytx/internal/jsengine"
)

func TestGetCachedEngineKeysByPlayerIdentity(t *testing.T) {
	old := GetEngineType()
	SetEngineType(jsengine.EngineQuickJS)
	defer SetEngineType(old)
	defer CloseCachedEngine()

	runtimeJS := []byte(`function fn(n){return n.slice(1)};_exposed['fn']=fn;`)

	engineA1, err := GetCachedEngine("player-a", runtimeJS, "fn")
	if err != nil {
		t.Fatalf("GetCachedEngine first call failed: %v", err)
	}
	engineA2, err := GetCachedEngine("player-a", runtimeJS, "fn")
	if err != nil {
		t.Fatalf("GetCachedEngine second call failed: %v", err)
	}
	engineB, err := GetCachedEngine("player-b", runtimeJS, "fn")
	if err != nil {
		t.Fatalf("GetCachedEngine different key failed: %v", err)
	}

	if engineA1 != engineA2 {
		t.Fatal("expected same engine instance for identical player cache key")
	}
	if engineA1 == engineB {
		t.Fatal("expected distinct engine instances for different player cache keys")
	}
}

func TestCloseCachedEngineKeyEvictsSingleEntry(t *testing.T) {
	runtime := NewRuntime()
	runtime.SetEngineType(jsengine.EngineQuickJS)
	defer runtime.CloseCachedEngine()

	runtimeJS := []byte(`function fn(n){return n.slice(1)};_exposed['fn']=fn;`)

	engineA1, err := runtime.GetCachedEngine(context.Background(), "player-a", runtimeJS, "fn")
	if err != nil {
		t.Fatalf("GetCachedEngine player-a failed: %v", err)
	}
	engineB1, err := runtime.GetCachedEngine(context.Background(), "player-b", runtimeJS, "fn")
	if err != nil {
		t.Fatalf("GetCachedEngine player-b failed: %v", err)
	}

	runtime.CloseCachedEngineKey("player-a")

	engineA2, err := runtime.GetCachedEngine(context.Background(), "player-a", runtimeJS, "fn")
	if err != nil {
		t.Fatalf("GetCachedEngine player-a after eviction failed: %v", err)
	}
	engineB2, err := runtime.GetCachedEngine(context.Background(), "player-b", runtimeJS, "fn")
	if err != nil {
		t.Fatalf("GetCachedEngine player-b after eviction failed: %v", err)
	}

	if engineA1 == engineA2 {
		t.Fatal("expected player-a engine to be recreated after key eviction")
	}
	if engineB1 != engineB2 {
		t.Fatal("expected player-b engine to remain cached")
	}
}
