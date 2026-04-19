package jsengine

import (
	"fmt"
	"strings"
)

// EngineType represents the JS engine to use for n-parameter transformation
type EngineType string

const (
	EngineAuto    EngineType = "auto"
	EngineQuickJS EngineType = "quickjs"
	EngineBun     EngineType = "bun"
	EngineNode    EngineType = "node"
)

// ParseEngineType parses a string into EngineType
func ParseEngineType(s string) (EngineType, error) {
	switch s {
	case "auto", "":
		return EngineAuto, nil
	case "quickjs":
		return EngineQuickJS, nil
	case "bun":
		return EngineBun, nil
	case "node":
		return EngineNode, nil
	default:
		return EngineAuto, fmt.Errorf("unknown engine type: %s (use: auto, quickjs, bun, node)", s)
	}
}

// JSEngine is the interface for JavaScript execution engines
type JSEngine interface {
	// TransformN transforms the n-parameter value
	TransformN(n string) (string, error)
	// TransformNBatch transforms multiple n-parameter values at once (optimization for bulk)
	TransformNBatch(nValues []string) ([]string, error)
	// Close releases resources
	Close()
	// Name returns the engine name for logging
	Name() string
}

// ValidateNTransformResult checks if the n-transform result is valid
// Returns the validated result and an error if invalid
func ValidateNTransformResult(original, transformed string) (string, error) {
	transformed = strings.Trim(transformed, "\"")
	if transformed == "" {
		return original, fmt.Errorf("n-transform returned empty result")
	}
	if transformed == original {
		return original, fmt.Errorf("n-transform returned unchanged value")
	}
	// _w8_ indicates throttled/failed transform
	if strings.Contains(transformed, "_w8_") {
		return original, fmt.Errorf("n-transform returned throttled marker: %s", transformed)
	}
	// "error" in result indicates JS error
	if strings.Contains(transformed, "error") {
		return original, fmt.Errorf("n-transform returned error: %s", transformed)
	}
	return transformed, nil
}
