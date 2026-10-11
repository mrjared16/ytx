package ytx

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
	"github.com/dop251/goja/token"

	"github.com/mrjared16/ytx/internal/jsengine"
)

// Modern URL-wrapper protocol support.
//
// Recognized players carry a small wrapper family such as:
//
//	jW=function(T,M,q){M=M===void 0?"":M;q=q===void 0?"":q;T=new g.$I(T,!0);
//	T.set("alr","yes");q&&(q=m5(5,5551,Qe(31,6145,q)),T[S[21]](M,Rm(10,2324,q)));
//	return T};
//
// The wrapper takes (alreadyHasN, queryKey, encodeURIComponent(value)),
// rebuilds the first argument as `new g.<class>(first, true)`, marks the same
// object with `set("alr","yes")`, and — when the third argument is present —
// signs that object under the second argument (the query key): the one
// signing call must sit inside the actual third-argument gate (`if(q){...}`
// or `q&&(...)` with the bare third parameter). The URL transform produced
// by this protocol replaces the separate signature-decrypt and n-transform
// loops with a single subprocess call (Bun/Node only).
//
// Detection is deliberately bounded: exact ALR marker literals are indexed
// and each candidate function is validated structurally by parsing only that
// small reconstructed function — never the whole AST or bundle. Decoy
// markers are ignored; malformed or unsupported candidates are rejected;
// multiple distinct qualifying wrappers are ambiguous and are handled
// fail-closed by callers.

const (
	// Trusted synthetic base for the public signature/n adapters. Real
	// wrappers reject non-googlevideo hosts, so the public DecryptSignature
	// and TransformN routes bind this adapter URL (n is inserted BEFORE the
	// call for TransformN; the signature is signed under the adapter URL for
	// DecryptSignature). Never a real stream URL.
	urlWrapperSyntheticURL  = "https://rr1.googlevideo.com/videoplayback"
	urlWrapperBackScanLimit = 4096 // backward window to the enclosing function brace
	urlWrapperHeaderScan    = 512  // window behind the brace for the function header
	urlWrapperBodyTailLimit = 2048 // forward window to the enclosing close brace
)

var urlWrapperAlrMarkers = [][]byte{
	[]byte(`set("alr","yes")`),
	[]byte(`set('alr','yes')`),
	[]byte(`set("alr", "yes")`),
	[]byte(`set('alr', 'yes')`),
}

type urlWrapperMarker struct {
	offset int
	length int
}

// URL-wrapper discovery outcome. Absent means no modern pattern at all (the
// legacy tier policy applies); Found means exactly one structurally valid
// wrapper; Ambiguous means more than one distinct qualifying wrapper and is
// handled fail-closed (no legacy global scan, no QuickJS fallback).
type urlWrapperDiscoveryStatus int

const (
	urlWrapperDiscoveryAbsent    urlWrapperDiscoveryStatus = 0
	urlWrapperDiscoveryFound     urlWrapperDiscoveryStatus = 1
	urlWrapperDiscoveryAmbiguous urlWrapperDiscoveryStatus = 2
)

type urlWrapperDiscovery struct {
	status urlWrapperDiscoveryStatus
	name   string
}

// urlWrapperDiscover scans every exact ALR marker occurrence and validates
// each enclosing function structurally. A single unique qualifying wrapper
// name is a Found result; decoy markers are ignored; two or more distinct
// qualifying wrappers are ambiguous. No minimum marker count is required: a
// valid wrapper without decoys must be recognized.
func urlWrapperDiscover(js []byte) urlWrapperDiscovery {
	seen := map[string]struct{}{}
	var name string
	for _, mk := range findURLWrapperMarkers(js) {
		candidate := urlWrapperNameAtMarker(js, mk)
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		if name != "" {
			return urlWrapperDiscovery{status: urlWrapperDiscoveryAmbiguous}
		}
		name = candidate
	}
	if name == "" {
		return urlWrapperDiscovery{status: urlWrapperDiscoveryAbsent}
	}
	return urlWrapperDiscovery{status: urlWrapperDiscoveryFound, name: name}
}

// findURLWrapperMarkers indexes every exact ALR marker literal occurrence.
func findURLWrapperMarkers(js []byte) []urlWrapperMarker {
	out := make([]urlWrapperMarker, 0, 8)
	for _, marker := range urlWrapperAlrMarkers {
		offset := 0
		for {
			idx := bytes.Index(js[offset:], marker)
			if idx == -1 {
				break
			}
			absIdx := offset + idx
			out = append(out, urlWrapperMarker{offset: absIdx, length: len(marker)})
			offset = absIdx + len(marker)
		}
	}
	return out
}

