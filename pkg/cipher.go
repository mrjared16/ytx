package ytx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"

	"strings"
	"sync"
	"time"

	"github.com/buke/quickjs-go"
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
)

// Cipher handles YouTube signature decryption using QuickJS
//
// DESIGN (10x engineer approach):
// 1. Use goja/parser to parse JS into AST (already a dependency)
// 2. Walk AST to find function and dependencies
// 3. Extract source text for self-contained code
//
// For n-function: Use kkdai's approach - extract raw function body and ExportTo
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

// Base.js URL pattern
var basejsPattern = regexp.MustCompile(`/s/player/[\w-]+/[\w./-]+/base\.js`)

var extractDefinitionPatternTemplates = []string{
	`(?m)(^|[;{}])\s*((?:var|let|const)\s+%s\s*=)`,
	`(?m)(^|[;{}])\s*(function\s+%s\s*\()`,
	`(?m)(^|[;{}])\s*(%s\s*=\s*function\b)`,
	`(?m)(^|[;{}])\s*(%s\s*=\s*[\[{])`,
	`(?m)(^|[;{}])\s*(%s\s*=)`,
}

type sigFunctionPattern struct {
	regex    *regexp.Regexp
	sigIdx   int
	paramIdx int
}

var sigFunctionPatterns = []sigFunctionPattern{
	{
		// Strong primary fallback: var && ((var)=sig(decodeURIComponent(var)))
		regex:    regexp.MustCompile(`\b([a-zA-Z0-9_$]+)\s*&&\s*\(\s*(?:\(\s*)?[a-zA-Z0-9_$]+\s*\)?\s*=\s*([a-zA-Z0-9_$]{2,})\(\s*decodeURIComponent\(\s*[a-zA-Z0-9_$]+\s*\)\s*\)\s*\)?`),
		sigIdx:   2,
		paramIdx: 0,
	},
	{
		regex:    regexp.MustCompile(`\b[a-zA-Z0-9_$]+\s*&&\s*\(\s*[a-zA-Z0-9_$]+\s*=\s*([a-zA-Z0-9_$]{2,})\(\s*(?:(\d+)\s*,\s*)?decodeURIComponent\(\s*[a-zA-Z0-9_$]+\s*\)\s*\)`),
		sigIdx:   1,
		paramIdx: 2,
	},
	{
		regex:    regexp.MustCompile(`(?:\b|[^a-zA-Z0-9_$])([a-zA-Z0-9_$]{2,})\s*=\s*function\(\s*a\s*\)\s*{\s*a\s*=\s*a\.split\(\s*""\s*\)(?:;[a-zA-Z0-9_$]{2}\.[a-zA-Z0-9_$]{2}\(a,\d+\))?`),
		sigIdx:   1,
		paramIdx: 0,
	},
	{
		regex:    regexp.MustCompile(`\bm=([a-zA-Z0-9$]{2,})\(decodeURIComponent\(h\.s\)\)`),
		sigIdx:   1,
		paramIdx: 0,
	},
	{
		regex:    regexp.MustCompile(`["']signature["']\s*,\s*([a-zA-Z0-9$]+)\(`),
		sigIdx:   1,
		paramIdx: 0,
	},
	{
		regex:    regexp.MustCompile(`\.sig\|\|([a-zA-Z0-9$]+)\(`),
		sigIdx:   1,
		paramIdx: 0,
	},
	{
		regex:    regexp.MustCompile(`\b[cs]\s*&&\s*[adf]\.set\([^,]+\s*,\s*([a-zA-Z0-9$]+)\(`),
		sigIdx:   1,
		paramIdx: 0,
	},
}

var extractDefinitionPatternCache sync.Map

var nRuntimeExposePattern = regexp.MustCompile(`\}\)\(_yt_player\);\s*$`)

// NewCipherWithCachedPath creates a new cipher, optionally using a cached base.js path
// If cachedPath is provided and valid, skips the embed page fetch (~150ms savings)
// baseURL allows domain reuse (e.g., music.youtube.com for music mode) to avoid extra TLS handshake
func NewCipherWithCachedPath(videoID string, httpClient *http.Client, cachedPath string, baseURL string) (*Cipher, string, error) {
	return NewCipherWithCachedPathContext(context.Background(), defaultRuntime, videoID, httpClient, cachedPath, baseURL)
}

func NewCipherWithCachedPathContext(ctx context.Context, runtime *Runtime, videoID string, httpClient *http.Client, cachedPath string, baseURL string) (*Cipher, string, error) {
	return newCipherWithCachedPathContext(ctx, runtime, videoID, httpClient, cachedPath, baseURL, true)
}

func fetchPlayerJSWatchFirstCipherContext(ctx context.Context, runtime *Runtime, videoID string, httpClient *http.Client, baseURL string, cacheManager *CacheManager) (*Cipher, string, error) {
	_ = cacheManager
	return newCipherWithCachedPathContext(ctx, runtime, videoID, httpClient, "", baseURL, false)
}

func newCipherWithCachedPathContext(ctx context.Context, runtime *Runtime, videoID string, httpClient *http.Client, cachedPath string, baseURL string, embedFirst bool) (*Cipher, string, error) {
	var playerPath string
	var playerJS []byte
	var err error
	warnings := make([]string, 0, 2)

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
	case EngineBun:
		engineName = "bun"
	case EngineNode:
		engineName = "node"
	case EngineQuickJS:
		engineName = ""
	case EngineAuto:
		engineName = "bun"
	}

	if engineName != "" {
		go func() {
			_ = runtime.PreSpawnJSProcess(context.Background(), engineName, nil, "")
		}()
	}

	t0 := time.Now()
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
			return nil, "", err
		}
	}
	tFetch := time.Since(t0).Milliseconds()

	t2 := time.Now()
	detail := &CipherAnalyzeDetail{
		PlayerJSBytes: len(playerJS),
	}

	// === THE DETECTION PIPELINE (FCIS Chain of Responsibility) ===
	var result cipherDetectionResult
	var ok bool

	if result, ok = detectFastSignature(playerJS, detail); ok {
		// Tier 1 Success
	} else if result, ok = detectWrapperSignature(playerJS, detail); ok {
		// Tier 2 Success
	} else if result, ok = detectGlobalSignature(playerJS, detail); ok {
		// Tier 3 Success
	} else {
		// Tier 4: Last resort - re-fetch player.js via alternate URL
		if cachedPath != "" && playerPath == cachedPath {
			if embedFirst {
				playerPath, playerJS, err = fetchPlayerJSContext(ctx, videoID, httpClient, playerBaseURL)
			} else {
				playerPath, playerJS, err = fetchPlayerJSWatchFirstContext(ctx, videoID, httpClient, playerBaseURL)
			}
			if err != nil {
				return nil, "", err
			}
			detail.PlayerJSBytes = len(playerJS)

			// Retry global signature on the new player
			tSigRetry := time.Now()
			result.SigName, result.SigParam, err = findSigFunctionName(playerJS)
			detail.SigMs += time.Since(tSigRetry).Milliseconds()
			
			if err == nil {
				detail.SigName = result.SigName
				detail.SigTier = "refetch"
			} else {
				// Try wrapper on re-fetched player
				wrapperNames := findURLTransformFunctionNames(playerJS)
				if len(wrapperNames) > 0 {
					result.SigName = wrapperNames[0]
					result.SigParam = 0
					result.SigUsesURLWrapper = true
					result.WrapperRuntimeJS = buildWrapperRuntimeJS(string(playerJS), result.SigName)
					
					detail.SigName = result.SigName
					detail.SigTier = "refetch_wrapper"
					err = nil
				}
			}
		}
	}

	// Stage 3: Universal N-Function application
	result.NName = detectNFunction(playerJS, result.SigUsesURLWrapper, detail)

	if result.SigName == "" && result.NName == "" {
		if err != nil {
			return nil, "", err
		}
		return nil, "", errors.New("failed to detect signature and n transform functions")
	}

	// --- Stage 4: Signature timestamp ---
	tSts := time.Now()
	sts := findSignatureTimestamp(playerJS)
	detail.StsMs = time.Since(tSts).Milliseconds()

	fingerprint := computePlayerFingerprint(playerJS)
	nRuntimeJS := buildNTransformRuntime(playerJS, result.NName)
	tAnalyze := time.Since(t2).Milliseconds()

	return &Cipher{
		runtime:            runtime,
		sigFunctionName:    result.SigName,
		sigParam:           result.SigParam,
		sigUsesURLWrapper:  result.SigUsesURLWrapper,
		nFunctionName:      result.NName,
		signatureTimestamp: sts,
		jsCode:             result.WrapperRuntimeJS,
		nRuntimeJS:         nRuntimeJS,
		playerURL:          playerPath,
		playerFingerprint:  fingerprint,
		playerJS:           playerJS,
		warnings:           warnings,
		PlayerJSFetchMs:    tFetch,
		CipherAnalyzeMs:    tAnalyze,
		AnalyzeDetail:      detail,
	}, playerPath, nil
}

