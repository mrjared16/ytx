package ytx

import (
	"fmt"
	"os"
	"time"

	"github.com/mrjared16/ytx/internal/cache"
)

// CacheInfo describes the public cache state returned by the ytx API.
type CacheInfo struct {
	CacheDir           string    `json:"cache_dir"`
	CachePath          string    `json:"cache_path"`
	PlayerCachePath    string    `json:"player_cache_path"`
	NRuntimeCachePath  string    `json:"n_runtime_cache_path"`
	Valid              bool      `json:"valid"`
	PlayerURL          string    `json:"player_url,omitempty"`
	BaseJSPath         string    `json:"basejs_path,omitempty"`
	PlayerFingerprint  string    `json:"player_fingerprint,omitempty"`
	SigFunction        string    `json:"sig_function,omitempty"`
	NFunction          string    `json:"n_function,omitempty"`
	SignatureTimestamp int       `json:"signature_timestamp,omitempty"`
	HasPlayerJS        bool      `json:"has_player_js"`
	HasNRuntimeJS      bool      `json:"has_n_runtime_js"`
	HasExtractedSig    bool      `json:"has_extracted_sig"`
	CreatedAt          time.Time `json:"created_at,omitempty"`
	ExpiresAt          time.Time `json:"expires_at,omitempty"`
}

// PurgeCache is a standalone function to purge cache without needing an extractor.
func PurgeCache() error {
	cm, err := cache.NewCacheManager()
	if err != nil {
		return err
	}
	return cm.Purge()
}

// GetCacheDir returns the cache directory path.
func GetCacheDir() (string, error) {
	cm, err := cache.NewCacheManager()
	if err != nil {
		return "", err
	}
	return cm.CacheDir(), nil
}

func GetCacheInfo() (*CacheInfo, error) {
	cm, err := cache.NewCacheManager()
	if err != nil {
		return nil, err
	}

	info := &CacheInfo{
		CacheDir:          cm.CacheDir(),
		CachePath:         cm.CachePath(),
		PlayerCachePath:   cm.PlayerCachePath(),
		NRuntimeCachePath: cm.NRuntimeCachePath(),
	}

	c, err := cm.Load()
	if err != nil {
		if os.IsNotExist(err) {
			return info, nil
		}
		return nil, err
	}

	info.Valid = cm.IsValid(c)
	info.PlayerURL = c.PlayerURL
	info.BaseJSPath = c.BaseJSPath
	info.PlayerFingerprint = c.PlayerFingerprint
	info.SigFunction = c.SigFunction
	info.NFunction = c.NFunction
	info.SignatureTimestamp = c.SignatureTimestamp
	info.HasExtractedSig = c.JSCode != ""
	info.CreatedAt = c.CreatedAt
	info.ExpiresAt = c.ExpiresAt
	if _, err := os.Stat(cm.PlayerCachePath()); err == nil {
		info.HasPlayerJS = true
	}
	if _, err := os.Stat(cm.NRuntimeCachePath()); err == nil {
		info.HasNRuntimeJS = true
	}

	return info, nil
}

func RefreshPlayerCache(videoID string) (*CacheInfo, []string, error) {
	if len(videoID) != 11 {
		return nil, nil, fmt.Errorf("invalid video ID: %s", videoID)
	}

	CloseCachedEngine()
	var oldKey string
	defaultRuntime.cipherCache.Lock()
	if defaultRuntime.cipherCache.cipher != nil {
		oldKey = defaultRuntime.cipherCache.cipher.engineCacheKey()
		defaultRuntime.cipherCache.cipher.Close()
	}
	defaultRuntime.cipherCache.cipher = nil
	defaultRuntime.cipherCache.expiry = time.Time{}
	defaultRuntime.cipherCache.updated = time.Time{}
	defaultRuntime.cipherCache.Unlock()
	if oldKey != "" {
		defaultRuntime.CloseCachedEngineKey(oldKey)
	}

	ext, err := NewExtractor(ModeVideo, "")
	if err != nil {
		return nil, nil, err
	}
	if ext.cacheManager == nil {
		return nil, nil, fmt.Errorf("cache manager unavailable")
	}
	if err := ext.cacheManager.Purge(); err != nil {
		return nil, nil, fmt.Errorf("failed to purge cache: %w", err)
	}

	cipher, err := ext.fetchAndCacheCipher(videoID)
	if err != nil {
		return nil, nil, err
	}
	if cipher.canPrecomputeSignatureRuntime() {
		if err := cipher.ensureSignatureReady(); err == nil {
			ext.cipher = cipher
			ext.persistCipherArtifacts()
		}
	}
	info, err := GetCacheInfo()
	if err != nil {
		return nil, cipher.Warnings(), err
	}
	return info, cipher.Warnings(), nil
}
