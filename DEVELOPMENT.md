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
│   ├── cipher.go      # Signature/n-transform via QuickJS (wrapper mode)
│   ├── jsengine.go    # JS engine interface (Bun/Node)
│   ├── jsrunner.go    # Subprocess-based JS runner (Bun/Node)
│   ├── quickjsrunner.go # QuickJS-based runner (in-process, no subprocess)
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
./ytx music VIDEO_ID --js-engine auto     # Auto: Bun → Node (default)
```

### Performance Optimizations (Latest)

1. **FCIS Tiered Architecture** — Fallback ladder of regex window/full-file scanning drastically dropping regex analysis (~4.2s → ~100ms)
2. **QuickJS bytecode caching** — Evaluates wrapper/URL functions as precompiled `libquickjs` bytecode rather than raw JS strings, dropping warmup from 500ms down to near 0ms in successive runs.
3. **Decoupled API/Cipher fetching** — Signals STS incredibly early allowing asynchronous HTTP fetches to completely lap the JS evaluations.
4. **Cache base.js URL path + payloads** — Skips embed page fetch AND HTTP player fetch on warm starts (100% network bypass).
5. **Send player.js via file path** — Writes to temp file instead of 1.5MB IPC transfer for Botguard `bun` challenges.
6. **Batch n-transform in bulk mode** — Single IPC call for all n-parameters
7. **HTTP/2 connection pooling** — Reuses connections for bulk requests
8. **Global visitorData & POToken cache** — Shared across extractions to seamlessly bypass repetitive 4-second Botguard challenges.

See `docs/architecture/extraction-pipeline.md` for architecture details and the tiered fallback flowchart.

### Profiling

```bash
# Enable timing breakdown
./ytx music VIDEO_ID --profile

# Output includes comprehensive FCIS diagnostics (in .timings):
{
  "timings": {
    "visitor_data_ms": 260,
    "player_api_ms": 290,
    "cipher_init_ms": 700,
    "n_transform_ms": 5,
    "total_ms": 900,
    "js_engine": "bun",
    "cipher_detail": {
      "sig_tier": "global_fallback",
      "wrapper_tier": "windowed",
      "n_func_tier": "skipped_wrapper",
      "player_js_bytes": 1666797,
      "marker_miss": [
        "set(\"alr\"",
        "sig_patterns"
      ]
    }
  }
}
```

## Testing

```bash
# Video mode (no auth needed)
time ./ytx video dQw4w9WgXcQ --profile

# Video mode with subtitles
./ytx video dQw4w9WgXcQ --subs                # Default: en
./ytx video dQw4w9WgXcQ --sub-langs all       # All languages
./ytx video dQw4w9WgXcQ --sub-langs en,es     # Specific languages

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

# Opt-in live probe tests (single and bulk music mode)
make test-music-probe

# Cache management
./ytx cache purge  # Clear all cache for fresh benchmark
```

Live probe tests are intentionally opt-in because they hit YouTube stream URLs directly.
Use `make test-music-probe` to run both the single-track probe and the 2-ID bulk probe.

## Performance Benchmarks

| Operation | Cold Start | Warm (disk) | Warm (memory) |
|-----------|------------|-------------|---------------|
| Video mode | ~400ms | ~250ms | ~250ms |
| Music mode | **~1.4s** | ~400ms | ~400ms |
| PO mode (Botguard) | ~4.6s | ~330ms | ~330ms |
| Bulk 5 tracks | ~700ms | ~500ms | ~500ms |
| n-transform | ~500ms (bootstrap) | < 1ms | < 1ms |

### Throughput (Bulk Mode)
- **7+ videos/sec** with warm cache
- All URLs verified HTTP 200 OK

## When YouTube Breaks Things

For the step-by-step operational workflow, use `docs/extractor-breakage-playbook.md`.

### Quick Diagnosis

```bash
# 1. Check if yt-dlp works (baseline)
yt-dlp -f 141 -g "https://music.youtube.com/watch?v=hbl2Cuw75oE"

# 2. Check if ytx gets the URL but it returns 403
./ytx music hbl2Cuw75oE | jq -r '.url' | xargs curl -sI | head -1
# If "HTTP 403" -> cipher/n-transform is broken
# If "HTTP 200" -> working correctly

