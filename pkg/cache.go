package ytx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const (
	cacheFileName = "cipher.json"
	cacheTTL      = 6 * time.Hour
)

// CipherCache represents the persisted cipher data
type CipherCache struct {
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	PlayerURL   string    `json:"player_url"`
	SigFunction string    `json:"sig_function"`
	SigParam    int       `json:"sig_param"`
	NFunction   string    `json:"n_function"`
	JSCode      string    `json:"js_code"`
}

// CacheManager handles persistent cipher caching
type CacheManager struct {
	cacheDir string
}

// NewCacheManager creates a cache manager using XDG cache directory
func NewCacheManager() (*CacheManager, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		// Fallback to home directory
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return nil, err
		}
		cacheDir = filepath.Join(home, ".cache")
	}

	dir := filepath.Join(cacheDir, "ytx")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	return &CacheManager{cacheDir: dir}, nil
}

// CachePath returns the full path to the cache file
func (cm *CacheManager) CachePath() string {
	return filepath.Join(cm.cacheDir, cacheFileName)
}

// Load reads the cipher cache from disk
func (cm *CacheManager) Load() (*CipherCache, error) {
	data, err := os.ReadFile(cm.CachePath())
	if err != nil {
		return nil, err
	}

	var cache CipherCache
	if err := json.Unmarshal(data, &cache); err != nil {
		// Corrupted cache - delete it
		_ = cm.Invalidate()
		return nil, err
	}

	return &cache, nil
}

// Save writes the cipher cache to disk
func (cm *CacheManager) Save(cache *CipherCache) error {
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}

	// Write atomically: write to temp file, then rename
	tmpPath := cm.CachePath() + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}

	return os.Rename(tmpPath, cm.CachePath())
}

// Invalidate removes the cache file
func (cm *CacheManager) Invalidate() error {
	err := os.Remove(cm.CachePath())
	if os.IsNotExist(err) {
		return nil // Already gone
	}
	return err
}

// IsValid checks if a cache entry is still valid (not expired)
func (cm *CacheManager) IsValid(cache *CipherCache) bool {
	if cache == nil {
		return false
	}
	return time.Now().Before(cache.ExpiresAt)
}