// NewCipherFromCache reconstructs a Cipher from cached data
func NewCipherFromCache(cache *CipherCache) *Cipher {
	return NewCipherFromCacheWithRuntime(defaultRuntime, cache)
}

func NewCipherFromCacheWithRuntime(runtime *Runtime, cache *CipherCache) *Cipher {
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
func (c *Cipher) ToCache() *CipherCache {
	now := time.Now()
	sigFunction := c.sigFunctionName
	sigParam := c.sigParam
	sigUsesURLWrapper := c.sigUsesURLWrapper
	isBytecode := c.isBytecode
	jsCode := c.jsCode

	return &CipherCache{
		Version:            currentCacheVersion,
		CreatedAt:          now,
		ExpiresAt:          now.Add(cacheTTL),
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

func (c *Cipher) canPrecomputeSignatureRuntime() bool {
	return c.sigFunctionName != "" && !c.sigUsesURLWrapper
}

func (c *Cipher) canPersistSignatureRuntime() bool {
	return c.sigFunctionName != "" && c.jsCode != ""
}

func (c *Cipher) canPrewarmSignatureRuntime() bool {
	return c.jsCode != ""
}

// Prewarm spins up the QuickJS runtime aggressively ahead of time.
// Must be called (and waited on) before first DecryptSignature call
// to avoid paying the ~270ms bootstrap cost on the hot path.
func (c *Cipher) Prewarm() error {
	c.lazyMu.Lock()
	defer c.lazyMu.Unlock()
	_, err := c.ensureWrapperContext(c.jsCode)
	return err
}

func (c *Cipher) runtimeOrDefault() *Runtime {
	if c.runtime != nil {
		return c.runtime
	}
	return defaultRuntime
}

func (c *Cipher) WarmNTransformEngine() error {
	return c.WarmNTransformEngineContext(context.Background())
}

func (c *Cipher) WarmNTransformEngineContext(ctx context.Context) error {
	if c.nFunctionName == "" {
		return nil
	}
	if len(c.nRuntimeJS) == 0 {
		c.nRuntimeJS = buildNTransformRuntime(c.playerJS, c.nFunctionName)
	}
	if len(c.nRuntimeJS) == 0 {
		return fmt.Errorf("no player.js available for n-transform")
	}
	_, err := c.runtimeOrDefault().GetCachedEngine(ctx, c.engineCacheKey(), c.nRuntimeJS, c.nFunctionName)
	return err
}

func (c *Cipher) ensureSignatureReady() error {
	if c.sigFunctionName == "" {
		return errors.New("signature decryption function unavailable in current player JS")
	}
	if c.sigUsesURLWrapper {
		return errors.New("url-wrapper signature fallback is runtime-only and cannot be precomputed")
	}
	if c.jsCode != "" {
		return nil
	}

	c.lazyMu.Lock()
	defer c.lazyMu.Unlock()

	if c.jsCode != "" {
		return nil
	}
	if len(c.playerJS) == 0 {
		return errors.New("no player.js available for signature decryption")
	}

	jsCode, err := extractWithAST(string(c.playerJS), c.sigFunctionName)
	if err != nil {
		jsCode, err = extractSimple(c.playerJS, c.sigFunctionName)
		if err != nil {
			return fmt.Errorf("failed to extract sig function: %w", err)
		}
	}
	c.jsCode = jsCode

	// Fast Boot Optimization: Automatically compile to QuickJS bytecode.
	if !c.isBytecode && len(c.jsCode) > 0 {
		rt := quickjs.NewRuntime()
		ctx := rt.NewContext()
		if bytecode, err := ctx.Compile(c.jsCode, quickjs.EvalFlagGlobal(true)); err == nil {
			c.jsCode = string(bytecode)
			c.isBytecode = true
		}
		ctx.Close()
		rt.Close()
	}

	return nil
}

// segment represents a top-level statement in player.js.
type segment struct {
	start     int
	end       int
	deps      []string
	depsReady bool
	stmt      ast.Statement
}

// definitionIndex maps names to segments.
type definitionIndex struct {
	segments  []*segment
	nameToSeg map[string]int
}

func buildDefinitionIndex(program *ast.Program, jsCode string) *definitionIndex {
	_ = jsCode
	base := int(program.File.Base())
	idx := &definitionIndex{
		segments:  make([]*segment, 0, 256),
		nameToSeg: make(map[string]int, 256),
	}

	for _, stmt := range collectIndexRoots(program) {
		start := int(stmt.Idx0()) - base
		end := int(stmt.Idx1()) - base

		switch s := stmt.(type) {
		case *ast.VariableStatement:
			seg := &segment{start: start, end: end, stmt: s}
			segID := len(idx.segments)
			idx.segments = append(idx.segments, seg)

			for _, decl := range s.List {
				if decl == nil {
					continue
				}
				if name := extractBindingIdentifierName(decl.Target); name != "" {
					idx.nameToSeg[name] = segID
				}
			}

		case *ast.FunctionDeclaration:
			if s.Function == nil {
				continue
			}
			seg := &segment{start: start, end: end, stmt: s}
			segID := len(idx.segments)
			idx.segments = append(idx.segments, seg)
			if s.Function.Name != nil {
				idx.nameToSeg[s.Function.Name.Name.String()] = segID
			}

		case *ast.ExpressionStatement:
			handleExpressionStatement(s, start, end, idx)
		}
	}

	return idx
}

func collectIndexRoots(program *ast.Program) []ast.Statement {
	roots := make([]ast.Statement, 0, len(program.Body)+512)
	roots = append(roots, program.Body...)

	for _, stmt := range program.Body {
		exprStmt, ok := stmt.(*ast.ExpressionStatement)
		if !ok {
			continue
		}
		if body := unwrapIIFEBody(exprStmt.Expression); body != nil {
			roots = append(roots, body.List...)
		}
	}

	return roots
}

func unwrapIIFEBody(expr ast.Expression) *ast.BlockStatement {
	switch e := expr.(type) {
	case *ast.CallExpression:
		switch callee := e.Callee.(type) {
		case *ast.FunctionLiteral:
			return callee.Body
		case *ast.DotExpression:
			if fn, ok := callee.Left.(*ast.FunctionLiteral); ok {
				return fn.Body
			}
		case *ast.BracketExpression:
			if fn, ok := callee.Left.(*ast.FunctionLiteral); ok {
				return fn.Body
			}
		}

	case *ast.SequenceExpression:
		for _, seqExpr := range e.Sequence {
			if body := unwrapIIFEBody(seqExpr); body != nil {
				return body
			}
		}

	case *ast.AssignExpression:
		return unwrapIIFEBody(e.Right)

	case *ast.UnaryExpression:
		return unwrapIIFEBody(e.Operand)

	case *ast.ConditionalExpression:
		if body := unwrapIIFEBody(e.Consequent); body != nil {
			return body
		}
		return unwrapIIFEBody(e.Alternate)
	}

	return nil
}

func handleExpressionStatement(stmt *ast.ExpressionStatement, start int, end int, idx *definitionIndex) {
	switch e := stmt.Expression.(type) {
	case *ast.AssignExpression:
		if ident, ok := e.Left.(*ast.Identifier); ok {
			seg := &segment{start: start, end: end, stmt: stmt}
			segID := len(idx.segments)
			idx.segments = append(idx.segments, seg)
			idx.nameToSeg[ident.Name.String()] = segID
		}

	case *ast.SequenceExpression:
		seg := &segment{start: start, end: end, stmt: stmt}
		segID := len(idx.segments)
		idx.segments = append(idx.segments, seg)

		for _, seqExpr := range e.Sequence {
			assign, ok := seqExpr.(*ast.AssignExpression)
			if !ok {
				continue
			}
			if ident, ok := assign.Left.(*ast.Identifier); ok {
				idx.nameToSeg[ident.Name.String()] = segID
			}
		}
	}
}

func segmentDependencies(seg *segment) []string {
	if seg == nil {
		return nil
	}
	if seg.depsReady {
		return seg.deps
	}

	deps := make(map[string]bool)
	switch s := seg.stmt.(type) {
	case *ast.VariableStatement:
		for _, decl := range s.List {
			if decl == nil || decl.Initializer == nil {
				continue
			}
			collectIdentifiers(decl.Initializer, deps)
		}

	case *ast.FunctionDeclaration:
		if s.Function != nil {
			collectIdentifiers(s.Function.Body, deps)
		}

	case *ast.ExpressionStatement:
		switch e := s.Expression.(type) {
		case *ast.AssignExpression:
			collectIdentifiers(e.Right, deps)
		case *ast.SequenceExpression:
			for _, seqExpr := range e.Sequence {
				assign, ok := seqExpr.(*ast.AssignExpression)
				if !ok {
					continue
				}
				collectIdentifiers(assign.Right, deps)
			}
		}
	}

	seg.deps = sortedKeys(deps)
	seg.depsReady = true
	return seg.deps
}

func resolveDependencyClosure(funcName string, idx *definitionIndex, builtins map[string]bool) []int {
	seenSegs := make(map[int]bool)
	result := make([]int, 0, 64)

	queue := []string{funcName}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]

		segID, ok := idx.nameToSeg[name]
		if !ok || seenSegs[segID] || builtins[name] {
			continue
		}
		seenSegs[segID] = true
		result = append(result, segID)

		for _, dep := range segmentDependencies(idx.segments[segID]) {
			if _, inIndex := idx.nameToSeg[dep]; inIndex && !builtins[dep] {
				queue = append(queue, dep)
			}
		}
	}

	sort.Slice(result, func(i int, j int) bool {
		return idx.segments[result[i]].start < idx.segments[result[j]].start
	})

	return result
}

func emitCodeFromIndex(segIDs []int, idx *definitionIndex, jsCode string) string {
	var buf strings.Builder
	buf.Grow(len(segIDs) * 200)

	for _, segID := range segIDs {
		if segID < 0 || segID >= len(idx.segments) {
			continue
		}
		seg := idx.segments[segID]
		if seg.start < 0 || seg.end > len(jsCode) || seg.start >= seg.end {
			continue
		}
		buf.WriteString(jsCode[seg.start:seg.end])
		buf.WriteString(";\n")
	}

	return buf.String()
}

func buildWrapperRuntimeJS(jsCode, functionName string) string {
	if result := buildWrapperRuntimeJSWindowed(jsCode, functionName); result != "" {
		return result
	}
	return buildWrapperRuntimeJSGlobal(jsCode, functionName)
}

type wrapperReplacement struct {
	pattern *regexp.Regexp
	replace string
	arrow   bool
}

func buildWrapperReplacements(functionName string) []wrapperReplacement {
	quotedName := regexp.QuoteMeta(functionName)
	return []wrapperReplacement{
		{
			pattern: regexp.MustCompile(`\bvar\s+` + quotedName + `\s*=\s*function\s*\(`),
			replace: `var ` + functionName + `=globalThis["` + functionName + `"]=function(`,
		},
		{
			pattern: regexp.MustCompile(`\bvar\s+` + quotedName + `\s*=\s*`),
			replace: `var ` + functionName + `=globalThis["` + functionName + `"]=` + functionName + `=`,
			arrow:   true,
		},
		{
			pattern: regexp.MustCompile(`\blet\s+` + quotedName + `\s*=\s*function\s*\(`),
			replace: `let ` + functionName + `=globalThis["` + functionName + `"]=function(`,
		},
		{
			pattern: regexp.MustCompile(`\blet\s+` + quotedName + `\s*=\s*`),
			replace: `let ` + functionName + `=globalThis["` + functionName + `"]=` + functionName + `=`,
			arrow:   true,
		},
		{
			pattern: regexp.MustCompile(`\bconst\s+` + quotedName + `\s*=\s*function\s*\(`),
			replace: `const ` + functionName + `=globalThis["` + functionName + `"]=function(`,
		},
		{
			pattern: regexp.MustCompile(`\bconst\s+` + quotedName + `\s*=\s*`),
			replace: `const ` + functionName + `=globalThis["` + functionName + `"]=` + functionName + `=`,
			arrow:   true,
		},
		{
			pattern: regexp.MustCompile(`\b` + quotedName + `\s*=\s*function\s*\(`),
			replace: `globalThis["` + functionName + `"]=` + functionName + `=function(`,
		},
		{
			pattern: regexp.MustCompile(`\b` + quotedName + `\s*=\s*`),
			replace: `globalThis["` + functionName + `"]=` + functionName + `=`,
			arrow:   true,
		},
		{
			pattern: regexp.MustCompile(`function\s+` + quotedName + `\s*\(`),
			replace: `globalThis["` + functionName + `"]=function ` + functionName + `(`,
		},
	}
}

// buildWrapperRuntimeJSWindowed uses string index to find function name occurrences,
// then applies regex on small windows around each occurrence.
func buildWrapperRuntimeJSWindowed(jsCode, functionName string) string {
	if jsCode == "" || functionName == "" {
		return ""
	}
	replacements := buildWrapperReplacements(functionName)

	var indices []int
	offset := 0
	for {
		idx := strings.Index(jsCode[offset:], functionName)
		if idx == -1 {
			break
		}
		absIdx := offset + idx
		indices = append(indices, absIdx)
		offset = absIdx + len(functionName)
	}

	for _, absIdx := range indices {
		start := absIdx - 30
		if start < 0 {
			start = 0
		}
		end := absIdx + len(functionName) + 30
		if end > len(jsCode) {
			end = len(jsCode)
		}
		chunk := jsCode[start:end]

		for _, candidate := range replacements {
			matches := candidate.pattern.FindAllStringIndex(chunk, -1)
			for _, loc := range matches {
				absLoc0 := start + loc[0]
				absLoc1 := start + loc[1]

				if !hasSafeJSIdentifierBoundary(jsCode, absLoc0, absLoc1) {
					continue
				}
				if candidate.arrow && !strings.HasPrefix(strings.TrimSpace(jsCode[absLoc1:]), "(") {
					continue
				}
				return jsCode[:absLoc0] + candidate.replace + jsCode[absLoc1:]
			}
		}
	}

	return ""
}

// buildWrapperRuntimeJSGlobal runs regex patterns on the entire file (slow but correct fallback).
func buildWrapperRuntimeJSGlobal(jsCode, functionName string) string {
	if jsCode == "" || functionName == "" {
		return ""
	}
	replacements := buildWrapperReplacements(functionName)

	for _, candidate := range replacements {
		matches := candidate.pattern.FindAllStringIndex(jsCode, -1)
		for _, loc := range matches {
			if !hasSafeJSIdentifierBoundary(jsCode, loc[0], loc[1]) {
				continue
			}
			if candidate.arrow && !strings.HasPrefix(strings.TrimSpace(jsCode[loc[1]:]), "(") {
				continue
			}
			return jsCode[:loc[0]] + candidate.replace + jsCode[loc[1]:]
		}
	}

	return ""
}

func hasSafeJSIdentifierBoundary(s string, start, end int) bool {
	if start > 0 && isJSIdentifierByte(s[start-1]) {
		return false
	}
	return true
}

func isJSIdentifierByte(b byte) bool {
	return b == '$' || b == '_' || ('0' <= b && b <= '9') || ('A' <= b && b <= 'Z') || ('a' <= b && b <= 'z')
}



// extractWithAST uses goja parser to extract a function and its dependencies.
func extractWithAST(jsCode string, funcName string) (string, error) {
	program, err := parser.ParseFile(nil, "", jsCode, 0)
	if err != nil {
		return "", fmt.Errorf("failed to parse JS: %w", err)
	}

	idx := buildDefinitionIndex(program, jsCode)

	if _, ok := idx.nameToSeg[funcName]; !ok {
		if code := extractDefinitionSimple(jsCode, funcName); code != "" {
			return code, nil
		}
		return "", fmt.Errorf("function %s not found", funcName)
	}

	builtins := defaultJSBuiltins()
	segIDs := resolveDependencyClosure(funcName, idx, builtins)
	if len(segIDs) == 0 {
		return "", fmt.Errorf("no code extracted for %s", funcName)
	}

	code := emitCodeFromIndex(segIDs, idx, jsCode)
	if code == "" {
		return "", fmt.Errorf("no code extracted for %s", funcName)
	}

	return code, nil
}

func defaultJSBuiltins() map[string]bool {
	return map[string]bool{
		"String": true, "Array": true, "Object": true, "Math": true,
		"parseInt": true, "parseFloat": true, "isNaN": true, "isFinite": true,
		"encodeURIComponent": true, "decodeURIComponent": true,
		"encodeURI": true, "decodeURI": true,
		"JSON": true, "console": true, "undefined": true, "null": true,
		"true": true, "false": true, "NaN": true, "Infinity": true,
		"Date": true, "RegExp": true, "Error": true,
	}
}

func sortedKeys[K ~string](m map[K]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	return keys
}

func extractBindingIdentifierName(target ast.BindingTarget) string {
	if target == nil {
		return ""
	}
	if ident, ok := target.(*ast.Identifier); ok {
		return ident.Name.String()
	}
	return ""
}

// collectIdentifiers walks an AST node and collects all identifiers.
func collectIdentifiers(node ast.Node, deps map[string]bool) {
	if node == nil {
		return
	}

	switch n := node.(type) {
	case *ast.Identifier:
		deps[n.Name.String()] = true
	case *ast.FunctionDeclaration:
		if n.Function != nil {
			collectIdentifiers(n.Function, deps)
		}
	case *ast.FunctionLiteral:
		for _, stmt := range n.Body.List {
			collectIdentifiers(stmt, deps)
		}
	case *ast.ExpressionStatement:
		collectIdentifiers(n.Expression, deps)
	case *ast.CallExpression:
		collectIdentifiers(n.Callee, deps)
		for _, arg := range n.ArgumentList {
			collectIdentifiers(arg, deps)
		}
	case *ast.DotExpression:
		collectIdentifiers(n.Left, deps)
	case *ast.BracketExpression:
		collectIdentifiers(n.Left, deps)
		collectIdentifiers(n.Member, deps)
	case *ast.BinaryExpression:
		collectIdentifiers(n.Left, deps)
		collectIdentifiers(n.Right, deps)
	case *ast.AssignExpression:
		collectIdentifiers(n.Left, deps)
		collectIdentifiers(n.Right, deps)
	case *ast.ConditionalExpression:
		collectIdentifiers(n.Test, deps)
		collectIdentifiers(n.Consequent, deps)
		collectIdentifiers(n.Alternate, deps)
	case *ast.VariableStatement:
		for _, decl := range n.List {
			collectIdentifiers(decl.Initializer, deps)
		}
	case *ast.IfStatement:
		collectIdentifiers(n.Test, deps)
		collectIdentifiers(n.Consequent, deps)
		collectIdentifiers(n.Alternate, deps)
	case *ast.ForStatement:
		collectIdentifiers(n.Initializer, deps)
		collectIdentifiers(n.Test, deps)
		collectIdentifiers(n.Update, deps)
		collectIdentifiers(n.Body, deps)
	case *ast.BlockStatement:
		for _, stmt := range n.List {
			collectIdentifiers(stmt, deps)
		}
	case *ast.ReturnStatement:
		collectIdentifiers(n.Argument, deps)
	case *ast.ArrayLiteral:
		for _, elem := range n.Value {
			collectIdentifiers(elem, deps)
		}
	case *ast.ObjectLiteral:
		for _, prop := range n.Value {
			if keyed, ok := prop.(*ast.PropertyKeyed); ok {
				collectIdentifiers(keyed.Value, deps)
			}
		}
	case *ast.UnaryExpression:
		collectIdentifiers(n.Operand, deps)
	case *ast.SequenceExpression:
		for _, expr := range n.Sequence {
			collectIdentifiers(expr, deps)
		}
	case *ast.SwitchStatement:
		collectIdentifiers(n.Discriminant, deps)
		for _, c := range n.Body {
			collectIdentifiers(c.Test, deps)
			for _, stmt := range c.Consequent {
				collectIdentifiers(stmt, deps)
			}
		}
	}
}

func extractDefinitionPatterns(name string) []*regexp.Regexp {
	if cached, ok := extractDefinitionPatternCache.Load(name); ok {
		return cached.([]*regexp.Regexp)
	}

	nameQ := regexp.QuoteMeta(name)
	patterns := make([]*regexp.Regexp, len(extractDefinitionPatternTemplates))
	for i, template := range extractDefinitionPatternTemplates {
		patterns[i] = regexp.MustCompile(fmt.Sprintf(template, nameQ))
	}

	actual, _ := extractDefinitionPatternCache.LoadOrStore(name, patterns)
	return actual.([]*regexp.Regexp)
}

// extractDefinitionSimple extracts a variable/function definition using string parsing.
func extractDefinitionSimple(js string, name string) string {
	patterns := extractDefinitionPatterns(name)

	best := -1
	for _, pattern := range patterns {
		indices := pattern.FindAllStringSubmatchIndex(js, -1)
		for _, idx := range indices {
			if len(idx) < 6 {
				continue
			}
			start := idx[4]
			if start < 0 {
				continue
			}
			if best == -1 || start < best {
				best = start
			}
		}
	}

	if best >= 0 {
		return extractStatement(js, best)
	}

	return ""
}

// extractStatement extracts a complete statement starting at idx.
func extractStatement(js string, idx int) string {
	pos := idx
	depth := 0
	parenDepth := 0
	inString := byte(0)

	for pos < len(js) {
		c := js[pos]

		if inString != 0 {
			if c == inString && (pos == 0 || js[pos-1] != '\\') {
				inString = 0
			}
		} else {
			switch c {
			case '"', '\'', '`':
				inString = c
			case '{':
				depth++
			case '}':
				depth--
			case '(':
				parenDepth++
			case ')':
				parenDepth--
			case ';':
				if depth == 0 && parenDepth == 0 {
					return js[idx : pos+1]
				}
			}
		}
		pos++
	}

	return js[idx:pos]
}

// extractSimple is the fallback extraction without AST
// NOTE: Uses hardcoded legacy dependency names as a safety net when AST parsing fails.
// The main path (extractWithAST) uses dynamic discovery and should be preferred.
func extractSimple(js []byte, funcName string) (string, error) {
	jsStr := string(js)

	// Extract known dependencies
	var result strings.Builder

	// z variable (complex: var z='...'.split(';'))
	zDef := extractDefinitionSimple(jsStr, "z")
	if zDef != "" {
		result.WriteString(zDef)
		result.WriteString("\n")
	}

	// zj function
	zjDef := extractDefinitionSimple(jsStr, "zj")
	if zjDef != "" {
		result.WriteString(zjDef)
		result.WriteString("\n")
	}

	// p6 object
	p6Def := extractDefinitionSimple(jsStr, "p6")
	if p6Def != "" {
		result.WriteString(p6Def)
		result.WriteString("\n")
	}

	// Main function
	funcDef := extractDefinitionSimple(jsStr, funcName)
	if funcDef == "" {
		return "", fmt.Errorf("function %s not found", funcName)
	}
	result.WriteString(funcDef)

	return result.String(), nil
}

// DecryptSignature decrypts the signature
const browserStubsJS = `
var _yt_player = {};
var _exposed = {};

// Make 'this' (the global object) behave like 'window'
this.window = this;
this.self = this;
this.globalThis = this;
this.top = this;
this.parent = this;
this.t = this;

// Attach browser properties to the global object
this.location = {
    hash: '',
    host: 'www.youtube.com',
    hostname: 'www.youtube.com',
    href: 'https://www.youtube.com/watch?v=ytx',
    origin: 'https://www.youtube.com',
    password: '',
    pathname: '/watch',
    port: '',
    protocol: 'https:',
    search: '?v=ytx',
    username: ''
};
this.document = {
    getElementsByTagName: function() { return []; },
    querySelector: function() { return null; },
    getElementById: function() { return null; },
    createElement: function(tag) { 
        return { 
            style: {}, 
            tagName: tag.toUpperCase(), 
            appendChild: function() {}, 
            setAttribute: function() {}, 
            getAttribute: function() { return null; } 
        }; 
    },
    createTextNode: function() { return {}; },
    documentElement: { style: {} },
    body: { appendChild: function() {} },
    head: { appendChild: function() {} },
    scripts: [{ src: 'https://www.youtube.com/s/player/placeholder/base.js' }],
    currentScript: { src: 'https://www.youtube.com/s/player/placeholder/base.js' },
    cookie: '',
    domain: 'youtube.com',
    location: this.location
};
this.navigator = { userAgent: 'Mozilla/5.0', platform: 'Win32', language: 'en-US', languages: ['en-US'] };
this.console = { log: function() {}, warn: function() {}, error: function() {}, info: function() {}, debug: function() {} };

var g = this.g || {};
this.g = g;
g.qJ = function(url) {
    var raw = typeof url === 'string' ? url : '';
    this.base = raw.split('?')[0] || '';
    this.params = {};
    var query = '';
    var qIdx = raw.indexOf('?');
    if (qIdx >= 0 && qIdx + 1 < raw.length) {
        query = raw.slice(qIdx + 1);
    }
    if (query) {
        var pairs = query.split('&');
        for (var i = 0; i < pairs.length; i++) {
            if (!pairs[i]) continue;
            var eq = pairs[i].indexOf('=');
            if (eq < 0) {
                this.params[pairs[i]] = '';
            } else {
                this.params[pairs[i].slice(0, eq)] = pairs[i].slice(eq + 1);
            }
        }
    }
};
g.qJ.prototype.set = function(k, v) {
    this.params[String(k)] = v == null ? '' : String(v);
    return this;
};
g.qJ.prototype.get = function(k) {
    var key = String(k);
    return Object.prototype.hasOwnProperty.call(this.params, key) ? this.params[key] : null;
};
g.qJ.prototype.clone = function() {
    return new g.qJ(this.toString());
};
g.qJ.prototype.toString = function() {
    var out = [];
    for (var key in this.params) {
        if (Object.prototype.hasOwnProperty.call(this.params, key)) {
            out.push(key + '=' + this.params[key]);
        }
    }
    return out.length ? this.base + '?' + out.join('&') : this.base;
};
var __qjMethodNames = ['append','update','setParam','add','put','setValue','setQuery'];
for (var __i = 0; __i < __qjMethodNames.length; __i++) {
    (function(name){
        g.qJ.prototype[name] = function(k, v) { return this.set(k, v); };
    })(__qjMethodNames[__i]);
}

function setTimeout(fn) { try { fn(); } catch(e) {} return 0; }
function setInterval() { return 0; }
function clearTimeout() {}
function clearInterval() {}
function requestAnimationFrame(fn) { try { fn(0); } catch(e) {} return 0; }
function cancelAnimationFrame() {}

function XMLHttpRequest() {
    this.readyState = 0; this.status = 0; this.responseText = '';
    this.open = function() { this.readyState = 1; };
    this.send = function() { this.readyState = 4; this.status = 200; };
    this.setRequestHeader = function() {};
    this.getResponseHeader = function() { return null; };
}

function fetch() { 
    return Promise.resolve({ 
        ok: true, 
        status: 200, 
        json: function() { return Promise.resolve({}); }, 
        text: function() { return Promise.resolve(''); } 
    }); 
}

this.localStorage = { getItem: function() { return null; }, setItem: function() {}, removeItem: function() {}, clear: function() {} };
this.sessionStorage = { getItem: function() { return null; }, setItem: function() {}, removeItem: function() {}, clear: function() {} };
this.performance = { now: function() { return Date.now(); }, timing: { navigationStart: Date.now() } };
this.history = { pushState: function() {}, replaceState: function() {} };
this.screen = { width: 1920, height: 1080 };
this.innerWidth = 1920; 
this.innerHeight = 1080; 
this.devicePixelRatio = 1;
this.crypto = { 
    getRandomValues: function(arr) { 
        for (var i = 0; i < arr.length; i++) arr[i] = Math.floor(Math.random() * 256); 
        return arr; 
    } 
};
`

func (c *Cipher) DecryptSignature(sig string) (string, error) {
	if c.sigUsesURLWrapper {
		if c.sigFunctionName != "" && c.jsCode != "" {
			if decrypted, err := c.transformWithURLWrapper(c.jsCode, c.sigFunctionName, sig, "s"); err == nil {
				return decrypted, nil
			}
		}

		codes := c.wrapperCodeCandidates()
		wrapperNames := c.wrapperFunctionCandidates()
		var lastErr error
		for _, wrapperName := range wrapperNames {
			for _, wrapperCode := range codes {
				decrypted, err := c.transformWithURLWrapper(wrapperCode, wrapperName, sig, "s")
				if err == nil {
					preferredCode := wrapperCode
					if len(c.playerJS) > 0 {
						fullPlayerJS := string(c.playerJS)
						if wrapperCode == fullPlayerJS {
							if built := buildWrapperRuntimeJS(fullPlayerJS, wrapperName); built != "" {
								preferredCode = built
							} else if extracted, extractErr := extractWithAST(fullPlayerJS, wrapperName); extractErr == nil && extracted != "" {
								preferredCode = extracted
							}
						}
					}

					c.lazyMu.Lock()
					c.sigFunctionName = wrapperName
					c.sigParam = 0
					c.jsCode = preferredCode
					c.isBytecode = false
					c.lazyMu.Unlock()
					return decrypted, nil
				}
				lastErr = err
			}
		}
		if lastErr != nil {
			return "", lastErr
		}
		return "", errors.New("url wrapper function unavailable in current player JS")
	}

	if err := c.ensureSignatureReady(); err != nil {
		return "", err
	}

	// Try pre-warmed context first or create one if not pre-warmed
	c.lazyMu.Lock()
	ctx, err := c.ensureWrapperContext(c.jsCode)
	c.lazyMu.Unlock()
	if err != nil {
		return "", err
	}
	// Intentionally don't close ctx here since it is cached in c.wrapperCtx

	// Call the function
	var callCode string
	if c.sigParam > 0 {
		callCode = fmt.Sprintf("%s(%d, '%s')", c.sigFunctionName, c.sigParam, escapeJSString(sig))
	} else {
		callCode = fmt.Sprintf("%s('%s')", c.sigFunctionName, escapeJSString(sig))
	}

	c.lazyMu.Lock()
	result := ctx.Eval(callCode, quickjs.EvalFlagGlobal(true))
	if ctx.HasException() {
		exc := ctx.Exception()
		result.Free()
		c.lazyMu.Unlock()
		return "", fmt.Errorf("failed to call %s: %v", c.sigFunctionName, exc)
	}

	if result.IsUndefined() || result.IsNull() {
		result.Free()
		c.lazyMu.Unlock()
		return "", errors.New("sig function returned null/undefined")
	}

	resultStr := result.String()
	result.Free()
	c.lazyMu.Unlock()
	return resultStr, nil
}

// TransformN transforms the n-parameter using the configured JS engine
// This is the critical function for bypassing YouTube's throttling
//
// Engine selection (via --js-engine flag):
// - auto: Bun → Node (default)
// - bun: Force Bun subprocess
// - node: Force Node.js subprocess
//
// Why we need a full JS runtime:
// - The n-function is only a thin wrapper: function(S){return MP[z[7]](this,19,S)}
// - MP is a 707KB function with 533 dependencies inside the IIFE closure
// - You cannot extract the n-function without the ENTIRE 2.6MB player.js
// - goja (ES5.1) fails with "ReferenceError: MP is not defined" and lacks ES2020+ support
func (c *Cipher) TransformN(n string) (string, error) {
	return c.TransformNContext(context.Background(), n)
}

func (c *Cipher) TransformNContext(ctx context.Context, n string) (string, error) {
	if c.nFunctionName == "" {
		if c.sigUsesURLWrapper {
			if c.sigFunctionName != "" && c.jsCode != "" {
				if transformed, wrapperErr := c.transformWithURLWrapper(c.jsCode, c.sigFunctionName, n, "n"); wrapperErr == nil && transformed != "" && transformed != n {
					return transformed, nil
				}
			}

			var lastErr error
			for _, wrapperCode := range c.wrapperCodeCandidates() {
				transformed, wrapperErr := c.transformWithURLWrapper(wrapperCode, c.sigFunctionName, n, "n")
				if wrapperErr == nil && transformed != "" && transformed != n {
					return transformed, nil
				}
				if wrapperErr != nil {
					lastErr = wrapperErr
				}
			}
			if len(n) > 1 {
				return n[1:], nil
			}
			return n, lastErr
		}
		return n, nil
	}

	// Need full player.js for the JS runtime
	if len(c.nRuntimeJS) == 0 {
		c.nRuntimeJS = buildNTransformRuntime(c.playerJS, c.nFunctionName)
	}
	if len(c.nRuntimeJS) == 0 {
		return n, fmt.Errorf("no player.js available for n-transform")
	}

	// Use cached engine for efficiency (engine type set via SetEngineType)
	engine, err := c.runtimeOrDefault().GetCachedEngine(ctx, c.engineCacheKey(), c.nRuntimeJS, c.nFunctionName)
	if err != nil {
		wrapperName := ""
		wrapperCodes := []string{}
		if c.sigUsesURLWrapper && c.sigFunctionName != "" {
			wrapperName = c.sigFunctionName
			wrapperCodes = c.wrapperCodeCandidates()
		} else if candidate := findURLTransformFunctionName(c.playerJS); candidate != "" {
			wrapperName = candidate
			wrapperCodes = []string{string(c.playerJS)}
		}

		if wrapperName != "" {
			for _, wrapperCode := range wrapperCodes {
				if wrapperCode == "" {
					continue
				}
				transformed, wrapperErr := c.transformWithURLWrapper(wrapperCode, wrapperName, n, "n")
				if wrapperErr == nil {
					return transformed, nil
				}
			}
		}

		return n, err
	}

	return engine.TransformN(n)
}

func (c *Cipher) TransformNBatchContext(ctx context.Context, nValues []string) ([]string, error) {
	if len(nValues) == 0 {
		return nValues, nil
	}
	if c.nFunctionName == "" {
		return nValues, nil
	}
	if len(c.nRuntimeJS) == 0 {
		c.nRuntimeJS = buildNTransformRuntime(c.playerJS, c.nFunctionName)
	}
	if len(c.nRuntimeJS) == 0 {
		return nValues, fmt.Errorf("no player.js available for n-transform")
	}
	engine, err := c.runtimeOrDefault().GetCachedEngine(ctx, c.engineCacheKey(), c.nRuntimeJS, c.nFunctionName)
	if err != nil {
		return nValues, err
	}
	return engine.TransformNBatch(nValues)
}

func (c *Cipher) wrapperCodeCandidates() []string {
	candidates := make([]string, 0, 8)
	seen := map[string]struct{}{}
	add := func(code string) {
		if code == "" {
			return
		}
		if _, ok := seen[code]; ok {
			return
		}
		seen[code] = struct{}{}
		candidates = append(candidates, code)
	}

	add(c.jsCode)
	if len(c.playerJS) > 0 {
		playerJS := string(c.playerJS)
		for _, name := range c.wrapperFunctionCandidates() {
			if built := buildWrapperRuntimeJS(playerJS, name); built != "" {
				add(built)
			}
			if extracted, err := extractWithAST(playerJS, name); err == nil {
				add(extracted)
			}
		}
		add(playerJS)
	}

	return candidates
}

func (c *Cipher) wrapperFunctionCandidates() []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || !isValidIdentifier(name) {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}

	add(c.sigFunctionName)
	if len(c.playerJS) > 0 {
		for _, name := range findURLTransformFunctionNames(c.playerJS) {
			add(name)
		}
	}
	return out
}