# 3. Compare n parameter between ytx and yt-dlp
YTX_N=$(./ytx music hbl2Cuw75oE | jq -r '.url' | tr '&' '\n' | grep '^n=' | cut -d= -f2)
YTDLP_N=$(yt-dlp -f 141 -g "https://music.youtube.com/watch?v=hbl2Cuw75oE" | tr '&' '\n' | grep '^n=' | cut -d= -f2)
echo "ytx n: $YTX_N"
echo "yt-dlp n: $YTDLP_N"
# If different -> n-function is wrong (most common issue)
```

### Root Cause: Missing signatureTimestamp (Dec 2025 Incident)

**Symptom:** ytx returns itag=140 (128kbps) instead of itag=141 (256kbps) even with Premium.

**Debugging approach:** Build isolated POCs to test each function in the extraction pipeline:
```
F0: HTML → visitorData
F1: player.js → signatureTimestamp
F2: API(visitorData, STS) → itag list  ← Problem was here
F3: DecryptSignature(s) → sig
F4: TransformN(n) → n'
```

**Root cause:** The `signatureTimestamp` field was missing from the API request body.

**Fix:** 
1. Extract STS from player.js: `findSignatureTimestamp()` in `pkg/cipher.go`
2. Include in API request: `SignatureTimestamp` field in `ContentPlaybackContext`
3. Wait for cipher before API call (cipher contains STS)

**Files changed:** `pkg/types.go`, `pkg/cipher.go`, `pkg/cache.go`, `pkg/extractor.go`

### Root Cause: Decoy Functions (Dec 2024 Incident)

YouTube adds **decoy wrapper functions** to break regex-based scrapers:

```javascript
// DECOY (what regex finds):
klj=function(S){return MP[z[7]](this,19,S)};  // Just a wrapper!

// REAL (what we need):
realFunc=function(S){var W=S.split("");try{...}catch(e){...}...}
```

**Solution:** Use yt-dlp's AST-based solver which validates function body structure.

### yt-dlp Reference Files

When yt-dlp releases a fix, check these files:

| File | Purpose |
|------|---------|
| `yt_dlp/extractor/youtube/jsc/_builtin/vendor/yt.solver.core.js` | AST-based function finder (meriyah parser) |
| `yt_dlp/extractor/youtube/_video.py:3400-3460` | Challenge solving in `_extract_formats_and_subtitles` |
| `yt_dlp/extractor/youtube/_video.py:2183-2215` | `_extract_signature_timestamp` - sts extraction |
| `yt_dlp/extractor/youtube/_video.py:2632-2650` | `_generate_player_context` - API request body |
| `yt_dlp/extractor/youtube/_base.py` | Client configs, headers, SAPISIDHASH |

### Testing with yt-dlp's Solver (POC)

```bash
# Run the POC to test yt-dlp's solver directly
bun poc_solver.mjs

# This uses yt-dlp's yt.solver.core.js to:
# 1. Parse player.js with meriyah AST parser
# 2. Find correct n-function and sig-function
# 3. Transform challenge values
```

If signature decryption fails, the issue is likely in `pkg/cipher_detect.go`. The system uses an FCIS (Functional Core/Imperative Shell) tiered fallback:

1. **Tier 1 (Fast Windowed Regex):** Uses `detectFastSignature` to scan an 8KB window of `player.js` for well-known structural clues (e.g. `encodeURIComponent(`).
2. **Tier 2 (Wrapper AST/Regex):** Uses `detectWrapperSignature` if Tier 1 misses.
3. **Tier 3 (Global Fallback):** Uses `detectGlobalSignature` to scan the full 1.66MB file using heavy recursive regex. This protects the pipeline against minification variable changes.

If YouTube completely overhauls the AST, we will see `detail.SigTier = "not_found"` in the `--profile` diagnostic output. Adjust the window markers (`sigFunctionPatterns`) in `cipher_detect.go`.

### Client Version Updates

Update `pkg/constants.go`:

```go
ClientVersion = "1.20251216.01.00"
```

### Diagnose Tool

When extraction breaks, use the built-in diagnostic tool to compare ytx vs yt-dlp:

```bash
go run cmd/diagnose/main.go VIDEO_ID
```

This tool:
1. Checks cookie/premium status
2. Runs yt-dlp as baseline
3. Runs ytx extraction
4. Tests API directly with signatureTimestamp
5. Verifies both URLs return HTTP 200

Example output:
```
[0] Cookie status:
    PREMIUM ✓ (authenticated with YouTube Premium)

[1] yt-dlp baseline (format 141):
    itag=141, n=KhRv-G0hELcWgw, sig=AJfQdSswRAIgcktRP4BM...

[2] ytx extraction:
    itag=141, bitrate=258412
    n=lfv_DeEHzIQxmA, sig=AJfQdSswRAIgCBF-drC3...

[3] Direct API test (with signatureTimestamp):
    signatureTimestamp: 20438
    Audio itags: 140 141 249 250 251 774 ✓

[4] URL verification:
    yt-dlp URL: HTTP 200
    ytx URL:    HTTP 200

DIAGNOSIS:
  Both return itag 141 - extraction working
```

### Clear Cache and Retry

```bash
rm ~/.cache/ytx/cipher.json
./ytx music VIDEO_ID
```

## Dependencies

- `github.com/buke/quickjs-go` - QuickJS runtime for wrapper-mode sig/n (ES2020+ required)
- `github.com/dop251/goja` - Pure Go JavaScript interpreter (legacy sig extraction, AST parsing)
- `github.com/dop251/goja/parser` - JS AST parsing

### External (for n-transform, fallback)
- `bun` - Preferred JS runtime (fastest subprocess)
- `node` - Fallback JS runtime
