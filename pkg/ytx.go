// Package ytx provides YouTube stream URL extraction functionality.
//
// YTX is a fast, lightweight Go CLI tool for extracting YouTube video and audio
// stream URLs. It supports both video mode (no authentication) and music mode
// (requires YouTube Premium cookies).
//
// Example usage:
//
//	// Video mode - no authentication required
//	extractor, _ := NewExtractor(ModeVideo, "")
//	result, _ := extractor.ExtractVideo("dQw4w9WgXcQ")
//
//	// Music mode - requires cookies
//	extractor, _ := NewExtractor(ModeMusic, "cookies.txt")
//	result, _ := extractor.Extract("dQw4w9WgXcQ")
package ytx

// Public API - these types are re-exported from internal packages
// This file serves as the main package interface