// findURLWrapperCandidates returns the single validated wrapper function
// name, or nil when the modern protocol is not recognized or is ambiguous.
func findURLWrapperCandidates(js []byte) []string {
	disc := urlWrapperDiscover(js)
	if disc.status != urlWrapperDiscoveryFound {
		return nil
	}
	return []string{disc.name}
}

// findURLWrapperCandidate returns the single validated wrapper name, or ""
// when the protocol is not recognized/ambiguous.
func findURLWrapperCandidate(js []byte) string {
	names := findURLWrapperCandidates(js)
	if len(names) != 1 {
		return ""
	}
	return names[0]
}

// isModernURLWrapperJS reports whether the full player bundle carries the
// recognized URL-wrapper protocol.
func isModernURLWrapperJS(js []byte) bool {
	return findURLWrapperCandidate(js) != ""
}

// urlWrapperNameAtMarker validates the function enclosing one ALR marker.
func urlWrapperNameAtMarker(js []byte, mk urlWrapperMarker) string {
	brace := urlWrapperEnclosingBrace(js, mk.offset)
	if brace < 0 {
		return ""
	}

	headerStart := brace - urlWrapperHeaderScan
	if headerStart < 0 {
		headerStart = 0
	}
	name, params, ok := urlWrapperFunctionHeader(string(js[headerStart : brace+1]))
	if !ok {
		return ""
	}
	if !isValidIdentifier(name) {
		return ""
	}

	paramNames := urlWrapperParamNames(params)
	if len(paramNames) != 3 {
		return ""
	}

	body := string(js[brace+1 : mk.offset+mk.length])
	tail := urlWrapperBodyTail(js, mk.offset+mk.length)
	if !urlWrapperStructuralChecks(name, params, body, tail, paramNames[0], paramNames[1], paramNames[2]) {
		return ""
	}
	return name
}

// urlWrapperEnclosingBrace finds the brace of the innermost block containing
// the marker with a bounded backward scan. The marker's immediately enclosing
// block must be the wrapper function body itself.
func urlWrapperEnclosingBrace(js []byte, mpos int) int {
	depth := 0
	for i := mpos - 1; i >= 0 && mpos-1-i <= urlWrapperBackScanLimit; i-- {
		switch js[i] {
		case '}':
			depth++
		case '{':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

// urlWrapperFunctionHeader parses a `name=function(params){` or
// `function name(params){` header from a window that must end at the opening
// brace.
func urlWrapperFunctionHeader(window string) (string, string, bool) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`([a-zA-Z0-9_$]{1,64})\s*=\s*function\s*\(([^)]*)\)\s*\{$`),
		regexp.MustCompile(`function\s+([a-zA-Z0-9_$]{1,64})\s*\(([^)]*)\)\s*\{$`),
	}
	for _, p := range patterns {
		m := p.FindSubmatch([]byte(window))
		if len(m) >= 3 {
			return string(m[1]), string(m[2]), true
		}
	}
	return "", "", false
}

// urlWrapperParamNames splits the wrapper parameter list into plain
// identifiers, accepting optional default values (ES6 default parameters such
// as q="", N=void 0, or K=!0) generically — no player-specific names are
// assumed. Segments are split at top-level commas only (quotes and nested
// parens respected); a default value after a top-level '=' is stripped and
// not validated. Any malformed segment yields nil. Defaults containing a ')'
// are not parseable by the bounded header matcher and are rejected upstream.
func urlWrapperParamNames(params string) []string {
	if params == "" {
		return nil
	}
	segs := urlWrapperSplitTopLevel(params, ',')
	out := make([]string, 0, 3)
	for _, seg := range segs {
		name := urlWrapperParamIdentifier(seg)
		if !isValidIdentifier(name) {
			return nil
		}
		out = append(out, name)
	}
	return out
}

// urlWrapperSplitTopLevel splits s on sep bytes that are not inside quotes or
// nested brackets.
func urlWrapperSplitTopLevel(s string, sep byte) []string {
	out := make([]string, 0, 3)
	depth := 0
	start := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case sep:
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

// urlWrapperParamIdentifier returns the identifier of one parameter segment,
// stripping any default value after a top-level '='. The default expression
// is not validated (it may reference other parameters or helpers).
func urlWrapperParamIdentifier(seg string) string {
	seg = strings.TrimSpace(seg)
	depth := 0
	var quote byte
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case '=':
			if depth == 0 {
				return strings.TrimSpace(seg[:i])
			}
		}
	}
	return seg
}

