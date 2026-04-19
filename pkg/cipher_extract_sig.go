package ytx

import (
	"bytes"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

type sigFunctionPattern struct {
	regex    *regexp.Regexp
	sigIdx   int
	paramIdx int
}

var sigFunctionPatterns = []sigFunctionPattern{
	{

		regex:    regexp.MustCompile(`\b([a-zA-Z0-9_$]+)\s*&&\s*\(\s*(?:\(\s*)?[a-zA-Z0-9_$]+\s*\)?\s*=\s*([a-zA-Z0-9_$]{2,})\(\s*decodeURIComponent\(\s*[a-zA-Z0-9_$]+\s*\)\s*\)\s*\)?`),
		sigIdx:   2,
		paramIdx: 0,
	},
	{
		regex:    regexp.MustCompile(`\b[a-zA-Z0-9_$]+\s*&&\s*\(\s*[a-zA-Z0-9_$]+\s*=\s*([a-zA-Z0-9_$]{2,})\(\s*(?:(\d+)\s*,\s*)?decodeURIComponent\(\s*[a-zA-Z0-9_$]+\s*\)\s*\)`),
		sigIdx:   1,
		paramIdx: 2,
	},
	{
		regex:    regexp.MustCompile(`(?:\b|[^a-zA-Z0-9_$])([a-zA-Z0-9_$]{2,})\s*=\s*function\(\s*a\s*\)\s*{\s*a\s*=\s*a\.split\(\s*""\s*\)(?:;[a-zA-Z0-9_$]{2}\.[a-zA-Z0-9_$]{2}\(a,\d+\))?`),
		sigIdx:   1,
		paramIdx: 0,
	},
	{
		regex:    regexp.MustCompile(`\bm=([a-zA-Z0-9$]{2,})\(decodeURIComponent\(h\.s\)\)`),
		sigIdx:   1,
		paramIdx: 0,
	},
	{
		regex:    regexp.MustCompile(`["']signature["']\s*,\s*([a-zA-Z0-9$]+)\(`),
		sigIdx:   1,
		paramIdx: 0,
	},
	{
		regex:    regexp.MustCompile(`\.sig\|\|([a-zA-Z0-9$]+)\(`),
		sigIdx:   1,
		paramIdx: 0,
	},
	{
		regex:    regexp.MustCompile(`\b[cs]\s*&&\s*[adf]\.set\([^,]+\s*,\s*(?:encodeURIComponent\s*\(\s*)?([a-zA-Z0-9$]+)\(`),
		sigIdx:   1,
		paramIdx: 0,
	},
}

// findSigFunctionName searches for the signature decryption function name.
func findSigFunctionName(js []byte) (string, int, error) {
	name, param, err := findSigFunctionNameFast(js)
	if err == nil {
		return name, param, nil
	}
	return findSigFunctionNameGlobal(js)
}

// findSigFunctionNameFast tries primary decodeURIComponent detection and
// windowed regex fallback only. Does NOT run expensive global regex scan.
func findSigFunctionNameFast(js []byte) (string, int, error) {

	marker := []byte(",decodeURIComponent(")
	idx := bytes.Index(js, marker)

	isWrapper := newWrapperChecker(js)

	for idx != -1 {
		start := idx - 60
		if start < 0 {
			start = 0
		}
		contextStr := string(js[start:idx])

		eqIdx := strings.LastIndex(contextStr, "=")
		if eqIdx != -1 {
			afterEq := contextStr[eqIdx+1:]
			parenIdx := strings.Index(afterEq, "(")
			if parenIdx != -1 {
				funcName := strings.TrimSpace(afterEq[:parenIdx])
				numStr := strings.TrimSpace(afterEq[parenIdx+1:])

				if isValidIdentifier(funcName) && isNumericStr(numStr) && !isWrapper(funcName) {
					beforeEq := contextStr[:eqIdx]
					if strings.Contains(beforeEq, "&&(") || strings.Contains(beforeEq, "||(") {
						param, _ := strconv.Atoi(numStr)
						return funcName, param, nil
					}
				}
			}
		}

		nextIdx := bytes.Index(js[idx+len(marker):], marker)
		if nextIdx == -1 {
			break
		}
		idx = idx + len(marker) + nextIdx
	}

	for _, p := range sigFunctionPatterns {
		var markers [][]byte
		if strings.Contains(p.regex.String(), "split") {
			markers = [][]byte{[]byte(`split("")`), []byte(`split('')`)}
		} else if strings.Contains(p.regex.String(), "decodeURIComponent") {
			markers = [][]byte{[]byte("decodeURIComponent(")}
		} else if strings.Contains(p.regex.String(), "signature") {
			markers = [][]byte{[]byte(`"signature"`), []byte(`'signature'`)}
		} else if strings.Contains(p.regex.String(), ".sig||") {
			markers = [][]byte{[]byte(".sig||")}
		} else if strings.Contains(p.regex.String(), ".set(") {
			markers = [][]byte{[]byte("encodeURIComponent(")}
		} else {

			continue
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

				m := p.regex.FindSubmatch(chunk)
				if len(m) <= p.sigIdx {
					continue
				}

				funcName := strings.TrimSpace(string(m[p.sigIdx]))
				if !isValidIdentifier(funcName) {
					continue
				}
				if isWrapper(funcName) {
					continue
				}

				param := 0
				if p.paramIdx > 0 && len(m) > p.paramIdx {
					numStr := strings.TrimSpace(string(m[p.paramIdx]))
					if isNumericStr(numStr) {
						param, _ = strconv.Atoi(numStr)
					}
				}

				return funcName, param, nil
			}
		}
	}

	return "", 0, errors.New("could not find signature function name (fast)")
}

