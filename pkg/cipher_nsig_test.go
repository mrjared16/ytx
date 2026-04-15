package ytx

import "testing"

func TestFindURLTransformFunctionName(t *testing.T) {
	js := `um=function(R,w,p){R=new g.qJ(R,!0);R.set("alr","yes");return R}`
	if got := findURLTransformFunctionName([]byte(js)); got != "um" {
		t.Fatalf("unexpected wrapper name: got %q want %q", got, "um")
	}
}

func TestFindNFunctionName(t *testing.T) {
	tests := []struct {
		name     string
		js       string
		wantName string
	}{
		{
			name:     "resolves array index from get n pattern",
			js:       `var RDD=[A0x,AbC,Z9k];if((new g.qJ(R,!0)).get("n")&&(b=RDD[1](q.get("n"))||x))return b;`,
			wantName: "AbC",
		},
		{
			name:     "falls back to direct call pattern",
			js:       `if(q.get('n')){b=QnR(q.get('n'));}`,
			wantName: "QnR",
		},
		{
			name:     "resolves bare array assignment",
			js:       `RDD=[A0x,AbC,Z9k];if((new g.qJ(R,!0)).get('n')&&(b=RDD[1](q.get('n'))||x))return b;`,
			wantName: "AbC",
		},
		{
			name:     "ignores unrelated Array fallback",
			js:       `function BQt(c){try{var r=(new g.XK(c,!0)).get("n");if(r){var b=c.match(/\/n\/([^/]+)/);if(b&&b[1]&&b[1]!==r)return c.replace("/n/"+b[1],"/n/"+r)}}catch(B){g.$E(B)}return c};b=Array(r)`,
			wantName: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := findNFunctionName([]byte(tc.js)); got != tc.wantName {
				t.Fatalf("unexpected n function: got %q want %q", got, tc.wantName)
			}
		})
	}
}

func TestDetectNFunctionWrapperUsesGlobalFallback(t *testing.T) {
	js := []byte(`var RDD=[a0x];function noop(){return RDD[0]}`)
	detail := &CipherAnalyzeDetail{}

	if got := detectNFunction(js, true, detail); got != "a0x" {
		t.Fatalf("unexpected n function: got %q want %q", got, "a0x")
	}
	if detail.NFuncTier != "global_fallback" {
		t.Fatalf("unexpected tier: got %q want %q", detail.NFuncTier, "global_fallback")
	}
	if len(detail.MarkerMiss) != 1 || detail.MarkerMiss[0] != `get("n")` {
		t.Fatalf("unexpected marker miss: %#v", detail.MarkerMiss)
	}
}
