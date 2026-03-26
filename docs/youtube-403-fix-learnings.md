# YouTube 403 Forbidden Fix: Technical Learnings

This document synthesizes the root cause analysis, technical decisions, and performance optimizations implemented to resolve systemic HTTP 403 Forbidden errors in `ytx music` mode and optimize the cipher extraction process.

## 1. The Problem
YouTube Music extraction (`ytx music`) using the `WEB_MUSIC` client was failing with HTTP 403 Forbidden errors during playback.
- **Symptoms**: Playable URLs were successfully generated but returned 403 when probed or played.
- **Scope**: systemic across music mode; `video` mode (`ANDROID_VR` client) remained unaffected.
- **Verification Baseline**: Canary ID `dQw4w9WgXcQ` consistently failed even after cache purge.

## 2. Root Cause Analysis
The failures were attributed to brittle extraction logic in `pkg/cipher.go`:

### Signature Decryption
- **Incomplete Dependency Extraction**: The extractor failed to capture auxiliary helper functions required by the main signature decryption function.
- **Execution Failures**: This led to `ReferenceError` (e.g., `d is not defined`) within the Goja JS engine.

### N-Transform Discovery
- **Outdated Patterns**: `findNFunctionName` relied on a restrictive 3-character regex that failed to match modern, more complex obfuscation patterns.
- **Runtime Environment Gaps**: Modern player shapes utilize helper wrappers (e.g., `urlwrap:<fn>`) and browser globals (e.g., `document.location`) which were missing from the JS execution context.

### Performance Bottleneck
- **O(n) Extraction**: The original `extractWithAST` performed a full regex scan of the 1.4MB `player.js` for each of the ~140 dependencies, resulting in ~8s extraction times.

## 3. The Solution: Proof-First Hardening
A "Proof-First" methodology was adopted, requiring cold-cache validation (`ytx cache purge`) for every fix.

### Hardened Extraction Logic
- **Recursive AST Extraction**: Redesigned `extractWithAST` to capture the full dependency closure of the signature function.
- **Multi-Strategy N-Discovery**:
  - Structural `.get("n")` window scanning.
  - Array-index resolution (`arr[idx](n)`).
  - URL-wrapper fallback discovery.
- **Browser Context Stubs**: Added browser-like global mocks to the JS engine to support wrapper-based transforms.

### Staged Validation Gate
Integrated an explicit validation pipeline in `pkg/extractor.go`:
1. **Post-Sig Assembly**: Construct URL with decrypted signature.
2. **Post-N Transform**: Apply N-parameter transformation.
3. **Playback Probe**: Final acceptance gate using HTTP Range requests (`bytes=0-1`). Only status `200` or `206` are accepted.
4. **Fallback**: If the first candidate fails, a fresh cipher is fetched and a secondary candidate is evaluated.

## 4. Optimization: Single-Pass AST Indexing
To achieve the 8x performance target, `extractWithAST` was completely refactored.

### Technical Implementation
- **Data Structures**:
  - `segment`: Stores `start`/`end` byte offsets and a list of dependencies (`deps`).
  - `definitionIndex`: A map of identifier names to their corresponding `segment`.
- **Single-Pass Indexing**: The `player.js` code is parsed once. A single AST walk identifies all `VariableStatement`, `FunctionDeclaration`, and `AssignExpression` nodes, populating the index.
- **BFS Resolution**: Dependencies are resolved in O(1) via the index, using a Breadth-First Search to collect all transitive requirements.
- **Offset Normalization**: `goja` 1-based indices are normalized to 0-based byte offsets: `int(idx) - program.File.Base()`.

## 5. Performance Metrics
| Metric | Before | After |
|--------|--------|-------|
| `extractWithAST` (Cold) | ~8196ms | **~1100ms** |
| `cipher_init_ms` | ~7788ms | **< 1000ms** |
| Success Rate (Music) | ~0% (Canary) | **100% (Canary)** |
| Memory Overhead | - | < 50MB |

## 6. Key Learnings & Gotchas
- **Cold-Cache Reliability**: Always verify with `ytx cache purge`. Cache often masks systemic failures during development.
- **Range Request Necessity**: HTTP 200/206 status is the only definitive proof of a valid signature/n-transform combination; mere string presence is insufficient.
- **Brittle Regex vs. AST**: Fixed-length regex patterns (like the old 3-char N-check) are highly susceptible to obfuscation drift. Structural AST analysis is significantly more resilient.
- **Dependency Filtering**: When performing recursive extraction, it is critical to filter out unsafe non-function snippets (e.g., `var F=d[v]`) that can drag in unresolved global references.
- **JS Engine Errors**: `ReferenceError` in the JS engine usually indicates a failure in the dependency crawler, not the engine itself.
- **Semicolon Handling**: `goja` `Idx1()` often excludes trailing semicolons; always append `;\n` during code emission to ensure valid JS.
