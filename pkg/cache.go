package ytx

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
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
	Version            int       `json:"version"`
	CreatedAt          time.Time `json:"created_at"`
	ExpiresAt          time.Time `json:"expires_at"`
	PlayerURL          string    `json:"player_url"` // e.g., /s/player/xxx/base.js
	PlayerFingerprint  string    `json:"player_fingerprint,omitempty"`
	BaseJSPath         string    `json:"basejs_path"` // Cached base.js path for fast refresh
	SigFunction        string    `json:"sig_function"`
	SigParam           int       `json:"sig_param"`
	SigUsesURLWrapper  bool      `json:"sig_uses_url_wrapper,omitempty"`
	NFunction          string    `json:"n_function"`
	SignatureTimestamp int       `json:"signature_timestamp"` // STS for API requests
	JSCode             string    `json:"js_code"`
}

// CacheManager handles persistent cipher caching
type CacheManager struct {
	cacheDir string
}

type CacheInfo struct {
	CacheDir           string    `json:"cache_dir"`
	CachePath          string    `json:"cache_path"`
	PlayerCachePath    string    `json:"player_cache_path"`
	Valid              bool      `json:"valid"`
	PlayerURL          string    `json:"player_url,omitempty"`
	BaseJSPath         string    `json:"basejs_path,omitempty"`
	PlayerFingerprint  string    `json:"player_fingerprint,omitempty"`
	SigFunction        string    `json:"sig_function,omitempty"`
	NFunction          string    `json:"n_function,omitempty"`
	SignatureTimestamp int       `json:"signature_timestamp,omitempty"`
	HasPlayerJS        bool      `json:"has_player_js"`
	HasExtractedSig    bool      `json:"has_extracted_sig"`
	CreatedAt          time.Time `json:"created_at,omitempty"`
	ExpiresAt          time.Time `json:"expires_at,omitempty"`
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
const currentCacheVersion = 5

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

func GetCacheInfo() (*CacheInfo, error) {
	cm, err := NewCacheManager()
	if err != nil {
		return nil, err
	}

	info := &CacheInfo{
		CacheDir:        cm.CacheDir(),
		CachePath:       cm.CachePath(),
		PlayerCachePath: cm.PlayerCachePath(),
	}

	cache, err := cm.Load()
	if err != nil {
		if os.IsNotExist(err) {
			return info, nil
		}
		return nil, err
	}

	info.Valid = cm.IsValid(cache)
	info.PlayerURL = cache.PlayerURL
	info.BaseJSPath = cache.BaseJSPath
	info.PlayerFingerprint = cache.PlayerFingerprint
	info.SigFunction = cache.SigFunction
	info.NFunction = cache.NFunction
	info.SignatureTimestamp = cache.SignatureTimestamp
	info.HasExtractedSig = cache.JSCode != ""
	info.CreatedAt = cache.CreatedAt
	info.ExpiresAt = cache.ExpiresAt
	if _, err := os.Stat(cm.PlayerCachePath()); err == nil {
		info.HasPlayerJS = true
	}

	return info, nil
}

func RefreshPlayerCache(videoID string) (*CacheInfo, []string, error) {
	if len(videoID) != 11 {
		return nil, nil, fmt.Errorf("invalid video ID: %s", videoID)
	}

	CloseCachedEngine()
	cipherCache.Lock()
	cipherCache.cipher = nil
	cipherCache.expiry = time.Time{}
	cipherCache.Unlock()

	ext, err := NewExtractor(ModeVideo, "")
	if err != nil {
		return nil, nil, err
	}
	if ext.cacheManager == nil {
		return nil, nil, fmt.Errorf("cache manager unavailable")
	}
	_ = ext.cacheManager.Purge()

	cipher, err := ext.fetchAndCacheCipher(videoID)
	if err != nil {
		return nil, nil, err
	}
	if err := cipher.ensureSignatureReady(); err == nil {
		ext.cipher = cipher
		ext.persistCipherArtifacts()
	}
	info, err := GetCacheInfo()
	if err != nil {
		return nil, cipher.Warnings(), err
	}
	return info, cipher.Warnings(), nil
}
