package ytx

import "testing"

func TestGetCachedEngineKeysByPlayerIdentity(t *testing.T) {
	old := GetEngineType()
	SetEngineType(EngineQuickJS)
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
