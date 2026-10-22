# Development Guide

## Project Structure

```
ytx/
├── cmd/
│   ├── ytx/           # CLI entry point, argument parsing
│   └── diagnose/      # ytx vs yt-dlp diagnostic tool
├── internal/
│   ├── cache/         # Versioned persistent cache (models, persistence)
│   ├── jsengine/      # JS engine interface, Bun/Node subprocess, legacy QuickJS
│   └── jsextract/     # Player/wrapper code extraction helpers
├── pkg/               # Core packages
│   ├── client.go      # Video native client chain + mode config factory
│   ├── extractor*.go  # Extraction flow, API calls, cipher integration, PO lane
│   ├── bulk.go        # Sequential bulk extraction with shared warmup and jitter
│   ├── cipher*.go     # Detection, URL-wrapper protocol, signature/n handling
│   ├── runtime*.go    # Engine selection, caching, modern runner lifecycle
│   ├── potrunner.go   # One Bun/Node subprocess per PO mint
│   ├── po_minter.mjs  # Embedded PO minter script
│   ├── auth.go        # SAPISIDHASH authentication for premium access
│   ├── constants.go   # Client constants, API endpoints, itag priorities
│   ├── types.go       # Data structures for requests/responses
│   ├── cache.go       # Public cache management helpers
│   └── ytx.go         # Public API exports
├── docs/              # Documentation
└── Makefile           # Canonical build/test/benchmark/cache commands
```

## Building

```bash
go build -o ytx ./cmd/ytx
./ytx video dQw4w9WgXcQ
./ytx music dQw4w9WgXcQ --cookies cookie.txt
```

## Architecture

### Dual-Client Design

| Mode | Client | Cipher/JS | Auth | Quality |
|------|--------|-----------|------|---------|
| video | Native chain: VISIONOS → ANDROID_VR → ANDROID → IOS | None (direct ranges + near-end probes) | No (guest-only) | Highest direct adaptive video + audio |
| music | WEB_REMIX | Yes (modern URL wrapper or legacy cipher) | SAPISIDHASH | Highest valid audio for the account/track |

Video mode is guest-only and returns direct-URL adaptive streams. It probes the final byte (after a 0-0 discovery range when the length is unknown) before accepting a candidate; that verifies range readability, not a decode. Music mode uses `WEB_REMIX`'s endpoint/key and the cipher pipeline.

### JS Engine Selection

```bash
./ytx music VIDEO_ID --js-engine bun      # Force Bun for the compatible path
./ytx music VIDEO_ID --js-engine node     # Force Node.js
./ytx music VIDEO_ID --js-engine quickjs  # Legacy-only embedded engine
./ytx music VIDEO_ID --js-engine auto     # Default: Bun → Node → QuickJS (legacy n-transform)
```

Modern URL-wrapper players require Bun or Node. `--js-engine quickjs` is rejected for those players; the modern path is fail-closed rather than silently falling back. For legacy players, engine selection applies to the n-transform: `auto` tries Bun first, then Node, then embedded QuickJS. Legacy signature decryption always uses the embedded QuickJS wrapper context.

### Performance Optimizations

1. **FCIS Tiered Architecture** — bounded window detection, then wrapper/global legacy fallbacks; modern URL-wrapper recognition runs before the legacy tiers.
2. **Modern wrapper transform** — one Bun/Node subprocess call produces signature, n, and `alr`, avoiding separate legacy decrypt/transform loops.
3. **Early STS + overlapping fetch** — STS is signalled as soon as the cipher is available so the API call can overlap remaining runtime preparation.
4. **Persistent identity-checked cache** — cipher metadata plus player/n-runtime artifacts, with full-content fingerprint and STS validation when a cached player bundle is present (bundle-less legacy caches keep their legacy gates).
5. **Player bundle over file path** — the subprocess receives a prepared file instead of a large IPC payload.
6. **Sequential bulk extraction** — one shared warmup, then per-ID extraction with a randomized inter-track delay. There is no batched n-transform in the bulk path.
7. **HTTP connection reuse** — shared clients/pooling for API and player requests.
8. **Visitor and PO token caches** — reuse short-lived bootstrap state; PO minting itself is not cached as a running process.

See `docs/architecture/extraction-pipeline.md` for architecture details and the tiered fallback flowchart.

### Profiling