// urlWrapperBodyTail returns the (bounded) text from `from` up to and
// excluding the enclosing close brace, or "" when the body is malformed or
// incomplete within the scan window. String literals are skipped with the
// same bound; an unterminated quote yields "", never an out-of-range slice.
func urlWrapperBodyTail(js []byte, from int) string {
	n := len(js)
	depth := 0
	i := from
	for i < n && i-from <= urlWrapperBodyTailLimit {
		switch js[i] {
		case '"', '\'', '`':
			j := i + 1
			closed := false
			for j < n && j-i <= urlWrapperBodyTailLimit {
				if js[j] == '\\' {
					j += 2
					continue
				}
				if js[j] == js[i] {
					closed = true
					j++
					break
				}
				j++
			}
			if !closed {
				return "" // unterminated string inside the window
			}
			i = j
		case '{':
			depth++
			i++
		case '}':
			if depth == 0 {
				return string(js[from:i])
			}
			depth--
			i++
		default:
			i++
		}
	}
	return "" // truncated/incomplete body: never a wrapper
}

// urlWrapperStructuralChecks validates the wrapper body shape: first param
// rebuilt as `new g.<class>(first, true)`, same object set alr=yes, and a
// scoped AST validation that exactly one signing call — second param as the
// query key on that same object — sits inside the actual third-param gated
// expression/block, with a single final `return p1`. The gating shape is
// bounded-supported: `if(p3)` consequences and `p3&&...` short-circuit
// right-hand sides with the BARE third parameter. Malformed, unsupported,
// or unparseable candidates are rejected.
func urlWrapperStructuralChecks(name, params, body, tail, p1, p2, p3 string) bool {
	ident := `[a-zA-Z0-9_$]{1,64}`
	q1 := regexp.QuoteMeta(p1)

	// First argument is rebuilt as a new g.<class> instance.
	if !regexp.MustCompile(fmt.Sprintf(`\b%s\s*=\s*new\s+g\.(%s)\s*\(\s*%s\s*,\s*(?:true|!0)\s*\)`, q1, ident, q1)).MatchString(body) {
		return false
	}
	// The same object is marked alr=yes.
	if !regexp.MustCompile(fmt.Sprintf(`\b%s\.set\s*\(\s*["']alr["']\s*,\s*["']yes["']\s*\)`, q1)).MatchString(body) {
		return false
	}
	if tail == "" {
		return false
	}
	// Gating, query key, and same-object return are validated together on the
	// small reconstructed function — never as independent token matches.
	return urlWrapperSignGated(name, params, body, tail, p1, p2, p3)
}

// urlWrapperSignProbe collects the outcome of the scoped walk: how many
// signing calls (member call on p1 whose first argument is p2) sat inside a
// p3-gated region, how many sat outside it, how many return statements exist
// in the wrapper scope, and whether every visited form is supported. Any
// unsupported statement/expression form (loops, try/switch, literals that
// can hold calls, nested functions, ...) fails the candidate closed: unknown
// forms are never treated as no-ops that could hide signing.
type urlWrapperSignProbe struct {
	gated     int
	ungated   int
	returns   int
	supported bool
}

// urlWrapperSignGated parses only the small candidate function
// `function NAME(PARAMS){BODY+TAIL}` with the installed goja parser (never
// the whole bundle) and walks that function's statements to require exactly
// one gated signing call, no ungated one, and a single final top-level
// `return p1`. Unparseable, exotic, or hidden-call bodies are rejected
// cleanly and the existing legacy fallback policy is untouched.
func urlWrapperSignGated(name, params, body, tail, p1, p2, p3 string) bool {
	code := fmt.Sprintf(`function %s(%s){%s%s}`, name, params, body, tail)
	program, err := parser.ParseFile(nil, "", code, 0)
	if err != nil {
		return false
	}
	for _, stmt := range program.Body {
		decl, ok := stmt.(*ast.FunctionDeclaration)
		if !ok || decl.Function == nil || decl.Function.Body == nil {
			continue
		}
		probe := &urlWrapperSignProbe{supported: true}
		urlWrapperProbeBody(decl.Function.Body, false, p1, p2, p3, probe)
		if !probe.supported || probe.gated != 1 || probe.ungated != 0 || probe.returns != 1 {
			return false
		}
		// The single return must be the final top-level statement and must
		// return the same object p1; unreachable, wrong, or earlier
		// alternate returns break the contract.
		list := decl.Function.Body.List
		if len(list) == 0 {
			return false
		}
		last, ok := list[len(list)-1].(*ast.ReturnStatement)
		if !ok || last.Argument == nil {
			return false
		}
		return urlWrapperIsName(last.Argument, p1)
	}
	return false
}

