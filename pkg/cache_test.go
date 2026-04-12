package ytx

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
		Version: poTokenCacheVersion,
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