```bash
# Enable timing breakdown
./ytx music VIDEO_ID --profile

# Output includes FCIS diagnostics (in .timings). Values below are illustrative,
# not expected timings or performance guarantees. This is a modern URL-wrapper
# example: n_transform_ms is legacy-only and not instrumented on this path; the
# unaccounted remainder is shown as other_ms. The displayed fields sum to total_ms.
{
  "timings": {
    "visitor_data_ms": 260,
    "player_api_ms": 290,
    "cipher_init_ms": 700,
    "other_ms": 50,
    "total_ms": 1300,
    "js_engine": "bun",
    "cipher_detail": {
      "sig_tier": "urlwrapper",
      "wrapper_tier": "protocol",
      "n_func_tier": "skipped_protocol",
      "player_js_bytes": 1666797
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

# Music mode (requires authenticated cookies; Premium may unlock higher formats)
time ./ytx music dQw4w9WgXcQ --profile

# Bulk mode
time ./ytx music --bulk id1,id2,id3,id4,id5

# Verify URL works
./ytx music dQw4w9WgXcQ | jq -r '.url' | xargs curl -sI | head -3

# Run the broad package suite (includes live regression coverage)
make test

# Focused offline unit checks (exact names; discover more with go test -list)
go test -v ./pkg -run '^TestFindSigFunctionName$' -count=1
go test -v ./internal/cache -run '^TestLoadVisitorDataAllowStaleWithinGrace$' -count=1

# Regression target: live, golden comparison, hard timing bound
make test-regression

# Update the golden image only after deliberately validating a behavior change
make test-regression-update

# Opt-in live probe tests (single and bulk music mode)
make test-music-probe

# Cache management
./ytx cache purge  # Remove cipher/player/PO artifacts; visitor.json is retained
```

`make test` runs all tests in `./pkg`, including the live `TestExtractorRegression`; it is not an offline-only target. Music subcases skip without cookies, and a live 403 can fail the run. `make test-regression` compares correctness and fails whenever `total_ms` exceeds the golden baseline by more than 300ms; it has no warning-only mode. `make test-regression-perf` sets `STRICT_REGRESSION=1`, but that variable is not read by the Go tests, so it is not a separate gate.

Live probe tests are intentionally opt-in because they hit YouTube stream URLs directly.
Use `make test-music-probe` to run both the single-track probes and the two-ID bulk probes. See `docs/guides/testing.md` for exact semantics.

## Performance Benchmarks

Do not use a fixed table here as a current promise. Measure the build, cache state, mode, runtime, and network in front of you. `make bench-bulk` runs one cold and three warm extractions of a single `TEST_VIDEO`; it is not a 50-track throughput test. `make bench` runs the package benchmarks with `-benchtime=3x`, but because it does not set `-run`, it also runs the package tests (including the live `TestExtractorRegression`) first.

For measurement guidance and historical context, see `docs/guides/benchmarking.md` and the dated historical tables in `docs/architecture/extraction-pipeline.md`.

### Bulk behavior

- Extraction is sequential per video ID.
- The first ID may start a shared visitor/cipher/n-runtime warmup in the background.
- Between non-final IDs, `pkg/bulk.go` sleeps a randomized 50–250ms (`150ms ± 100ms`).
- There is no fixed videos-per-second claim and no batched n-transform in the bulk path.

## When YouTube Breaks Things

For the step-by-step operational workflow, use `docs/extractor-breakage-playbook.md`.

### Quick Diagnosis

```bash
# 1. Check if yt-dlp works (baseline; 141 is not offered for every track/account)
yt-dlp -f bestaudio/best -g "https://music.youtube.com/watch?v=hbl2Cuw75oE"

# 2. Check if ytx gets the URL but it returns 403
./ytx music hbl2Cuw75oE | jq -r '.url' | xargs curl -sI | head -1
# "HTTP 403" can be signature/n, a PO requirement, cookies, or YouTube-side blocking.
# "HTTP 200" means at least the probed request was accepted.

# 3. Compare n parameter as a lead, not as a pass/fail test
YTX_N=$(./ytx music hbl2Cuw75oE | jq -r '.url' | tr '&' '\n' | grep '^n=' | cut -d= -f2)
YTDLP_N=$(yt-dlp -f bestaudio/best -g "https://music.youtube.com/watch?v=hbl2Cuw75oE" | tr '&' '\n' | grep '^n=' | cut -d= -f2)
echo "ytx n: $YTX_N"
echo "yt-dlp n: $YTDLP_N"
# Independently obtained values can differ without proving the decoder is
# wrong. Decode equivalence requires the same player and the same input run
# through a known-valid independent oracle; matching range reachability is
# not decode proof.
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
3. Gate the API call on a positive STS from the player/cipher (the current pipeline can signal STS before full cipher initialization)

**Files changed:** `pkg/types.go`, `pkg/cipher.go`, `pkg/cache.go`, `pkg/extractor.go`

### Root Cause: Decoy Functions (Dec 2024 Incident)

YouTube adds **decoy wrapper functions** to break regex-based scrapers:

```javascript
// DECOY (what regex finds):
klj=function(S){return MP[z[7]](this,19,S)};  // Just a wrapper!

