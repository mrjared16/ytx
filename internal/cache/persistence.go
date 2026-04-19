package cache

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (cm *CacheManager) writeAtomic(path string, write func(*os.File) error) error {
	tmp, err := os.CreateTemp(cm.cacheDir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	if err := write(tmp); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return nil
}

// NewCacheManager creates a cache manager using XDG cache directory.
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

	return NewCacheManagerWithDir(dir), nil
}

// NewCacheManagerWithDir creates a CacheManager with a specific directory
func NewCacheManagerWithDir(dir string) *CacheManager {
	return &CacheManager{cacheDir: dir}
}

// CachePath returns the full path to the cache file.
func (cm *CacheManager) CachePath() string {
	return filepath.Join(cm.cacheDir, cacheFileName)
}

// PlayerCachePath returns the path to the compressed player.js cache.
func (cm *CacheManager) PlayerCachePath() string {
	return filepath.Join(cm.cacheDir, playerCacheFile)
}

// NRuntimeCachePath returns the path to the prepared n-runtime artifact.
func (cm *CacheManager) NRuntimeCachePath() string {
	return filepath.Join(cm.cacheDir, nRuntimeCacheFile)
}

func (cm *CacheManager) visitorCachePath() string {
	return filepath.Join(cm.cacheDir, visitorCacheFile)
}

func (cm *CacheManager) poCachePath() string {
	return filepath.Join(cm.cacheDir, poCacheFile)
}

func (cm *CacheManager) codeCachePath() string {
	return filepath.Join(cm.cacheDir, "cipher_code.bin")
}

func (cm *CacheManager) wrapperCodeCachePath() string {
	return filepath.Join(cm.cacheDir, wrapperCodeCacheFile)
}

// CacheDir returns the cache directory path.
func (cm *CacheManager) CacheDir() string {
	return cm.cacheDir
}

// Load reads the cipher cache from disk.
func (cm *CacheManager) Load() (*CipherCache, error) {
	data, err := os.ReadFile(cm.CachePath())
	if err != nil {
		return nil, err
	}

	var cache CipherCache
	if err := json.Unmarshal(data, &cache); err != nil {
		// Corrupted cache - delete it
		_ = cm.InvalidateCipherArtifacts()
		return nil, err
	}

	// Fast load the raw bytecode/js payload avoiding JSON overhead.
	// Wrapper-mode caches use raw player.js as the source of truth and must
	// never hydrate wrapper-mutated runtime code from cipher_code.bin.
	if !cache.SigUsesURLWrapper {
		codePath := cm.codeCachePath()
		if codeData, err := os.ReadFile(codePath); err == nil {
			cache.JSCode = string(codeData)
		}
	}

	return &cache, nil
}

// Save writes the cipher cache to disk.
func (cm *CacheManager) Save(cache *CipherCache) error {
	// Strip the massive JS payload from the JSON
	jsCode := cache.JSCode
	cache.JSCode = ""

	data, err := json.MarshalIndent(cache, "", "  ")

	// Restore memory format
	cache.JSCode = jsCode

	if err != nil {
		return err
	}

	if err := cm.writeAtomic(cm.CachePath(), func(f *os.File) error {
		_, err := f.Write(data)
		return err
	}); err != nil {
		return err
	}

	// Write the massive JS payload atomically to a raw binary file.
	// Wrapper-mode caches intentionally do not persist wrapper runtime here;
	// raw player.js.gz is the authoritative cached source instead.
	if jsCode != "" && !cache.SigUsesURLWrapper {
		_ = cm.writeAtomic(cm.codeCachePath(), func(f *os.File) error {
			_, err := f.Write([]byte(jsCode))
			return err
		})
	} else {
		_ = os.Remove(cm.codeCachePath())
	}

	return nil
}

// LoadVisitorData reads the visitor cache from disk.
func (cm *CacheManager) LoadVisitorData() (*VisitorCache, error) {
	cache, stale, err := cm.LoadVisitorDataAllowStale(0)
	if err != nil {
		return nil, err
	}
	if stale {
		return nil, fmt.Errorf("visitor cache expired")
	}
	return cache, nil
}

// LoadVisitorDataAllowStale reads the visitor cache and optionally accepts
// expired entries within a stale-while-revalidate grace period.
func (cm *CacheManager) LoadVisitorDataAllowStale(grace time.Duration) (*VisitorCache, bool, error) {
	data, err := os.ReadFile(cm.visitorCachePath())
	if err != nil {
		return nil, false, err
	}

	var cache VisitorCache
	if err := json.Unmarshal(data, &cache); err != nil {
		_ = os.Remove(cm.visitorCachePath())
		return nil, false, err
	}

	now := time.Now()
	if now.After(cache.ExpiresAt) {
		if grace <= 0 || now.After(cache.ExpiresAt.Add(grace)) {
			return nil, false, fmt.Errorf("visitor cache expired")
		}
		return &cache, true, nil
	}

	return &cache, false, nil
}

