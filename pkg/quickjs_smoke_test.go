package ytx

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/buke/quickjs-go"
)

func TestQuickJS(t *testing.T) {
	rt := quickjs.NewRuntime()
	defer rt.Close()
	ctx := rt.NewContext()
	defer ctx.Close()

	t.Run("Arithmetic", func(t *testing.T) {
		val := ctx.Eval("1 + 1")
		defer val.Free()
		if ctx.HasException() {
			t.Fatalf("Eval failed: %v", ctx.Exception())
		}
		if val.Int32() != 2 {
			t.Errorf("Expected 2, got %v", val.Int32())
		}
	})

	t.Run("OptionalChaining", func(t *testing.T) {
		code := `const x = null; const y = x?.foo ?? 'fallback'; y;`
		val := ctx.Eval(code)
		defer val.Free()
		if ctx.HasException() {
			t.Fatalf("Eval failed: %v", ctx.Exception())
		}
		if val.String() != "fallback" {
			t.Errorf("Expected 'fallback', got %v", val.String())
		}
	})

	t.Run("Performance100LineFunction", func(t *testing.T) {
		lines := make([]string, 100)
		for i := 0; i < 99; i++ {
			lines[i] = fmt.Sprintf("let a%d = %d;", i, i)
		}
		lines[99] = "a0 + a1;"
		code := fmt.Sprintf("(function() {\n%s\n})()", strings.Join(lines, "\n"))

		start := time.Now()
		val := ctx.Eval(code)
		duration := time.Since(start)
		defer val.Free()

		if ctx.HasException() {
			t.Fatalf("Eval failed: %v", ctx.Exception())
		}

		t.Logf("100-line function eval took: %v", duration)
		if duration > 100*time.Millisecond {
			t.Errorf("Eval took too long: %v", duration)
		}
	})

	t.Run("NativeObjects", func(t *testing.T) {
		objects := []string{"URL", "URLSearchParams", "TextEncoder", "TextDecoder"}
		for _, obj := range objects {
			val := ctx.Eval(fmt.Sprintf("typeof %s", obj))
			if ctx.HasException() {
				t.Logf("%s check: NOT FOUND (exception)", obj)
				_ = ctx.Exception() // clear exception
				val.Free()
				continue
			}
			t.Logf("%s type: %s", obj, val.String())
			val.Free()
		}
	})
}