// REAL (what we need):
realFunc=function(S){var W=S.split("");try{...}catch(e){...}...}
```

**Historical response:** adopt yt-dlp-style AST/structural validation instead of accepting the first matching wrapper. Current detection validates structure in `pkg/cipher_detect.go` and `internal/jsextract`; modern URL wrappers are recognized by bounded markers.

### yt-dlp Reference Files

When yt-dlp releases a fix, check these files:

| File | Purpose |
|------|---------|
| `yt_dlp/extractor/youtube/jsc/_builtin/vendor/yt.solver.core.js` | AST-based function finder (meriyah parser) |
| `yt_dlp/extractor/youtube/_video.py:3400-3460` | Challenge solving in `_extract_formats_and_subtitles` |
| `yt_dlp/extractor/youtube/_video.py:2183-2215` | `_extract_signature_timestamp` - sts extraction |
| `yt_dlp/extractor/youtube/_video.py:2632-2650` | `_generate_player_context` - API request body |
| `yt_dlp/extractor/youtube/_base.py` | Client configs, headers, SAPISIDHASH |

### Comparing with yt-dlp

The repository does not ship a standalone solver POC script. Use the shipped diagnostics instead:

```bash
# Side-by-side ytx vs yt-dlp, including direct API and URL probes
make diagnose DEBUG_VIDEO=dQw4w9WgXcQ

# Probe the extracted URL with an HTTP Range request
make probe-url DEBUG_MODE=music DEBUG_VIDEO=dQw4w9WgXcQ

# Full-stream download check (distinguishes blocking from throttling)
make probe-download DEBUG_MODE=music DEBUG_VIDEO=dQw4w9WgXcQ
```

> **Signed URLs:** both `probe-url` and `probe-download` echo the full extracted
> URL (`URL: ...`). That URL is a short-lived credential. Redact it before
> pasting logs, issues, or retained files; use `https://...` placeholders in
> anything public. The commands do not print cookies.

For the underlying comparison, the diagnostic runs `yt-dlp -f 141 -g` and reports both tools' `sig`/`n` values. Format 141 must actually be offered for the track/account; a failure there is not a universal baseline. Read the output before changing detection code.

If signature decryption fails, the cause depends on the path:

- **Modern URL wrapper**: inspect wrapper recognition/validation and Bun/Node availability. Ambiguous wrappers are fail-closed and do not use legacy QuickJS.
- **Legacy**: inspect the FCIS ladder in `pkg/cipher_detect.go` and the wrapper/signature extraction helpers. A `global_fallback` tier means Tier 1 missed.

The legacy ladder is:

1. **Tier 1 (Fast Windowed Regex):** `detectFastSignature` scans a small window of `player.js` for structural clues.
2. **Tier 2 (Wrapper AST/Regex):** `detectWrapperSignature` runs when Tier 1 misses.
3. **Tier 3 (Global Fallback):** `detectGlobalSignature` scans the full file. This protects against minification variable changes.

If YouTube completely overhauls the player shape, extraction fails with an error instead of a profile: `--profile` output is emitted only on a successful extraction, and the CLI prints a JSON error without a profile on failure. Inspect the extraction error, especially signature detection failures or modern wrapper ambiguity, then adjust the relevant bounded markers/validation rather than adding a broad fallback.

### Client Version Updates

Update `pkg/constants.go`:

```go
ClientVersion = "1.20260114.03.00"
```

Use the version currently observed on `music.youtube.com`; the checked-in value is the source of truth.

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

Example output (illustrative; values and extraction state will differ):
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

Use the cache command so all cipher/player/runtime artifacts are removed:

```bash
./ytx cache purge
./ytx music VIDEO_ID --profile
```

`cache purge` preserves `visitor.json`. Deleting only `cipher.json` from the cache directory leaves player/n-runtime artifacts behind and is not a full reset.

## Dependencies

- `github.com/buke/quickjs-go` — embedded QuickJS runtime for the **legacy** path and legacy wrapper bytecode (CGO)
- `github.com/dop251/goja` — pure-Go JavaScript parser/interpreter used by legacy extraction helpers (e.g. `internal/jsextract`)
- `github.com/dop251/goja/parser` — JS AST parsing

### External runtimes
- `bun` — preferred Bun/Node runtime for the modern URL-wrapper path and legacy subprocess path
- `node` — fallback subprocess runtime. The subprocess lookup also accepts the
  `nodejs` binary name after `node` is missing (explicit `--js-engine node` and
  the modern URL-wrapper path); legacy `auto` checks `node` only.
- PO mode additionally needs `jsdom` and `bgutils-js` resolvable by the chosen runtime. They are **not** vendored, and the repository has no package manifest or install step for them; a fresh checkout is not guaranteed to mint PO tokens. See `docs/architecture/po-token.md`.
