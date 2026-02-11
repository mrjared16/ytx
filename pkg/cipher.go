package ytx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	sigFunctionName    string
	sigParam           int
	nFunctionName      string
	signatureTimestamp int    // STS from player.js, needed for API requests
	jsCode             string // Self-contained JS with function + dependencies
	playerURL          string // Player JS URL (for cache versioning)
	playerJS           []byte // Full player JS for n-function extraction
}

// NFunctionName returns the n-function name (for debugging)
func (c *Cipher) NFunctionName() string { return c.nFunctionName }

// PlayerJSLen returns the length of playerJS (for debugging)
func (c *Cipher) PlayerJSLen() int { return len(c.playerJS) }

// SignatureTimestamp returns the STS value needed for API requests
func (c *Cipher) SignatureTimestamp() int { return c.signatureTimestamp }

// Base.js URL pattern
var basejsPattern = regexp.MustCompile(`/s/player/[\w-]+/[\w./-]+/base\.js`)

var extractDefinitionPatternTemplates = []string{
	`(?m)(^|[;{}])\s*((?:var|let|const)\s+%s\s*=)`,
	`(?m)(^|[;{}])\s*(function\s+%s\s*\()`,
	`(?m)(^|[;{}])\s*(%s\s*=\s*function\b)`,
	`(?m)(^|[;{}])\s*(%s\s*=\s*[\[{])`,
	`(?m)(^|[;{}])\s*(%s\s*=)`,
}

var extractDefinitionPatternCache sync.Map

// NewCipherWithCachedPath creates a new cipher, optionally using a cached base.js path
// If cachedPath is provided and valid, skips the embed page fetch (~150ms savings)
// baseURL allows domain reuse (e.g., music.youtube.com for music mode) to avoid extra TLS handshake
func NewCipherWithCachedPath(videoID string, httpClient *http.Client, cachedPath string, baseURL string) (*Cipher, string, error) {
	var playerPath string
	var playerJS []byte
	var err error

	// For player.js, prefer the provided baseURL (for connection reuse with music.youtube.com)
	// But for embed page, always use youtube.com (music.youtube.com/embed doesn't work)
	playerBaseURL := baseURL
	if playerBaseURL == "" {
		playerBaseURL = PlayerJSURLBase
	}

	// Determine engine type for pre-spawning
	engineType := GetEngineType()
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
		go PreSpawnJSProcess(engineName)
	}

	// Try cached path first (skip embed page fetch)
	// Use playerBaseURL to benefit from connection reuse
	if cachedPath != "" {
		playerURL := playerBaseURL + cachedPath
		playerJS, err = httpGetBytes(httpClient, playerURL)
		if err == nil {
			playerPath = cachedPath
		}
		// If cached path fails, fall through to embed page fetch
	}

	// Fallback: fetch embed page to find current base.js URL
	// Always use youtube.com for embed page (music.youtube.com/embed doesn't work)
	if playerPath == "" {
		embedURL := fmt.Sprintf("%s/embed/%s?hl=en", PlayerJSURLBase, videoID)
		embedBody, err := httpGetBytes(httpClient, embedURL)
		if err != nil {
			return nil, "", fmt.Errorf("failed to fetch embed page: %w", err)
		}

		playerPath = basejsPattern.FindString(string(embedBody))
		if playerPath == "" {
			return nil, "", errors.New("unable to find base.js URL in embed page")
		}

		// Use playerBaseURL for the actual player.js fetch (connection reuse)
		playerURL := playerBaseURL + playerPath
		playerJS, err = httpGetBytes(httpClient, playerURL)
		if err != nil {
			return nil, "", fmt.Errorf("failed to fetch player JS: %w", err)
		}
	}

	// Find signature function name and param
	sigName, sigParam, err := findSigFunctionName(playerJS)
	if err != nil {
		return nil, "", err
	}

	// Extract sig function and dependencies using AST
	jsCode, err := extractWithAST(string(playerJS), sigName)
	if err != nil {
		// Fallback to simple extraction if AST fails
		jsCode, err = extractSimple(playerJS, sigName)
		if err != nil {
			return nil, "", fmt.Errorf("failed to extract sig function: %w", err)
		}
	}

	// Find n-function name (we don't need the body, playerJS is used directly)
	nName := findNFunctionName(playerJS)

	// Extract signatureTimestamp (STS) from player.js
	sts := findSignatureTimestamp(playerJS)

	return &Cipher{
		sigFunctionName:    sigName,
		sigParam:           sigParam,
		nFunctionName:      nName,
		signatureTimestamp: sts,
		jsCode:             jsCode,
		playerURL:          playerPath,
		playerJS:           playerJS,
	}, playerPath, nil
}

