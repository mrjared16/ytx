# Extraction Pipeline

This document details the low-level execution flow and timing profiles of the `ytx` extraction process.

## Call Chain

The `Extract(videoID)` function orchestrates a sequence of parallel and sequential steps:

```text
Extract(videoID)
  ├─ [goroutine] fetchVisitorData → visitorData
  ├─ [goroutine] getCachedCipher  → Cipher, STS (early signal)
  │   ├─ 1. in-memory cache       → hit? return
  │   ├─ 2. disk cache (cipher.json + player.js.gz) → hit? return
  │   └─ 3. NewCipherWithCachedPath
  │       ├─ fetchPlayerJS (embed page → base.js download)
  │       ├─ findURLTransformFunctionName → wrapper name
  │       ├─ findSignatureTimestamp → STS
  │       └─ save to disk cache
  ├─ ← stsCh (STS ready, API can start)
  ├─ callPlayerAPI(videoID, visitorData, STS) → streamFormats
  ├─ ← cipherCh (full cipher ready)
  ├─ findBestAudioStream → selected format
  └─ getStreamURL
      ├─ DecryptSignature (wrapper mode)
      │   ├─ ensureWrapperContext → [cached QuickJS + 2.7MB eval]
      │   └─ ctx.Eval(sigScript) → decrypted sig
      └─ TransformN (wrapper mode)
          ├─ ensureWrapperContext → [reuse cached context]
          └─ ctx.Eval(nScript) → transformed n
```

## Timing Profiles (Measured 2026-03-26)

### Cold Start (No Cache)
The cold path is network-bound, primarily by the download and evaluation of the ~2.7MB `player.js`.

| Component | Time (ms) | Notes |
| :--- | :--- | :--- |
| **Visitor Data** | ~300ms | Parallel with cipher |
| **Cipher Init** | ~2300ms | Includes base.js fetch and pattern matching |
| **API Call** | ~130ms | Overlaps with cipher tail |
| **Sig Decrypt** | ~500ms | QuickJS bootstrap (one-time) |
| **Total** | **~3200ms** | |

### Warm Start (Cached)
With a warm cache, `ytx` achieves significantly faster extraction.

| Component | Time (ms) | Notes |
| :--- | :--- | :--- |
| **Visitor Data** | ~300ms | |
| **Cipher Init** | ~0ms | Restored from memory |
| **Sig Decrypt** | ~5ms | Context reused from previous call |
| **Total** | **~450ms** | |

## Key Invariants

1.  **STS Synchronization**: The `signatureTimestamp` (STS) must match the version of `player.js` used for decryption.
2.  **Cipher Lifetime**: `Cipher.Close()` must be called when the cached cipher is invalidated or replaced.
3.  **Thread Safety**: Access to the `wrapperCtx` requires a `lazyMu` lock as the JS context is not thread-safe.
4.  **Browser Stubs**: The wrapper function depends on `URL` and `URLSearchParams` polyfills provided by `browserStubsJS`.
