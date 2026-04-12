package ytx

import (
	"bytes"
	"time"
)

type cipherDetectionResult struct {
	SigName           string
	SigParam          int
	SigUsesURLWrapper bool
	WrapperRuntimeJS  string
	NName             string
}

// detectFastSignature attempts Tier 1 detection (fast windowed/primary logic)
func detectFastSignature(js []byte, detail *CipherAnalyzeDetail) (cipherDetectionResult, bool) {
	tSig := time.Now()
	var result cipherDetectionResult

	sigName, sigParam, err := findSigFunctionNameFast(js)
	detail.SigMs += time.Since(tSig).Milliseconds()

	if err != nil {
		return result, false
	}

	result.SigName = sigName
	result.SigParam = sigParam

	detail.SigName = sigName
	if bytes.Contains(js, []byte(",decodeURIComponent(")) {
		detail.SigTier = "primary"
	} else {
		detail.SigTier = "windowed"
	}
	detail.WrapperTier = "skipped"

	return result, true
}

// detectWrapperSignature attempts Tier 2 detection (URL Wrapper windowed/global)
func detectWrapperSignature(js []byte, detail *CipherAnalyzeDetail) (cipherDetectionResult, bool) {
	tWrapper := time.Now()
	var result cipherDetectionResult

	wrapperNames := findURLTransformFunctionNamesWindowed(js)
	if len(wrapperNames) > 0 {
		detail.WrapperTier = "windowed"
	} else {
		wrapperNames = findURLTransformFunctionNamesGlobal(js)
		if len(wrapperNames) > 0 {
			detail.WrapperTier = "global_fallback"
			detail.MarkerMiss = append(detail.MarkerMiss, `set("alr"`)
		}
	}
	detail.WrapperMs += time.Since(tWrapper).Milliseconds()

	if len(wrapperNames) == 0 {
		return result, false
	}

	result.SigName = wrapperNames[0]
	result.SigParam = 0
	result.SigUsesURLWrapper = true
	detail.SigName = result.SigName
	detail.SigTier = "wrapper"

	tBuild := time.Now()
	wrapperRuntimeJS := buildWrapperRuntimeJSWindowed(string(js), result.SigName)
	if wrapperRuntimeJS != "" {
		detail.WrapperBuildTier = "windowed"
	} else {
		wrapperRuntimeJS = buildWrapperRuntimeJSGlobal(string(js), result.SigName)
		if wrapperRuntimeJS != "" {
			detail.WrapperBuildTier = "global_fallback"
		}
	}
	detail.WrapperBuildMs += time.Since(tBuild).Milliseconds()

	result.WrapperRuntimeJS = wrapperRuntimeJS
	return result, true
}

// detectGlobalSignature attempts Tier 3 detection (slow regex over full file)
func detectGlobalSignature(js []byte, detail *CipherAnalyzeDetail) (cipherDetectionResult, bool) {
	tSigGlobal := time.Now()
	var result cipherDetectionResult

	sigName, sigParam, err := findSigFunctionNameGlobal(js)
	detail.SigMs += time.Since(tSigGlobal).Milliseconds()

	if err != nil {
		return result, false
	}

	result.SigName = sigName
	result.SigParam = sigParam

	detail.SigName = sigName
	detail.SigTier = "global_fallback"
	detail.MarkerMiss = append(detail.MarkerMiss, "sig_patterns")
	detail.WrapperTier = "skipped"

	return result, true
}

// detectNFunction runs the universal N-function detection
func detectNFunction(js []byte, isWrapper bool, detail *CipherAnalyzeDetail) string {
	tN := time.Now()
	var nName string

	if isWrapper {
		nName = findNFunctionNameWindowed(js)
		if nName != "" {
			detail.NFuncTier = "windowed"
		} else {
			detail.NFuncTier = "skipped_wrapper"
		}
	} else {
		nName = findNFunctionNameWindowed(js)
		if nName != "" {
			detail.NFuncTier = "windowed"
		} else {
			nName = findNFunctionNameGlobal(js)
			if nName != "" {
				detail.NFuncTier = "global_fallback"
				detail.MarkerMiss = append(detail.MarkerMiss, `get("n")`)
			} else {
				detail.NFuncTier = "not_found"
			}
		}
	}

	detail.NFuncMs += time.Since(tN).Milliseconds()
	detail.NFuncName = nName
	return nName
}
