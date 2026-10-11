package jsextract

import (
	"strings"
	"testing"

	"github.com/buke/quickjs-go"
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/unistring"
)

// extractLabeledAndTryJS is a synthetic player shard whose helper calls live
// inside a labeled block and a try/catch/finally, plus an external guard
// constant used from the labeled block and the finally branch.
const extractLabeledAndTryJS = `
var GUARD=7;
var helperLab=function(x){return x+2;};
var helperTry=function(x){return x+3;};
var helperCatch=function(x){return x+5;};
var helperFinally=function(x){return x+11;};
var target=function(a){
  var r=a;
  lbl: { r=helperLab(r); if(GUARD){r=r*2;} }
  try { r=helperTry(r); } catch(e) { r=helperCatch(r); } finally { r=helperFinally(r); r=r+GUARD; }
  return r;
}
`

// evalJS runs a self-contained JS expression through the in-process QuickJS
// engine and returns the resulting int32. Throws a fatal test error on any
// evaluation exception.
func evalJS(t *testing.T, code string) int32 {
	t.Helper()
	rt := quickjs.NewRuntime(quickjs.WithMemoryLimit(64 * 1024 * 1024))
	defer rt.Close()
	ctx := rt.NewContext()
	defer ctx.Close()

	val := ctx.Eval(code, quickjs.EvalFlagGlobal(true))
	defer val.Free()
	if ctx.HasException() {
		t.Fatalf("JS evaluation failed: %v\ncode:\n%s", ctx.Exception(), code)
	}
	return val.Int32()
}

func TestExtractWithASTLabeledAndTryDeps(t *testing.T) {
	slice, err := ExtractWithAST(extractLabeledAndTryJS, "target")
	if err != nil {
		t.Fatalf("ExtractWithAST failed: %v", err)
	}

	// Dependencies hidden behind LabelledStatement and TryStatement branches
	// (try body, catch, finally) must all be present in the emitted slice.
	for _, name := range []string{"GUARD", "helperLab=", "helperTry=", "helperCatch=", "helperFinally=", "lbl:", "try {"} {
		if !strings.Contains(slice, name) {
			t.Fatalf("slice missing dependency %q; slice:\n%s", name, slice)
		}
	}

	// Source order must be preserved: each definition appears after the
	// previous one in the emitted slice.
	order := []string{"GUARD=", "helperLab=", "helperTry=", "helperCatch=", "helperFinally=", "target="}
	prev := -1
	for _, name := range order {
		pos := strings.Index(slice, name)
		if pos < 0 {
			t.Fatalf("slice missing ordered definition %q", name)
		}
		if pos < prev {
			t.Fatalf("definition %q out of source order (pos %d < prev %d)", name, pos, prev)
		}
		prev = pos
	}

	// The slice must evaluate correctly on its own, with every branch
	// contributing: target(0): +2 -> *2 (guard in labeled block) -> +3 (try)
	// -> +11 (finally) -> +7 = 25. The second check proves the catch-referenced
	// helper is emitted and callable from outside the slice.
	for _, entry := range []struct {
		expr string
		want int32
	}{
		{expr: `result=target(0);`, want: 25},
		{expr: `result=target(0); try { throw 1; } catch(e) { result=target(0) + helperCatch(0); }`, want: 30},
	} {
		got := evalJS(t, slice+"\n"+entry.expr)
		if got != entry.want {
			t.Fatalf("expected %q to evaluate to %d in extracted slice, got %d\nslice:\n%s", entry.expr, entry.want, got, slice)
		}
	}
}

// extractTryFinallyNoCatchJS: a try/finally statement with no catch clause,
// so the AST TryStatement has a nil Catch pointer.
const extractTryFinallyNoCatchJS = `
var helperTryFinally=function(x){return x+3;};
var helperFinally=function(x){return x+11;};
var target=function(a){
  var r=a;
  try { r=helperTryFinally(r); } finally { r=helperFinally(r); }
  return r;
}
`

// extractTryCatchNoFinallyJS: a try/catch statement with no finally clause,
// so the AST TryStatement has a nil Finally pointer.
const extractTryCatchNoFinallyJS = `
var helperTryCatch=function(x){return x+3;};
var helperCatchOnly=function(x){return x+5;};
var target=function(a){
  var r=a;
  try { r=helperTryCatch(r); } catch(e) { r=helperCatchOnly(r); }
  return r;
}
`

// extractNestedLabelCatchJS: nested labels plus try/catch/finally chains,
// including a try nested inside a catch handler and a label inside catch.
const extractNestedLabelCatchJS = `
var GUARD2=3;
var helperInner=function(x){return x+1;};
var helperCatchNested=function(x){return x+7;};
var target=function(a){
  var r=a;
  outer: { inner: { r=helperInner(r); } try { r=r+GUARD2; } catch(e) { catchInner: { r=helperCatchNested(r); } } }
  try { r=r+2; } catch(e) { try { throw 1; } catch(e2){ r=helperCatchNested(r); } } finally { r=r+1; }
  return r;
}
`