// ensureWrapperContext bootstraps a QuickJS runtime with the given jsCode,
// caching it for reuse across multiple wrapper calls (sig + n).
// This avoids the ~540ms cost of eval'ing 2.7MB player.js on each call.
func (c *Cipher) ensureWrapperContext(jsCode string) (*quickjs.Context, error) {
	// Fast path: context already bootstrapped with the same code
	if c.wrapperReady && c.wrapperJSCode == jsCode && c.wrapperCtx != nil {
		return c.wrapperCtx, nil
	}

	// Close any stale context
	if c.wrapperCtx != nil {
		c.wrapperCtx.Close()
		c.wrapperCtx = nil
	}
	if c.wrapperRT != nil {
		c.wrapperRT.Close()
		c.wrapperRT = nil
	}
	c.wrapperReady = false

	rt := quickjs.NewRuntime(
		quickjs.WithMemoryLimit(256 * 1024 * 1024),
		// Stack size 0 = disable check. Required because the runtime may be
		// bootstrapped in one goroutine (prewarm) and used from another
		// (DecryptSignature). QuickJS's JS_SetMaxStackSize compares the current
		// C stack pointer against the initial thread's stack base, which produces
		// false "stack overflow" when goroutines migrate between OS threads.
	)
	ctx := rt.NewContext()

	stubsVal := ctx.Eval(browserStubsJS, quickjs.EvalFlagGlobal(true))
	if ctx.HasException() {
		exc := ctx.Exception()
		stubsVal.Free()
		ctx.Close()
		rt.Close()
		return nil, fmt.Errorf("failed to inject browser stubs: %v", exc)
	}
	stubsVal.Free()

	// Evaluate context payloads
	var val *quickjs.Value
	if c.isBytecode && jsCode == c.jsCode {
		val = ctx.EvalBytecode([]byte(jsCode))
	} else {
		val = ctx.Eval(jsCode, quickjs.EvalFlagGlobal(true))
	}

	if ctx.HasException() {
		exc := ctx.Exception()
		val.Free()
		ctx.Close()
		rt.Close()
		return nil, fmt.Errorf("failed to load JS: %v", exc)
	}
	val.Free()

	c.wrapperRT = rt
	c.wrapperCtx = ctx
	c.wrapperReady = true
	c.wrapperJSCode = jsCode
	return ctx, nil
}

