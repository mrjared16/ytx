# Extraction Pipeline

This document describes the low-level execution flow of the `ytx` extraction process, including the Functional Core / Imperative Shell (FCIS) detection ladder and the two cipher execution paths.

## Call chain

`Extract(videoID)` overlaps visitor bootstrap, cipher preparation, and (in `--po` mode) token minting:

```text
Extract(videoID)
  ├─ [goroutine] fetchVisitorData → visitorData
  ├─ [goroutine] getCachedCipher  → Cipher, STS (early signal)
  │   ├─ 1. in-memory cache       → fresh? return
  │   ├─ 2. disk cache (version 7, fingerprint + STS identity) → hit? return
  │   └─ 3. fetch player.js (embed first, watch fallback)
  │       ├─ Tier 0: urlWrapperDetect (modern wrapper, bounded markers)
  │       ├─ Tier 1: detectFastSignature (windowed regex)
  │       ├─ Tier 2: detectWrapperSignature
  │       ├─ Tier 3: detectGlobalSignature (full-file scan)
  │       ├─ findSignatureTimestamp → STS
  │       └─ save cipher/player artifacts
  ├─ ← stsCh (positive STS; API can start)
  ├─ callPlayerAPI(videoID, visitorData, STS) → streamFormats
  ├─ ← cipherCh (full cipher ready)
  ├─ findBestAudioStream → selected format
  └─ getStreamURL
      ├─ modern URL-wrapper: one TransformURLContext call
      │   └─ Bun/Node subprocess → signature + n + alr
      └─ legacy: DecryptSignature → TransformN
          ├─ signature: embedded QuickJS
          └─ n-transform: Bun/Node or QuickJS, depending on engine selection
```

Important distinctions:

- Tier 0 is checked before the legacy tiers. A validated modern wrapper skips legacy n-function discovery. An **ambiguous** wrapper is fail-closed: there is no fallthrough to QuickJS or a global scan; a cached ambiguous cipher is invalidated and refetched, while a fresh ambiguous player is an explicit error.
- The modern path requires Bun or Node. QuickJS is not a modern fallback.
- `callPlayerAPI` can start on the early STS signal before the full cipher runtime is ready.
- In `--po` mode, token minting runs on its own goroutine and is awaited before the API call. The mint is a short-lived Bun/Node subprocess, not a persistent daemon.
- Direct-URL (pre-signed) formats that carry an `n` parameter still flow through n handling; modern ciphers route them through the same single URL transform.

## Video mode's separate native chain

Video mode does not use the cipher pipeline above. `ExtractVideoContext` runs a sequential guest-only native chain of immutable profiles:

```text
VISIONOS → ANDROID_VR → ANDROID → IOS
```

Each profile is attempted in order. A profile that returns a playable response must also supply adaptive direct-URL audio and video streams. Before a candidate is accepted, `validateStreamEnds` probes the final byte when `Content-Length` is known; otherwise it first discovers the total with a `0-0` range and then probes the final byte. Each probe must return HTTP 206 with an exact matching `Content-Range`. This is evidence that the direct range is readable, **not** proof that signature/n decoding is correct. The native chain intentionally has no cipher, PO, or JS work.

## Timing profiles (historical measurements from April 2026)

The tables below are historical observations from April 2026. They are retained for context, not as current performance promises or fixed latency targets. Hardware, network, player size, cache state, YouTube behavior, and the Bun/Node process startup all change the result. The modern URL-wrapper path also supersedes the QuickJS-first assumptions in these measurements.

### Historical cold path

| Component | Time (ms) | Notes |
| :--- | :--- | :--- |
| **Visitor Data / API** | ~160 | Overlapped fetch |
| **Player.js Fetch** | ~930 | Downloading the JS payload |
| **Cipher Analysis** | ~100 | FCIS fast path (8KB window) |
| **QuickJS Prewarm** | ~1400 | Legacy wrapper evaluation |
| **Total** | ~1400 | Observation, not a guarantee |

### Historical cached path

| Component | Time (ms) | Notes |
| :--- | :--- | :--- |
| **Visitor Data** | ~0 | Reused within its own TTL/grace window |
| **API Call** | ~400 | Innertube player fetch |
| **Cipher Init** | ~0 | Restored from metadata |
| **QuickJS Prewarm** | ~320 | Legacy bytecode execution |
| **Total** | ~440 | Observation, not a guarantee |

### Historical PO mode

| State | PO Mint (ms) | Total Extraction (ms) | Notes |
| :--- | :--- | :--- | :--- |
| **Cold** | ~4100 | ~4600 | CPU-bound mint in a Bun/Node subprocess |
| **Warm** | ~0 | ~330 | Cached token reused |

PO-mode requirements and caveats are covered in `docs/architecture/po-token.md`. The historical numbers above should not be used to claim a current response time.

## Key invariants

1. **Fail-closed modern recognition**: Ambiguous URL-wrapper candidates are never guessed at and do not silently degrade to legacy QuickJS.
2. **STS synchronization**: The `signatureTimestamp` must belong to the same player version used for cipher work. When a cached player bundle is present, identity checks compare the full-content player fingerprint and, where available, the STS; bundle-less legacy caches use their legacy gates.
3. **Cipher lifetime**: `Cipher.Close()` releases cached QuickJS resources and modern runner keys when a cached cipher is invalidated or replaced.
4. **No universal 403 self-heal**: A final stream URL returning 403 does not automatically purge the cache and retry. Legacy signature-decrypt errors have a fail-forward retry; modern URL-wrapper failures return explicit errors.
5. **PO is a token lane, not a decoder fix**: A successful PO mint can still be followed by a signature/n failure. Mint failures and decoder failures are reported separately.