// NewCipherFromCache reconstructs a Cipher from cached data
func NewCipherFromCache(cache *CipherCache) *Cipher {
	return &Cipher{
		sigFunctionName:    cache.SigFunction,
		sigParam:           cache.SigParam,
		nFunctionName:      cache.NFunction,
		signatureTimestamp: cache.SignatureTimestamp,
		jsCode:             cache.JSCode,
		playerURL:          cache.PlayerURL,
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
		SigFunction:        c.sigFunctionName,
		SigParam:           c.sigParam,
		NFunction:          c.nFunctionName,
		SignatureTimestamp: c.signatureTimestamp,
		JSCode:             c.jsCode,
	}
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
		"a": true, "b": true, "c": true, "d": true, "e": true, "f": true,
		"g": true, "h": true, "i": true, "j": true,
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
this.location = { href: 'https://www.youtube.com/' };
this.document = {
    getElementsByTagName: function() { return []; },
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
    cookie: '',
    domain: 'youtube.com'
};
this.navigator = { userAgent: 'Mozilla/5.0', platform: 'Win32', language: 'en-US', languages: ['en-US'] };
this.console = { log: function() {}, warn: function() {}, error: function() {}, info: function() {}, debug: function() {} };

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
	rt := quickjs.NewRuntime(
		quickjs.WithMemoryLimit(256*1024*1024),
		quickjs.WithMaxStackSize(1024*1024),
	)
	defer rt.Close()
	ctx := rt.NewContext()
	defer ctx.Close()

	// Inject browser stubs first (needed for signature function)
	stubsVal := ctx.Eval(browserStubsJS, quickjs.EvalFlagGlobal(true))
	if ctx.HasException() {
		exc := ctx.Exception()
		stubsVal.Free()
		return "", fmt.Errorf("failed to inject browser stubs: %v", exc)
	}
	stubsVal.Free()

	val := ctx.Eval(c.jsCode, quickjs.EvalFlagGlobal(true))
	if ctx.HasException() {
		exc := ctx.Exception()
		val.Free()
		return "", fmt.Errorf("failed to load JS: %v", exc)
	}
	val.Free()

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
	if c.nFunctionName == "" {
		return n, nil
	}

	// Need full player.js for the JS runtime
	if len(c.playerJS) == 0 {
		return n, fmt.Errorf("no player.js available for n-transform")
	}

	// Use cached engine for efficiency (engine type set via SetEngineType)
	engine, err := GetCachedEngine(c.playerJS, c.nFunctionName)
	if err != nil {
		return n, err
	}

	return engine.TransformN(n)
}

// TransformNBatch transforms multiple n-parameters in a single IPC call
func (c *Cipher) TransformNBatch(nValues []string) ([]string, error) {
	if c.nFunctionName == "" || len(nValues) == 0 {
		return nValues, nil
	}

	if len(c.playerJS) == 0 {
		return nValues, fmt.Errorf("no player.js available for n-transform")
	}

	engine, err := GetCachedEngine(c.playerJS, c.nFunctionName)
	if err != nil {
		return nValues, err
	}

	return engine.TransformNBatch(nValues)
}

// findSigFunctionName finds the signature function name and optional param
func findSigFunctionName(js []byte) (string, int, error) {
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

// findNFunctionName finds the n-parameter function name
func findNFunctionName(js []byte) string {
	// Pattern from pytubefix: exactly 3-char variable and function names
	pattern := regexp.MustCompile(`var\s+[a-zA-Z0-9$_]{3}\s*=\s*\[([a-zA-Z0-9$_]{3})\]`)
	match := pattern.FindSubmatch(js)
	if len(match) >= 2 {
		return string(match[1])
	}

	// Fallback pattern for older player.js versions
	pattern2 := regexp.MustCompile(`\.get\("n"\)\)&&\(b=([a-zA-Z0-9$]{1,3})\[(\d+)\](.+)\|\|([a-zA-Z0-9]{1,3})`)
	match = pattern2.FindSubmatch(js)
	if len(match) >= 5 {
		idx, _ := strconv.Atoi(string(match[2]))
		if idx == 0 {
			return string(match[4])
		}
		return string(match[1])
	}

	return ""
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
	if len(s) < 1 || len(s) > 10 {
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
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	return io.ReadAll(resp.Body)
}