// findSigFunctionNameGlobal runs all regex patterns on the full player.js (slow but correct).
func findSigFunctionNameGlobal(js []byte) (string, int, error) {
	isWrapper := newWrapperChecker(js)

	for _, p := range sigFunctionPatterns {
		m := p.regex.FindSubmatch(js)
		if len(m) <= p.sigIdx {
			continue
		}
		funcName := strings.TrimSpace(string(m[p.sigIdx]))
		if !isValidIdentifier(funcName) {
			continue
		}
		if isWrapper(funcName) {
			continue
		}
		param := 0
		if p.paramIdx > 0 && len(m) > p.paramIdx {
			numStr := strings.TrimSpace(string(m[p.paramIdx]))
			if isNumericStr(numStr) {
				param, _ = strconv.Atoi(numStr)
			}
		}
		return funcName, param, nil
	}

	return "", 0, errors.New("could not find signature function name")
}

// findSignatureTimestamp extracts the signatureTimestamp (STS) from player.js
// This is required for the player API to return premium formats
func findSignatureTimestamp(js []byte) int {

	pattern := regexp.MustCompile(`["\']?signatureTimestamp["\']?\s*[=:]\s*(\d+)`)
	match := pattern.FindSubmatch(js)
	if len(match) >= 2 {
		sts, _ := strconv.Atoi(string(match[1]))
		return sts
	}

	pattern2 := regexp.MustCompile(`\bsts\s*=\s*(\d{4,6})\b`)
	match = pattern2.FindSubmatch(js)
	if len(match) >= 2 {
		sts, _ := strconv.Atoi(string(match[1]))
		return sts
	}

	return 0
}

func isValidIdentifier(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}

	switch s {
	case "function", "return", "var", "let", "const", "if", "for", "while", "do", "switch", "case", "default", "new", "this", "class", "extends", "try", "catch", "finally", "throw", "typeof", "void", "delete", "in", "instanceof", "null", "true", "false", "undefined":
		return false
	}

	for i, c := range s {
		if i == 0 {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '$') {
				return false
			}
		} else {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$') {
				return false
			}
		}
	}
	return true
}

func isNumericStr(s string) bool {
	if len(s) == 0 || len(s) > 5 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func escapeJSString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "\\'")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	return s
}
