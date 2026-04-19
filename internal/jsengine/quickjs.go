package jsengine

import (
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/buke/quickjs-go"
)

// Compile-time interface check
var _ JSEngine = (*QuickJSRunner)(nil)

// QuickJSRunner executes n-parameter transforms via embedded QuickJS, implementing JSEngine.
type QuickJSRunner struct {
	mu         sync.Mutex
	closed     bool
	runtime    *quickjs.Runtime
	ctx        *quickjs.Context
	nTransform *quickjs.Value
	playerJS   []byte
	nFuncName  string
}

// NewQuickJSRunner creates a QuickJS-based JSEngine for n-parameter transformation.
func NewQuickJSRunner(playerJS []byte, nFuncName string) (*QuickJSRunner, error) {
	start := time.Now()

	rt := quickjs.NewRuntime(
		quickjs.WithMemoryLimit(256*1024*1024),
		quickjs.WithMaxStackSize(1024*1024),
	)

	ctx := rt.NewContext()

	runner := &QuickJSRunner{
		runtime:   rt,
		ctx:       ctx,
		playerJS:  playerJS,
		nFuncName: nFuncName,
	}

	if err := runner.injectAtobBtoa(); err != nil {
		runner.Close()
		return nil, fmt.Errorf("quickjs: failed to inject atob/btoa: %w", err)
	}

	stubsVal := ctx.Eval(BrowserStubsJS, quickjs.EvalFlagGlobal(true))
	if ctx.HasException() {
		exc := ctx.Exception()
		stubsVal.Free()
		runner.Close()
		return nil, fmt.Errorf("quickjs: failed to inject browser stubs: %v", exc)
	}
	stubsVal.Free()

	if err := runner.loadPlayerJS(playerJS, nFuncName); err != nil {
		runner.Close()
		return nil, fmt.Errorf("quickjs: failed to load player.js: %w", err)
	}

	if err := runner.cacheNTransformFunc(nFuncName); err != nil {
		runner.Close()
		return nil, fmt.Errorf("quickjs: failed to cache n-transform function: %w", err)
	}

	_ = time.Since(start)

	return runner, nil
}

func (r *QuickJSRunner) injectAtobBtoa() error {
	globals := r.ctx.Globals()

	atobFn := r.ctx.Function(func(ctx *quickjs.Context, this *quickjs.Value, args []*quickjs.Value) *quickjs.Value {
		if len(args) < 1 {
			return ctx.ThrowTypeError("atob requires 1 argument")
		}
		encoded := args[0].String()
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			// base64 input from JS often lacks padding; normalize before retry
			for len(encoded)%4 != 0 {
				encoded += "="
			}
			decoded, err = base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return ctx.ThrowTypeError("atob: invalid base64 string: %s", err.Error())
			}
		}
		return ctx.String(string(decoded))
	})
	globals.Set("atob", atobFn)

	btoaFn := r.ctx.Function(func(ctx *quickjs.Context, this *quickjs.Value, args []*quickjs.Value) *quickjs.Value {
		if len(args) < 1 {
			return ctx.ThrowTypeError("btoa requires 1 argument")
		}
		input := args[0].String()
		encoded := base64.StdEncoding.EncodeToString([]byte(input))
		return ctx.String(encoded)
	})
	globals.Set("btoa", btoaFn)

	return nil
}

func (r *QuickJSRunner) loadPlayerJS(playerJS []byte, funcName string) error {
	_ = funcName
	val := r.ctx.Eval(string(playerJS), quickjs.EvalFlagGlobal(true))
	if r.ctx.HasException() {
		exc := r.ctx.Exception()
		val.Free()
		return fmt.Errorf("player.js eval error: %v", exc)
	}
	val.Free()

	return nil
}

func (r *QuickJSRunner) cacheNTransformFunc(funcName string) error {
	globals := r.ctx.Globals()

	exposedObj := globals.Get("_exposed")
	defer exposedObj.Free()

	if exposedObj.IsUndefined() || exposedObj.IsNull() {
		return fmt.Errorf("_exposed object not found")
	}

	fn := exposedObj.Get(funcName)
	if fn.IsUndefined() || fn.IsNull() {
		fn.Free()
		return fmt.Errorf("function %s not found in _exposed", funcName)
	}

	if !fn.IsFunction() {
		fn.Free()
		return fmt.Errorf("_exposed['%s'] is not a function", funcName)
	}

	r.nTransform = fn
	return nil
}

// TransformN transforms a single n-parameter value using the cached QuickJS function.
func (r *QuickJSRunner) TransformN(n string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return n, fmt.Errorf("quickjs runner is closed")
	}

	if r.nTransform == nil {
		return n, fmt.Errorf("no n-transform function loaded")
	}

	arg := r.ctx.String(n)
	callThis := r.ctx.Globals()
	result := r.nTransform.Execute(callThis, arg)
	arg.Free()

	if r.ctx.HasException() {
		exc := r.ctx.Exception()
		result.Free()
		return n, fmt.Errorf("n-transform JS error: %v", exc)
	}

	if result.IsException() || result.IsError() {
		errMsg := result.String()
		result.Free()
		return n, fmt.Errorf("n-transform returned error: %s", errMsg)
	}

	transformed := result.String()
	result.Free()

	return ValidateNTransformResult(n, transformed)
}

// TransformNBatch transforms multiple n-parameter values at once.
func (r *QuickJSRunner) TransformNBatch(nValues []string) ([]string, error) {
	if len(nValues) == 0 {
		return nValues, nil
	}

	if len(nValues) == 1 {
		result, err := r.TransformN(nValues[0])
		return []string{result}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nValues, fmt.Errorf("quickjs runner is closed")
	}

	if r.nTransform == nil {
		return nValues, fmt.Errorf("no n-transform function loaded")
	}

	results := make([]string, len(nValues))
	var firstErr error
	callThis := r.ctx.Globals()

	for i, n := range nValues {
		arg := r.ctx.String(n)
		result := r.nTransform.Execute(callThis, arg)
		arg.Free()

		if r.ctx.HasException() {
			exc := r.ctx.Exception()
			result.Free()
			results[i] = n
			if firstErr == nil {
				firstErr = fmt.Errorf("n-transform JS error on index %d: %v", i, exc)
			}
			continue
		}

		if result.IsException() || result.IsError() {
			result.Free()
			results[i] = n
			if firstErr == nil {
				firstErr = fmt.Errorf("n-transform returned error on index %d", i)
			}
			continue
		}

		transformed := result.String()
		result.Free()

		validated, err := ValidateNTransformResult(n, transformed)
		if err != nil {
			results[i] = n
			if firstErr == nil {
				firstErr = err
			}
		} else {
			results[i] = validated
		}
	}

	return results, firstErr
}

// Close releases all QuickJS resources.
func (r *QuickJSRunner) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}
	r.closed = true

	if r.nTransform != nil {
		r.nTransform.Free()
		r.nTransform = nil
	}

	if r.ctx != nil {
		r.ctx.Close()
		r.ctx = nil
	}

	if r.runtime != nil {
		r.runtime.Close()
		r.runtime = nil
	}
}

// Name returns the engine name for logging.
func (r *QuickJSRunner) Name() string {
	return "quickjs"
}
