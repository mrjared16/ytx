# Development Guide

## Project Structure

```
ytx/
├── cmd/
│   └── ytx/           # CLI entry point, argument parsing
├── pkg/               # Core packages
│   ├── client.go      # Client configuration factory (ANDROID_VR vs WEB_MUSIC)
│   ├── extractor.go   # Core extraction logic (API calls, stream selection)
│   ├── bulk.go        # Pipelined bulk extraction with rate limiting
│   ├── cipher.go      # Signature decryption (music mode only)
│   ├── auth.go        # SAPISIDHASH authentication for premium access
│   ├── constants.go   # Client constants, API endpoints, itag priorities
│   ├── types.go       # Data structures for requests/responses
│   ├── cache.go       # Persistent cipher cache
│   └── ytx.go         # Public API exports
├── internal/          # Private packages
└── docs/             # Documentation
```

## Building

```bash
make build
ytx video dQw4w9WgXcQ
ytx music dQw4w9WgXcQ --cookies cookie.txt
```

## Architecture

### Dual-Client Design

| Mode | Client | Cipher | Auth | Speed | Quality |
|------|--------|--------|------|-------|---------|
| video | ANDROID_VR | No | No | ~220ms | 1080p video + audio |
| music | WEB_MUSIC | Yes | SAPISIDHASH | ~300ms (cached) | 256kbps audio |

### Performance Optimizations

1. **File-based cipher cache** - Persists across CLI invocations (~/.cache/ytx/)
2. **Pre-compiled JS** - goja programs compiled once, reused
3. **Bucket-based batching** - 10 tracks/batch with 200ms inter-batch delay
4. **Dynamic dependency discovery** - Self-healing when YouTube renames variables

### Self-Healing Cipher

The cipher extraction uses AST-based dynamic dependency discovery:

```go
// Old (fragile):
knownDeps := []string{"z", "zj", "p6"}  // Breaks when renamed

// New (self-healing):
for dep := range discoveredDeps {
    if !isBuiltin(dep) {
        extractDependency(dep)
    }
}
```

This means the tool automatically adapts when YouTube changes variable names.

## Testing

```bash
# Video mode (no auth needed)
time ytx video dQw4w9WgXcQ

# Music mode (needs Premium cookies)
time ytx music dQw4w9WgXcQ --cookies cookie.txt

# Bulk mode
time ytx music --bulk id1,id2,id3 --cookies cookie.txt

# Check cache
cat ~/.cache/ytx/cipher.json | jq .sig_function
```

## When YouTube Breaks Things

### Cipher Function Pattern Changes

If signature decryption fails, check `pkg/cipher.go:findSigFunctionName()`. The marker pattern may need updating:

```go
marker := []byte(",decodeURIComponent(")
```

### Client Version Updates

Update `pkg/constants.go`:

```go
ClientVersion = "1.20251216.01.00"
```

### New Dependencies

The dynamic discovery should handle this automatically. If not, check the builtins filter in `pkg/cipher.go:extractWithAST()`.

## Performance Benchmarks

| Operation | Cold Cache | Warm Cache |
|-----------|------------|------------|
| Video mode | ~220ms | ~220ms |
| Music mode | ~1800ms | ~300ms |
| Bulk 10 tracks | - | ~600ms |
| Bulk 50 tracks | - | ~3s |

## Dependencies

- `github.com/dop251/goja` - Pure Go JavaScript interpreter
- `github.com/dop251/goja/parser` - JS AST parsing