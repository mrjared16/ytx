package ytx

import (
	"errors"
	"fmt"
	"hash/fnv"
	gruntime "runtime"
	rdebug "runtime/debug"

	"github.com/buke/quickjs-go"
	"github.com/mrjared16/ytx/internal/jsengine"
	"github.com/mrjared16/ytx/internal/jsextract"
)

func (c *Cipher) canPrecomputeSignatureRuntime() bool {
	return c.sigFunctionName != "" && !c.sigUsesURLWrapper
}

func (c *Cipher) canPersistSignatureRuntime() bool {
	if c.sigFunctionName == "" {
		return false
	}
	if c.sigUsesURLWrapper {
		return len(c.playerJS) > 0
	}
	return c.jsCode != ""
}

const wrapperBytecodeBuildVersion = "wrapper-bytecode-v2"
const quickJSModulePath = "github.com/buke/quickjs-go/v0"

func quickJSBytecodeABIComponent() string {
	buildInfo, ok := rdebug.ReadBuildInfo()
	if !ok || buildInfo == nil {
		return quickJSModulePath + "@unknown"
	}

	for _, dep := range buildInfo.Deps {
		if dep.Path != quickJSModulePath {
			continue
		}

		version := dep.Version
		if dep.Replace != nil {
			if dep.Replace.Version != "" {
				version = dep.Replace.Version
			} else if dep.Replace.Path != "" {
				version = "replace:" + dep.Replace.Path
			}
		}
		if version == "" {
			version = "unknown"
		}

		sum := dep.Sum
		if sum == "" {
			sum = "nosum"
		}

		return dep.Path + "@" + version + "+" + sum
	}

	return quickJSModulePath + "@unknown"
}

func currentWrapperBytecodeBuildID() string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(wrapperBytecodeBuildVersion))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(quickJSBytecodeABIComponent()))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(gruntime.GOOS))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(gruntime.GOARCH))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(jsengine.BrowserStubsJS))
	return fmt.Sprintf("%s-%016x", wrapperBytecodeBuildVersion, h.Sum64())
}

func compileToQuickJSBytecode(jsCode string) ([]byte, error) {
	if jsCode == "" {
		return nil, errors.New("empty js code")
	}
	rt := quickjs.NewRuntime()
	defer rt.Close()
	ctx := rt.NewContext()
	defer ctx.Close()
	return ctx.Compile(jsCode, quickjs.EvalFlagGlobal(true))
}

func (c *Cipher) wrapperRuntimeBytecode() ([]byte, error) {
	if !c.sigUsesURLWrapper {
		return nil, errors.New("wrapper runtime bytecode requested for non-wrapper cipher")
	}
	if c.isBytecode && c.jsCode != "" {
		return []byte(c.jsCode), nil
	}
	runtimeJS := c.jsCode
	if runtimeJS == "" {
		if len(c.playerJS) == 0 {
			return nil, errors.New("no player.js available for wrapper runtime")
		}
		built, err := jsextract.BuildURLWrapperCode(string(c.playerJS), c.sigFunctionName)
		if err != nil {
			return nil, err
		}
		runtimeJS = built
	}
	return compileToQuickJSBytecode(runtimeJS)
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

	jsCode, err := jsextract.ExtractWithAST(string(c.playerJS), c.sigFunctionName)
	if err != nil {
		jsCode, err = jsextract.ExtractSimple(c.playerJS, c.sigFunctionName)
		if err != nil {
			return fmt.Errorf("failed to extract sig function: %w", err)
		}
	}
	c.jsCode = jsCode

	if !c.isBytecode && len(c.jsCode) > 0 {
		if bytecode, err := compileToQuickJSBytecode(c.jsCode); err == nil {
			c.jsCode = string(bytecode)
			c.isBytecode = true
		}
	}

	return nil
}

// DecryptSignature decrypts the signature.
func (c *Cipher) DecryptSignature(sig string) (string, error) {

	if c.sigUsesURLWrapper {
		return c.tryWrapperCandidates(sig, "s")
	}

	if err := c.ensureSignatureReady(); err != nil {
		return "", err
	}

	c.lazyMu.Lock()
	defer c.lazyMu.Unlock()

	ctx, err := c.ensureWrapperContext(c.jsCode)
	if err != nil {
		return "", err
	}

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

func saneDecryptedSignature(input, output string) bool {
	if len(input) < 80 {
		return true
	}
	if len(output) < 90 {
		return false
	}
	if output == input {
		return false
	}
	return true
}

// ensureWrapperContext bootstraps a QuickJS runtime with the given jsCode,
// caching it for reuse across multiple wrapper calls (sig + n).
func (c *Cipher) ensureWrapperContext(jsCode string) (*quickjs.Context, error) {
	if c.wrapperReady && c.wrapperJSCode == jsCode && c.wrapperCtx != nil {
		return c.wrapperCtx, nil
	}

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
	)
	ctx := rt.NewContext()

	stubsVal := ctx.Eval(jsengine.BrowserStubsJS, quickjs.EvalFlagGlobal(true))
	if ctx.HasException() {
		exc := ctx.Exception()
		stubsVal.Free()
		ctx.Close()
		rt.Close()
		return nil, fmt.Errorf("failed to inject browser stubs: %v", exc)
	}
	stubsVal.Free()

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

func (c *Cipher) saneDecrypted(input, output, field string) bool {
	if field == "n" {
		return output != "" && output != input
	}
	return saneDecryptedSignature(input, output)
}

func (c *Cipher) tryWrapperCandidates(value, field string) (string, error) {
	var lastErr error

	if c.sigFunctionName != "" && c.jsCode != "" {
		transformed, err := c.transformWithURLWrapper(c.jsCode, c.sigFunctionName, value, field)
		if err == nil {
			if !c.saneDecrypted(value, transformed, field) {
				lastErr = fmt.Errorf("invalid decrypted %s shape from cached wrapper runtime: %q", field, transformed)
			} else {
				return transformed, nil
			}
		} else {
			lastErr = err
		}
	}

	codes := c.wrapperCodeCandidates()
	wrapperNames := c.wrapperFunctionCandidates()

	for _, wrapperName := range wrapperNames {
		for _, wrapperCode := range codes {
			transformed, err := c.transformWithURLWrapper(wrapperCode, wrapperName, value, field)
			if err == nil {
				if !c.saneDecrypted(value, transformed, field) {
					err = fmt.Errorf("invalid decrypted %s shape from wrapper %s", field, wrapperName)
					lastErr = err
					continue
				}

				if field == "s" {
					preferredCode := wrapperCode
					if len(c.playerJS) > 0 {
						fullPlayerJS := string(c.playerJS)
						if wrapperCode == fullPlayerJS {
							if built, err := jsextract.BuildURLWrapperCode(fullPlayerJS, wrapperName); err == nil && built != "" {
								preferredCode = built
							} else if extracted, extractErr := jsextract.ExtractWithAST(fullPlayerJS, wrapperName); extractErr == nil && extracted != "" {
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
				}
				return transformed, nil
			}
			lastErr = err
		}
	}

	if lastErr != nil {
		if field == "s" {
			return "", lastErr
		}
		return value, lastErr
	}
	err := fmt.Errorf("url wrapper function unavailable in current player JS for %s", field)
	if field == "s" {
		return "", err
	}
	return value, err
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