func (c *Cipher) transformWithURLWrapper(jsCode, functionName, value, field string) (string, error) {
	if functionName == "" || jsCode == "" {
		return "", errors.New("url wrapper function unavailable in current player JS")
	}

	var script string
	escapedName := escapeJSString(functionName)
	escapedValue := escapeJSString(value)
	if field == "s" {
		script = fmt.Sprintf(`(function(){
if(typeof g==='undefined'){var g={};}
if(typeof g.qJ!=='function'){
  if(typeof qJ==='function'){g.qJ=qJ;}
  else if(typeof globalThis.qJ==='function'){g.qJ=globalThis.qJ;}
}
var __fn=globalThis['%s'];
if(typeof __fn!=='function'){
  try{if(typeof %s==='function'){__fn=%s;}}catch(e){}
}
if(typeof __fn!=='function')throw new Error('wrapper function not found: %s');
var __u;
for(var __a=0;__a<3;__a++){try{__u=__fn('https://www.youtube.com/watch?v=ytx','s',encodeURIComponent('%s'));break;}catch(e){if(__a===2)throw e;}}
if(!__u||typeof __u.get!=='function')return '';
if(typeof __u.update==='function'){try{__u.update();}catch(e){}}
var __s=__u.get('s');
return __s?decodeURIComponent(__s):'';
})()`, escapedName, functionName, functionName, escapedName, escapedValue)
	} else {
		script = fmt.Sprintf(`(function(){
if(typeof g==='undefined'){var g={};}
if(typeof g.qJ!=='function'){
  if(typeof qJ==='function'){g.qJ=qJ;}
  else if(typeof globalThis.qJ==='function'){g.qJ=globalThis.qJ;}
}
var __fn=globalThis['%s'];
if(typeof __fn!=='function'){
  try{if(typeof %s==='function'){__fn=%s;}}catch(e){}
}
if(typeof __fn!=='function')return '%s';
var __u;
for(var __a=0;__a<3;__a++){try{__u=__fn('https://www.youtube.com/watch?v=ytx','s',undefined);break;}catch(e){if(__a===2)return '%s';}}
if(!__u||typeof __u.set!=='function'||typeof __u.get!=='function')return '%s';
__u.set('n','%s');
if(typeof __u.update==='function'){try{__u.update();}catch(e){}}
var __n=__u.get('n');
return __n||'%s';
})()`, escapedName, functionName, functionName, escapedValue, escapedValue, escapedValue, escapedValue, escapedValue)
	}

	c.lazyMu.Lock()
	defer c.lazyMu.Unlock()
	ctx, err := c.ensureWrapperContext(jsCode)
	if err != nil {
		return "", err
	}

	result := ctx.Eval(script, quickjs.EvalFlagGlobal(true))
	if ctx.HasException() {
		exc := ctx.Exception()
		result.Free()
		// Context may be corrupted, invalidate it
		c.wrapperReady = false
		return "", fmt.Errorf("failed to call url wrapper %s: %v", functionName, exc)
	}

	if result.IsUndefined() || result.IsNull() {
		result.Free()
		if field == "n" {
			return value, nil
		}
		return "", errors.New("url wrapper returned null/undefined")
	}

	resultStr := result.String()
	result.Free()
	if field == "n" && resultStr == "" {
		return value, nil
	}
	return resultStr, nil
}

