package ytx

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var nTransformIndexPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\.get\(["']n["']\)\)&&\(b=([a-zA-Z0-9$_]+)\[(\d+)\]`),
	regexp.MustCompile(`([a-zA-Z0-9$_]+)\[(\d+)\]\(\s*[a-zA-Z0-9$_]+\.get\(["']n["']\)\s*\)`),
	regexp.MustCompile(`\.get\(["']n["']\)[^\n]{0,120}?\b([a-zA-Z0-9$_]+)\[(\d+)\]`),
}

var nTransformDirectPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bb\s*=\s*([a-zA-Z0-9$_]+)(?:\.call\([^,]+,\s*|\()\s*[a-zA-Z0-9$_]+\.get\(["']n["']\)`),
	regexp.MustCompile(`\bb\s*=\s*([a-zA-Z0-9$_]+)\s*\(\s*[a-zA-Z0-9$_]+\.get\(["']n["']\)`),
}

var nTransformLegacyPattern = regexp.MustCompile(`(?:var|let|const)?\s*([a-zA-Z0-9$_]{2,})\s*=\s*\[([a-zA-Z0-9$_]{2,})\]`)

func findNFunctionName(js []byte) string {
	if name := findNFunctionNameWindowed(js); name != "" {
		return name
	}

	return findNFunctionNameGlobal(js)
}

// findNFunctionNameWindowed uses marker-based windowing for fast extraction.
func findNFunctionNameWindowed(js []byte) string {
	markers := [][]byte{[]byte(`get("n")`), []byte(`get('n')`)}

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

			for _, p := range nTransformIndexPatterns {
				matches := p.FindAllSubmatch(chunk, -1)
				for _, m := range matches {
					if len(m) < 3 {
						continue
					}
					arrName := string(m[1])
					idx, err := strconv.Atoi(string(m[2]))
					if err != nil {
						continue
					}
					if fn := resolveArrayFunctionName(js, arrName, idx); fn != "" {
						return fn
					}
				}
			}

			for _, p := range nTransformDirectPatterns {
				m := p.FindSubmatch(chunk)
				if len(m) >= 2 {
					name := string(m[1])
					if isValidIdentifier(name) {
						return name
					}
				}
			}
		}
	}
	return ""
}

// findNFunctionNameGlobal is the slow but robust global scan fallback.
func findNFunctionNameGlobal(js []byte) string {
	for _, p := range nTransformIndexPatterns {
		matches := p.FindAllSubmatch(js, -1)
		for _, m := range matches {
			if len(m) < 3 {
				continue
			}
			arrName := string(m[1])
			idx, err := strconv.Atoi(string(m[2]))
			if err != nil {
				continue
			}
			if fn := resolveArrayFunctionName(js, arrName, idx); fn != "" {
				return fn
			}
		}
	}

	for _, p := range nTransformDirectPatterns {
		m := p.FindSubmatch(js)
		if len(m) >= 2 {
			name := string(m[1])
			if isValidIdentifier(name) {
				return name
			}
		}
	}

	legacy := nTransformLegacyPattern.FindSubmatch(js)
	if len(legacy) >= 3 && isValidIdentifier(string(legacy[2])) {
		return string(legacy[2])
	}

	return ""
}

func resolveArrayFunctionName(js []byte, arrName string, idx int) string {
	if idx < 0 {
		return ""
	}

	arrayPattern := regexp.MustCompile(fmt.Sprintf(`(?:var|let|const)\s+%s\s*=\s*\[([^\]]+)\]|\b%s\s*=\s*\[([^\]]+)\]`, regexp.QuoteMeta(arrName), regexp.QuoteMeta(arrName)))
	match := arrayPattern.FindSubmatch(js)
	if len(match) < 2 {
		return ""
	}

	arrayText := ""
	if len(match) > 1 && len(match[1]) > 0 {
		arrayText = string(match[1])
	} else if len(match) > 2 {
		arrayText = string(match[2])
	}
	elements := strings.Split(arrayText, ",")
	if idx >= len(elements) {
		return ""
	}

	candidate := strings.TrimSpace(elements[idx])
	candidate = strings.Trim(candidate, "'\"")
	if !isValidIdentifier(candidate) {
		return ""
	}
	return candidate
}
