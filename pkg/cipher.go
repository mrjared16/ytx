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
		regex:    regexp.MustCompile(`(?:\b|[^a-zA-Z0-9_$])([a-zA-Z0-9_$]{2,})\s*=\s*function\(\s*a\s*\)\s*{\s*a\s*=\s*a\.split\(\s*""\s*\)(?:;[a-zA-Z0-9_$]{2}\.[a-zA-Z0-9_$]{2}\(a,\d+\))?`),
		sigIdx:   1,
		paramIdx: 0,
	},
	{
		regex:    regexp.MustCompile(`\b[a-zA-Z0-9_$]+\s*&&\s*\(\s*[a-zA-Z0-9_$]+\s*=\s*([a-zA-Z0-9_$]{2,})\(\s*(?:(\d+)\s*,\s*)?decodeURIComponent\(\s*[a-zA-Z0-9_$]+\s*\)\s*\)`),
		sigIdx:   1,
		paramIdx: 2,
	},
	{
		regex:    regexp.MustCompile(`\b[cs]\s*&&\s*[adf]\.set\([^,]+\s*,\s*encodeURIComponent\s*\(\s*([a-zA-Z0-9$]+)\(`),
		sigIdx:   1,
		paramIdx: 0,
	},
	{
		regex:    regexp.MustCompile(`\b[a-zA-Z0-9]+\s*&&\s*[a-zA-Z0-9]+\.set\([^,]+\s*,\s*encodeURIComponent\s*\(\s*([a-zA-Z0-9$]+)\(`),
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

	// Determine engine type for pre-spawning
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
		// Auto mode: try bun first
		engineName = "bun"
	}

	// Pre-spawn JS process in parallel with player.js download (saves ~100-150ms)
	// This overlaps process startup with network I/O
	if engineName != "" {
		go func() {
			_ = runtime.PreSpawnJSProcess(context.Background(), engineName, nil, "")
		}()
	}

	// Try cached path first (skip embed page fetch)
	// Use playerBaseURL to benefit from connection reuse
	if cachedPath != "" {
		playerURL := playerBaseURL + cachedPath
		playerJS, err = httpGetBytesContext(ctx, httpClient, playerURL)
		if err == nil {
			playerPath = cachedPath
		} else {
			warnings = append(warnings, fmt.Sprintf("cached player path %s failed, falling back to discovery: %v", cachedPath, err))
		}
		// If cached path fails, fall through to embed page fetch
	}

	if playerPath == "" {
		playerPath, playerJS, err = fetchPlayerJSContext(ctx, videoID, httpClient, playerBaseURL)
		if err != nil {
			return nil, "", err
		}
	}

	sigName, sigParam, err := findSigFunctionName(playerJS)
	sigUsesURLWrapper := false
	if err != nil {
		if cachedPath != "" && playerPath == cachedPath {
			playerPath, playerJS, err = fetchPlayerJSContext(ctx, videoID, httpClient, playerBaseURL)
			if err != nil {
				return nil, "", err
			}
			sigName, sigParam, err = findSigFunctionName(playerJS)
		}
	}
	if err != nil {
		if wrapperName := findURLTransformFunctionName(playerJS); wrapperName != "" {
			sigName = wrapperName
			sigParam = 0
			sigUsesURLWrapper = true
			err = nil
		}
	}

	// Find n-function name (we don't need the body, playerJS is used directly)
	nName := findNFunctionName(playerJS)
	if sigName == "" && nName == "" {
		if err != nil {
			return nil, "", err
		}
		return nil, "", errors.New("failed to detect signature and n transform functions")
	}

	// Extract signatureTimestamp (STS) from player.js
	sts := findSignatureTimestamp(playerJS)
	fingerprint := computePlayerFingerprint(playerJS)
	nRuntimeJS := buildNTransformRuntime(playerJS, nName)

	return &Cipher{
		runtime:            runtime,
		sigFunctionName:    sigName,
		sigParam:           sigParam,
		sigUsesURLWrapper:  sigUsesURLWrapper,
		nFunctionName:      nName,
		signatureTimestamp: sts,
		nRuntimeJS:         nRuntimeJS,
		playerURL:          playerPath,
		playerFingerprint:  fingerprint,
		playerJS:           playerJS,
		warnings:           warnings,
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
	return &CipherCache{
		Version:            currentCacheVersion,
		CreatedAt:          now,
		ExpiresAt:          now.Add(cacheTTL),
		PlayerURL:          c.playerURL,
		PlayerFingerprint:  c.playerFingerprint,
		SigFunction:        c.sigFunctionName,
		SigParam:           c.sigParam,
		SigUsesURLWrapper:  c.sigUsesURLWrapper,
		IsBytecode:         c.isBytecode,
		NFunction:          c.nFunctionName,
		SignatureTimestamp: c.signatureTimestamp,
		JSCode:             c.jsCode,
	}
}

