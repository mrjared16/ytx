package ytx

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mrjared16/ytx/internal/cache"
)

// getCachedCipher returns a cached cipher or creates a new one
// Priority: 1) in-memory cache, 2) file cache, 3) fetch fresh
func (e *Extractor) getCachedCipher(videoID string) (*Cipher, error) {
	return e.getCachedCipherContext(context.Background(), videoID)
}

func (e *Extractor) getCachedCipherContext(ctx context.Context, videoID string) (*Cipher, error) {
	now := time.Now()

	// 1. Try shared runtime cache first.
	e.runtime.cipherCache.RLock()
	memCipher := e.runtime.cipherCache.cipher
	memExpiry := e.runtime.cipherCache.expiry
	memUpdated := e.runtime.cipherCache.updated
	e.runtime.cipherCache.RUnlock()
	if memCipher != nil {
		if now.Before(memExpiry) {
			return memCipher, nil
		}
		if now.Before(memUpdated.Add(cipherSWRGrace)) {
			e.refreshCipherAsync(ctx, videoID)
			return memCipher, nil
		}
	}

	// 2. Try persistent cache.
	if e.cacheManager != nil {
		if cache, err := e.cacheManager.Load(); err == nil {
			if !e.cacheManager.IsCompatible(cache) {
				_ = e.cacheManager.InvalidateCipherArtifacts()
			} else {
				fresh := e.cacheManager.IsFresh(cache)
				staleWithinGrace := !fresh && now.Before(cache.ExpiresAt.Add(cipherSWRGrace))
				if fresh || staleWithinGrace {
					cipher := NewCipherFromCacheWithRuntime(e.runtime, cache)
					if nRuntimeJS, err := e.cacheManager.LoadNRuntimeJS(); err == nil {
						cipher.nRuntimeJS = nRuntimeJS
					}

					needPlayerJS := cache.SigUsesURLWrapper || cache.JSCode == "" || (cache.NFunction != "" && len(cipher.nRuntimeJS) == 0)
					if needPlayerJS {
						if playerJS, err := e.cacheManager.LoadPlayerJS(); err == nil {
							cipher.playerJS = playerJS
						}
					}
					if cache.SigUsesURLWrapper && cache.WrapperBuildID == currentWrapperBytecodeBuildID() {
						if wrapperBytecode, err := e.cacheManager.LoadWrapperRuntimeBytecode(); err == nil && len(wrapperBytecode) > 0 {
							cipher.jsCode = string(wrapperBytecode)
							cipher.isBytecode = true
						}
					}
					if cache.SigUsesURLWrapper && len(cipher.playerJS) == 0 {
						_ = e.cacheManager.InvalidateCipherArtifacts()
						goto fetchFreshCipher
					}

					e.storeCipherCache(cipher, cache.ExpiresAt)
					e.warmNTransformAsync(ctx, cipher)
					if staleWithinGrace {
						e.refreshCipherAsync(ctx, videoID)
					}
					return cipher, nil
				}
				_ = e.cacheManager.InvalidateCipherArtifacts()
			}
		}
	}

	// 3. Fetch fresh cipher (singleflighted).
fetchFreshCipher:
	v, err, _ := e.runtime.cipherGroup.Do("shared", func() (any, error) {
		return e.fetchAndCacheCipherContext(ctx, videoID)
	})
	if err != nil {
		return nil, err
	}
	return v.(*Cipher), nil
}

func (e *Extractor) ensureCipherContext(ctx context.Context, videoID string) (*Cipher, error) {
	if e.cipher != nil {
		return e.cipher, nil
	}

	e.cipherMu.Lock()
	defer e.cipherMu.Unlock()

	if e.cipher != nil {
		return e.cipher, nil
	}

	cipher, err := e.getCachedCipherContext(ctx, videoID)
	if err != nil {
		return nil, err
	}
	e.cipher = cipher
	return cipher, nil
}

// fetchAndCacheCipher fetches a new cipher and saves to both caches
func (e *Extractor) fetchAndCacheCipher(videoID string) (*Cipher, error) {
	return e.fetchAndCacheCipherContext(context.Background(), videoID)
}

