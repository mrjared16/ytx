package ytx

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"strings"
	"sync"
	"time"

	"github.com/mrjared16/ytx/internal/cache"
	"github.com/mrjared16/ytx/internal/jsengine"
	"github.com/mrjared16/ytx/internal/jsextract"

	"github.com/buke/quickjs-go"
)

// Cipher handles YouTube signature and N-parameter decoding.
// We extract the JavaScript dependencies via fallback patterns and
// evaluate them efficiently using a QuickJS runtime.
type Cipher struct {
	runtime            *Runtime
	sigFunctionName    string
	sigParam           int
	sigUsesURLWrapper  bool
	nFunctionName      string
	signatureTimestamp int    // STS from player.js, needed for API requests
	jsCode             string // Self-contained JS with function + dependencies
	nRuntimeJS         []byte // Prepared player runtime with exposed n-function
	playerURL          string // Player JS URL (for cache versioning)
	playerFingerprint  string
	playerJS           []byte // Full player JS for n-function extraction
	warnings           []string
	isBytecode         bool
	lazyMu             sync.Mutex

	// Cached QuickJS wrapper runtime — reused for both sig and n calls.
	// Instead of creating a fresh runtime per call (~540ms each for 2.7MB eval),
	// we bootstrap once and call the wrapper function multiple times.
	wrapperRT     *quickjs.Runtime
	wrapperCtx    *quickjs.Context
	wrapperReady  bool
	wrapperJSCode string // the jsCode used to bootstrap this context

	// Profiling metrics generated during initialization
	PlayerJSFetchMs int64
	CipherAnalyzeMs int64
	AnalyzeDetail   *CipherAnalyzeDetail
}

// Close releases cached QuickJS resources. Call when the Cipher is no longer needed.
func (c *Cipher) Close() {
	c.lazyMu.Lock()
	defer c.lazyMu.Unlock()
	if c.wrapperCtx != nil {
		c.wrapperCtx.Close()
		c.wrapperCtx = nil
	}
	if c.wrapperRT != nil {
		c.wrapperRT.Close()
		c.wrapperRT = nil
	}
	c.wrapperReady = false
}

// NFunctionName returns the n-function name (for debugging)
func (c *Cipher) NFunctionName() string { return c.nFunctionName }

// PlayerJSLen returns the length of playerJS (for debugging)
func (c *Cipher) PlayerJSLen() int { return len(c.playerJS) }

// SignatureTimestamp returns the STS value needed for API requests
func (c *Cipher) SignatureTimestamp() int { return c.signatureTimestamp }

func (c *Cipher) PlayerFingerprint() string { return c.playerFingerprint }

func (c *Cipher) Warnings() []string {
	if len(c.warnings) == 0 {
		return nil
	}
	out := make([]string, len(c.warnings))
	copy(out, c.warnings)
	return out
}

// NewCipherWithCachedPath creates a new cipher, optionally using a cached base.js path
// If cachedPath is provided and valid, skips the embed page fetch (~150ms savings)
// baseURL allows domain reuse (e.g., music.youtube.com for music mode) to avoid extra TLS handshake
func NewCipherWithCachedPath(videoID string, httpClient *http.Client, cachedPath string, baseURL string) (*Cipher, string, error) {
	return NewCipherWithCachedPathContext(context.Background(), defaultRuntime, videoID, httpClient, cachedPath, baseURL)
}

func NewCipherWithCachedPathContext(ctx context.Context, runtime *Runtime, videoID string, httpClient *http.Client, cachedPath string, baseURL string) (*Cipher, string, error) {
	return newCipherWithCachedPathContext(ctx, runtime, videoID, httpClient, cachedPath, baseURL, true)
}

func fetchPlayerJSWatchFirstCipherContext(ctx context.Context, runtime *Runtime, videoID string, httpClient *http.Client, baseURL string, cacheManager *cache.CacheManager) (*Cipher, string, error) {
	_ = cacheManager
	return newCipherWithCachedPathContext(ctx, runtime, videoID, httpClient, "", baseURL, false)
}

type playerJSPhaseResult struct {
	Path     string
	JS       []byte
	Warnings []string
	FetchMs  int64
}

type analyzePhaseResult struct {
	Result    cipherDetectionResult
	JS        []byte
	Path      string
	STS       int
	AnalyzeMs int64
}

