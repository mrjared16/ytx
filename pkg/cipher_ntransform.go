package ytx

import (
	"context"
	"fmt"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"

	"github.com/mrjared16/ytx/internal/jsextract"
)

var nRuntimeExposePattern = regexp.MustCompile(`\}\)\(_yt_player\);\s*$`)

func (c *Cipher) WarmNTransformEngine() error {
	return c.WarmNTransformEngineContext(context.Background())
}

func (c *Cipher) ensureNRuntime() ([]byte, error) {
	c.lazyMu.Lock()
	defer c.lazyMu.Unlock()
	if len(c.nRuntimeJS) == 0 {
		c.nRuntimeJS = buildNTransformRuntime(c.playerJS, c.nFunctionName)
	}
	if len(c.nRuntimeJS) == 0 {
		return nil, fmt.Errorf("no player.js available for n-transform")
	}
	return c.nRuntimeJS, nil
}

func (c *Cipher) WarmNTransformEngineContext(ctx context.Context) error {
	if c.nFunctionName == "" {
		return nil
	}
	nRuntimeJS, err := c.ensureNRuntime()
	if err != nil {
		return err
	}
	_, err = c.runtimeOrDefault().GetCachedEngine(ctx, c.engineCacheKey(), nRuntimeJS, c.nFunctionName)
	return err
}

// TransformN transforms the n-parameter using the configured JS engine.
func (c *Cipher) TransformN(n string) (string, error) {
	return c.TransformNContext(context.Background(), n)
}

func (c *Cipher) TransformNContext(ctx context.Context, n string) (string, error) {
	if c.nFunctionName == "" {
		if c.sigUsesURLWrapper {
			return c.tryWrapperCandidates(n, "n")
		}
		return n, nil
	}

	nRuntimeJS, err := c.ensureNRuntime()
	if err != nil {
		return n, err
	}

	engine, err := c.runtimeOrDefault().GetCachedEngine(ctx, c.engineCacheKey(), nRuntimeJS, c.nFunctionName)
	if err != nil {
		if c.sigUsesURLWrapper || findURLTransformFunctionName(c.playerJS) != "" {
			transformed, wrapperErr := c.tryWrapperCandidates(n, "n")
			if wrapperErr == nil {
				return transformed, nil
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
	nRuntimeJS, err := c.ensureNRuntime()
	if err != nil {
		return nValues, err
	}
	engine, err := c.runtimeOrDefault().GetCachedEngine(ctx, c.engineCacheKey(), nRuntimeJS, c.nFunctionName)
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
			if built, err := jsextract.BuildURLWrapperCode(playerJS, name); err == nil && built != "" {
				add(built)
			}
			if extracted, err := jsextract.ExtractWithAST(playerJS, name); err == nil {
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

// TransformNBatch transforms multiple n-parameters in a single IPC call.
func (c *Cipher) TransformNBatch(nValues []string) ([]string, error) {
	return c.TransformNBatchContext(context.Background(), nValues)
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
