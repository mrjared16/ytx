package ytx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
)

// Cipher handles YouTube signature decryption using goja
//
// DESIGN (10x engineer approach):
// 1. Use goja/parser to parse JS into AST (already a dependency)
// 2. Walk AST to find function and dependencies
// 3. Extract source text for self-contained code
// 4. Pre-compile JS program once, reuse for all decryptions
//
// This is more robust than regex and doesn't require new dependencies
type Cipher struct {
	sigFunctionName string
	sigParam        int
	nFunctionName   string
	jsCode          string        // Self-contained JS with function + dependencies
	playerURL       string        // Player JS URL (for cache versioning)
	compiled        *goja.Program // Pre-compiled JS program for fast execution
}

// Base.js URL pattern
var basejsPattern = regexp.MustCompile(`/s/player/[\w-]+/[\w./-]+/base\.js`)

// NewCipher creates a new cipher by fetching and parsing the player JS
func NewCipher(videoID string, httpClient *http.Client) (*Cipher, error) {
	cipher, _, err := NewCipherWithURL(videoID, httpClient)
	return cipher, err
}

// NewCipherWithURL creates a new cipher and returns the player URL for caching
func NewCipherWithURL(videoID string, httpClient *http.Client) (*Cipher, string, error) {
	// 1. Get embed page to find player JS URL
	embedURL := fmt.Sprintf("https://www.youtube.com/embed/%s?hl=en", videoID)
	embedBody, err := httpGetBytes(httpClient, embedURL)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch embed page: %w", err)
	}

	// 2. Extract player JS path
	playerPath := basejsPattern.FindString(string(embedBody))
	if playerPath == "" {
		return nil, "", errors.New("unable to find base.js URL in embed page")
	}

	// 3. Fetch player JS
	playerURL := "https://www.youtube.com" + playerPath
	playerJS, err := httpGetBytes(httpClient, playerURL)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch player JS: %w", err)
	}

	// 4. Find signature function name and param
	sigName, sigParam, err := findSigFunctionName(playerJS)
	if err != nil {
		return nil, "", err
	}

	// 5. Extract sig function and dependencies using AST
	jsCode, err := extractWithAST(string(playerJS), sigName)
	if err != nil {
		// Fallback to simple extraction if AST fails
		jsCode, err = extractSimple(playerJS, sigName)
		if err != nil {
			return nil, "", fmt.Errorf("failed to extract sig function: %w", err)
		}
	}

	// 6. Find n-function (optional)
	nName := findNFunctionName(playerJS)
	if nName != "" {
		nCode, err := extractWithAST(string(playerJS), nName)
		if err == nil {
			jsCode += "\n" + nCode
		}
	}

	// 7. Pre-compile the JS program for faster execution
	compiled, err := goja.Compile("cipher", jsCode, false)
	if err != nil {
		return nil, "", fmt.Errorf("failed to compile cipher JS: %w", err)
	}

	return &Cipher{
		sigFunctionName: sigName,
		sigParam:        sigParam,
		nFunctionName:   nName,
		jsCode:          jsCode,
		playerURL:       playerPath,
		compiled:        compiled,
	}, playerPath, nil
}

// NewCipherFromCache reconstructs a Cipher from cached data
func NewCipherFromCache(cache *CipherCache) *Cipher {
	// Pre-compile the JS program (ignore errors, will fail at runtime if invalid)
	compiled, _ := goja.Compile("cipher", cache.JSCode, false)

	return &Cipher{
		sigFunctionName: cache.SigFunction,
		sigParam:        cache.SigParam,
		nFunctionName:   cache.NFunction,
		jsCode:          cache.JSCode,
		playerURL:       cache.PlayerURL,
		compiled:        compiled,
	}
}

// ToCache converts a Cipher to a cacheable format
func (c *Cipher) ToCache() *CipherCache {
	now := time.Now()
	return &CipherCache{
		Version:     1,
		CreatedAt:   now,
		ExpiresAt:   now.Add(cacheTTL),
		PlayerURL:   c.playerURL,
		SigFunction: c.sigFunctionName,
		SigParam:    c.sigParam,
		NFunction:   c.nFunctionName,
		JSCode:      c.jsCode,
	}
}