// TransformNBatch transforms multiple n-parameters in a single IPC call
func (c *Cipher) TransformNBatch(nValues []string) ([]string, error) {
	return c.TransformNBatchContext(context.Background(), nValues)
}

// findSigFunctionName finds the signature function name and optional param.
// It tries all tiers: primary → windowed fallback → global fallback.
func findSigFunctionName(js []byte) (string, int, error) {
	name, param, err := findSigFunctionNameFast(js)
	if err == nil {
		return name, param, nil
	}
	return findSigFunctionNameGlobal(js)
}

// findSigFunctionNameFast tries primary decodeURIComponent detection and
// windowed regex fallback only. Does NOT run expensive global regex scan.
func findSigFunctionNameFast(js []byte) (string, int, error) {
	// PRIMARY: original decodeURIComponent-based detection (proven in v1.1.1)
	marker := []byte(",decodeURIComponent(")
	idx := bytes.Index(js, marker)

	var cachedWrappers []string
	var wrappersFetched bool
	isWrapper := func(name string) bool {
		if !wrappersFetched {
			cachedWrappers = findURLTransformFunctionNames(js)
			wrappersFetched = true
		}
		for _, w := range cachedWrappers {
			if w == name {
				return true
			}
		}
		return false
	}

	for idx != -1 {
		start := idx - 60
		if start < 0 {
			start = 0
		}
		contextStr := string(js[start:idx])

		eqIdx := strings.LastIndex(contextStr, "=")
		if eqIdx != -1 {
			afterEq := contextStr[eqIdx+1:]
			parenIdx := strings.Index(afterEq, "(")
			if parenIdx != -1 {
				funcName := strings.TrimSpace(afterEq[:parenIdx])
				numStr := strings.TrimSpace(afterEq[parenIdx+1:])

				if isValidIdentifier(funcName) && isNumericStr(numStr) && !isWrapper(funcName) {
					beforeEq := contextStr[:eqIdx]
					if strings.Contains(beforeEq, "&&(") || strings.Contains(beforeEq, "||(") {
						param, _ := strconv.Atoi(numStr)
						return funcName, param, nil
					}
				}
			}
		}

		nextIdx := bytes.Index(js[idx+len(marker):], marker)
		if nextIdx == -1 {
			break
		}
		idx = idx + len(marker) + nextIdx
	}

	// WINDOWED FALLBACK: broader regex patterns on small chunks
	// Only use patterns with discriminating markers — skip those that would
	// match thousands of times (e.g., "function(" has 9000+ hits in player.js).
	for _, p := range sigFunctionPatterns {
		var markers [][]byte
		if strings.Contains(p.regex.String(), "split") {
			markers = [][]byte{[]byte(`split("")`), []byte(`split('')`)}
		} else if strings.Contains(p.regex.String(), "decodeURIComponent") {
			markers = [][]byte{[]byte("decodeURIComponent(")}
		} else if strings.Contains(p.regex.String(), "signature") {
			markers = [][]byte{[]byte(`"signature"`), []byte(`'signature'`)}
		} else if strings.Contains(p.regex.String(), ".sig||") {
			markers = [][]byte{[]byte(".sig||")}
		} else if strings.Contains(p.regex.String(), ".set(") {
			markers = [][]byte{[]byte("encodeURIComponent(")}
		} else {
			// Patterns without a discriminating marker (e.g., generic function patterns)
			// are too expensive for windowed scan. Let them fall through to global fallback.
			continue
		}

		for _, marker := range markers {
			offset := 0
			for {
				idx := bytes.Index(js[offset:], marker)
				if idx == -1 {
					break
				}
				absIdx := offset + idx
				offset = absIdx + len(marker)

				start := absIdx - 4000
				if start < 0 {
					start = 0
				}
				end := absIdx + 4000
				if end > len(js) {
					end = len(js)
				}
				chunk := js[start:end]

				m := p.regex.FindSubmatch(chunk)
				if len(m) <= p.sigIdx {
					continue
				}

				funcName := strings.TrimSpace(string(m[p.sigIdx]))
				if !isValidIdentifier(funcName) {
					continue
				}
				if isWrapper(funcName) {
					continue
				}

				param := 0
				if p.paramIdx > 0 && len(m) > p.paramIdx {
					numStr := strings.TrimSpace(string(m[p.paramIdx]))
					if isNumericStr(numStr) {
						param, _ = strconv.Atoi(numStr)
					}
				}

				return funcName, param, nil
			}
		}
	}

	return "", 0, errors.New("could not find signature function name (fast)")
}

