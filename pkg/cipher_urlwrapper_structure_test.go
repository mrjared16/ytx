package ytx

import (
	"fmt"
	"strings"
	"testing"
)

// urlWrapperStructurePlayable renders an embed-style player bundle carrying
// the given wrapper definitions plus the standard decoy functions.
func urlWrapperStructurePlayable(wrappers string) []byte {
	return []byte(fmt.Sprintf(`
(function(_yt_player){
var g={};
%s
g.zc=function(T,M,q){var Q=new g.$I(M,!0);Q.get("alr")||Q.set("alr","yes");T.K&&Q.set("x",1);return Q};
Veg=function(P,O){this.ZD=new g.$I(P,!0);this.ZD.set("alr", "yes");return this};
})(_yt_player);
`, wrappers))
}

func TestURLWrapperStructureEmptyGateDecoyRejected(t *testing.T) {
	// Reported false positive: the old check matched the third-param gate and
	// the signing call as two independent tokens, so an `if(q){}` empty gate
	// plus an unconditional signing call was accepted as modern. The signing
	// call is NOT inside the gated block and must be rejected.
	decoy := `F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");if(q){}T.set(M,"fixed");return T};`
	if got := findURLWrapperCandidate([]byte(decoy)); got != "" {
		t.Fatalf("empty-gate decoy alone must be rejected, got %q", got)
	}

	// The same decoy beside a valid wrapper must not disturb discovery: only
	// the valid wrapper qualifies.
	js := urlWrapperStructurePlayable(
		`jW=function(T,M,q){M=M===void 0?"":M;q=q===void 0?"":q;T=new g.$I(T,!0);T.set("alr","yes");q&&(q=Qe(q),T[S[21]](M,q));return T};
Qe=function(x){return x};
F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");if(q){}T.set(M,"fixed");return T};`,
	)
	disc := urlWrapperDiscover(js)
	if disc.status != urlWrapperDiscoveryFound || disc.name != "jW" {
		t.Fatalf("valid wrapper beside the empty-gate decoy must be found alone, got %#v", disc)
	}
	if got := findURLWrapperCandidates(js); len(got) != 1 || got[0] != "jW" {
		t.Fatalf("expected exactly one candidate jW, got %#v", got)
	}
}

func TestURLWrapperStructureGatedConstantSignRejected(t *testing.T) {
	// Reported false positive: the gated-sign shape accepted a signing call
	// whose VALUE is a bare constant (`if(q){T.set(M,"fixed")}` and its
	// short-circuit twin) — the wrapper would sign the same constant whatever
	// the input signature, so there is no dependence on the third parameter.
	// These decoys must be rejected.
	cases := []struct {
		name string
		js   string
	}{
		{
			"if-gated-constant",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");if(q){T.set(M,"fixed")}return T};`,
		},
		{
			"short-circuit-constant",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&(T.set(M,"fixed"));return T};`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findURLWrapperCandidate([]byte(tc.js)); got != "" {
				t.Fatalf("gated constant-sign decoy %q must be rejected, got %q", tc.js, got)
			}
		})
	}

	// The same decoy beside a valid wrapper must not disturb discovery: only
	// the valid wrapper qualifies.
	js := urlWrapperStructurePlayable(
		`jW=function(T,M,q){M=M===void 0?"":M;q=q===void 0?"":q;T=new g.$I(T,!0);T.set("alr","yes");q&&(q=Qe(q),T[S[21]](M,q));return T};
Qe=function(x){return x};
F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");if(q){T.set(M,"fixed")}return T};`,
	)
	disc := urlWrapperDiscover(js)
	if disc.status != urlWrapperDiscoveryFound || disc.name != "jW" {
		t.Fatalf("valid wrapper beside the gated constant-sign decoy must be found alone, got %#v", disc)
	}
	if got := findURLWrapperCandidates(js); len(got) != 1 || got[0] != "jW" {
		t.Fatalf("expected exactly one candidate jW, got %#v", got)
	}
}

