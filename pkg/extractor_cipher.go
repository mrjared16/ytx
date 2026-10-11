package ytx

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mrjared16/ytx/internal/cache"
	"github.com/mrjared16/ytx/internal/jsengine"
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
					// Load the cached bundle EXACTLY ONCE: the same bytes
					// feed the identity validation and the modern/legacy
					// hydration below, so a validator-vs-hydrator reload can
					// never mix bytes from two different bundles.
					playerJS, perr := e.cacheManager.LoadPlayerJS()
					bundleValid := perr == nil && len(playerJS) > 0
					// Identity validation: when the cached bundle is present, it
					// must be the exact player the metadata describes. The full-
					// content fingerprint already derived from the bundle must
					// match the metadata; a positive bundle STS must match the
					// metadata STS; and URL-wrapper (music) caches additionally
					// require positive STS corroboration, so a stale or missing
					// STS is never used. Old sampled-region fingerprints never
					// match the full digest and migrate with one explicit refetch;
					// bundle-less legacy caches keep the existing gates.
					if bundleValid {
						fp := computePlayerFingerprint(playerJS)
						derivedSTS := findSignatureTimestamp(playerJS)
						identityOK := cache.PlayerFingerprint != "" && cache.PlayerFingerprint == fp
						if identityOK && derivedSTS > 0 {
							identityOK = cache.SignatureTimestamp == derivedSTS
						}
						if identityOK && cache.SigUsesURLWrapper && urlWrapperDiscover(playerJS).status != urlWrapperDiscoveryAbsent {
							identityOK = cache.SignatureTimestamp > 0 && derivedSTS > 0
						}
						if !identityOK {
							_ = e.cacheManager.InvalidateCipherArtifacts()
							goto fetchFreshCipher
						}
					}
					cipher := NewCipherFromCacheWithRuntime(e.runtime, cache)

					// Modern URL-wrapper protocol: derive and revalidate from
					// the cached player bundle without refetch or purge.
					// Cached n=xp values and QuickJS wrapper bytecode are
					// discarded for this mode; the runner source is rebuilt
					// from the cached player JS instead.
					modern := false
					if cache.SigUsesURLWrapper {
						if bundleValid {
							cipher.playerJS = playerJS
							disc := urlWrapperDiscover(playerJS)
							// Ambiguous candidates are fail-closed here too:
							// do not silently reuse legacy QuickJS/global-scan
							// paths for a suspicious player — refetch.
							if disc.status == urlWrapperDiscoveryAmbiguous {
								_ = e.cacheManager.InvalidateCipherArtifacts()
								goto fetchFreshCipher
							}
							if disc.status == urlWrapperDiscoveryFound {
								modern = true
								cipher.urlWrapperProtocol = true
								cipher.urlWrapperName = disc.name
								cipher.sigFunctionName = disc.name // revalidate cached name
								cipher.nFunctionName = ""          // discard cached n=xp
								cipher.jsCode = ""
								cipher.isBytecode = false
							}
						}
					}

					if !modern {
						if nRuntimeJS, err := e.cacheManager.LoadNRuntimeJS(); err == nil {
							cipher.nRuntimeJS = nRuntimeJS
						}

						needPlayerJS := cache.SigUsesURLWrapper || cache.JSCode == "" || (cache.NFunction != "" && len(cipher.nRuntimeJS) == 0)
						if needPlayerJS && len(cipher.playerJS) == 0 && bundleValid {
							cipher.playerJS = playerJS
						}
						if cache.SigUsesURLWrapper && cache.WrapperBuildID == currentWrapperBytecodeBuildID() {
							if wrapperBytecode, err := e.cacheManager.LoadWrapperRuntimeBytecode(); err == nil && len(wrapperBytecode) > 0 {
								cipher.jsCode = string(wrapperBytecode)
								cipher.isBytecode = true
							}
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

	// 3. Fetch fresh cipher: caller-cancel-safe, shared singleflight.
fetchFreshCipher:
	return e.sharedCipherFetch(ctx, videoID)
}

// cipherFetchTimeout bounds one shared cipher-fetch flight.
const cipherFetchTimeout = 30 * time.Second

// sharedCipherFetch starts or joins THE shared cipher-fetch flight for the
// runtime: the flight runs exactly once under a bounded, caller-detached
// context, so one caller's cancellation can never abort the shared work or
// fail a joiner. Callers select on their own cancellation versus the flight
// result and check cancellation both before registering and after receiving.
func (e *Extractor) sharedCipherFetch(ctx context.Context, videoID string) (*Cipher, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	ch := e.runtime.cipherGroup.DoChan("shared", func() (any, error) {
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cipherFetchTimeout)
		defer cancel()
		return e.fetchAndCacheCipherContext(flightCtx, videoID)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*Cipher), nil
	}
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
	if cipher.canPrecomputeSignatureRuntime() {
		if err := cipher.ensureSignatureReady(); err != nil {
			cipher.warnings = append(cipher.warnings,
				fmt.Sprintf("failed to precompute signature runtime during cache warmup: %v", err))
		}
	}

	expiry := time.Now().Add(cache.CacheTTL)
	e.storeCipherCache(cipher, expiry)

	// Save to file cache (ignore errors - graceful degradation)
	if e.cacheManager != nil {
		cacheData := cipher.ToCache()
		cacheData.BaseJSPath = playerPath // Store base.js path for next time
		if cipher.sigUsesURLWrapper && !cipher.isModernURLWrapperProtocol() {
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
	var oldKeys []string
	e.runtime.cipherCache.Lock()
	if e.runtime.cipherCache.cipher != nil {
		oldKeys = e.runtime.cipherCache.cipher.engineCacheKeys()
		e.runtime.cipherCache.cipher.Close()
	}
	e.runtime.cipherCache.cipher = nil
	e.runtime.cipherCache.expiry = time.Time{}
	e.runtime.cipherCache.updated = time.Time{}
	e.runtime.cipherCache.Unlock()

	for _, key := range oldKeys {
		e.runtime.CloseCachedEngineKey(key)
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
	// Modern URL-wrapper protocol: a single TransformURLContext operation
	// produces the validated signature and n (plus the alr marker), replacing
	// the separate decrypt and n-transform loops. Only a DIRECT (non-empty)
	// stream URL takes this early path; a signature-only Format falls through
	// to the strict SignatureCipher parse and the equal modern branch below.
	if e.cipher != nil && e.cipher.isModernURLWrapperProtocol() && stream.URL != "" {
		streamURL, err := e.getStreamURLModernContext(ctx, e.cipher, stream.URL, "sig", "")
		if err != nil {
			return "", err
		}
		return e.applyPOToken(streamURL), nil
	}

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

	if e.cipher != nil && e.cipher.isModernURLWrapperProtocol() {
		streamURL, err := e.getStreamURLModernContext(ctx, e.cipher, baseURL, sigParam, sig)
		if err != nil {
			return "", err
		}
		return e.applyPOToken(streamURL), nil
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

	// Add decrypted signature to URL. The query is parsed STRICTLY: a
	// malformed escape (e.g. keep=%ZZ) is an explicit error — never a
	// silently rebuilt URL with unrelated parameters dropped.
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	query, err := url.ParseQuery(parsedURL.RawQuery)
	if err != nil {
		return "", fmt.Errorf("malformed query in stream URL: %v", err)
	}
	query.Set(sigParam, decryptedSig)
	parsedURL.RawQuery = query.Encode()

	// Apply n-parameter transformation (throttle bypass)
	streamURL, err := e.unthrottleContext(ctx, videoID, parsedURL.String())
	if err != nil {
		return "", err
	}

	return e.applyPOToken(streamURL), nil
}

// getStreamURLModernContext applies the modern URL-wrapper protocol to a
// stream URL: one TransformURLContext call, with validated Signature/N
// applied via url.Values.Set once and the alr marker propagated when the
// wrapper returned one. The original parsed URL/query is preserved; when
// nothing changed the base URL is returned byte-exact. The query is parsed
// STRICTLY BEFORE the runner call: a malformed query is an explicit error
// and never reaches the subprocess.
func (e *Extractor) getStreamURLModernContext(ctx context.Context, cipher *Cipher, baseURL, sigParam, sig string) (string, error) {
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	query, err := url.ParseQuery(parsedURL.RawQuery)
	if err != nil {
		return "", fmt.Errorf("malformed query in stream URL: %v", err)
	}

	res, err := cipher.TransformURLContext(ctx, baseURL, sigParam, sig)
	if err != nil {
		return "", err
	}

	changed := false

	if sig != "" {
		if res.Signature == "" {
			return "", fmt.Errorf("url wrapper returned empty signature for input signature")
		}
		if !saneDecryptedSignature(sig, res.Signature) {
			return "", fmt.Errorf("invalid decrypted signature shape from url wrapper: %q", res.Signature)
		}
		query.Set(sigParam, res.Signature)
		changed = true
	}
	if nParam := query.Get("n"); nParam != "" {
		if res.N == "" {
			return "", fmt.Errorf("url wrapper returned empty n for input n parameter")
		}
		validated, err := jsengine.ValidateNTransformResult(nParam, res.N)
		if err != nil {
			return "", err
		}
		query.Set("n", validated)
		changed = true
	}
	if res.ALR != "" {
		query.Set("alr", res.ALR)
		changed = true
	}

	if !changed {
		return baseURL, nil
	}
	parsedURL.RawQuery = query.Encode()
	return parsedURL.String(), nil
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
	var oldKeys []string
	var newKeys []string
	if cipher != nil {
		newKeys = cipher.engineCacheKeys()
	}

	e.runtime.cipherCache.Lock()
	if e.runtime.cipherCache.cipher != nil {
		oldKeys = e.runtime.cipherCache.cipher.engineCacheKeys()
	}
	e.runtime.cipherCache.cipher = cipher
	e.runtime.cipherCache.expiry = expiry
	e.runtime.cipherCache.updated = time.Now()
	e.runtime.cipherCache.Unlock()

	for _, oldKey := range oldKeys {
		stillOwned := false
		for _, newKey := range newKeys {
			if newKey == oldKey {
				stillOwned = true
				break
			}
		}
		if !stillOwned {
			e.runtime.CloseCachedEngineKey(oldKey)
		}
	}
}

func (e *Extractor) refreshCipherAsync(parent context.Context, videoID string) {
	go func() {
		// A canceled parent must not drop the scheduled refresh: derive the
		// caller-local wait context with the parent's cancellation stripped
		// (and warmup bounded) BEFORE the shared fetch registers, so the
		// foreground pre-registration cancel check is never tripped by the
		// parent's end state. The flight itself stays detached under
		// cipherFetchTimeout inside sharedCipherFetch.
		ctx, cancel := backgroundRefreshContext(parent, backgroundWarmupTTL)
		defer cancel()
		_, _ = e.sharedCipherFetch(ctx, videoID)
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

	// Strict parse BEFORE any cipher work: a malformed query is an explicit
	// error and never silently rebuilt.
	query, err := url.ParseQuery(parsedURL.RawQuery)
	if err != nil {
		return "", fmt.Errorf("malformed query in stream URL: %v", err)
	}
	if query.Get("n") == "" {
		// No n-parameter, return as-is
		return streamURL, nil
	}

	cipher, err := e.ensureCipherContext(ctx, videoID)
	if err != nil {
		e.addWarningf("n transform skipped due to cipher initialization failure: %v", err)
		return streamURL, nil
	}

	// Modern URL-wrapper protocol initialized lazily by this call (e.g. a
	// direct URL arriving before any cipher was loaded): route through the
	// full URL transform and propagate failures strictly. There is no silent
	// untransformed-URL output on the modern path.
	if cipher.isModernURLWrapperProtocol() {
		transformed, err := e.getStreamURLModernContext(ctx, cipher, streamURL, "sig", "")
		if err != nil {
			return "", err
		}
		return transformed, nil
	}

	// Transform n-parameter
	var transformStart time.Time
	if e.profile {
		transformStart = time.Now()
	}
	transformedN, err := cipher.TransformNContext(ctx, query.Get("n"))
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