// findSigFunctionNameGlobal runs all regex patterns on the full player.js (slow but correct).
func findSigFunctionNameGlobal(js []byte) (string, int, error) {
	var cachedWrappers []string
	var wrappersFetched bool
	isWrapper := func(name string) bool {
		if !wrappersFetched {
			cachedWrappers = findURLTransformFunctionNames(js)
			wrappersFetched = true
		}
		for _, w := range cachedWrappers {
			if w == name {
				return true
			}
		}
		return false
	}

	for _, p := range sigFunctionPatterns {
		m := p.regex.FindSubmatch(js)
		if len(m) <= p.sigIdx {
			continue
		}
		funcName := strings.TrimSpace(string(m[p.sigIdx]))
		if !isValidIdentifier(funcName) {
			continue
		}
		if isWrapper(funcName) {
			continue
		}
		param := 0
		if p.paramIdx > 0 && len(m) > p.paramIdx {
			numStr := strings.TrimSpace(string(m[p.paramIdx]))
			if isNumericStr(numStr) {
				param, _ = strconv.Atoi(numStr)
			}
		}
		return funcName, param, nil
	}

	return "", 0, errors.New("could not find signature function name")
}

func findURLTransformFunctionName(js []byte) string {
	names := findURLTransformFunctionNames(js)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

var urlTransformPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b([a-zA-Z0-9_$]{2,})\s*=\s*function\([^)]*\)\{[^\{\}]{0,800}\.set\("alr","yes"\)`),
	regexp.MustCompile(`\b([a-zA-Z0-9_$]{2,})\s*=\s*function\([^)]*\)\{[^\{\}]{0,800}\.set\('alr','yes'\)`),
	regexp.MustCompile(`function\s+([a-zA-Z0-9_$]{2,})\([^)]*\)\{[^\{\}]{0,800}\.set\("alr","yes"\)`),
	regexp.MustCompile(`function\s+([a-zA-Z0-9_$]{2,})\([^)]*\)\{[^\{\}]{0,800}\.set\('alr','yes'\)`),
}

var urlTransformInvalidPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\.call\(this\)`),
	regexp.MustCompile(`\.policy\s*=`),
}