func TestURLWrapperStructureRejectsBadlyScopedSign(t *testing.T) {
	// Each variant carries the marker and the rebuild but fails the scoped
	// gating/signing/return contract in one dimension.
	cases := []struct {
		name string
		js   string
	}{
		{
			"ungated-sign",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");T.set(M,"fixed");return T};`,
		},
		{
			"gate-on-second-param",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");M&&(T.set(M,"fixed"));return T};`,
		},
		{
			"key-not-second-param",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&(T.set(X,"fixed"));return T};`,
		},
		{
			"gated-and-ungated-twin",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&(T.set(M,"x"));T.set(M,"fixed");return T};`,
		},
		{
			"return-other-object",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&(T.set(M,"x"));return U};`,
		},
		{
			"sign-in-nested-function",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&(function(){T.set(M,"x")})();return T};`,
		},
		{
			"sign-in-skipped-statement",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");if(q){for(i=0;i<1;i++){T.set(M,"x")}}return T};`,
		},
		{
			"comment-fakes-return",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&(q=Qe(q),T[S[21]](M,q));/* return T};`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findURLWrapperCandidate([]byte(tc.js)); got != "" {
				t.Fatalf("decoy %q must be rejected, got %q", tc.js, got)
			}
		})
	}
}

func TestURLWrapperStructureRejectsGateNotDirectThirdParam(t *testing.T) {
	// Gate conditions that merely mention the third parameter somewhere are
	// not the supported "present when p3 is set" truthiness gate: only a
	// bare `p3` condition (parens are stripped by the parser) or a bare
	// `p3 && ...` left-hand side gates.
	cases := []struct {
		name string
		js   string
	}{
		{
			"negated-gate",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");if(!q){T.set(M,"x")}return T};`,
		},
		{
			"tautological-gate",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");if(q||true){T.set(M,"x")}return T};`,
		},
		{
			"call-gate",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");if(Qe(q)){T.set(M,"x")}return T};`,
		},
		{
			"negated-short-circuit",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");!q&&(T.set(M,"x"));return T};`,
		},
		{
			"non-gate-short-circuit",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");(M&&q)&&(T.set(M,"x"));return T};`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findURLWrapperCandidate([]byte(tc.js)); got != "" {
				t.Fatalf("indirect third-param condition %q must be rejected, got %q", tc.js, got)
			}
		})
	}
}

func TestURLWrapperStructureUnsupportedFormsFailClosed(t *testing.T) {
	// A second signing call hidden inside an unvisited statement or
	// expression form must reject the candidate — unknown forms are never
	// treated as no-ops.
	cases := []struct {
		name string
		js   string
	}{
		{
			"loop-hides-second-sign",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&T.set(M,"x");for(let i=0;i<1;i++){T.set(M,"fixed")}return T};`,
		},
		{
			"while-hides-sign",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&(T.set(M,"x"));while(q){T.set(M,"fixed")}return T};`,
		},
		{
			"try-hides-sign",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&(T.set(M,"x"));try{T.set(M,"fixed")}catch(e){}return T};`,
		},
		{
			"object-literal-hides-sign",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&(T.set(M,"x"));var o={y:T.set(M,"fixed")};return T};`,
		},
		{
			"array-literal-hides-sign",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&(T.set(M,"x"));var a=[T.set(M,"fixed")];return T};`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findURLWrapperCandidate([]byte(tc.js)); got != "" {
				t.Fatalf("unsupported form %q must be rejected, got %q", tc.js, got)
			}
		})
	}
}

func TestURLWrapperStructureReturnContractRejects(t *testing.T) {
	// Any return that is not THE single final top-level `return p1` breaks
	// the wrapper contract: wrong, unreachable, or earlier alternate returns
	// are all rejected regardless of how many `return T` texts appear.
	cases := []struct {
		name string
		js   string
	}{
		{
			"unreachable-return-then-other",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&(T.set(M,"x"));if(false){return T}return U};`,
		},
		{
			"alternate-return-breaks-contract",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&(T.set(M,"x"));if(q){return U}return T};`,
		},
		{
			"early-same-object-return",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&(T.set(M,"x"));if(q){return T}return T};`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findURLWrapperCandidate([]byte(tc.js)); got != "" {
				t.Fatalf("return contract break %q must be rejected, got %q", tc.js, got)
			}
		})
	}
}

func TestURLWrapperStructurePreservesWrapperFormats(t *testing.T) {
	// Actual wrapper formats must keep being recognized: the classic
	// `q&&(...)` short-circuit style, the ES6 default parameters, renamed
	// arguments, quote variants, and `if`-gated variants with dot-set or
	// bracket-subscript signing.
	cases := []struct {
		name string
		js   string
		want string
	}{
		{
			"short-circuit-jw",
			`jW=function(T,M,q){M=M===void 0?"":M;q=q===void 0?"":q;T=new g.$I(T,!0);T.set("alr","yes");q&&(q=Qe(q),T[S[21]](M,q));return T};`,
			"jW",
		},
		{
			"watch-style-pm",
			`pm=function(S,x,Q){x=x===void 0?"":x;Q=Q===void 0?"":Q;S=new g.jJ(S,!0);S.set("alr","yes");Q&&(Q=wh(9,7481,r5(6,5833,Q)),S[SE[22]](x,ZU(15,2967,Q)));return S};`,
			"pm",
		},
		{
			"es6-defaults-u8",
			`u8=function(K,q="",N=""){K=new g.jD(K,!0);K.set("alr","yes");N&&(N=Jn(25,2163,Wq(2,3394,N)),K[F[14]](q,QE(6,223,N)));return K};`,
			"u8",
		},
		{
			"renamed-single-quote",
			`qz=function(a,b,c){c=c===void 0?"":c;a=new g.zz(a,!0);a.set('alr','yes');c&&(c=hh(c),a[K1[3]](b,c));return a};`,
			"qz",
		},
		{
			"plain-params",
			`um=function(R,w,p){p=p===void 0?"":p;R=new g.qJ(R,!0);R.set("alr","yes");p&&(p=X9(p),R[K1[0]](w,p));return R};`,
			"um",
		},
		{
			"if-block-dot-set",
			`F=function(T,M,q){M=M===void 0?"":M;q=q===void 0?"":q;T=new g.$I(T,!0);T.set('alr','yes');if(q){q=Qe(q);T.set(M,q)}return T};`,
			"F",
		},
		{
			"if-statement-no-braces",
			`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");if(q)q=Qe(q),T[S[21]](M,q);return T};`,
			"F",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findURLWrapperCandidate([]byte(tc.js)); got != tc.want {
				t.Fatalf("wrapper must be recognized as %q, got %q (%q)", tc.want, got, tc.js)
			}
		})
	}
}

func TestURLWrapperStructureAmbiguityStaysFailClosed(t *testing.T) {
	a := `jW=function(T,M,q){M=M===void 0?"":M;q=q===void 0?"":q;T=new g.$I(T,!0);T.set("alr","yes");q&&(q=Qe(q),T[S[21]](M,q));return T};`
	b := `pm=function(S,x,Q){x=x===void 0?"":x;Q=Q===void 0?"":Q;S=new g.jJ(S,!0);S.set("alr","yes");Q&&(Q=wh(9,7481,r5(6,5833,Q)),S[SE[22]](x,ZU(15,2967,Q)));return S};`
	js := []byte(a + b)
	if disc := urlWrapperDiscover(js); disc.status != urlWrapperDiscoveryAmbiguous {
		t.Fatalf("two distinct valid wrappers must stay ambiguous, got %#v", disc)
	}
	if got := findURLWrapperCandidates(js); got != nil {
		t.Fatalf("ambiguous candidates must yield no wrapper, got %#v", got)
	}
}

func TestURLWrapperStructureBoundedSourceRejected(t *testing.T) {
	// Tail beyond the 2048-byte body-tail window: incomplete body, rejected.
	var sb strings.Builder
	for i := 0; i < 2200; i++ {
		sb.WriteString(" ")
	}
	longTail := []byte(`F=function(T,M,q){T=new g.X(T,!0);T.set("alr","yes");q&&(q=Qe(q),T[S[21]](M,q));` + sb.String() + `return T};`)
	if got := findURLWrapperCandidate(longTail); got != "" {
		t.Fatalf("tail beyond the scan window must be rejected, got %q", got)
	}

	// Marker deeper than the 4096-byte backward brace scan: rejected.
	var sb2 strings.Builder
	for i := 0; i < 5000; i++ {
		sb2.WriteString(" ")
	}
	deep := []byte(`F=function(T,M,q){` + sb2.String() + `T=new g.X(T,!0);T.set("alr","yes");q&&(q=Qe(q),T[S[21]](M,q));return T};`)
	if got := findURLWrapperCandidate(deep); got != "" {
		t.Fatalf("marker beyond the backward brace scan must be rejected, got %q", got)
	}
}
