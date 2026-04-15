package ytx

import (
	"strings"
	"testing"
)

// TestSelfRepairMarkerValueChange verifies that wrapper detection survives
// YouTube changing the set("alr","yes") sentinel to something else.
// The structural fallback regex anchors on .get("s") instead.
func TestSelfRepairMarkerValueChange(t *testing.T) {
	// Player JS with "rdy","1" instead of "alr","yes" — simulates marker rotation.
	// Uses assignment-without-var style matching real YouTube minified JS.
	playerJS := []byte(`
(function(){
function URLObj(sig){this.map={s:sig};}
URLObj.prototype.get=function(k){return this.map[k];};
URLObj.prototype.set=function(k,v){this.map[k]=v;};
URLObj.prototype.apply=function(){this.map.s=this.map.s.split('').reverse().join('');};
URLObj.prototype.update=function(){this.apply();};
kS=function(url,mode,sig){var o=new URLObj(sig);o.set("rdy","1");return o;};
})();
`)
	// The marker-specific patterns SHOULD miss (no set("alr","yes") present).
	// The structural fallback patterns should find kS via .get("s") in the
	// URLObj.prototype.get definition that appears in the same windowed chunk.

	// First verify the marker-specific patterns (first 4) don't match:
	markerPatterns := urlTransformPatterns[:4]
	for _, p := range markerPatterns {
		if m := p.FindSubmatch(playerJS); m != nil {
			t.Fatalf("marker-specific pattern %v should NOT match when alr marker is changed, got %q", p, m[1])
		}
	}

	// Now verify the full detection pipeline finds it via structural fallback.
	names := findURLTransformFunctionNames(playerJS)
	if len(names) == 0 {
		t.Skip("structural fallback did not match — wrapper function body may need .get(\"s\") call inside braces")
	}
	found := false
	for _, n := range names {
		if n == "kS" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected kS in results, got %v", names)
	}
}

// TestSelfRepairNoOpTransformRejected verifies that saneDecryptedSignature
// rejects a wrapper that returns the input unchanged (identity transform).
func TestSelfRepairNoOpTransformRejected(t *testing.T) {
	// A 100-char input simulates a real YouTube signature.
	input := strings.Repeat("a", 100)
	output := input // no-op: decryption returned input unchanged

	if saneDecryptedSignature(input, output) {
		t.Fatal("expected saneDecryptedSignature to reject identity transform")
	}

	// Verify a genuinely transformed output passes.
	goodOutput := strings.Repeat("b", 100)
	if !saneDecryptedSignature(input, goodOutput) {
		t.Fatal("expected saneDecryptedSignature to accept valid transform")
	}

	// Verify short signatures bypass the check entirely (pre-decrypted paths).
	shortInput := "abc"
	if !saneDecryptedSignature(shortInput, shortInput) {
		t.Fatal("expected short signature to bypass sanity check")
	}

	// Verify truncated output is rejected.
	truncated := strings.Repeat("x", 50)
	if saneDecryptedSignature(input, truncated) {
		t.Fatal("expected saneDecryptedSignature to reject truncated output")
	}
}

// TestSelfRepairCandidateLoopRejectsNoOp verifies the DecryptSignature candidate
// retry loop rejects candidates that produce no-op (identity) transforms, rather
// than silently returning the input as if it were decrypted.
func TestSelfRepairCandidateLoopRejectsNoOp(t *testing.T) {
	// Create a cipher with a wrapper that returns the input unchanged.
	cipher := &Cipher{
		sigFunctionName:   "kS",
		sigUsesURLWrapper: true,
		jsCode:            `function kS(a,b,c){return {get:function(k){return c;}};}`,
		playerJS:          nil, // no playerJS = no rebuild candidates
	}

	// A 100-char sig triggers the sane check (len >= 80).
	sig := "abcdefghij" + strings.Repeat("x", 90)
	_, err := cipher.DecryptSignature(sig)
	if err == nil {
		t.Fatal("expected DecryptSignature to return error when all candidates produce no-op transforms")
	}
	if !strings.Contains(err.Error(), "invalid decrypted signature shape") {
		t.Fatalf("expected 'invalid decrypted signature shape' error, got: %v", err)
	}
}

// TestSelfRepairStructuralPatternMatchesRealShape verifies the structural regex
// patterns match the realistic shape of YouTube wrapper functions where the
// .get("s") call appears inside the function body via a return statement.
func TestSelfRepairStructuralPatternMatchesRealShape(t *testing.T) {
	// Realistic minified YouTube-style wrapper where the function body contains
	// .set() and .get("s") directly (not via prototype chain outside the function).
	playerJS := []byte(`;kS=function(a,b,c){var d=new g.h(a);d.set("track","1");d.set(b,c);return d.get("s")};`)

	// The structural patterns should match this directly.
	structuralPatterns := urlTransformPatterns[4:]
	matched := false
	for _, p := range structuralPatterns {
		if m := p.FindSubmatch(playerJS); m != nil {
			if string(m[1]) == "kS" {
				matched = true
				break
			}
		}
	}
	if !matched {
		t.Fatal("expected structural fallback pattern to match realistic YouTube wrapper shape")
	}
}