func (e *Extractor) fetchAndCacheCipherContext(ctx context.Context, videoID string) (*Cipher, error) {
	// Try to use cached base.js path (saves ~150ms by skipping embed page)
	var cachedPath string
	var previousCache *cache.CipherCache
	if e.cacheManager != nil {
		if cache, err := e.cacheManager.Load(); err == nil {
			previousCache = cache
		}
		if previousCache != nil && previousCache.BaseJSPath != "" {
			cachedPath = previousCache.BaseJSPath
		}
	}

	// Fetch new cipher (will try cached path first, fallback to embed page)
	// Use config.Origin for domain reuse (music.youtube.com for music mode)
	// This saves ~50-80ms by reusing existing HTTP/2 connection instead of new TLS handshake
	cipher, playerPath, err := NewCipherWithCachedPathContext(ctx, e.runtime, videoID, e.httpClient, cachedPath, e.config.Origin)
	if err != nil {
		return nil, err
	}
	if previousCache != nil {
		if previousCache.PlayerFingerprint != "" && cipher.playerFingerprint != "" && previousCache.PlayerFingerprint != cipher.playerFingerprint {
			cipher.warnings = append(cipher.warnings,
				fmt.Sprintf("player fingerprint changed from %s to %s; cached player may have been outdated", previousCache.PlayerFingerprint, cipher.playerFingerprint))
		}
		if previousCache.BaseJSPath != "" && previousCache.BaseJSPath != playerPath {
			cipher.warnings = append(cipher.warnings,
				fmt.Sprintf("player path changed from %s to %s; cache refreshed via slow path", previousCache.BaseJSPath, playerPath))
		}
	}
	e.absorbCipherWarnings(cipher)
	if cipher.canPrecomputeSignatureRuntime() {
		if err := cipher.ensureSignatureReady(); err != nil {
			cipher.warnings = append(cipher.warnings,
				fmt.Sprintf("failed to precompute signature runtime during cache warmup: %v", err))
			e.absorbCipherWarnings(cipher)
		}
	}

	if cipher.PlayerJSFetchMs > 0 {
		e.timings.PlayerJSFetchMs = cipher.PlayerJSFetchMs
	}
	if cipher.CipherAnalyzeMs > 0 {
		e.timings.CipherAnalyzeMs = cipher.CipherAnalyzeMs
	}
	if cipher.AnalyzeDetail != nil {
		e.timings.CipherDetail = cipher.AnalyzeDetail
	}

	expiry := time.Now().Add(cache.CacheTTL)
	e.storeCipherCache(cipher, expiry)

	// Save to file cache (ignore errors - graceful degradation)
	if e.cacheManager != nil {
		cacheData := cipher.ToCache()
		cacheData.BaseJSPath = playerPath // Store base.js path for next time
		if cipher.sigUsesURLWrapper {
			if wrapperBytecode, err := cipher.wrapperRuntimeBytecode(); err == nil && len(wrapperBytecode) > 0 {
				cacheData.WrapperBuildID = currentWrapperBytecodeBuildID()
				_ = e.cacheManager.SaveWrapperRuntimeBytecode(wrapperBytecode)
			}
		}
		_ = e.cacheManager.Save(cacheData)
		// Also save player.js (compressed) for n-transform
		if len(cipher.playerJS) > 0 {
			_ = e.cacheManager.SavePlayerJS(cipher.playerJS)
		}
		if len(cipher.nRuntimeJS) > 0 {
			_ = e.cacheManager.SaveNRuntimeJS(cipher.nRuntimeJS)
		}
	}

	e.warmNTransformAsync(ctx, cipher)

	return cipher, nil
}

// invalidateCipherCache clears both in-memory and file caches
func (e *Extractor) invalidateCipherCache() {
	var oldKey string
	e.runtime.cipherCache.Lock()
	if e.runtime.cipherCache.cipher != nil {
		oldKey = e.runtime.cipherCache.cipher.engineCacheKey()
		e.runtime.cipherCache.cipher.Close()
	}
	e.runtime.cipherCache.cipher = nil
	e.runtime.cipherCache.expiry = time.Time{}
	e.runtime.cipherCache.updated = time.Time{}
	e.runtime.cipherCache.Unlock()

	if oldKey != "" {
		e.runtime.CloseCachedEngineKey(oldKey)
	}

	if e.cacheManager != nil {
		_ = e.cacheManager.InvalidateCipherArtifacts()
	}
}

func watchFirstRetryBaseURL(defaultOrigin, playerURL string) string {
	origin := strings.TrimRight(defaultOrigin, "/")
	if playerURL == "" {
		return origin
	}
	parsed, err := url.Parse(playerURL)
	if err != nil || !parsed.IsAbs() || parsed.Scheme == "" || parsed.Host == "" {
		return origin
	}
	return parsed.Scheme + "://" + parsed.Host
}

// getStreamURL extracts the final stream URL, decrypting if necessary
func (e *Extractor) getStreamURL(videoID string, stream *Format) (string, error) {
	return e.getStreamURLContext(context.Background(), videoID, stream)
}