func fetchPlayerJSPhase(ctx context.Context, videoID string, httpClient *http.Client, cachedPath string, playerBaseURL string, embedFirst bool) (playerJSPhaseResult, error) {
	t0 := time.Now()
	var playerPath string
	var playerJS []byte
	var err error
	warnings := make([]string, 0, 2)

	if cachedPath != "" {
		playerURL := playerBaseURL + cachedPath
		playerJS, err = httpGetBytesContext(ctx, httpClient, playerURL)
		if err == nil {
			playerPath = cachedPath
		} else {
			warnings = append(warnings, fmt.Sprintf("cached player path %s failed, falling back to discovery: %v", cachedPath, err))
		}
	}

	if playerPath == "" {
		if embedFirst {
			playerPath, playerJS, err = fetchPlayerJSContext(ctx, videoID, httpClient, playerBaseURL)
		} else {
			playerPath, playerJS, err = fetchPlayerJSWatchFirstContext(ctx, videoID, httpClient, playerBaseURL)
		}
		if err != nil {
			return playerJSPhaseResult{}, err
		}
	}

	return playerJSPhaseResult{
		Path:     playerPath,
		JS:       playerJS,
		Warnings: warnings,
		FetchMs:  time.Since(t0).Milliseconds(),
	}, nil
}

var ErrStaleCacheRefetchRequired = errors.New("stale empty cache requires network refetch")

func analyzePlayerJSPhase(res *playerJSPhaseResult, detail *CipherAnalyzeDetail, cachedPath string) (analyzePhaseResult, error) {
	t2 := time.Now()
	var result cipherDetectionResult
	var ok bool
	playerJS := res.JS

	if result, ok = detectFastSignature(playerJS, detail); ok {

	} else if result, ok = detectWrapperSignature(playerJS, detail); ok {

	} else if result, ok = detectGlobalSignature(playerJS, detail); ok {

	} else {
		if cachedPath != "" && res.Path == cachedPath {
			return analyzePhaseResult{}, ErrStaleCacheRefetchRequired
		}
	}

	result.NName = detectNFunction(playerJS, result.SigUsesURLWrapper, detail)
	if result.SigName == "" && result.NName == "" {
		return analyzePhaseResult{}, errors.New("failed to detect signature and n transform functions")
	}

	tSts := time.Now()
	sts := findSignatureTimestamp(playerJS)
	detail.StsMs = time.Since(tSts).Milliseconds()

	return analyzePhaseResult{
		Result:    result,
		JS:        playerJS,
		Path:      res.Path,
		STS:       sts,
		AnalyzeMs: time.Since(t2).Milliseconds(),
	}, nil
}

func analyzeFallbackPlayerJSPhase(res *playerJSPhaseResult, detail *CipherAnalyzeDetail) (analyzePhaseResult, error) {
	t2 := time.Now()
	var result cipherDetectionResult
	var err error
	playerJS := res.JS

	tSigRetry := time.Now()
	result.SigName, result.SigParam, err = findSigFunctionName(playerJS)
	detail.SigMs += time.Since(tSigRetry).Milliseconds()

	if err == nil {
		detail.SigName = result.SigName
		detail.SigTier = "refetch"
	} else {
		wrapperNames := findURLTransformFunctionNames(playerJS)
		if len(wrapperNames) > 0 {
			result.SigName = wrapperNames[0]
			result.SigParam = 0
			result.SigUsesURLWrapper = true
			result.WrapperRuntimeJS = jsextract.BuildWrapperRuntimeJS(string(playerJS), result.SigName)

			detail.SigName = result.SigName
			detail.SigTier = "refetch_wrapper"
			err = nil
		}
	}

	result.NName = detectNFunction(playerJS, result.SigUsesURLWrapper, detail)
	if result.SigName == "" && result.NName == "" {
		if err != nil {
			return analyzePhaseResult{}, err
		}
		return analyzePhaseResult{}, errors.New("failed to detect signature and n transform functions after refetch")
	}

	tSts := time.Now()
	sts := findSignatureTimestamp(playerJS)
	detail.StsMs = time.Since(tSts).Milliseconds()

	return analyzePhaseResult{
		Result:    result,
		JS:        playerJS,
		Path:      res.Path,
		STS:       sts,
		AnalyzeMs: time.Since(t2).Milliseconds(),
	}, nil
}

