package cache

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadVisitorDataAllowStaleWithinGrace(t *testing.T) {
	cm := &CacheManager{cacheDir: t.TempDir()}
	stored := &VisitorCache{
		Data:         "VISITOR",
		SessionIndex: "3",
		DelegatedSID: "DELEGATED",
		IsAuth:       true,
		AuthKey:      "music:test",
		ExpiresAt:    time.Now().Add(-time.Minute),
	}
	if err := cm.SaveVisitorData(stored); err != nil {
		t.Fatalf("SaveVisitorData failed: %v", err)
	}

	loaded, stale, err := cm.LoadVisitorDataAllowStale(2 * time.Minute)
	if err != nil {
		t.Fatalf("LoadVisitorDataAllowStale failed: %v", err)
	}
	if !stale {
		t.Fatalf("expected stale=true")
	}
	if loaded == nil || loaded.Data != stored.Data || loaded.AuthKey != stored.AuthKey {
		t.Fatalf("unexpected visitor cache: %#v", loaded)
	}

	if _, stale, err := cm.LoadVisitorDataAllowStale(30 * time.Second); err == nil || stale {
		t.Fatalf("expected expired cache outside grace to fail, got stale=%v err=%v", stale, err)
	}
}

func TestSaveAndLoadNRuntimeJS(t *testing.T) {
	cm := &CacheManager{cacheDir: t.TempDir()}
	want := []byte("console.log('n-runtime');")
	if err := cm.SaveNRuntimeJS(want); err != nil {
		t.Fatalf("SaveNRuntimeJS failed: %v", err)
	}
	got, err := cm.LoadNRuntimeJS()
	if err != nil {
		t.Fatalf("LoadNRuntimeJS failed: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("unexpected n-runtime contents: got %q want %q", got, want)
	}
}

func TestCacheValidityHelpersSeparateCompatibilityAndFreshness(t *testing.T) {
	cm := &CacheManager{cacheDir: t.TempDir()}
	now := time.Now()
	cache := &CipherCache{
		Version:   CurrentCacheVersion,
		ExpiresAt: now.Add(-time.Minute),
	}
	if !cm.IsCompatible(cache) {
		t.Fatal("expected current version cache to be compatible")
	}
	if cm.IsFresh(cache) {
		t.Fatal("expected expired cache to be stale")
	}
	if cm.IsValid(cache) {
		t.Fatal("expected expired cache to be invalid")
	}

	if cm.IsCompatible(&CipherCache{Version: CurrentCacheVersion - 1, ExpiresAt: now.Add(time.Hour)}) {
		t.Fatal("expected older cache version to be incompatible")
	}
}

func TestWrapperCacheIgnoresPersistedCodeArtifact(t *testing.T) {
	cm := &CacheManager{cacheDir: t.TempDir()}
	cache := &CipherCache{
		Version:           CurrentCacheVersion,
		CreatedAt:         time.Now(),
		ExpiresAt:         time.Now().Add(time.Hour),
		SigFunction:       "kS",
		SigUsesURLWrapper: true,
		JSCode:            "wrapper-runtime-should-not-persist",
	}
	if err := cm.Save(cache); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cm.cacheDir, "cipher_code.bin")); !os.IsNotExist(err) {
		t.Fatalf("expected wrapper cache save to avoid cipher_code.bin, got err=%v", err)
	}

	// Simulate a stale legacy artifact on disk; wrapper-mode loads must ignore it.
	if err := os.WriteFile(filepath.Join(cm.cacheDir, "cipher_code.bin"), []byte("legacy-runtime"), 0644); err != nil {
		t.Fatalf("failed to write legacy code artifact: %v", err)
	}

	loaded, err := cm.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded.JSCode != "" {
		t.Fatalf("expected wrapper cache load to ignore code artifact, got %q", loaded.JSCode)
	}
}

func TestSaveAndLoadWrapperRuntimeBytecode(t *testing.T) {
	cm := &CacheManager{cacheDir: t.TempDir()}
	want := []byte{0x01, 0x02, 0x03, 0x04}
	if err := cm.SaveWrapperRuntimeBytecode(want); err != nil {
		t.Fatalf("SaveWrapperRuntimeBytecode failed: %v", err)
	}
	got, err := cm.LoadWrapperRuntimeBytecode()
	if err != nil {
		t.Fatalf("LoadWrapperRuntimeBytecode failed: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("unexpected wrapper bytecode contents: got %v want %v", got, want)
	}
}

func TestLoadPOTokenLegacyTokenFallback(t *testing.T) {
	cm := &CacheManager{cacheDir: t.TempDir()}
	legacy := `{"token":"LEGACY","video_id":"vid","expires_at":"2099-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(cm.cacheDir, poCacheFile), []byte(legacy), 0644); err != nil {
		t.Fatalf("failed to write legacy cache: %v", err)
	}

	loaded, err := cm.LoadPOToken("vid", "", "")
	if err != nil {
		t.Fatalf("LoadPOToken failed: %v", err)
	}
	if loaded.PlayerToken != "LEGACY" || loaded.URLToken != "LEGACY" {
		t.Fatalf("expected legacy token to populate split fields, got %#v", loaded)
	}
}

func TestLoadPOTokenFromLayeredStoreByBinding(t *testing.T) {
	cm := &CacheManager{cacheDir: t.TempDir()}
	store := poTokenCacheStore{
		Version: POTokenCacheVersion,
		Entries: map[string]POTokenCache{
			poTokenCacheKey("vid-a", "visitor-a", "0", ""): {
				PlayerToken:  "PLAYER_A",
				URLToken:     "URL_A",
				VideoID:      "vid-a",
				VisitorData:  "visitor-a",
				SessionIndex: "0",
				ExpiresAt:    time.Now().Add(10 * time.Minute),
			},
			poTokenCacheKey("vid-b", "visitor-b", "0", ""): {
				PlayerToken:  "PLAYER_B",
				URLToken:     "URL_B",
				VideoID:      "vid-b",
				VisitorData:  "visitor-b",
				SessionIndex: "0",
				ExpiresAt:    time.Now().Add(10 * time.Minute),
			},
		},
	}

	data, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("failed to marshal store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cm.cacheDir, poCacheFile), data, 0644); err != nil {
		t.Fatalf("failed to write layered po cache: %v", err)
	}

	loaded, err := cm.LoadPOToken("vid-b", "visitor-b", "0")
	if err != nil {
		t.Fatalf("LoadPOToken failed: %v", err)
	}
	if loaded.PlayerToken != "PLAYER_B" || loaded.URLToken != "URL_B" {
		t.Fatalf("expected B entry, got %#v", loaded)
	}
}
