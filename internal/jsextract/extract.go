package jsextract

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
)

var extractDefinitionPatternTemplates = []string{
	`(?m)(^|[;{}])\s*((?:var|let|const)\s+%s\s*=)`,
	`(?m)(^|[;{}])\s*(function\s+%s\s*\()`,
	`(?m)(^|[;{}])\s*(%s\s*=\s*function\b)`,
	`(?m)(^|[;{}])\s*(%s\s*=\s*[\[{])`,
	`(?m)(^|[;{}])\s*(%s\s*=)`,
}

type segment struct {
	start     int
	end       int
	deps      []string
	depsReady bool
	stmt      ast.Statement
}

type definitionIndex struct {
	segments  []*segment
	nameToSeg map[string]int
}

type wrapperReplacement struct {
	pattern *regexp.Regexp
	replace string
	arrow   bool
}

var (
	extractDefinitionPatternCache   sync.Map
	wrapperReplacementsPatternCache sync.Map
)

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

func buildWrapperReplacements(functionName string) []wrapperReplacement {
	if cached, ok := wrapperReplacementsPatternCache.Load(functionName); ok {
		return cached.([]wrapperReplacement)
	}

	quotedName := regexp.QuoteMeta(functionName)
	replacements := []wrapperReplacement{
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
			pattern: regexp.MustCompile(quotedName + `\s*=\s*function\s*\(`),
			replace: `globalThis["` + functionName + `"]=` + functionName + `=function(`,
		},
		{
			pattern: regexp.MustCompile(`function\s+` + quotedName + `\s*\(`),
			replace: `globalThis["` + functionName + `"]=function ` + functionName + `(`,
		},
		{
			pattern: regexp.MustCompile(quotedName + `\s*=\s*\(`),
			replace: "globalThis[\"" + functionName + "\"]=" + functionName + "=(",
			arrow:   true,
		},
	}
	wrapperReplacementsPatternCache.Store(functionName, replacements)
	return replacements
}

// BuildWrapperRuntimeJS attempts to expose a wrapper function on globalThis.
func BuildWrapperRuntimeJS(jsCode, functionName string) string {
	if jsCode == "" || functionName == "" {
		return ""
	}
	replacements := buildWrapperReplacements(functionName)

	replaced := false
	for _, candidate := range replacements {
		matches := candidate.pattern.FindAllStringIndex(jsCode, -1)
		for i := len(matches) - 1; i >= 0; i-- {
			loc := matches[i]
			if !hasSafeJSIdentifierBoundary(jsCode, loc[0], loc[1]) {
				continue
			}
			if candidate.arrow && !strings.HasPrefix(strings.TrimSpace(jsCode[loc[1]:]), "(") {
				continue
			}

			jsCode = jsCode[:loc[0]] + candidate.replace + jsCode[loc[1]:]
			replaced = true
		}
	}

	if replaced {
		return jsCode
	}
	return ""
}

func hasSafeJSIdentifierBoundary(s string, start, end int) bool {
	if start > 0 {
		prev := s[start-1]
		if IsJSIdentifierByte(prev) {
			return false
		}
		if prev == '.' {
			return false
		}
	}
	return true
}

func IsJSIdentifierByte(b byte) bool {
	return b == '$' || b == '_' || ('0' <= b && b <= '9') || ('A' <= b && b <= 'Z') || ('a' <= b && b <= 'z')
}

// ExtractWithAST extracts a function and its dependencies from player JS.
func ExtractWithAST(jsCode string, funcName string) (string, error) {
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

// ExtractSimple is the fallback extraction without AST.
func ExtractSimple(js []byte, funcName string) (string, error) {
	jsStr := string(js)

	var result strings.Builder

	zDef := extractDefinitionSimple(jsStr, "z")
	if zDef != "" {
		result.WriteString(zDef)
		result.WriteString("\n")
	}

	zjDef := extractDefinitionSimple(jsStr, "zj")
	if zjDef != "" {
		result.WriteString(zjDef)
		result.WriteString("\n")
	}

	p6Def := extractDefinitionSimple(jsStr, "p6")
	if p6Def != "" {
		result.WriteString(p6Def)
		result.WriteString("\n")
	}

	funcDef := extractDefinitionSimple(jsStr, funcName)
	if funcDef == "" {
		return "", fmt.Errorf("function %s not found", funcName)
	}
	result.WriteString(funcDef)

	return result.String(), nil
}

// BuildURLWrapperCode builds a globally exposed wrapper function runtime.
func BuildURLWrapperCode(jsCode, functionName string) (string, error) {
	if jsCode == "" || functionName == "" {
		return "", errors.New("url wrapper function unavailable in current player JS")
	}

	if built := BuildWrapperRuntimeJS(jsCode, functionName); built != "" {
		if built != jsCode {
			return built, nil
		}
		return jsCode, nil
	}

	if strings.Contains(jsCode, `globalThis["`+functionName+`"]`) || strings.Contains(jsCode, `globalThis['`+functionName+`']`) {
		return jsCode, nil
	}

	if extracted, err := ExtractWithAST(jsCode, functionName); err == nil && extracted != "" {
		return extracted, nil
	} else if err != nil {
		return "", fmt.Errorf("malformed JS for url wrapper %s: %w", functionName, err)
	}

	return "", fmt.Errorf("url wrapper function %s unavailable in current player JS", functionName)
}
