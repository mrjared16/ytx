package ytx

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/mrjared16/ytx/internal/jsextract"
)

var urlTransformPatterns = []*regexp.Regexp{

	regexp.MustCompile(`(?:^|[^a-zA-Z0-9_$])([a-zA-Z0-9_$]{1,})\s*=\s*function\([^)]*\)\{[^\{\}]{0,800}(?:[^.a-zA-Z0-9_$]|^)[a-zA-Z0-9_$]+\.set\("alr","yes"\)`),
	regexp.MustCompile(`(?:^|[^a-zA-Z0-9_$])([a-zA-Z0-9_$]{1,})\s*=\s*function\([^)]*\)\{[^\{\}]{0,800}(?:[^.a-zA-Z0-9_$]|^)[a-zA-Z0-9_$]+\.set\('alr','yes'\)`),
	regexp.MustCompile(`function\s+([a-zA-Z0-9_$]{1,})\([^)]*\)\{[^\{\}]{0,800}(?:[^.a-zA-Z0-9_$]|^)[a-zA-Z0-9_$]+\.set\("alr","yes"\)`),
	regexp.MustCompile(`function\s+([a-zA-Z0-9_$]{1,})\([^)]*\)\{[^\{\}]{0,800}(?:[^.a-zA-Z0-9_$]|^)[a-zA-Z0-9_$]+\.set\('alr','yes'\)`),

	regexp.MustCompile(`(?:^|[^a-zA-Z0-9_$])([a-zA-Z0-9_$]{1,})\s*=\s*function\([^)]*,[^)]*,[^)]*\)\{[^\{\}]{0,1000}\.set\([^)]+\)[^\{\}]{0,800}\.get\(["']s["']\)`),
	regexp.MustCompile(`function\s+([a-zA-Z0-9_$]{1,})\([^)]*,[^)]*,[^)]*\)\{[^\{\}]{0,1000}\.set\([^)]+\)[^\{\}]{0,800}\.get\(["']s["']\)`),
}

var urlTransformInvalidPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\.call\(this\)`),
	regexp.MustCompile(`\.policy\s*=`),
}

func newWrapperChecker(js []byte) func(string) bool {
	var cachedWrappers []string
	var wrappersFetched bool
	return func(name string) bool {
		if !wrappersFetched {
			cachedWrappers = findURLTransformFunctionNames(js)
			wrappersFetched = true
		}
		for _, w := range cachedWrappers {
			if w == name {
				return true
			}
		}
		return false
	}
}

func findURLTransformFunctionName(js []byte) string {
	names := findURLTransformFunctionNames(js)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func findURLTransformFunctionNames(js []byte) []string {
	if out := findURLTransformFunctionNamesWindowed(js); len(out) > 0 {
		return out
	}

	return findURLTransformFunctionNamesGlobal(js)
}

// findURLTransformFunctionNamesWindowed uses marker-based windowing for fast extraction.
func findURLTransformFunctionNamesWindowed(js []byte) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)

	markers := [][]byte{
		[]byte(`set("alr"`), []byte(`set('alr'`),

		[]byte(`.get("s")`), []byte(`.get('s')`),
	}

	for _, marker := range markers {
		offset := 0
		for {
			idx := bytes.Index(js[offset:], marker)
			if idx == -1 {
				break
			}
			absIdx := offset + idx
			offset = absIdx + len(marker)

			start := absIdx - 4000
			if start < 0 {
				start = 0
			}
			end := absIdx + 4000
			if end > len(js) {
				end = len(js)
			}
			chunk := js[start:end]

			for _, p := range urlTransformPatterns {
				matches := p.FindAllSubmatchIndex(chunk, -1)
				for _, mIdx := range matches {
					if len(mIdx) < 4 {
						continue
					}
					fullMatch := chunk[mIdx[0]:mIdx[1]]
					absStart := start + mIdx[0]
					if absStart > 0 && jsextract.IsJSIdentifierByte(js[absStart-1]) {
						continue
					}
					name := strings.TrimSpace(string(chunk[mIdx[2]:mIdx[3]]))

					invalid := false
					for _, invalidP := range urlTransformInvalidPatterns {
						if invalidP.MatchString(string(fullMatch)) {
							invalid = true
							break
						}
					}
					if invalid || name == "" || !isValidIdentifier(name) {
						continue
					}
					if _, ok := seen[name]; !ok {
						seen[name] = struct{}{}
						out = append(out, name)
					}
				}
			}
		}
	}
	return out
}

// findURLTransformFunctionNamesGlobal is the slow but robust global scan fallback.
func findURLTransformFunctionNamesGlobal(js []byte) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	for _, p := range urlTransformPatterns {
		matches := p.FindAllSubmatchIndex(js, -1)
		for _, mIdx := range matches {
			if len(mIdx) < 4 {
				continue
			}
			fullMatch := js[mIdx[0]:mIdx[1]]
			if mIdx[0] > 0 && jsextract.IsJSIdentifierByte(js[mIdx[0]-1]) {
				continue
			}
			name := strings.TrimSpace(string(js[mIdx[2]:mIdx[3]]))

			invalid := false
			for _, invalidP := range urlTransformInvalidPatterns {
				if invalidP.MatchString(string(fullMatch)) {
					invalid = true
					break
				}
			}
			if invalid || name == "" || !isValidIdentifier(name) {
				continue
			}
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
				out = append(out, name)
			}
		}
	}
	return out
}

func isWrapperSignatureCandidate(js []byte, funcName string) bool {
	if funcName == "" {
		return false
	}

	wrappers := findURLTransformFunctionNames(js)
	for _, w := range wrappers {
		if w == funcName {
			return true
		}
	}
	return false
}