func TestExtractWithASTTryFinallyNoCatch(t *testing.T) {
	slice, err := ExtractWithAST(extractTryFinallyNoCatchJS, "target")
	if err != nil {
		t.Fatalf("ExtractWithAST failed: %v", err)
	}

	for _, name := range []string{"helperTryFinally=", "helperFinally=", "try {"} {
		if !strings.Contains(slice, name) {
			t.Fatalf("slice missing dependency %q; slice:\n%s", name, slice)
		}
	}

	// Source order preserved and slice evaluates: 0 -> +3 (try) -> +11 (finally).
	got := evalJS(t, slice+"\nresult=target(0);")
	if got != 14 {
		t.Fatalf("expected target(0) to evaluate to 14, got %d\nslice:\n%s", got, slice)
	}
}

func TestExtractWithASTTryCatchNoFinally(t *testing.T) {
	slice, err := ExtractWithAST(extractTryCatchNoFinallyJS, "target")
	if err != nil {
		t.Fatalf("ExtractWithAST failed: %v", err)
	}

	for _, name := range []string{"helperTryCatch=", "helperCatchOnly=", "try {"} {
		if !strings.Contains(slice, name) {
			t.Fatalf("slice missing dependency %q; slice:\n%s", name, slice)
		}
	}

	// Slice evaluates with no exception: 0 -> +3 (try body).
	got := evalJS(t, slice+"\nresult=target(0);")
	if got != 3 {
		t.Fatalf("expected target(0) to evaluate to 3, got %d\nslice:\n%s", got, slice)
	}

	// Catch-referenced helper is callable from outside the slice.
	got = evalJS(t, slice+"\nresult=helperCatchOnly(0);")
	if got != 5 {
		t.Fatalf("expected helperCatchOnly(0) to evaluate to 5, got %d\nslice:\n%s", got, slice)
	}
}

func TestExtractWithASTNestedLabelCatch(t *testing.T) {
	slice, err := ExtractWithAST(extractNestedLabelCatchJS, "target")
	if err != nil {
		t.Fatalf("ExtractWithAST failed: %v", err)
	}

	for _, name := range []string{"GUARD2=", "helperInner=", "helperCatchNested=", "outer:", "catchInner:"} {
		if !strings.Contains(slice, name) {
			t.Fatalf("slice missing dependency %q; slice:\n%s", name, slice)
		}
	}

	// 0 -> +1 (inner label) -> +3 (GUARD2) -> +2 (second try) -> +1 (finally)
	// = 7. The catch handlers do not run, but the nested catch helper must
	// still be emitted and callable from outside the slice.
	got := evalJS(t, slice+"\nresult=target(0);")
	if got != 7 {
		t.Fatalf("expected target(0) to evaluate to 7, got %d\nslice:\n%s", got, slice)
	}
	got = evalJS(t, slice+"\nresult=helperCatchNested(0);")
	if got != 7 {
		t.Fatalf("expected helperCatchNested(0) to evaluate to 7, got %d\nslice:\n%s", got, slice)
	}
}

// idStmt wraps an identifier reference in an expression statement so it can
// sit inside a synthetic AST statement list.
func idStmt(name string) ast.Statement {
	return &ast.ExpressionStatement{Expression: &ast.Identifier{Name: unistring.NewFromString(name)}}
}

func TestCollectIdentifiersOptionalNilNodes(t *testing.T) {
	// Typed-nil pointers dispatched through the ast.Node interface must not
	// panic in the BlockStatement/CatchStatement cases.
	deps := map[string]bool{}
	collectIdentifiers((*ast.BlockStatement)(nil), deps)
	collectIdentifiers((*ast.CatchStatement)(nil), deps)

	// TryStatement with every optional field nil (invalid JS, but a valid
	// synthetic AST) must not panic.
	collectIdentifiers(&ast.TryStatement{}, deps)

	// Body present, Catch and Finally nil: body identifiers still collected.
	deps = map[string]bool{}
	collectIdentifiers(&ast.TryStatement{
		Body: &ast.BlockStatement{List: []ast.Statement{idStmt("fromBody")}},
	}, deps)
	if !deps["fromBody"] {
		t.Fatalf("expected dependency %q collected from try body, got %v", "fromBody", deps)
	}

	// Full nested chain: labelled -> try (catch+finally) -> labelled catch
	// body, plus identifiers in each branch.
	deps = map[string]bool{}
	collectIdentifiers(&ast.LabelledStatement{
		Label: &ast.Identifier{Name: unistring.NewFromString("lbl")},
		Statement: &ast.TryStatement{
			Body: &ast.BlockStatement{List: []ast.Statement{idStmt("fromBody")}},
			Catch: &ast.CatchStatement{
				Body: &ast.BlockStatement{List: []ast.Statement{
					&ast.LabelledStatement{
						Label:     &ast.Identifier{Name: unistring.NewFromString("catchLbl")},
						Statement: idStmt("fromCatch"),
					},
				}},
			},
			Finally: &ast.BlockStatement{List: []ast.Statement{idStmt("fromFinally")}},
		},
	}, deps)
	for _, name := range []string{"fromBody", "fromCatch", "fromFinally"} {
		if !deps[name] {
			t.Fatalf("expected dependency %q collected, got %v", name, deps)
		}
	}
}