// urlWrapperProbeBody walks one statement block, threading the gating state
// so a signing call counts only when it is textually inside the gated region.
func urlWrapperProbeBody(block *ast.BlockStatement, inGate bool, p1, p2, p3 string, probe *urlWrapperSignProbe) {
	if block == nil {
		return
	}
	for _, stmt := range block.List {
		urlWrapperProbeStmt(stmt, inGate, p1, p2, p3, probe)
	}
}

func urlWrapperProbeStmt(stmt ast.Statement, inGate bool, p1, p2, p3 string, probe *urlWrapperSignProbe) {
	if stmt == nil || !probe.supported {
		return
	}
	switch s := stmt.(type) {
	case *ast.ExpressionStatement:
		urlWrapperProbeExpr(s.Expression, inGate, p1, p2, p3, probe)
	case *ast.BlockStatement:
		urlWrapperProbeBody(s, inGate, p1, p2, p3, probe)
	case *ast.IfStatement:
		gated := inGate || urlWrapperIsName(s.Test, p3)
		urlWrapperProbeExpr(s.Test, inGate, p1, p2, p3, probe)
		urlWrapperProbeStmt(s.Consequent, gated, p1, p2, p3, probe)
		urlWrapperProbeStmt(s.Alternate, inGate, p1, p2, p3, probe)
	case *ast.ReturnStatement:
		probe.returns++
		if s.Argument != nil {
			urlWrapperProbeExpr(s.Argument, inGate, p1, p2, p3, probe)
		}
	case *ast.ThrowStatement:
		if s.Argument != nil {
			urlWrapperProbeExpr(s.Argument, inGate, p1, p2, p3, probe)
		}
	case *ast.VariableStatement:
		urlWrapperProbeBindings(s.List, inGate, p1, p2, p3, probe)
	case *ast.LexicalDeclaration:
		urlWrapperProbeBindings(s.List, inGate, p1, p2, p3, probe)
	case *ast.EmptyStatement:
		// nothing to probe
	default:
		// Loops, try/switch, labelled/with, and nested function declarations
		// can carry or hide signing calls: unsupported, fail closed.
		probe.supported = false
	}
}

func urlWrapperProbeBindings(list []*ast.Binding, inGate bool, p1, p2, p3 string, probe *urlWrapperSignProbe) {
	for _, decl := range list {
		if decl == nil || decl.Initializer == nil {
			continue
		}
		urlWrapperProbeExpr(decl.Initializer, inGate, p1, p2, p3, probe)
	}
}

func urlWrapperProbeExpr(expr ast.Expression, inGate bool, p1, p2, p3 string, probe *urlWrapperSignProbe) {
	if expr == nil || !probe.supported {
		return
	}
	switch e := expr.(type) {
	case *ast.Identifier, *ast.StringLiteral, *ast.NumberLiteral, *ast.BooleanLiteral, *ast.NullLiteral, *ast.RegExpLiteral:
		// leaf nodes: nothing to probe, nothing to hide
	case *ast.CallExpression:
		if urlWrapperIsSignCall(e, p1, p2, p3) {
			if inGate {
				probe.gated++
			} else {
				probe.ungated++
			}
		}
		urlWrapperProbeExpr(e.Callee, inGate, p1, p2, p3, probe)
		for _, arg := range e.ArgumentList {
			urlWrapperProbeExpr(arg, inGate, p1, p2, p3, probe)
		}
	case *ast.NewExpression:
		urlWrapperProbeExpr(e.Callee, inGate, p1, p2, p3, probe)
		for _, arg := range e.ArgumentList {
			urlWrapperProbeExpr(arg, inGate, p1, p2, p3, probe)
		}
	case *ast.DotExpression:
		urlWrapperProbeExpr(e.Left, inGate, p1, p2, p3, probe)
	case *ast.BracketExpression:
		urlWrapperProbeExpr(e.Left, inGate, p1, p2, p3, probe)
		urlWrapperProbeExpr(e.Member, inGate, p1, p2, p3, probe)
	case *ast.BinaryExpression:
		if e.Operator == token.LOGICAL_AND {
			urlWrapperProbeExpr(e.Left, inGate, p1, p2, p3, probe)
			urlWrapperProbeExpr(e.Right, inGate || urlWrapperIsName(e.Left, p3), p1, p2, p3, probe)
		} else {
			urlWrapperProbeExpr(e.Left, inGate, p1, p2, p3, probe)
			urlWrapperProbeExpr(e.Right, inGate, p1, p2, p3, probe)
		}
	case *ast.AssignExpression:
		urlWrapperProbeExpr(e.Left, inGate, p1, p2, p3, probe)
		urlWrapperProbeExpr(e.Right, inGate, p1, p2, p3, probe)
	case *ast.ConditionalExpression:
		urlWrapperProbeExpr(e.Test, inGate, p1, p2, p3, probe)
		urlWrapperProbeExpr(e.Consequent, inGate, p1, p2, p3, probe)
		urlWrapperProbeExpr(e.Alternate, inGate, p1, p2, p3, probe)
	case *ast.SequenceExpression:
		for _, el := range e.Sequence {
			urlWrapperProbeExpr(el, inGate, p1, p2, p3, probe)
		}
	case *ast.UnaryExpression:
		urlWrapperProbeExpr(e.Operand, inGate, p1, p2, p3, probe)
	default:
		// Array/object/template/function/class literals, optional chains and
		// other exotic containers could hide signing: unsupported, fail closed.
		probe.supported = false
	}
}