func findURLTransformFunctionNames(js []byte) []string {
	if out := findURLTransformFunctionNamesWindowed(js); len(out) > 0 {
		return out
	}
	// Fallback: markers not found or changed — full global scan (slower but correct)
	return findURLTransformFunctionNamesGlobal(js)
}

// findURLTransformFunctionNamesWindowed uses marker-based windowing for fast extraction.
func findURLTransformFunctionNamesWindowed(js []byte) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)

	markers := [][]byte{[]byte(`set("alr"`), []byte(`set('alr'`)}

	for _, marker := range markers {
		offset := 0
		for {
			idx := bytes.Index(js[offset:], marker)
			if idx == -1 {
				break
			}
			absIdx := offset + idx
			offset = absIdx + len(marker)

			start := absIdx - 4000
			if start < 0 {
				start = 0
			}
			end := absIdx + 4000
			if end > len(js) {
				end = len(js)
			}
			chunk := js[start:end]

			for _, p := range urlTransformPatterns {
				matches := p.FindAllSubmatchIndex(chunk, -1)
				for _, mIdx := range matches {
					if len(mIdx) < 4 {
						continue
					}
					fullMatch := chunk[mIdx[0]:mIdx[1]]
					absStart := start + mIdx[0]
					if absStart > 0 && isJSIdentifierByte(js[absStart-1]) {
						continue
					}
					name := strings.TrimSpace(string(chunk[mIdx[2]:mIdx[3]]))

					invalid := false
					for _, invalidP := range urlTransformInvalidPatterns {
						if invalidP.MatchString(string(fullMatch)) {
							invalid = true
							break
						}
					}
					if invalid || name == "" || !isValidIdentifier(name) {
						continue
					}
					if _, ok := seen[name]; !ok {
						seen[name] = struct{}{}
						out = append(out, name)
					}
				}
			}
		}
	}
	return out
}

// findURLTransformFunctionNamesGlobal is the slow but robust global scan fallback.
func findURLTransformFunctionNamesGlobal(js []byte) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	for _, p := range urlTransformPatterns {
		matches := p.FindAllSubmatchIndex(js, -1)
		for _, mIdx := range matches {
			if len(mIdx) < 4 {
				continue
			}
			fullMatch := js[mIdx[0]:mIdx[1]]
			if mIdx[0] > 0 && isJSIdentifierByte(js[mIdx[0]-1]) {
				continue
			}
			name := strings.TrimSpace(string(js[mIdx[2]:mIdx[3]]))

			invalid := false
			for _, invalidP := range urlTransformInvalidPatterns {
				if invalidP.MatchString(string(fullMatch)) {
					invalid = true
					break
				}
			}
			if invalid || name == "" || !isValidIdentifier(name) {
				continue
			}
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
				out = append(out, name)
			}
		}
	}
	return out
}

var nTransformIndexPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\.get\(["']n["']\)\)&&\(b=([a-zA-Z0-9$_]+)\[(\d+)\]`),
	regexp.MustCompile(`([a-zA-Z0-9$_]+)\[(\d+)\]\(\s*[a-zA-Z0-9$_]+\.get\(["']n["']\)\s*\)`),
	regexp.MustCompile(`\.get\(["']n["']\)[^\n]{0,120}?\b([a-zA-Z0-9$_]+)\[(\d+)\]`),
}

var nTransformDirectPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bb\s*=\s*([a-zA-Z0-9$_]+)(?:\.call\([^,]+,\s*|\()\s*[a-zA-Z0-9$_]+\.get\(["']n["']\)`),
	regexp.MustCompile(`\bb\s*=\s*([a-zA-Z0-9$_]+)\s*\(\s*[a-zA-Z0-9$_]+\.get\(["']n["']\)`),
}

var nTransformLegacyPattern = regexp.MustCompile(`(?:var|let|const)?\s*([a-zA-Z0-9$_]{2,})\s*=\s*\[([a-zA-Z0-9$_]{2,})\]`)

func findNFunctionName(js []byte) string {
	if name := findNFunctionNameWindowed(js); name != "" {
		return name
	}
	// Fallback: markers not found or changed — full global scan (slower but correct)
	return findNFunctionNameGlobal(js)
}

// findNFunctionNameWindowed uses marker-based windowing for fast extraction.
func findNFunctionNameWindowed(js []byte) string {
	markers := [][]byte{[]byte(`get("n")`), []byte(`get('n')`)}

	for _, marker := range markers {
		offset := 0
		for {
			idx := bytes.Index(js[offset:], marker)
			if idx == -1 {
				break
			}
			absIdx := offset + idx
			offset = absIdx + len(marker)

			start := absIdx - 4000
			if start < 0 {
				start = 0
			}
			end := absIdx + 4000
			if end > len(js) {
				end = len(js)
			}
			chunk := js[start:end]

			for _, p := range nTransformIndexPatterns {
				matches := p.FindAllSubmatch(chunk, -1)
				for _, m := range matches {
					if len(m) < 3 {
						continue
					}
					arrName := string(m[1])
					idx, err := strconv.Atoi(string(m[2]))
					if err != nil {
						continue
					}
					if fn := resolveArrayFunctionName(js, arrName, idx); fn != "" {
						return fn
					}
				}
			}

			for _, p := range nTransformDirectPatterns {
				m := p.FindSubmatch(chunk)
				if len(m) >= 2 {
					name := string(m[1])
					if isValidIdentifier(name) {
						return name
					}
				}
			}
		}
	}
	return ""
}

// findNFunctionNameGlobal is the slow but robust global scan fallback.
func findNFunctionNameGlobal(js []byte) string {
	for _, p := range nTransformIndexPatterns {
		matches := p.FindAllSubmatch(js, -1)
		for _, m := range matches {
			if len(m) < 3 {
				continue
			}
			arrName := string(m[1])
			idx, err := strconv.Atoi(string(m[2]))
			if err != nil {
				continue
			}
			if fn := resolveArrayFunctionName(js, arrName, idx); fn != "" {
				return fn
			}
		}
	}

	for _, p := range nTransformDirectPatterns {
		m := p.FindSubmatch(js)
		if len(m) >= 2 {
			name := string(m[1])
			if isValidIdentifier(name) {
				return name
			}
		}
	}

	legacy := nTransformLegacyPattern.FindSubmatch(js)
	if len(legacy) >= 3 && isValidIdentifier(string(legacy[2])) {
		return string(legacy[2])
	}

	return ""
}

func resolveArrayFunctionName(js []byte, arrName string, idx int) string {
	if idx < 0 {
		return ""
	}

	arrayPattern := regexp.MustCompile(fmt.Sprintf(`(?:var|let|const)\s+%s\s*=\s*\[([^\]]+)\]|\b%s\s*=\s*\[([^\]]+)\]`, regexp.QuoteMeta(arrName), regexp.QuoteMeta(arrName)))
	match := arrayPattern.FindSubmatch(js)
	if len(match) < 2 {
		return ""
	}

	arrayText := ""
	if len(match) > 1 && len(match[1]) > 0 {
		arrayText = string(match[1])
	} else if len(match) > 2 {
		arrayText = string(match[2])
	}
	elements := strings.Split(arrayText, ",")
	if idx >= len(elements) {
		return ""
	}

	candidate := strings.TrimSpace(elements[idx])
	candidate = strings.Trim(candidate, "'\"")
	if !isValidIdentifier(candidate) {
		return ""
	}
	return candidate
}

func isWrapperSignatureCandidate(js []byte, funcName string) bool {
	if funcName == "" {
		return false
	}
	// Instead of evaluating back-tracking regexes on every occurrence of the funcName,
	// just look up the definite wrapper names and compare.
	wrappers := findURLTransformFunctionNames(js)
	for _, w := range wrappers {
		if w == funcName {
			return true
		}
	}
	return false
}

// findSignatureTimestamp extracts the signatureTimestamp (STS) from player.js
// This is required for the player API to return premium formats
func findSignatureTimestamp(js []byte) int {
	// Pattern: signatureTimestamp:12345 or "signatureTimestamp":12345
	pattern := regexp.MustCompile(`["\']?signatureTimestamp["\']?\s*[=:]\s*(\d+)`)
	match := pattern.FindSubmatch(js)
	if len(match) >= 2 {
		sts, _ := strconv.Atoi(string(match[1]))
		return sts
	}

	// Fallback: look for sts= pattern
	pattern2 := regexp.MustCompile(`\bsts\s*=\s*(\d{4,6})\b`)
	match = pattern2.FindSubmatch(js)
	if len(match) >= 2 {
		sts, _ := strconv.Atoi(string(match[1]))
		return sts
	}

	return 0
}

func isValidIdentifier(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}

	switch s {
	case "function", "return", "var", "let", "const", "if", "for", "while", "do", "switch", "case", "default", "new", "this", "class", "extends", "try", "catch", "finally", "throw", "typeof", "void", "delete", "in", "instanceof", "null", "true", "false", "undefined":
		return false
	}

	for i, c := range s {
		if i == 0 {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '$') {
				return false
			}
		} else {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$') {
				return false
			}
		}
	}
	return true
}

func isNumericStr(s string) bool {
	if len(s) == 0 || len(s) > 5 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func escapeJSString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "\\'")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	return s
}

func httpGetBytes(client *http.Client, url string) ([]byte, error) {
	return httpGetBytesContext(context.Background(), client, url)
}

func httpGetBytesContext(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	return io.ReadAll(resp.Body)
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

func computePlayerFingerprint(playerJS []byte) string {
	if len(playerJS) == 0 {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(strconv.Itoa(len(playerJS))))
	_, _ = h.Write([]byte{0})
	sample := func(chunk []byte) {
		if len(chunk) == 0 {
			return
		}
		_, _ = h.Write(chunk)
		_, _ = h.Write([]byte{0})
	}
	const edge = 2048
	sample(playerJS[:min(len(playerJS), edge)])
	if len(playerJS) > edge*2 {
		mid := len(playerJS) / 2
		start := max(0, mid-edge/2)
		end := min(len(playerJS), start+edge)
		sample(playerJS[start:end])
	}
	if len(playerJS) > edge {
		sample(playerJS[max(0, len(playerJS)-edge):])
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

func buildNTransformRuntime(playerJS []byte, funcName string) []byte {
	if len(playerJS) == 0 || funcName == "" {
		return nil
	}
	code := string(playerJS)
	exposed := fmt.Sprintf("_exposed['%s']=%s;})(_yt_player);", funcName, funcName)
	modified := nRuntimeExposePattern.ReplaceAllString(code, exposed)
	if modified == code {
		return nil
	}
	return []byte(modified)
}

func fetchPlayerJS(videoID string, httpClient *http.Client, playerBaseURL string) (string, []byte, error) {
	return fetchPlayerJSContext(context.Background(), videoID, httpClient, playerBaseURL)
}

func playerPageURLs(videoID, playerBaseURL string, embedFirst bool) []string {
	pageURLs := make([]string, 0, 4)
	addPageURL := func(base string, first string, second string) {
		pageURLs = append(pageURLs,
			fmt.Sprintf(first, base, videoID),
			fmt.Sprintf(second, base, videoID),
		)
	}

	if embedFirst {
		addPageURL(playerBaseURL, "%s/embed/%s?hl=en", "%s/watch?v=%s")
		if playerBaseURL != PlayerJSURLBase {
			addPageURL(PlayerJSURLBase, "%s/embed/%s?hl=en", "%s/watch?v=%s")
		}
		return pageURLs
	}

	addPageURL(playerBaseURL, "%s/watch?v=%s", "%s/embed/%s?hl=en")
	if playerBaseURL != PlayerJSURLBase {
		addPageURL(PlayerJSURLBase, "%s/watch?v=%s", "%s/embed/%s?hl=en")
	}
	return pageURLs
}

func fetchPlayerJSContext(ctx context.Context, videoID string, httpClient *http.Client, playerBaseURL string) (string, []byte, error) {
	pageURLs := playerPageURLs(videoID, playerBaseURL, true)

	return fetchPlayerJSFromPageURLsContext(ctx, httpClient, playerBaseURL, pageURLs)
}

func fetchPlayerJSWatchFirstContext(ctx context.Context, videoID string, httpClient *http.Client, playerBaseURL string) (string, []byte, error) {
	return fetchPlayerJSFromPageURLsContext(ctx, httpClient, playerBaseURL, playerPageURLs(videoID, playerBaseURL, false))
}

func fetchPlayerJSFromPageURLsContext(ctx context.Context, httpClient *http.Client, playerBaseURL string, pageURLs []string) (string, []byte, error) {
	var lastErr error
	for _, pageURL := range pageURLs {
		body, err := httpGetBytesContext(ctx, httpClient, pageURL)
		if err != nil {
			lastErr = err
			continue
		}

		playerPath := preferredPlayerPathFromPage(body)
		if playerPath == "" {
			lastErr = fmt.Errorf("unable to find base.js URL in %s", pageURL)
			continue
		}

		pageBaseURL := playerBaseURL
		if parsedPageURL, parseErr := url.Parse(pageURL); parseErr == nil {
			pageBaseURL = parsedPageURL.Scheme + "://" + parsedPageURL.Host
		}

		playerJS, err := httpGetBytesContext(ctx, httpClient, pageBaseURL+playerPath)
		if err != nil {
			lastErr = fmt.Errorf("failed to fetch player JS from %s: %w", pageURL, err)
			continue
		}

		return playerPath, playerJS, nil
	}

	if lastErr == nil {
		lastErr = errors.New("unable to fetch player JS")
	}

	return "", nil, lastErr
}

func preferredPlayerPathFromPage(body []byte) string {
	matches := basejsPattern.FindAllString(string(body), -1)
	if len(matches) == 0 {
		return ""
	}

	seen := make(map[string]struct{}, len(matches))
	paths := make([]string, 0, len(matches))
	for _, match := range matches {
		if _, ok := seen[match]; ok {
			continue
		}
		seen[match] = struct{}{}
		paths = append(paths, match)
	}

	best := paths[0]
	bestScore := playerPathPreferenceScore(best)
	for _, path := range paths[1:] {
		score := playerPathPreferenceScore(path)
		if score > bestScore || (score == bestScore && len(path) < len(best)) {
			best = path
			bestScore = score
		}
	}

	return best
}

func playerPathPreferenceScore(path string) int {
	score := 100
	if strings.Contains(path, "player_embed") {
		score += 50
	}
	if strings.Contains(path, "/embed") {
		score -= 10
	}
	if strings.Contains(path, "player_ias") {
		score -= 50
	}
	return score
}
