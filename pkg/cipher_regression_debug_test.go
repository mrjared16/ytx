package ytx

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrjared16/ytx/internal/cache"
	"github.com/mrjared16/ytx/internal/jsextract"
)

func TestDebugCipherRegressionPath(t *testing.T) {
	if os.Getenv("RUN_DEBUG_REGRESSION") != "1" {
		t.Skip("set RUN_DEBUG_REGRESSION=1 to run")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	cookiePath := filepath.Join(home, ".config", "ytx", "cookies.txt")
	if _, err := os.Stat(cookiePath); err != nil {
		t.Fatalf("cookie file unavailable: %v", err)
	}

	cacheDir := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}

	ext, err := NewExtractor(ModeMusic, cookiePath, WithCacheManager(cache.NewCacheManagerWithDir(cacheDir)))
	if err != nil {
		t.Fatalf("new extractor: %v", err)
	}

	ctx := context.Background()
	videoID := "hbl2Cuw75oE"

	embedCipher, embedPath, embedErr := newCipherWithCachedPathContext(ctx, ext.runtime, videoID, ext.httpClient, "", ext.config.Origin, true)
	if embedErr != nil {
		t.Fatalf("newCipherWithCachedPathContext(embedFirst=true): %v", embedErr)
	}
	embedDirectName, _, embedDirectErr := findSigFunctionName(embedCipher.playerJS)
	embedWrapperName := findURLTransformFunctionName(embedCipher.playerJS)
	t.Logf("embedFirst path=%q sigFunctionName=%q sigUsesURLWrapper=%v directName=%q directErr=%v wrapperName=%q", embedPath, embedCipher.sigFunctionName, embedCipher.sigUsesURLWrapper, embedDirectName, embedDirectErr, embedWrapperName)
	if embedCipher.sigUsesURLWrapper {
		if err := embedCipher.ensureSignatureReady(); err != nil {
			t.Fatalf("embedCipher.ensureSignatureReady: %v", err)
		}
		for _, n := range embedCipher.wrapperFunctionCandidates() {
			for idx, code := range embedCipher.wrapperCodeCandidates() {
				out, err := embedCipher.transformWithURLWrapper(code, n, "abcdef", "s")
				t.Logf("embed wrapper try name=%q code[%d]Len=%d out=%q err=%v", n, idx, len(code), out, err)
			}
		}
	}

	watchCipher, watchPath, watchErr := newCipherWithCachedPathContext(ctx, ext.runtime, videoID, ext.httpClient, "", ext.config.Origin, false)
	if watchErr != nil {
		t.Fatalf("newCipherWithCachedPathContext(embedFirst=false): %v", watchErr)
	}
	watchDirectName, _, watchDirectErr := findSigFunctionName(watchCipher.playerJS)
	watchWrapperName := findURLTransformFunctionName(watchCipher.playerJS)
	t.Logf("watchFirst path=%q sigFunctionName=%q sigUsesURLWrapper=%v directName=%q directErr=%v wrapperName=%q", watchPath, watchCipher.sigFunctionName, watchCipher.sigUsesURLWrapper, watchDirectName, watchDirectErr, watchWrapperName)
	if watchCipher.sigUsesURLWrapper {
		if err := watchCipher.ensureSignatureReady(); err != nil {
			t.Fatalf("watchCipher.ensureSignatureReady: %v", err)
		}
		for _, n := range watchCipher.wrapperFunctionCandidates() {
			for idx, code := range watchCipher.wrapperCodeCandidates() {
				out, err := watchCipher.transformWithURLWrapper(code, n, "abcdef", "s")
				t.Logf("watch wrapper try name=%q code[%d]Len=%d out=%q err=%v", n, idx, len(code), out, err)
			}
		}
	}

	cipher, err := ext.fetchAndCacheCipherContext(ctx, videoID)
	if err != nil {
		t.Fatalf("fetchAndCacheCipherContext: %v", err)
	}

	directName, directParam, directErr := findSigFunctionName(cipher.playerJS)
	wrapperName := findURLTransformFunctionName(cipher.playerJS)
	playerLen := len(cipher.playerJS)

	t.Logf("fresh cipher: sigFunctionName=%q sigParam=%d sigUsesURLWrapper=%v playerURL=%q playerJSLen=%d", cipher.sigFunctionName, cipher.sigParam, cipher.sigUsesURLWrapper, cipher.playerURL, playerLen)
	t.Logf("detectors: directName=%q directParam=%d directErr=%v wrapperName=%q", directName, directParam, directErr, wrapperName)

	if err := cipher.ensureSignatureReady(); err != nil {
		t.Fatalf("ensureSignatureReady: %v", err)
	}
	t.Logf("signature ready: jsCodeLen=%d isBytecode=%v", len(cipher.jsCode), cipher.isBytecode)

	cache := cipher.ToCache()
	t.Logf("cache persisted sigUsesURLWrapper=%v sigFunction=%q jsCodeLen=%d", cache.SigUsesURLWrapper, cache.SigFunction, len(cache.JSCode))
	if err := ext.cacheManager.Save(cache); err != nil {
		t.Fatalf("save cache: %v", err)
	}
	loaded, err := ext.cacheManager.Load()
	if err != nil {
		t.Fatalf("load cache: %v", err)
	}
	reloaded := NewCipherFromCache(loaded)
	t.Logf("reloaded cache: sigFunctionName=%q sigUsesURLWrapper=%v jsCodeLen=%d isBytecode=%v", reloaded.sigFunctionName, reloaded.sigUsesURLWrapper, len(reloaded.jsCode), reloaded.isBytecode)

	if wrapperName != "" {
		wrapperJS := jsextract.BuildWrapperRuntimeJS(string(cipher.playerJS), wrapperName)
		t.Logf("wrapper runtime candidate: wrapperName=%q wrapperJSLen=%d", wrapperName, len(wrapperJS))
		if wrapperJS == "" {
			t.Fatalf("wrapper runtime candidate unexpectedly empty")
		}
	}

	if cipher.sigUsesURLWrapper {
		names := cipher.wrapperFunctionCandidates()
		codes := cipher.wrapperCodeCandidates()
		for _, n := range names {
			for idx, code := range codes {
				out, err := cipher.transformWithURLWrapper(code, n, "abcdef", "s")
				t.Logf("wrapper try name=%q code[%d]Len=%d out=%q err=%v", n, idx, len(code), out, err)
			}
		}
	}
}