// urlWrapperIsSignCall matches the wrapper's signing-call shape: a call on
// the first parameter object (`p1[any](...)` or `p1.set(...)`) whose first
// argument is exactly the second parameter (the query key) and whose signed
// value (second argument) meaningfully depends on the third parameter — a
// bare constant (`if(q){p1.set(M,"fixed")}`) would sign the same value no
// matter what the input cipher was and is rejected. Dot members are
// restricted to `set`; bracket members accept any subscript (S[21], F[14]).
func urlWrapperIsSignCall(call *ast.CallExpression, p1, p2, p3 string) bool {
	if len(call.ArgumentList) < 2 || !urlWrapperIsName(call.ArgumentList[0], p2) {
		return false
	}
	if !urlWrapperSignValueDependsOnThird(call.ArgumentList[1], p3) {
		return false
	}
	switch callee := call.Callee.(type) {
	case *ast.DotExpression:
		return urlWrapperIsName(callee.Left, p1) && callee.Identifier.Name.String() == "set"
	case *ast.BracketExpression:
		return urlWrapperIsName(callee.Left, p1)
	}
	return false
}

// urlWrapperSignValueDependsOnThird reports whether the signing value follows
// one of the retained known shapes: the bare third parameter, a helper call
// carrying it directly (`Rm(10,2324,q)`), or a helper call whose direct
// arguments are themselves calls carrying it (`m5(5,5551,Qe(31,6145,q))`).
// Other literals, free identifiers, and opaque expressions are rejected: this
// bounded structural gate cannot prove arbitrary semantics, so unsupported
// shapes fail closed rather than being guessed at.
func urlWrapperSignValueDependsOnThird(value ast.Expression, p3 string) bool {
	if urlWrapperIsName(value, p3) {
		return true
	}
	call, ok := value.(*ast.CallExpression)
	if !ok {
		return false
	}
	for _, arg := range call.ArgumentList {
		if urlWrapperIsName(arg, p3) {
			return true
		}
	}
	for _, arg := range call.ArgumentList {
		if inner, ok := arg.(*ast.CallExpression); ok {
			for _, iarg := range inner.ArgumentList {
				if urlWrapperIsName(iarg, p3) {
					return true
				}
			}
		}
	}
	return false
}

// urlWrapperIsName reports whether the expression is exactly the identifier
// with the given name. Gate conditions only count when they are this bare
// identifier (parentheses are already stripped by the parser): a condition
// that merely mentions the third parameter — `!q`, `q||true`, `Qe(q)` — is
// not the supported truthiness gate and never gates.
func urlWrapperIsName(expr ast.Expression, name string) bool {
	if id, ok := expr.(*ast.Identifier); ok {
		return id.Name.String() == name
	}
	return false
}