func newCipherWithCachedPathContext(ctx context.Context, runtime *Runtime, videoID string, httpClient *http.Client, cachedPath string, baseURL string, embedFirst bool) (*Cipher, string, error) {
	if runtime == nil {
		runtime = defaultRuntime
	}
	playerBaseURL := PlayerJSURLBase
	if baseURL != "" {
		playerBaseURL = strings.TrimRight(baseURL, "/")
	}

	engineType := runtime.GetEngineType()
	var engineName string
	switch engineType {
	case jsengine.EngineBun:
		engineName = "bun"
	case jsengine.EngineNode:
		engineName = "node"
	case jsengine.EngineQuickJS:
		engineName = ""
	case jsengine.EngineAuto:
		engineName = "bun"
	}

	if engineName != "" {
		go func() {
			if err := runtime.PreSpawnJSProcess(context.Background(), engineName, nil, ""); err != nil {
				return
			}
		}()
	}

	fetchRes, err := fetchPlayerJSPhase(ctx, videoID, httpClient, cachedPath, playerBaseURL, embedFirst)
	if err != nil {
		return nil, "", err
	}

	detail := &CipherAnalyzeDetail{
		PlayerJSBytes: len(fetchRes.JS),
	}

	analyzeRes, err := analyzePlayerJSPhase(&fetchRes, detail, cachedPath)
	if err == ErrStaleCacheRefetchRequired {
		if embedFirst {
			fetchRes.Path, fetchRes.JS, err = fetchPlayerJSContext(ctx, videoID, httpClient, playerBaseURL)
		} else {
			fetchRes.Path, fetchRes.JS, err = fetchPlayerJSWatchFirstContext(ctx, videoID, httpClient, playerBaseURL)
		}
		if err != nil {
			return nil, "", err
		}
		detail.PlayerJSBytes = len(fetchRes.JS)
		analyzeRes, err = analyzeFallbackPlayerJSPhase(&fetchRes, detail)
	}

	if err != nil {
		return nil, "", err
	}

	fingerprint := computePlayerFingerprint(analyzeRes.JS)
	nRuntimeJS := buildNTransformRuntime(analyzeRes.JS, analyzeRes.Result.NName)

	cipher := &Cipher{
		runtime:            runtime,
		sigFunctionName:    analyzeRes.Result.SigName,
		sigParam:           analyzeRes.Result.SigParam,
		sigUsesURLWrapper:  analyzeRes.Result.SigUsesURLWrapper,
		nFunctionName:      analyzeRes.Result.NName,
		signatureTimestamp: analyzeRes.STS,
		jsCode:             analyzeRes.Result.WrapperRuntimeJS,
		nRuntimeJS:         nRuntimeJS,
		playerURL:          analyzeRes.Path,
		playerFingerprint:  fingerprint,
		playerJS:           analyzeRes.JS,
		warnings:           fetchRes.Warnings,
		PlayerJSFetchMs:    fetchRes.FetchMs,
		CipherAnalyzeMs:    analyzeRes.AnalyzeMs,
		AnalyzeDetail:      detail,
	}

	if detail.SigTier == "global_fallback" || detail.NFuncTier == "global_fallback" {
		cipher.warnings = append(cipher.warnings, fmt.Sprintf(
			"cipher detection degraded (sig=%s nfunc=%s markers_missed=%v) — consider updating patterns",
			detail.SigTier, detail.NFuncTier, detail.MarkerMiss))
	}

	if cipher.sigUsesURLWrapper && cipher.jsCode != "" {
		if bytecode, err := compileToQuickJSBytecode(cipher.jsCode); err == nil {
			cipher.jsCode = string(bytecode)
			cipher.isBytecode = true
		}
	}

	return cipher, analyzeRes.Path, nil
}

// NewCipherFromCache reconstructs a Cipher from cached data
func NewCipherFromCache(cache *cache.CipherCache) *Cipher {
	return NewCipherFromCacheWithRuntime(defaultRuntime, cache)
}

func NewCipherFromCacheWithRuntime(runtime *Runtime, cache *cache.CipherCache) *Cipher {
	if runtime == nil {
		runtime = defaultRuntime
	}
	return &Cipher{
		runtime:            runtime,
		sigFunctionName:    cache.SigFunction,
		sigParam:           cache.SigParam,
		sigUsesURLWrapper:  cache.SigUsesURLWrapper,
		isBytecode:         cache.IsBytecode,
		nFunctionName:      cache.NFunction,
		signatureTimestamp: cache.SignatureTimestamp,
		jsCode:             cache.JSCode,
		playerURL:          cache.PlayerURL,
		playerFingerprint:  cache.PlayerFingerprint,
	}
}

// ToCache converts a Cipher to a cacheable format
func (c *Cipher) ToCache() *cache.CipherCache {
	now := time.Now()
	sigFunction := c.sigFunctionName
	sigParam := c.sigParam
	sigUsesURLWrapper := c.sigUsesURLWrapper
	isBytecode := c.isBytecode
	jsCode := c.jsCode
	if sigUsesURLWrapper {
		jsCode = ""
	}

	return &cache.CipherCache{
		Version:            cache.CurrentCacheVersion,
		CreatedAt:          now,
		ExpiresAt:          now.Add(cache.CacheTTL),
		PlayerURL:          c.playerURL,
		PlayerFingerprint:  c.playerFingerprint,
		SigFunction:        sigFunction,
		SigParam:           sigParam,
		SigUsesURLWrapper:  sigUsesURLWrapper,
		IsBytecode:         isBytecode,
		NFunction:          c.nFunctionName,
		SignatureTimestamp: c.signatureTimestamp,
		JSCode:             jsCode,
	}
}

func (c *Cipher) runtimeOrDefault() *Runtime {
	if c.runtime != nil {
		return c.runtime
	}
	return defaultRuntime
}

func (c *Cipher) engineCacheKey() string {
	if c.playerURL == "" {
		return c.playerFingerprint
	}
	if c.playerFingerprint == "" {
		return c.playerURL
	}
	return c.playerURL + "#" + c.playerFingerprint
}