func (e *Extractor) getStreamURLContext(ctx context.Context, videoID string, stream *Format) (string, error) {
	// If URL is directly available (pre-signed), use it
	if stream.URL != "" {
		streamURL, err := e.unthrottleContext(ctx, videoID, stream.URL)
		if err != nil {
			return "", err
		}
		return e.applyPOToken(streamURL), nil
	}

	// Otherwise, decrypt the signature cipher
	if stream.SignatureCipher == "" {
		return "", fmt.Errorf("no URL or signature cipher available")
	}

	// Parse the cipher parameters
	params, err := url.ParseQuery(stream.SignatureCipher)
	if err != nil {
		return "", fmt.Errorf("failed to parse signature cipher: %w", err)
	}

	baseURL := params.Get("url")
	sig := params.Get("s")
	sigParam := params.Get("sp")
	if sigParam == "" {
		sigParam = "sig"
	}
	if baseURL == "" {
		return "", fmt.Errorf("signature cipher missing base URL")
	}

	if sig == "" {
		streamURL, err := e.unthrottleContext(ctx, videoID, baseURL)
		if err != nil {
			return "", err
		}
		return e.applyPOToken(streamURL), nil
	}

	// Initialize cipher if needed (using cache)
	if _, err := e.ensureCipherContext(ctx, videoID); err != nil {
		return "", fmt.Errorf("failed to initialize cipher: %w", err)
	}

	// Decrypt signature with fail-forward retry
	var sigDecryptStart time.Time
	if e.profile {
		sigDecryptStart = time.Now()
	}
	decryptedSig, err := e.decryptWithRetryContext(ctx, videoID, sig)
	if e.profile {
		e.timings.SigDecryptMs += time.Since(sigDecryptStart).Milliseconds()
	}
	if err != nil {
		return "", fmt.Errorf("failed to decrypt signature: %w", err)
	}
	// Only persist cache if the first-try succeeded.
	// If a retry happened, the retry cipher may be from a different player variant
	// (e.g. player_ias from /watch instead of player_embed from /embed).
	// Persisting that would poison the disk cache for the next CLI invocation.
	if !e.cipherRetried.Load() {
		e.persistCipherArtifacts()
	}

	// Add decrypted signature to URL
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	query := parsedURL.Query()
	query.Set(sigParam, decryptedSig)
	parsedURL.RawQuery = query.Encode()

	// Apply n-parameter transformation (throttle bypass)
	streamURL, err := e.unthrottleContext(ctx, videoID, parsedURL.String())
	if err != nil {
		return "", err
	}

	return e.applyPOToken(streamURL), nil
}

func (e *Extractor) warningsForResult() []string {
	if len(e.warnings) == 0 {
		return nil
	}
	out := make([]string, len(e.warnings))
	copy(out, e.warnings)
	return out
}

func (e *Extractor) absorbCipherWarnings(cipher *Cipher) {
	if cipher == nil {
		return
	}
	for _, warning := range cipher.Warnings() {
		e.addWarning(warning)
	}
}

func (e *Extractor) persistCipherArtifacts() {
	if e.cacheManager == nil || e.cipher == nil || len(e.cipher.playerJS) == 0 || !e.cipher.canPersistSignatureRuntime() {
		return
	}

	cacheData := e.cipher.ToCache()
	cacheData.BaseJSPath = e.cipher.playerURL
	if e.cipher.sigUsesURLWrapper {
		if wrapperBytecode, err := e.cipher.wrapperRuntimeBytecode(); err == nil && len(wrapperBytecode) > 0 {
			cacheData.WrapperBuildID = currentWrapperBytecodeBuildID()
			_ = e.cacheManager.SaveWrapperRuntimeBytecode(wrapperBytecode)
		}
	}
	_ = e.cacheManager.Save(cacheData)
	_ = e.cacheManager.SavePlayerJS(e.cipher.playerJS)
	if len(e.cipher.nRuntimeJS) > 0 {
		_ = e.cacheManager.SaveNRuntimeJS(e.cipher.nRuntimeJS)
	}

	e.runtime.cipherCache.Lock()
	defer e.runtime.cipherCache.Unlock()
	if time.Now().Before(e.runtime.cipherCache.expiry) {
		e.runtime.cipherCache.cipher = e.cipher
		e.runtime.cipherCache.updated = time.Now()
	}
}

func (e *Extractor) storeCipherCache(cipher *Cipher, expiry time.Time) {
	var oldEngineKey string
	var newEngineKey string
	if cipher != nil {
		newEngineKey = cipher.engineCacheKey()
	}

	e.runtime.cipherCache.Lock()
	if e.runtime.cipherCache.cipher != nil {
		oldEngineKey = e.runtime.cipherCache.cipher.engineCacheKey()
	}
	e.runtime.cipherCache.cipher = cipher
	e.runtime.cipherCache.expiry = expiry
	e.runtime.cipherCache.updated = time.Now()
	e.runtime.cipherCache.Unlock()

	if oldEngineKey != "" && oldEngineKey != newEngineKey {
		e.runtime.CloseCachedEngineKey(oldEngineKey)
	}
}