// Prewarm spins up the QuickJS runtime aggressively ahead of time
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

	if c.sigUsesURLWrapper {
		c.jsCode = buildWrapperRuntimeJS(string(c.playerJS), c.sigFunctionName)
		if c.jsCode == "" {
			c.jsCode = string(c.playerJS)
		}
	} else {
		jsCode, err := extractWithAST(string(c.playerJS), c.sigFunctionName)
		if err != nil {
			jsCode, err = extractSimple(c.playerJS, c.sigFunctionName)
			if err != nil {
				return fmt.Errorf("failed to extract sig function: %w", err)
			}
		}
		c.jsCode = jsCode
	}

	// Fast Boot Optimization: Automatically compile to QuickJS Bytecode if plain script.
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
	if jsCode == "" || functionName == "" {
		return ""
	}

	quotedName := regexp.QuoteMeta(functionName)
	replacements := []struct {
		pattern *regexp.Regexp
		replace string
	}{
		{
			pattern: regexp.MustCompile(`\bvar\s+` + quotedName + `\s*=\s*function\s*\(`),
			replace: `var ` + functionName + `=globalThis["` + functionName + `"]=function(`,
		},
		{
			pattern: regexp.MustCompile(`\blet\s+` + quotedName + `\s*=\s*function\s*\(`),
			replace: `let ` + functionName + `=globalThis["` + functionName + `"]=function(`,
		},
		{
			pattern: regexp.MustCompile(`\bconst\s+` + quotedName + `\s*=\s*function\s*\(`),
			replace: `const ` + functionName + `=globalThis["` + functionName + `"]=function(`,
		},
		{
			pattern: regexp.MustCompile(`\b` + quotedName + `\s*=\s*function\s*\(`),
			replace: `globalThis["` + functionName + `"]=` + functionName + `=function(`,
		},
		{
			pattern: regexp.MustCompile(`function\s+` + quotedName + `\s*\(`),
			replace: `globalThis["` + functionName + `"]=function ` + functionName + `(`,
		},
	}

	for _, candidate := range replacements {
		loc := candidate.pattern.FindStringIndex(jsCode)
		if loc == nil {
			continue
		}
		return jsCode[:loc[0]] + candidate.replace + jsCode[loc[1]:]
	}

	return ""
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
	if err := c.ensureSignatureReady(); err != nil {
		return "", err
	}

	if c.sigUsesURLWrapper {
		codes := c.wrapperCodeCandidates()
		var lastErr error
		for _, wrapperCode := range codes {
			decrypted, err := c.transformWithURLWrapper(wrapperCode, c.sigFunctionName, sig, "s")
			if err == nil {
				return decrypted, nil
			}
			lastErr = err
		}
		if lastErr != nil {
			return "", lastErr
		}
		return "", errors.New("url wrapper function unavailable in current player JS")
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

	result := ctx.Eval(callCode, quickjs.EvalFlagGlobal(true))
	if ctx.HasException() {
		exc := ctx.Exception()
		result.Free()
		return "", fmt.Errorf("failed to call %s: %v", c.sigFunctionName, exc)
	}

	if result.IsUndefined() || result.IsNull() {
		result.Free()
		return "", errors.New("sig function returned null/undefined")
	}

	resultStr := result.String()
	result.Free()
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
	candidates := make([]string, 0, 2)
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
		add(string(c.playerJS))
	}

	return candidates
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
		quickjs.WithMemoryLimit(256*1024*1024),
		quickjs.WithMaxStackSize(8*1024*1024),
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
	if c.isBytecode {
		val = ctx.EvalBytecode([]byte(c.jsCode))
	} else {
		val = ctx.Eval(c.jsCode, quickjs.EvalFlagGlobal(true))
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
var __fn=null;
try{
  if(typeof %s==='function'){
    __fn=%s;
  }
}catch(e){}
if(typeof globalThis['%s']==='function'){
  __fn=globalThis['%s'];
}else{
  for(var __gk in globalThis){
    try{
      var __gv=globalThis[__gk];
      if(__gv&&typeof __gv==='object'&&typeof __gv['%s']==='function'){
        __fn=__gv['%s'];
        break;
      }
    }catch(e){}
  }
}
if(typeof __fn!=='function')throw new Error('wrapper function not found: %s');
var __u=__fn('https://www.youtube.com/watch?v=ytx','s',encodeURIComponent('%s'));
if(!__u||typeof __u.get!=='function')return '';
var __p=Object.getPrototypeOf(__u)||{};
var __keys=Object.keys(__p).concat(Object.getOwnPropertyNames(__p));
for(var __i=0;__i<__keys.length;__i++){
  var __k=__keys[__i];
  if(__k==='constructor'||__k==='set'||__k==='get'||__k==='clone')continue;
  try{if(typeof __u[__k]==='function'){__u[__k]();break;}}catch(e){}
}
var __s=__u.get('s');
return __s?decodeURIComponent(__s):'';
})()`, functionName, functionName, escapedName, escapedName, escapedName, escapedName, escapedName, escapedValue)
	} else {
		script = fmt.Sprintf(`(function(){
if(typeof g==='undefined'){var g={};}
if(typeof g.qJ!=='function'){
  if(typeof qJ==='function'){g.qJ=qJ;}
  else if(typeof globalThis.qJ==='function'){g.qJ=globalThis.qJ;}
}
var __fn=null;
try{
  if(typeof %s==='function'){
    __fn=%s;
  }
}catch(e){}
if(typeof globalThis['%s']==='function'){
  __fn=globalThis['%s'];
}else{
  for(var __gk in globalThis){
    try{
      var __gv=globalThis[__gk];
      if(__gv&&typeof __gv==='object'&&typeof __gv['%s']==='function'){
        __fn=__gv['%s'];
        break;
      }
    }catch(e){}
  }
}
if(typeof __fn!=='function')return '%s';
var __u=__fn('https://www.youtube.com/watch?v=ytx','s',undefined);
if(!__u||typeof __u.set!=='function'||typeof __u.get!=='function')return '%s';
__u.set('n','%s');
var __p=Object.getPrototypeOf(__u)||{};
var __keys=Object.keys(__p).concat(Object.getOwnPropertyNames(__p));
for(var __i=0;__i<__keys.length;__i++){
  var __k=__keys[__i];
  if(__k==='constructor'||__k==='set'||__k==='get'||__k==='clone')continue;
  try{if(typeof __u[__k]==='function'){__u[__k]();break;}}catch(e){}
}
var __n=__u.get('n');
return __n||'%s';
})()`, functionName, functionName, escapedName, escapedName, escapedName, escapedName, escapedValue, escapedValue, escapedValue, escapedValue)
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

// findSigFunctionName finds the signature function name and optional param
func findSigFunctionName(js []byte) (string, int, error) {
	for _, p := range sigFunctionPatterns {
		m := p.regex.FindSubmatch(js)
		if len(m) <= p.sigIdx {
			continue
		}

		funcName := strings.TrimSpace(string(m[p.sigIdx]))
		if !isValidIdentifier(funcName) {
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

	marker := []byte(",decodeURIComponent(")
	idx := bytes.Index(js, marker)

	for idx != -1 {
		start := idx - 60
		if start < 0 {
			start = 0
		}
		context := string(js[start:idx])

		eqIdx := strings.LastIndex(context, "=")
		if eqIdx != -1 {
			afterEq := context[eqIdx+1:]
			parenIdx := strings.Index(afterEq, "(")
			if parenIdx != -1 {
				funcName := strings.TrimSpace(afterEq[:parenIdx])
				numStr := strings.TrimSpace(afterEq[parenIdx+1:])

				if isValidIdentifier(funcName) && isNumericStr(numStr) {
					beforeEq := context[:eqIdx]
					if strings.Contains(beforeEq, "&&(") {
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

	return "", 0, errors.New("could not find signature function name")
}

func findURLTransformFunctionName(js []byte) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\b([a-zA-Z0-9_$]{2,})\s*=\s*function\([^)]*\)\{[^\{\}]{0,800}\.set\("alr","yes"\)`),
		regexp.MustCompile(`\b([a-zA-Z0-9_$]{2,})\s*=\s*function\([^)]*\)\{[^\{\}]{0,800}\.set\('alr','yes'\)`),
		regexp.MustCompile(`function\s+([a-zA-Z0-9_$]{2,})\([^)]*\)\{[^\{\}]{0,800}\.set\("alr","yes"\)`),
		regexp.MustCompile(`function\s+([a-zA-Z0-9_$]{2,})\([^)]*\)\{[^\{\}]{0,800}\.set\('alr','yes'\)`),
	}

	invalidPatterns := []*regexp.Regexp{
		regexp.MustCompile(`\.call\(this\)`),
		regexp.MustCompile(`\.policy\s*=`),
	}

	for _, p := range patterns {
		matches := p.FindAllSubmatchIndex(js, -1)
		for _, idx := range matches {
			if len(idx) < 4 {
				continue
			}
			fullMatch := js[idx[0]:idx[1]]
			name := strings.TrimSpace(string(js[idx[2]:idx[3]]))

			// Skip invalid constructor matches (like Xo)
			invalid := false
			for _, invalidP := range invalidPatterns {
				if invalidP.Match(fullMatch) {
					invalid = true
					break
				}
			}
			if invalid {
				continue
			}

			if isValidIdentifier(name) {
				return name
			}
		}
	}

	return ""
}

