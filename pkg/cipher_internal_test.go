package ytx

import (
	"strings"
	"testing"
)

func TestCipherSaneDecrypted(t *testing.T) {
	c := &Cipher{}

	// Test field "s"
	if !c.saneDecrypted(strings.Repeat("a", 100), strings.Repeat("b", 100), "s") {
		t.Error("expected valid s transform to be sane")
	}
	if c.saneDecrypted(strings.Repeat("a", 100), strings.Repeat("a", 100), "s") {
		t.Error("expected identity s transform to be rejected")
	}
	if c.saneDecrypted(strings.Repeat("a", 100), "short", "s") {
		t.Error("expected truncated output to be rejected")
	}

	// Test field "n"
	if !c.saneDecrypted("input", "output", "n") {
		t.Error("expected valid n transform to be sane")
	}
	if c.saneDecrypted("input", "input", "n") {
		t.Error("expected identity n transform to be rejected")
	}
	if c.saneDecrypted("input", "", "n") {
		t.Error("expected empty n transform to be rejected")
	}
}

func TestTryWrapperCandidatesFailures(t *testing.T) {
	c := &Cipher{
		sigFunctionName: "kS",
		jsCode:          "function kS(a,b,c){return {get:function(){return '';}};} // bad wrapper",
	}
	// Missing wrapper context ensures it will fail during evaluation or return identity

	// 's' field failure should return ""
	res, err := c.tryWrapperCandidates(strings.Repeat("a", 100), "s")
	if err == nil {
		t.Error("expected error for bad wrapper")
	}
	if res != "" {
		t.Errorf("expected empty string on 's' failure, got %q", res)
	}

	// 'n' field failure should return original value
	res, err = c.tryWrapperCandidates("some_n_value", "n")
	if err == nil {
		t.Error("expected error for bad wrapper")
	}
	if res != "some_n_value" {
		t.Errorf("expected original value on 'n' failure, got %q", res)
	}
}

func TestEnsureNRuntime(t *testing.T) {
	c := &Cipher{
		nFunctionName: "n_step",
		playerJS:      []byte(`function n_step(a){return a}; })(_yt_player);`),
	}
	rt, err := c.ensureNRuntime()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(rt), `_exposed['n_step']=n_step`) {
		t.Errorf("missing exposed wrapper in runtime: %q", string(rt))
	}

	// Fetch again to ensure caching works (doesn't overwrite incorrectly)
	rt2, err := c.ensureNRuntime()
	if err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	if string(rt) != string(rt2) {
		t.Error("consecutive calls to ensureNRuntime returned different results")
	}

	// Error case
	cEmpty := &Cipher{}
	_, err = cEmpty.ensureNRuntime()
	if err == nil {
		t.Error("expected error for empty cipher")
	}
}