// exposeURLWrapperSource cheaply prepares the full player bundle for the
// URL-wrapper runner: trailing whitespace is trimmed, an exact
// `})(_yt_player);` suffix is required, and the exposure assignment is
// inserted before the suffix by concatenation. The recognized protocol skips
// BuildWrapperRuntimeJS, AST extraction, and QuickJS compilation entirely.
func exposeURLWrapperSource(playerJS []byte, wrapperName string) (string, error) {
	if len(playerJS) == 0 || wrapperName == "" || !isValidIdentifier(wrapperName) {
		return "", fmt.Errorf("invalid player source or wrapper name for url wrapper exposure")
	}
	code := string(playerJS)
	for len(code) > 0 {
		last := code[len(code)-1]
		if last == ' ' || last == '\t' || last == '\n' || last == '\r' {
			code = code[:len(code)-1]
			continue
		}
		break
	}

	const suffix = `})(_yt_player);`
	if len(code) < len(suffix) || code[len(code)-len(suffix):] != suffix {
		return "", fmt.Errorf("player bundle missing exact %q suffix", suffix)
	}

	exposed := fmt.Sprintf(`_exposed['%s']=%s;`, wrapperName, wrapperName)
	return code[:len(code)-len(suffix)] + exposed + suffix, nil
}

// isModernURLWrapperProtocol reports whether this cipher carries a validated
// modern URL-wrapper protocol binding.
func (c *Cipher) isModernURLWrapperProtocol() bool {
	return c.urlWrapperProtocol && c.urlWrapperName != "" && isValidIdentifier(c.urlWrapperName)
}

// WarmURLTransformContext eagerly creates the (cached) bun/node URL-wrapper
// runner so the first TransformURLContext call does not pay process spawn.
// Errors are returned; callers decide whether to warn or ignore (prewarm).
func (c *Cipher) WarmURLTransformContext(ctx context.Context) error {
	if !c.isModernURLWrapperProtocol() {
		return nil
	}
	if len(c.playerJS) == 0 {
		return fmt.Errorf("no player.js available for url wrapper protocol")
	}
	_, err := c.runtimeOrDefault().GetURLWrapperRunner(ctx, c.playerJS, c.playerURL, c.playerFingerprint, c.urlWrapperName)
	return err
}

// TransformURLContext applies the modern URL-wrapper protocol to a stream
// URL in a single subprocess operation (Bun/Node only; never QuickJS).
func (c *Cipher) TransformURLContext(ctx context.Context, streamURL, sigParam, sig string) (jsengine.URLTransformResult, error) {
	var empty jsengine.URLTransformResult
	if !c.isModernURLWrapperProtocol() {
		return empty, fmt.Errorf("url wrapper protocol not recognized for this player")
	}
	if len(c.playerJS) == 0 {
		return empty, fmt.Errorf("no player.js available for url wrapper protocol")
	}
	runner, err := c.runtimeOrDefault().GetURLWrapperRunner(ctx, c.playerJS, c.playerURL, c.playerFingerprint, c.urlWrapperName)
	if err != nil {
		return empty, err
	}
	res, err := runner.TransformURLContext(ctx, streamURL, sigParam, sig)
	if err != nil && runner.IsClosed() {
		// Terminal failure: the subprocess is gone. Evict the exact cached
		// instance so the next call rebuilds a fresh runner. A canceled
		// queued call never closes a healthy runner, so it is left cached.
		c.runtimeOrDefault().EvictURLWrapperRunner(
			urlWrapperRunnerCacheKey(c.playerURL, c.playerFingerprint, c.urlWrapperName),
			runner,
		)
	}
	if err != nil {
		return empty, err
	}
	return res, nil
}

// transformNModernContext transforms n via the URL-wrapper protocol using a
// synthetic URL that carries the n parameter. Strict: failures return errors
// — there is no silent n passthrough on the modern path.
func (c *Cipher) transformNModernContext(ctx context.Context, n string) (string, error) {
	if n == "" {
		return "", fmt.Errorf("cannot transform empty n value")
	}
	parsedURL, err := url.Parse(urlWrapperSyntheticURL)
	if err != nil {
		return "", err
	}
	query := parsedURL.Query()
	query.Set("n", n)
	parsedURL.RawQuery = query.Encode()

	res, err := c.TransformURLContext(ctx, parsedURL.String(), "s", "")
	if err != nil {
		return "", err
	}
	if res.N == "" {
		return "", fmt.Errorf("url wrapper returned empty n for input n parameter")
	}
	// Strict: the transformed n must actually differ from the input and be
	// free of throttle/error markers. No silently successful passthrough.
	validated, err := jsengine.ValidateNTransformResult(n, res.N)
	if err != nil {
		return "", err
	}
	return validated, nil
}