// findNFunctionName finds the n-parameter function name
func findNFunctionName(js []byte) string {
	indexPatterns := []*regexp.Regexp{
		regexp.MustCompile(`\.get\("n"\)\)&&\(b=([a-zA-Z0-9$_]+)\[(\d+)\]`),
		regexp.MustCompile(`([a-zA-Z0-9$_]+)\[(\d+)\]\(\s*[a-zA-Z0-9$_]+\.get\("n"\)\s*\)`),
	}

	for _, p := range indexPatterns {
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

	directPatterns := []*regexp.Regexp{
		regexp.MustCompile(`\bb\s*=\s*([a-zA-Z0-9$_]+)(?:\.call\([^,]+,\s*|\()\s*[a-zA-Z0-9$_]+\.get\("n"\)`),
	}
	for _, p := range directPatterns {
		m := p.FindSubmatch(js)
		if len(m) < 2 {
			continue
		}
		name := string(m[1])
		if isValidIdentifier(name) {
			return name
		}
	}

	legacyPattern := regexp.MustCompile(`var\s+[a-zA-Z0-9$_]{2,}\s*=\s*\[([a-zA-Z0-9$_]{2,})\]`)
	legacy := legacyPattern.FindSubmatch(js)
	if len(legacy) >= 2 && isValidIdentifier(string(legacy[1])) {
		return string(legacy[1])
	}

	return ""
}

func resolveArrayFunctionName(js []byte, arrName string, idx int) string {
	if idx < 0 {
		return ""
	}

	arrayPattern := regexp.MustCompile(fmt.Sprintf(`(?:var|let|const)\s+%s\s*=\s*\[([^\]]+)\]`, regexp.QuoteMeta(arrName)))
	match := arrayPattern.FindSubmatch(js)
	if len(match) < 2 {
		return ""
	}

	elements := strings.Split(string(match[1]), ",")
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

func fetchPlayerJSContext(ctx context.Context, videoID string, httpClient *http.Client, playerBaseURL string) (string, []byte, error) {
	pageURLs := make([]string, 0, 4)
	addPageURL := func(base string) {
		pageURLs = append(pageURLs,
			fmt.Sprintf("%s/embed/%s?hl=en", base, videoID),
			fmt.Sprintf("%s/watch?v=%s", base, videoID),
		)
	}
	addPageURL(playerBaseURL)
	if playerBaseURL != PlayerJSURLBase {
		addPageURL(PlayerJSURLBase)
	}

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