// SaveVisitorData writes the visitor cache to disk atomically.
func (cm *CacheManager) SaveVisitorData(cache *VisitorCache) error {
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}

	return cm.writeAtomic(cm.visitorCachePath(), func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

// LoadPOToken reads a cached PO token and validates binding + expiry.
func (cm *CacheManager) LoadPOToken(videoID, visitorData, sessionIndex string) (*POTokenCache, error) {
	data, err := os.ReadFile(cm.poCachePath())
	if err != nil {
		return nil, err
	}
	if store, ok := decodePOTokenStore(data); ok {
		key := poTokenCacheKey(videoID, visitorData, sessionIndex, "")
		if candidate, ok := store.Entries[key]; ok {
			cache := normalizePOTokenCache(candidate)
			if err := validatePOTokenCache(cache, videoID, visitorData, sessionIndex, ""); err == nil {
				return cache, nil
			}
		}

		for _, candidate := range store.Entries {
			cache := normalizePOTokenCache(candidate)
			if err := validatePOTokenCache(cache, videoID, visitorData, sessionIndex, ""); err == nil {
				return cache, nil
			}
		}
		return nil, fmt.Errorf("po token cache expired")
	}

	var cache POTokenCache
	if err := json.Unmarshal(data, &cache); err != nil {
		_ = os.Remove(cm.poCachePath())
		return nil, err
	}
	normalized := normalizePOTokenCache(cache)
	if err := validatePOTokenCache(normalized, videoID, visitorData, sessionIndex, ""); err != nil {
		return nil, err
	}

	return normalized, nil
}

// SavePOToken persists PO token data atomically.
func (cm *CacheManager) SavePOToken(cache *POTokenCache) error {
	if cache == nil {
		return nil
	}
	normalized := normalizePOTokenCache(*cache)
	if normalized.PlayerToken == "" || normalized.URLToken == "" {
		return nil
	}

	store := &poTokenCacheStore{Version: POTokenCacheVersion, Entries: map[string]POTokenCache{}}
	if existingData, err := os.ReadFile(cm.poCachePath()); err == nil {
		if existingStore, ok := decodePOTokenStore(existingData); ok {
			store = existingStore
		} else {
			var legacy POTokenCache
			if json.Unmarshal(existingData, &legacy) == nil {
				legacyNorm := normalizePOTokenCache(legacy)
				if legacyNorm.PlayerToken != "" && legacyNorm.URLToken != "" && time.Now().Before(legacyNorm.ExpiresAt) {
					legacyKey := poTokenCacheKey(legacyNorm.VideoID, legacyNorm.VisitorData, legacyNorm.SessionIndex, legacyNorm.NetworkKey)
					store.Entries[legacyKey] = *legacyNorm
				}
			}
		}
	}

	for key, entry := range store.Entries {
		if time.Now().After(entry.ExpiresAt) {
			delete(store.Entries, key)
		}
	}

	entryKey := poTokenCacheKey(normalized.VideoID, normalized.VisitorData, normalized.SessionIndex, normalized.NetworkKey)
	store.Entries[entryKey] = *normalized

	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}

	return cm.writeAtomic(cm.poCachePath(), func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

func decodePOTokenStore(data []byte) (*poTokenCacheStore, bool) {
	var store poTokenCacheStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, false
	}
	if store.Version <= 0 || len(store.Entries) == 0 {
		return nil, false
	}
	if store.Entries == nil {
		store.Entries = map[string]POTokenCache{}
	}
	return &store, true
}

func poTokenCacheKey(videoID, visitorData, sessionIndex, networkKey string) string {
	return strings.Join([]string{videoID, visitorData, sessionIndex, networkKey}, "|")
}

func normalizePOTokenCache(cache POTokenCache) *POTokenCache {
	if cache.PlayerToken == "" {
		cache.PlayerToken = cache.Token
	}
	if cache.URLToken == "" {
		cache.URLToken = cache.Token
	}
	if cache.Token == "" {
		cache.Token = cache.URLToken
		if cache.Token == "" {
			cache.Token = cache.PlayerToken
		}
	}
	return &cache
}

func validatePOTokenCache(cache *POTokenCache, videoID, visitorData, sessionIndex, networkKey string) error {
	if cache == nil {
		return fmt.Errorf("po token cache missing")
	}
	if cache.PlayerToken == "" || cache.URLToken == "" || time.Now().After(cache.ExpiresAt) {
		return fmt.Errorf("po token cache expired")
	}
	if cache.VideoID != "" && videoID != "" && cache.VideoID != videoID {
		return fmt.Errorf("po token cache video mismatch")
	}
	if cache.VisitorData != "" && visitorData != "" && cache.VisitorData != visitorData {
		return fmt.Errorf("po token cache visitor mismatch")
	}
	if cache.SessionIndex != "" && sessionIndex != "" && cache.SessionIndex != sessionIndex {
		return fmt.Errorf("po token cache session mismatch")
	}
	if cache.NetworkKey != "" && networkKey != "" && cache.NetworkKey != networkKey {
		return fmt.Errorf("po token cache network mismatch")
	}
	return nil
}

// InvalidatePOToken removes cached PO token data.
func (cm *CacheManager) InvalidatePOToken() error {
	err := os.Remove(cm.poCachePath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Invalidate removes the cache file.
func (cm *CacheManager) Invalidate() error {
	err := os.Remove(cm.CachePath())
	if os.IsNotExist(err) {
		return nil // Already gone
	}
	return err
}

// InvalidateCipherArtifacts removes cipher metadata + runtime artifacts,
// while preserving visitor/PO caches.
func (cm *CacheManager) InvalidateCipherArtifacts() error {
	var lastErr error

	if err := os.Remove(cm.CachePath()); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	if err := os.Remove(cm.codeCachePath()); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	if err := os.Remove(cm.PlayerCachePath()); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	if err := os.Remove(cm.NRuntimeCachePath()); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	if err := os.Remove(cm.wrapperCodeCachePath()); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	return lastErr
}

// IsValid checks if a cache entry is still valid (not expired and correct version).
func (cm *CacheManager) IsValid(cache *CipherCache) bool {
	if cache == nil {
		return false
	}
	if !cm.IsCompatible(cache) {
		return false
	}
	return cm.IsFresh(cache)
}

// IsCompatible checks whether a cache entry can still be used at all.
func (cm *CacheManager) IsCompatible(cache *CipherCache) bool {
	if cache == nil {
		return false
	}
	return cache.Version == CurrentCacheVersion
}

// IsFresh checks whether a cache entry is still within its hard TTL.
func (cm *CacheManager) IsFresh(cache *CipherCache) bool {
	if cache == nil {
		return false
	}
	return time.Now().Before(cache.ExpiresAt)
}

// SavePlayerJS saves player.js compressed with gzip.
func (cm *CacheManager) SavePlayerJS(playerJS []byte) error {
	return cm.writeAtomic(cm.PlayerCachePath(), func(f *os.File) error {
		gzw := gzip.NewWriter(f)
		if _, err := gzw.Write(playerJS); err != nil {
			_ = gzw.Close()
			return err
		}
		return gzw.Close()
	})
}

// LoadPlayerJS loads the compressed player.js cache.
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

// SaveNRuntimeJS saves the prepared n-runtime artifact.
func (cm *CacheManager) SaveNRuntimeJS(runtimeJS []byte) error {
	if len(runtimeJS) == 0 {
		return nil
	}
	return cm.writeAtomic(cm.NRuntimeCachePath(), func(f *os.File) error {
		_, err := f.Write(runtimeJS)
		return err
	})
}

// LoadNRuntimeJS loads the prepared n-runtime artifact.
func (cm *CacheManager) LoadNRuntimeJS() ([]byte, error) {
	return os.ReadFile(cm.NRuntimeCachePath())
}

// SaveWrapperRuntimeBytecode saves the compiled wrapper artifact.
func (cm *CacheManager) SaveWrapperRuntimeBytecode(bytecode []byte) error {
	if len(bytecode) == 0 {
		return nil
	}
	return cm.writeAtomic(cm.wrapperCodeCachePath(), func(f *os.File) error {
		_, err := f.Write(bytecode)
		return err
	})
}

// LoadWrapperRuntimeBytecode loads the compiled wrapper artifact.
func (cm *CacheManager) LoadWrapperRuntimeBytecode() ([]byte, error) {
	return os.ReadFile(cm.wrapperCodeCachePath())
}

func (cm *CacheManager) Purge() error {
	var lastErr error

	// Remove cipher cache
	if err := os.Remove(cm.CachePath()); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	// Remove isolated JS binary code cache
	codePath := cm.codeCachePath()
	if err := os.Remove(codePath); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	// Remove player.js cache
	if err := os.Remove(cm.PlayerCachePath()); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	if err := os.Remove(cm.NRuntimeCachePath()); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	if err := os.Remove(cm.wrapperCodeCachePath()); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	if err := os.Remove(cm.poCachePath()); err != nil && !os.IsNotExist(err) {
		lastErr = err
	}

	return lastErr
}
