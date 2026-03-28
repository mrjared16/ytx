package ytx

import (
	"bytes"
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
