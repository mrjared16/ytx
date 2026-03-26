# Music Extraction Pipeline

## Call Chain

```
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

## Wrapper Mode (Current YouTube Architecture)

YouTube eliminated separate sig/n functions. A single **URL wrapper function** handles both.

| Old (broken) | Current (working) |
|---|---|
| `findSigFunctionName` → extract → goja eval | `findURLTransformFunctionName` → QuickJS eval full player.js |
| `findNFunctionName` → separate n-func | Same wrapper handles n via URL `.set("n", value)` |
| goja (ES5.1), extracted ~50 lines | QuickJS (ES2020+), full 2.7MB context required |

Detection: `findURLTransformFunctionName` searches for `.set("alr","yes")` call pattern.
Wrapper name rotates with each player.js version (`kS`, `of`, `xj`, etc.).

## Player Variants

Two base.js files exist. Both contain the wrapper function.

| Variant | Size | Source |
|---|---|---|
| `player_embed.vflset/en_US/base.js` | **1.6MB** | `fetchPlayerJS` returns this |
| `player_ias.vflset/en_US/base.js` | **2.7MB** | Referenced in embed page HTML |

Both share the same `/s/player/HASH/` prefix per version.

## QuickJS Context Reuse

`ensureWrapperContext` bootstraps QuickJS once per cipher lifetime:

```
First call (sig):  NewRuntime + NewContext + Eval(stubs) + Eval(playerJS) → ~500ms
                   Stored in cipher.wrapperRT, cipher.wrapperCtx

Second call (n):   Reuse cipher.wrapperCtx → ctx.Eval(nScript) → ~2ms
```

`Cipher.Close()` releases the runtime. Called automatically by `invalidateCipherCache`.

## Two-Phase Init

API only needs STS (integer). Cipher goroutine signals STS via `stsCh` before finishing heavy work. API call starts immediately, overlapping with remaining cipher init.

```
cipher goroutine:  [fetch player.js ~~~ extract STS] → stsCh
                   [pattern detection, runtime build] → cipherCh (later)
API call:          starts after stsCh, before cipherCh
```

Uses `Extractor.earlySTSOverride` field, reset to 0 after API returns.

## Profiled Timing (2026-03-26)

### Cold (no cache)

```
visitor     ████████        ~300ms   (parallel with cipher)
cipher      ████████████████████████████████  ~2300ms  (network-bound)
  embed       ██         ~113ms
  base.js     ███        ~90ms (cold conn) / ~62ms (warm conn)
  patterns    ▌          ~5ms
  wrapper-rt  ████████   ~309ms  (regex patch 2.7MB)
API         ████          ~130ms   (overlaps with cipher tail)
sig decrypt ████████████  ~500ms   (QuickJS bootstrap, first call)
n transform ▌             ~2ms     (context reuse)
────────────────────────────────────
total       ████████████████████████████████████  ~3200ms
```

### Warm (cipher in memory)

```
visitor     ████████        ~300ms
cipher      ▌               ~0ms   (from memory)
API         ████            ~130ms
sig decrypt ████████████    ~500ms  (QuickJS bootstrap, first per-process)
n transform ▌               ~2ms   (context reuse)
────────────────────────────────────
total       ████████████████████    ~600-1000ms
```

### Warm+Warm (2nd+ video, same process)

```
visitor     ████████        ~300ms
cipher      ▌               ~0ms
API         ████            ~130ms
sig decrypt ▌               ~5ms   (context already bootstrapped)
n transform ▌               ~2ms
────────────────────────────────────
total       ████████████            ~450ms
```

## Optimization Roadmap

| # | Opportunity | Estimated Save | Complexity | Notes |
|---|---|---|---|---|
| 1 | **Use 1.6MB embed player** instead of 2.7MB | ~200ms eval, ~30ms download | Low | Both have wrapper. Verify edge cases. |
| 2 | **QuickJS bytecode caching** | ~500ms (skip re-eval) | Medium | `JS_WriteObject`/`JS_ReadObject` if supported by buke/quickjs-go |
| 3 | **Eliminate `buildWrapperRuntimeJS`** | ~309ms | Medium | Regex patches 2.7MB — may be cacheable or eliminable |
| 4 | **Parallel TLS warmup** | ~200ms cold | Low | Start connection to youtube.com at process init |
| 5 | **Visitor cache improvements** | ~300ms | Low | Check if `NeedsCookies` path bypasses `visitorDataCache` |
| 6 | **Player.js diff updates** | variable | High | Track version hash, skip re-download if unchanged |

## Key Invariants

- STS must match the player.js version used for wrapper calls
- `Cipher.Close()` must be called when replacing cached cipher
- `wrapperCtx` access requires `cipher.lazyMu` lock (not thread-safe)
- Wrapper function uses `URL` and `URLSearchParams` polyfills from `browserStubsJS`
- `playerJS` in disk cache is gzip-compressed (`player.js.gz`)
- Cipher disk cache TTL: 6 hours
- In-memory cipher is shared globally via `cipherCache` (RWMutex-protected)