// extractWithAST uses goja's parser to extract a function and its dependencies
func extractWithAST(jsCode string, funcName string) (string, error) {
	// Parse the JavaScript
	program, err := parser.ParseFile(nil, "", jsCode, 0)
	if err != nil {
		return "", fmt.Errorf("parse error: %w", err)
	}

	// Find the function assignment and collect dependencies
	var funcStart, funcEnd int
	deps := make(map[string]bool)

	// Walk the AST to find our function
	for _, stmt := range program.Body {
		if exprStmt, ok := stmt.(*ast.ExpressionStatement); ok {
			if assignExpr, ok := exprStmt.Expression.(*ast.AssignExpression); ok {
				if ident, ok := assignExpr.Left.(*ast.Identifier); ok {
					if ident.Name.String() == funcName {
						funcStart = int(stmt.Idx0()) - 1
						funcEnd = int(stmt.Idx1())

						// Find dependencies in the function body
						collectIdentifiers(assignExpr.Right, deps)
					}
				}
			}
		}
	}

	if funcStart == 0 && funcEnd == 0 {
		return "", fmt.Errorf("function %s not found in AST", funcName)
	}

	// Remove the function name itself from deps
	delete(deps, funcName)

	// Filter out JavaScript built-ins that don't need extraction
	builtins := map[string]bool{
		"String": true, "Array": true, "Object": true, "Math": true,
		"parseInt": true, "parseFloat": true, "isNaN": true, "isFinite": true,
		"encodeURIComponent": true, "decodeURIComponent": true,
		"encodeURI": true, "decodeURI": true,
		"JSON": true, "console": true, "undefined": true, "null": true,
		"true": true, "false": true, "NaN": true, "Infinity": true,
		"a": true, "b": true, "c": true, "d": true, // Common param names
	}

	// Build the output
	var result strings.Builder

	// Extract ALL discovered dependencies (self-healing approach)
	for dep := range deps {
		if builtins[dep] {
			continue
		}
		depCode := extractDefinitionSimple(jsCode, dep)
		if depCode != "" {
			result.WriteString(depCode)
			result.WriteString("\n")
		}
	}

	// Add the main function
	if funcEnd <= len(jsCode) {
		result.WriteString(jsCode[funcStart:funcEnd])
	}

	return result.String(), nil
}

// collectIdentifiers walks an AST node and collects all identifiers
func collectIdentifiers(node ast.Node, deps map[string]bool) {
	if node == nil {
		return
	}

	switch n := node.(type) {
	case *ast.Identifier:
		deps[n.Name.String()] = true
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

// extractDefinitionSimple extracts a variable/function definition using string parsing
func extractDefinitionSimple(js string, name string) string {
	// Try: var name='...'.split(';')
	patterns := []string{
		"var " + name + "=",
		name + "=function",
		"var " + name + "={",
	}

	for _, pattern := range patterns {
		idx := strings.Index(js, pattern)
		if idx >= 0 {
			return extractStatement(js, idx)
		}
	}

	return ""
}

// extractStatement extracts a complete statement starting at idx
func extractStatement(js string, idx int) string {
	pos := idx
	depth := 0     // Track {} depth
	parenDepth := 0 // Track () depth
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
func (c *Cipher) DecryptSignature(sig string) (string, error) {
	vm := goja.New()

	// Use pre-compiled program if available (faster), fallback to RunString
	if c.compiled != nil {
		_, err := vm.RunProgram(c.compiled)
		if err != nil {
			return "", fmt.Errorf("failed to load JS: %w", err)
		}
	} else {
		_, err := vm.RunString(c.jsCode)
		if err != nil {
			return "", fmt.Errorf("failed to load JS: %w", err)
		}
	}

	// Call the function
	var callCode string
	if c.sigParam > 0 {
		callCode = fmt.Sprintf("%s(%d, '%s')", c.sigFunctionName, c.sigParam, escapeJSString(sig))
	} else {
		callCode = fmt.Sprintf("%s('%s')", c.sigFunctionName, escapeJSString(sig))
	}

	result, err := vm.RunString(callCode)
	if err != nil {
		return "", fmt.Errorf("failed to call %s: %w", c.sigFunctionName, err)
	}

	if result == nil || goja.IsUndefined(result) || goja.IsNull(result) {
		return "", errors.New("sig function returned null/undefined")
	}

	return result.String(), nil
}

// TransformN transforms the n-parameter
func (c *Cipher) TransformN(n string) (string, error) {
	if c.nFunctionName == "" {
		return n, nil
	}

	vm := goja.New()

	// Use pre-compiled program if available
	if c.compiled != nil {
		if _, err := vm.RunProgram(c.compiled); err != nil {
			return n, nil
		}
	} else {
		if _, err := vm.RunString(c.jsCode); err != nil {
			return n, nil
		}
	}

	result, err := vm.RunString(fmt.Sprintf("%s('%s')", c.nFunctionName, escapeJSString(n)))
	if err != nil {
		return n, nil
	}

	if result == nil || goja.IsUndefined(result) || goja.IsNull(result) {
		return n, nil
	}

	return result.String(), nil
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
	pattern := regexp.MustCompile(`\.get\("n"\)\)&&\(b=([a-zA-Z0-9$]{0,3})\[(\d+)\](.+)\|\|([a-zA-Z0-9]{0,3})`)
	match := pattern.FindSubmatch(js)
	if len(match) >= 5 {
		idx, _ := strconv.Atoi(string(match[2]))
		if idx == 0 {
			return string(match[4])
		}
		return string(match[1])
	}

	pattern2 := regexp.MustCompile(`var\s+[a-zA-Z0-9_$]{1,4}\s*=\s*\[([a-zA-Z0-9_$]{1,4})\]`)
	match = pattern2.FindSubmatch(js)
	if len(match) >= 2 {
		return string(match[1])
	}

	return ""
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
