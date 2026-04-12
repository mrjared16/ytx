package ytx

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPOCWrapperBootstrapCost(t *testing.T) {
	if os.Getenv("RUN_ROOTCAUSE_POC") != "1" {
		t.Skip("set RUN_ROOTCAUSE_POC=1 to run")
	}

	localRoot := os.Getenv("LOCAL_TMP_ROOT")
	if localRoot == "" {
		t.Skip("set LOCAL_TMP_ROOT to a workspace-local directory")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	cookiePath := filepath.Join(home, ".config", "ytx", "cookies.txt")
	if _, err := os.Stat(cookiePath); err != nil {
		t.Fatalf("cookie file unavailable: %v", err)
	}

	cacheDir := filepath.Join(localRoot, "cache")
	_ = os.RemoveAll(cacheDir)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}

	ext, err := NewExtractor(ModeMusic, cookiePath, WithCacheManager(&CacheManager{cacheDir: cacheDir}))
	if err != nil {
		t.Fatalf("new extractor: %v", err)
	}
	ctx := context.Background()
	videoID := "dQw4w9WgXcQ"

	cipher, err := ext.fetchAndCacheCipherContext(ctx, videoID)
	if err != nil {
		t.Fatalf("fetchAndCacheCipherContext: %v", err)
	}
	if !cipher.sigUsesURLWrapper {
		t.Fatalf("expected wrapper mode for PoC, got sigFunction=%q nFunction=%q", cipher.sigFunctionName, cipher.nFunctionName)
	}

	wrapperName := cipher.sigFunctionName
	playerJS := string(cipher.playerJS)
	built := buildWrapperRuntimeJS(playerJS, wrapperName)
	extracted, _ := extractWithAST(playerJS, wrapperName)

	t.Logf("detected wrapper mode: sigFunction=%q nFunction=%q playerJSLen=%d builtLen=%d extractedLen=%d cacheJSLen=%d", wrapperName, cipher.nFunctionName, len(playerJS), len(built), len(extracted), len(cipher.jsCode))

	measure := func(label, code string) {
		if code == "" {
			t.Logf("%s: skipped (empty code)", label)
			return
		}

		c := &Cipher{runtime: NewRuntime(), sigFunctionName: wrapperName, sigUsesURLWrapper: true, jsCode: code, playerJS: cipher.playerJS}
		defer c.Close()

		start1 := time.Now()
		_, err1 := c.transformWithURLWrapper(code, wrapperName, "abcdef", "s")
		dur1 := time.Since(start1)

		start2 := time.Now()
		_, err2 := c.transformWithURLWrapper(code, wrapperName, "abcdef", "s")
		dur2 := time.Since(start2)

		t.Logf("%s: codeLen=%d first=%s err1=%v second=%s err2=%v", label, len(code), dur1, err1, dur2, err2)
	}

	measure("full-player", playerJS)
	measure("build-wrapper-runtime", built)
	measure("ast-extracted", extracted)

	if err := ext.fetchVisitorDataContext(ctx, videoID); err != nil {
		t.Fatalf("fetchVisitorDataContext: %v", err)
	}
	ext.earlySTSOverride = cipher.SignatureTimestamp()
	playerResp, err := ext.callPlayerAPIContext(ctx, videoID)
	ext.earlySTSOverride = 0
	if err != nil {
		t.Fatalf("callPlayerAPIContext: %v", err)
	}
	stream := ext.findBestAudioStream(playerResp.StreamingData.AdaptiveFormats)
	if stream == nil || stream.SignatureCipher == "" {
		t.Fatalf("expected signature cipher in player response")
	}
	params, err := url.ParseQuery(stream.SignatureCipher)
	if err != nil {
		t.Fatalf("parse signature cipher: %v", err)
	}
	realSig := params.Get("s")
	if realSig == "" {
		t.Fatalf("missing encrypted signature in player response")
	}
	t.Logf("real encrypted sig length=%d", len(realSig))

	loaded, err := ext.cacheManager.Load()
	if err != nil {
		t.Fatalf("load cache: %v", err)
	}
	reloadedForReal := NewCipherFromCacheWithRuntime(NewRuntime(), loaded)
	if playerJSReloaded, err := ext.cacheManager.LoadPlayerJS(); err == nil {
		reloadedForReal.playerJS = playerJSReloaded
	}
	defer reloadedForReal.Close()
	realStart1 := time.Now()
	_, realErr1 := reloadedForReal.DecryptSignature(realSig)
	realFirst := time.Since(realStart1)
	realStart2 := time.Now()
	_, realErr2 := reloadedForReal.DecryptSignature(realSig)
	realSecond := time.Since(realStart2)
	t.Logf("fresh-reloaded-real-decrypt: sigLen=%d first=%s err1=%v second=%s err2=%v", len(realSig), realFirst, realErr1, realSecond, realErr2)

	reloaded := NewCipherFromCacheWithRuntime(NewRuntime(), loaded)
	if playerJSReloaded, err := ext.cacheManager.LoadPlayerJS(); err == nil {
		reloaded.playerJS = playerJSReloaded
	}
	defer reloaded.Close()

	t.Logf("reloaded cache: sigFunction=%q wrapper=%v jsCodeLen=%d isBytecode=%v", reloaded.sigFunctionName, reloaded.sigUsesURLWrapper, len(reloaded.jsCode), reloaded.isBytecode)

	start1 := time.Now()
	_, err1 := reloaded.transformWithURLWrapper(reloaded.jsCode, reloaded.sigFunctionName, "abcdef", "s")
	reloadFirst := time.Since(start1)
	start2 := time.Now()
	_, err2 := reloaded.transformWithURLWrapper(reloaded.jsCode, reloaded.sigFunctionName, "abcdef", "s")
	reloadSecond := time.Since(start2)
	t.Logf("reloaded-cache-js: codeLen=%d first=%s err1=%v second=%s err2=%v", len(reloaded.jsCode), reloadFirst, err1, reloadSecond, err2)
}
