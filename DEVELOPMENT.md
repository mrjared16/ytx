# Development Guide

## Project Structure

```
ytx/
├── cmd/
│   └── ytx/           # CLI entry point, argument parsing
├── pkg/               # Core packages
│   ├── client.go      # Client configuration factory (ANDROID_VR vs WEB_MUSIC)
│   ├── extractor.go   # Core extraction logic (API calls, stream selection)
│   ├── bulk.go        # Optimized bulk extraction with batch n-transform
│   ├── cipher.go      # Signature decryption (music mode only)
│   ├── jsengine.go    # JS engine interface (Bun/Node/QuickJS)
│   ├── jsrunner.go    # Subprocess-based JS runner (Bun/Node)
│   ├── jsrunner_quickjs.go  # Embedded QuickJS fallback
│   ├── nrunner.mjs    # Node/Bun script for n-transform
│   ├── auth.go        # SAPISIDHASH authentication for premium access
│   ├── constants.go   # Client constants, API endpoints, itag priorities
│   ├── types.go       # Data structures for requests/responses
│   ├── cache.go       # Persistent cipher cache with base.js path caching
│   └── ytx.go         # Public API exports
├── docs/             # Documentation
└── dev/              # Development notes (can be deleted)
```

## Building

```bash
go build -o ytx ./cmd/ytx
./ytx video dQw4w9WgXcQ
./ytx music dQw4w9WgXcQ --cookies cookie.txt
```

## Architecture

### Dual-Client Design

| Mode | Client | Cipher | Auth | Speed | Quality |
|------|--------|--------|------|-------|---------|
| video | ANDROID_VR | No | No | ~400ms | 1080p video + audio |
| music | WEB_MUSIC | Yes | SAPISIDHASH | ~400-900ms | 256kbps audio |

### JS Engine Selection

```bash
./ytx music VIDEO_ID --js-engine bun      # Force Bun (fastest)
./ytx music VIDEO_ID --js-engine node     # Force Node.js
./ytx music VIDEO_ID --js-engine quickjs  # Force embedded QuickJS
./ytx music VIDEO_ID --js-engine auto     # Auto: Bun → Node → QuickJS
```

### Performance Optimizations (Latest)

1. **Pre-warm JS engine during cipher fetch** - Starts Bun subprocess while API call is in flight
2. **Cache base.js URL path** - Skips embed page fetch on warm starts (~150ms savings)
3. **Send player.js via file path** - Writes to temp file instead of 1.5MB IPC transfer
4. **Batch n-transform in bulk mode** - Single IPC call for all n-parameters
5. **HTTP/2 connection pooling** - Reuses connections for bulk requests
6. **Global visitorData cache** - 30-minute TTL, shared across extractions

### Profiling

```bash
# Enable timing breakdown
./ytx music VIDEO_ID --profile

# Output includes:
{
  "timings": {
    "visitor_data_ms": 260,
    "player_api_ms": 290,
    "cipher_init_ms": 700,
    "n_transform_ms": 5,
    "total_ms": 900,
    "js_engine": "bun"
  }
}
```

## Testing

```bash
# Video mode (no auth needed)
time ./ytx video dQw4w9WgXcQ --profile

# Music mode (needs Premium cookies)
time ./ytx music dQw4w9WgXcQ --profile

# Bulk mode
time ./ytx music --bulk id1,id2,id3,id4,id5

# Verify URL works
./ytx music dQw4w9WgXcQ | jq -r '.url' | xargs curl -sI | head -3

# Run regression tests
go test -v ./pkg/... -run TestExtractorRegression

# Update golden image
UPDATE_GOLDEN=1 go test -v ./pkg/... -run TestExtractorRegression

# Cache management
./ytx cache purge  # Clear all cache for fresh benchmark
```

## Performance Benchmarks

| Operation | Cold Start | Warm Start |
|-----------|------------|------------|
| Video mode | ~400ms | ~400ms |
| Music mode | ~900ms | ~400ms |
| Bulk 5 tracks | ~700ms | ~500ms |
| n-transform | ~5ms | ~1ms |

### Throughput (Bulk Mode)
- **7+ videos/sec** with warm cache
- All URLs verified HTTP 200 OK

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

## Dependencies

- `github.com/dop251/goja` - Pure Go JavaScript interpreter (signature decryption)
- `github.com/dop251/goja/parser` - JS AST parsing
- `github.com/buke/quickjs-go` - Embedded QuickJS (n-transform fallback)

### External (optional, for faster n-transform)
- `bun` - Preferred JS runtime (2x faster than Node)
- `node` - Fallback JS runtime
