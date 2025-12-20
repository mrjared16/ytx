package ytx

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	cacheFileName   = "cipher.json"
	playerCacheFile = "player.js.gz" // Compressed player.js for n-transform
	cacheTTL        = 6 * time.Hour
)

// CipherCache represents the persisted cipher data
type CipherCache struct {
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	PlayerURL   string    `json:"player_url"`  // e.g., /s/player/xxx/base.js
	BaseJSPath  string    `json:"basejs_path"` // Cached base.js path for fast refresh
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

// currentCacheVersion is the current cache format version
// Increment when cache structure changes to invalidate old caches
const currentCacheVersion = 2

// IsValid checks if a cache entry is still valid (not expired and correct version)
func (cm *CacheManager) IsValid(cache *CipherCache) bool {
	if cache == nil {
		return false
	}
	// Invalidate old cache versions
	if cache.Version < currentCacheVersion {
		return false
	}
	return time.Now().Before(cache.ExpiresAt)
}

// PlayerCachePath returns the path to the compressed player.js cache
func (cm *CacheManager) PlayerCachePath() string {
	return filepath.Join(cm.cacheDir, playerCacheFile)
}

// SavePlayerJS saves player.js compressed with gzip
func (cm *CacheManager) SavePlayerJS(playerJS []byte) error {
	tmpPath := cm.PlayerCachePath() + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gzw := gzip.NewWriter(f)
	if _, err := gzw.Write(playerJS); err != nil {
		return err
	}
	if err := gzw.Close(); err != nil {
		return err
	}

	return os.Rename(tmpPath, cm.PlayerCachePath())
}

// LoadPlayerJS loads the compressed player.js cache
func (cm *CacheManager) LoadPlayerJS() ([]byte, error) {
	f, err := os.Open(cm.PlayerCachePath())
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gzr.Close()

	return io.ReadAll(gzr)
}

// Purge removes all cache files (cipher.json and player.js.gz)
func (cm *CacheManager) Purge() error {
	var lastErr error

	// Remove cipher cache
	if err := os.Remove(cm.CachePath()); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	// Remove player.js cache
	if err := os.Remove(cm.PlayerCachePath()); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	return lastErr
}

// CacheDir returns the cache directory path
func (cm *CacheManager) CacheDir() string {
	return cm.cacheDir
}

// PurgeCache is a standalone function to purge cache without needing an extractor
func PurgeCache() error {
	cm, err := NewCacheManager()
	if err != nil {
		return err
	}
	return cm.Purge()
}

// GetCacheDir returns the cache directory path
func GetCacheDir() (string, error) {
	cm, err := NewCacheManager()
	if err != nil {
		return "", err
	}
	return cm.CacheDir(), nil
}