func (e *Extractor) refreshCipherAsync(parent context.Context, videoID string) {
	go func() {
		ctx, cancel := backgroundRefreshContext(parent, backgroundWarmupTTL)
		defer cancel()
		_, _, _ = e.runtime.cipherGroup.Do("shared", func() (any, error) {
			return e.fetchAndCacheCipherContext(ctx, videoID)
		})
	}()
}

// warmNTransformAsync warms the n-transform JS engine in the background.
// Signature runtime prewarm is done synchronously before cipher is sent on cipherCh.
func (e *Extractor) warmNTransformAsync(parent context.Context, cipher *Cipher) {
	if cipher == nil {
		return
	}
	go func() {
		ctx, cancel := backgroundRefreshContext(parent, backgroundWarmupTTL)
		defer cancel()
		_ = cipher.WarmNTransformEngineContext(ctx)
	}()
}

// decryptWithRetry attempts decryption, retrying once with fresh cipher if it fails
func (e *Extractor) decryptWithRetry(videoID, sig string) (string, error) {
	return e.decryptWithRetryContext(context.Background(), videoID, sig)
}

func (e *Extractor) decryptWithRetryContext(ctx context.Context, videoID, sig string) (string, error) {
	result, err := e.cipher.DecryptSignature(sig)
	if err == nil {
		return result, nil
	}
	useWatchFirst := strings.Contains(err.Error(), "url wrapper") || strings.Contains(err.Error(), "wrapper function not found")

	maxRetries := 1
	if e.cipher != nil && e.cipher.sigUsesURLWrapper {
		maxRetries = 2
	}

	// Fail-forward: retry with fresh cipher when live player variants are flaky.
	e.addWarningf("sig decrypt failed (will retry): %v [fn=%s jsCodeLen=%d playerJSLen=%d]",
		err, e.cipher.sigFunctionName, len(e.cipher.jsCode), len(e.cipher.playerJS))
	for attempt := 0; attempt < maxRetries; attempt++ {
		oldCipher := e.cipher
		e.cipherRetried.Store(true)
		e.invalidateCipherCache()
		e.cipherMu.Lock()
		e.cipher = nil
		e.cipherMu.Unlock()

		// Fetch fresh cipher
		var newCipher *Cipher
		var fetchErr error
		if useWatchFirst && attempt == 0 {
			playerBaseURL := watchFirstRetryBaseURL(e.config.Origin, "")
			if oldCipher != nil {
				playerBaseURL = watchFirstRetryBaseURL(e.config.Origin, oldCipher.playerURL)
			}
			newCipher, _, fetchErr = fetchPlayerJSWatchFirstCipherContext(ctx, e.runtime, videoID, e.httpClient, playerBaseURL, e.cacheManager)
		} else {
			newCipher, fetchErr = e.getCachedCipherContext(ctx, videoID)
		}
		if fetchErr != nil {
			return "", fmt.Errorf("retry failed: %w", fetchErr)
		}
		e.cipherMu.Lock()
		e.cipher = newCipher
		e.cipherMu.Unlock()

		// Retry decryption
		result, err = newCipher.DecryptSignature(sig)
		if err == nil {
			return result, nil
		}
		if attempt+1 < maxRetries {
			e.addWarningf("sig decrypt retry %d failed: %v [fn=%s jsCodeLen=%d playerJSLen=%d]",
				attempt+1, err, newCipher.sigFunctionName, len(newCipher.jsCode), len(newCipher.playerJS))
		}
	}

	return "", err
}

// unthrottle applies the n-parameter transformation to bypass throttling
func (e *Extractor) unthrottle(videoID, streamURL string) (string, error) {
	return e.unthrottleContext(context.Background(), videoID, streamURL)
}

func (e *Extractor) unthrottleContext(ctx context.Context, videoID, streamURL string) (string, error) {
	parsedURL, err := url.Parse(streamURL)
	if err != nil {
		return "", err
	}

	query := parsedURL.Query()
	nParam := query.Get("n")
	if nParam == "" {
		// No n-parameter, return as-is
		return streamURL, nil
	}

	cipher, err := e.ensureCipherContext(ctx, videoID)
	if err != nil {
		e.addWarningf("n transform skipped due to cipher initialization failure: %v", err)
		return streamURL, nil
	}

	// Transform n-parameter
	var transformStart time.Time
	if e.profile {
		transformStart = time.Now()
	}
	transformedN, err := cipher.TransformNContext(ctx, nParam)
	if e.profile {
		e.timings.NTransformMs += time.Since(transformStart).Milliseconds()
	}
	if err != nil {
		e.addWarningf("n transform failed (returning untransformed URL): %v", err)
		return streamURL, nil
	}

	query.Set("n", transformedN)
	parsedURL.RawQuery = query.Encode()

	return parsedURL.String(), nil
}
