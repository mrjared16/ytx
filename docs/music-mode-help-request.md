# ytx Music Mode: Context, Current State, Goals, and Help Request

This document is intended to give enough context for an external engineer to help debug and optimize `ytx music` mode without needing the full conversation history.

It focuses on:

- what this project is trying to do
- the exact goals we are trying to hit
- what was broken
- what has already been changed
- what is experimentally proven vs still uncertain
- where the remaining bottlenecks and flaky behavior are
- what kind of help is needed now

---

## 1. Project Context

Repository: `github.com/mrjared16/ytx`

Language: Go

Main relevant paths:

- `pkg/extractor.go` — high-level extraction flow
- `pkg/cipher.go` — player bootstrap, signature decryption, n-transform discovery/execution
- `pkg/cache.go` — persistent cache for player/cipher artifacts
- `pkg/jsengine.go` — JS engine selection/cache for n-transform
- `pkg/quickjsrunner.go` / `pkg/jsrunner.go` / `pkg/nrunner.mjs` — runtime execution paths
- `pkg/extractor_test.go` — integration/regression tests
- `docs/TECHNICAL.md` — intended architecture/design reference

The repo supports two major modes:

- `video` mode: simpler, generally healthy
- `music` mode: authenticated/premium path, much more fragile

The current work is specifically about making **music mode** correct and fast.

---

## 2. The Two Main Goals

These are the actual acceptance criteria.

### Goal 1 — Correctness

Music mode must:

1. extract a premium audio stream successfully (`itag 141` and ideally `774`)
2. return a URL that actually downloads the complete file
3. not fail midstream or produce a truncated response

### Goal 2 — Performance

Music mode should be competitive with or faster than `yt-dlp`:

- **cold run target:** about `500–1200ms`
- **warm run target:** about `300–700ms`

Warm means we should be reusing as much player/cipher/runtime state as is safely reusable per player version.

---

## 3. Short Status Summary

Current status as of the latest verification:

### Correctness

- `itag 141` has been extracted successfully in real CLI runs.
- A returned `141` URL was fully downloaded and its byte count matched `clen` exactly.
- `itag 774` has **not** been separately proven in the latest verification pass.
- There is still some **flakiness**: a fresh `TestMusicModeWithCookies` run returned `HTTP 403` even while other direct CLI/diagnose runs succeeded.

### Performance

- **Cold:** still far too slow.
- **Warm:** improved relative to cold, but still far above target.

So the current status is:

- **Goal 1:** partially achieved / mostly yes for `141`, not fully locked down for `774`, not fully reliable
- **Goal 2:** **not achieved**

---

## 4. What Was Broken Originally

The repo had drifted into several bad states during debugging:

1. **Broken signature detection / execution** after player changes
2. **Broken n-function detection**, including false-positive matches like `Array`
3. A temporary **POT-first path** that was not part of the intended architecture and did not reliably solve the real issue
4. Test harness behavior that **hid failures by skipping music cases** and made progress look better than it was

We explicitly backed away from the POT-first path and restored the documented extractor/cipher architecture from `docs/TECHNICAL.md` as the primary design.

---

## 5. Current Intended Architecture

The intended architecture now matches the documented path:

1. fetch music watch-page/session bootstrap data
2. fetch/cache player `base.js`
3. detect player-version-specific metadata (`sig`, `n`, `sts`)
4. call Innertube player API
5. choose best audio stream
6. decrypt `s` if the chosen stream is `signatureCipher`
7. transform `n` if present
8. return final playable URL

No external POT service is part of the default production path anymore.

---

## 6. Current Music Flow (ASCII)

```text
INPUT
  video ID + cookies + config(WEB_REMIX/music)
    |
    v
+-------------------------------+
| fetchMusicVisitorData()       |
| pkg/extractor.go              |
|-------------------------------|
| Reads watch-page/session data |
| Output: visitor/session info  |
+-------------------------------+
    |
    +---------------------------+
    |                           |
    | in parallel               | in parallel
    v                           v
+-------------------------------+     +----------------------------------+
| getCachedCipher()             |     | JS runtime pre-spawn (optional) |
| pkg/extractor.go              |     | pkg/cipher.go / jsrunner.go     |
|-------------------------------|     |----------------------------------|
| Memory cache -> disk cache -> |     | Hides Bun/Node subprocess spawn |
| fresh player bootstrap        |     +----------------------------------+
| Output: Cipher metadata       |
+-------------------------------+
    |
    v
+-------------------------------+
| callPlayerAPI()               |
| pkg/extractor.go              |
|-------------------------------|
| Sends WEB_REMIX player call   |
| Needs visitor data + STS      |
| Output: adaptiveFormats       |
+-------------------------------+
    |
    v
+-------------------------------+
| findBestAudioStream()         |
| pkg/extractor.go              |
|-------------------------------|
| Picks preferred audio itag    |
| Output: chosen format         |
+-------------------------------+
    |
    v
+-------------------------------+
| getStreamURL()                |
| pkg/extractor.go              |
|-------------------------------|
| Direct URL? return fast path  |
| signatureCipher? decrypt sig  |
+-------------------------------+
    |
    v
+-------------------------------+
| DecryptSignature()            |
| pkg/cipher.go                 |
|-------------------------------|
| QuickJS on extracted sig code |
| Output: signed URL            |
+-------------------------------+
    |
    v
+-------------------------------+
| unthrottle()/TransformN()     |
| pkg/extractor.go / cipher.go  |
|-------------------------------|
| If URL has n=..., transform   |
| Output: final playable URL    |
+-------------------------------+
    |
    v
FINAL OUTPUT
  premium audio URL
```

---

## 7. What Has Already Been Changed

These are the major implemented changes so far.

### 7.1 Correctness fixes

- Removed the broken **POT-first** critical path from production extraction.
- Restored the documented cipher-based music path.
- Fixed wrapper-based signature execution in `pkg/cipher.go`.
- Fixed `n` detection false positives, especially the bad `Array` match that caused 403s.
- Removed test masking/skips that previously hid music failures.

### 7.2 Player bootstrap changes

- `fetchPlayerJS()` now prefers the faster `embed` page before the slower `watch` page for base.js discovery.
- Added tests for:
  - embed-first bootstrap
  - watch fallback when embed lacks the player path

### 7.3 Lazy / reusable artifacts

- Signature JS extraction is now **lazy**, not fully front-loaded during cipher initialization.
- Added persistence of extracted sig artifacts back into cache after first successful decrypt.

### 7.4 Player-version cache/fingerprint work

- Added a quick **player fingerprint** to cache.
- Added cache info fields for:
  - player URL
  - base.js path
  - player fingerprint
  - sig function
  - n function
  - STS
  - whether player.js is cached
  - whether extracted sig JS is cached
- Added warning behavior when cached player path fails and the code falls back to discovery.

### 7.5 Engine cache work

- Replaced the single unscoped engine singleton with per-player identity caching.
- `n` runtime is now keyed by player identity/fingerprint rather than one global engine slot.

### 7.6 Manual cache commands

Added CLI support for:

- `ytx cache purge`
- `ytx cache info`
- `ytx cache refresh VIDEO_ID`

Also added Make targets for cache info/refresh.

---

## 8. What Is Experimentally Proven vs Inferred

This distinction matters.

### Experimentally proven

The following have direct evidence from fresh runs/tests:

1. **Premium `141` extraction works in real CLI runs**
   - `ytx music dQw4w9WgXcQ --profile` returned itag `141`

2. **A returned `141` URL downloaded the complete file**
   - downloaded bytes exactly matched `clen`
   - proof values from one run:
     - `DOWNLOAD_STATUS=200`
     - `DOWNLOAD_BYTES=6858879`
     - `EXPECTED_CLEN=6858879`
     - `CLEN_MATCH=True`

3. **Cold and warm player cache state works**
   - `ytx cache info` shows valid cached player metadata
   - `ytx cache refresh VIDEO_ID` primes cache and extracted sig artifact

4. **The current performance targets are not met**
   - cold run after purge: ~`4226ms`
   - warm run immediately after: ~`1923ms`

5. **Main time sinks were measured, not guessed**
   - cold:
     - `cipher_init_ms ~2490`
     - `n_transform_ms ~558`
     - `visitor_data_ms ~361`
   - warm:
     - `n_transform_ms ~622`
     - `visitor_data_ms ~378`
     - `cipher_init_ms ~378`

### Inferred / not yet fully proven

1. Whether the current architecture can be brought down to the target latency **without a larger redesign**
2. Whether `itag 774` is now as reliable as `141`
3. Exactly why `TestMusicModeWithCookies` can still produce a fresh `403` while direct CLI/diagnose runs sometimes succeed
4. Whether `n_transform_ms` can be meaningfully reduced without changing runtime strategy more aggressively
5. Whether a lighter-weight `n` execution model is feasible without full player-context semantics

---

## 9. Fresh Evidence Snapshot

### 9.1 Cold run after purge

Command:

```bash
go run ./cmd/ytx cache purge && go run ./cmd/ytx music dQw4w9WgXcQ --profile
```

Observed result:

- `itag=141`
- `bitrate=258454`
- `visitor_data_ms=361`
- `player_api_ms=138`
- `cipher_init_ms=2490`
- `n_transform_ms=558`
- `total_ms=4226`

### 9.2 Warm run immediately after

Command:

```bash
go run ./cmd/ytx music dQw4w9WgXcQ --profile
```

Observed result:

- `itag=141`
- `bitrate=258454`
- `visitor_data_ms=378`
- `player_api_ms=145`
- `cipher_init_ms=378`
- `n_transform_ms=622`
- `total_ms=1923`

### 9.3 Full-file validation

Validation script behavior:

1. run `ytx music ... --profile`
2. parse returned URL
3. extract `clen`
4. download the entire URL
5. compare actual byte count to `clen`

Observed result:

- HTTP `200`
- bytes downloaded matched `clen` exactly

### 9.4 Flaky failure evidence

Fresh `go test ./pkg -run '^TestMusicModeWithCookies$' -v -count=1` produced:

- `itag=141`
- `bitrate=258454`
- `N-parameter transformed: true`
- `HTTP status: 403`

So we have both:

- successful direct CLI/diagnose runs
- and at least one recent integration-test `403`

That means reliability is still not fully solved.

---

## 10. What Seems to Be Slow

### Cold path bottleneck

The main bottleneck is still **cipher initialization**.

Measured cold profile:

```text
cipher_init_ms ≈ 2490
n_transform_ms ≈ 558
visitor_data_ms ≈ 361
player_api_ms   ≈ 138
total_ms        ≈ 4226
```

This means the dominant cost is not the API call itself.

### Warm path bottleneck

Warm profile:

```text
n_transform_ms ≈ 622
visitor_data_ms ≈ 378
cipher_init_ms ≈ 378
player_api_ms   ≈ 145
total_ms        ≈ 1923
```

Warm path suggests:

- player bootstrap cache helps
- but `n` execution and request bootstrap still dominate too much

---

## 11. Current Working Theory of the Remaining Problems

### Reliability / occasional 403

Possible causes:

1. some remaining race/timing/cache invalidation issue
2. some difference between test harness and direct CLI path
3. `n` runtime or final URL validity still being fragile for some runs
4. player-version reuse is still not scoped or invalidated perfectly

### Performance

Likely remaining expensive areas:

1. **player/cipher bootstrap still too heavy on cold path**
2. **n-transform execution still too expensive even when warm**
3. **music watch-page/session bootstrap remains non-trivial**

---

## 12. Important Architectural Constraints

These constraints matter for anyone proposing help.

### 12.1 We do not want to go back to a default external POT dependency

That path was explored and intentionally backed out from the critical production flow.

### 12.2 The intended primary architecture is the documented internal cipher path

We want the default path to stay aligned with `docs/TECHNICAL.md`.

### 12.3 Caching should be player-version-centric, not video-centric

The algorithms and player artifacts are tied to the player rollout, not to a specific video.

### 12.4 False-outdated but cheap fingerprinting is acceptable

The requirement here is pragmatic:

- better to occasionally invalidate too aggressively
- than to make fingerprinting precise but slow

### 12.5 Help should distinguish proven facts from speculation

The user explicitly requested this. If proposing an optimization, it helps to say whether it is:

- experimentally proven in this repo
- strongly supported by code/measurements
- or still just a hypothesis

---

## 13. Relevant Commands for Reproduction

### Basic verification

```bash
go test ./pkg -run '^TestMusicModeWithCookies$' -v -count=1
go test ./pkg -run '^TestExtractorRegression$' -v -count=1
go test ./...
go build ./...
```

### Diagnose against yt-dlp baseline

```bash
go run ./cmd/diagnose/main.go dQw4w9WgXcQ
```

### Cold/warm timings

```bash
go run ./cmd/ytx cache purge
go run ./cmd/ytx music dQw4w9WgXcQ --profile
go run ./cmd/ytx music dQw4w9WgXcQ --profile
```

### Cache inspection

```bash
go run ./cmd/ytx cache info
go run ./cmd/ytx cache refresh dQw4w9WgXcQ
```

---

## 14. Relevant Files to Inspect

If someone wants to help, these are the best entry points.

### Hot path

- `pkg/extractor.go`
  - `Extract(...)`
  - `callPlayerAPI(...)`
  - `getCachedCipher(...)`
  - `fetchAndCacheCipher(...)`
  - `getStreamURL(...)`
  - `unthrottle(...)`

### Cipher/player logic

- `pkg/cipher.go`
  - `NewCipherWithCachedPath(...)`
  - `ensureSignatureReady()`
  - `DecryptSignature(...)`
  - `TransformN(...)`
  - `TransformNBatch(...)`
  - `fetchPlayerJS(...)`
  - `buildNTransformRuntime(...)`
  - `computePlayerFingerprint(...)`

### Runtime caching

- `pkg/jsengine.go`
- `pkg/quickjsrunner.go`
- `pkg/jsrunner.go`
- `pkg/nrunner.mjs`

### Persistent cache

- `pkg/cache.go`

### Tests

- `pkg/extractor_test.go`
- `pkg/cipher_sig_test.go`
- `pkg/jsengine_test.go`

---

## 15. What Kind of Help Is Needed Now

The most valuable help would be one or more of these:

### A. Reliability help

Why can `TestMusicModeWithCookies` still produce a fresh `403` while:

- `ytx music ... --profile` can succeed
- `diagnose` can succeed
- the returned `141` URL can fully download

### B. Performance help

How can we reduce:

1. **cold `cipher_init_ms`** from ~`2490ms` toward target
2. **warm `n_transform_ms`** from ~`622ms` toward target
3. **overall warm total** from ~`1923ms` toward `300–700ms`

### C. Architecture help

Questions worth answering:

1. Can `n` execution be made lighter without losing correctness?
2. Can more of the prepared runtime be precomputed and reused safely per player version?
3. Is there a better way to split cheap player metadata from heavy transform runtime work?
4. Is the current runtime strategy for `n` fundamentally too expensive for the target latency?

### D. Premium-stream coverage help

We still need stronger proof or a dedicated path/validation for `itag 774`, not just `141`.

---

## 16. Suggested Questions to Ask Others

If sharing this externally, these are good concrete questions:

1. **Why might a signed + n-transformed premium music URL still intermittently 403 even though other runs for the same video succeed?**
2. **What is the cheapest reliable way to execute or emulate the current `n` transform for a player-version-scoped cache?**
3. **Would you keep full-player runtime execution for `n`, or replace it with another extraction strategy? Why?**
4. **Is there a known reason `141` would be stable while `774` remains less clearly proven?**
5. **What player-version-scoped artifacts would you cache beyond what is already being cached here?**
6. **If the target is warm `300–700ms`, is the present Go + QuickJS/Bun architecture sufficient, or does it need a bigger redesign?**

---

## 17. Bottom Line

The repo is in a much better state than before:

- music is no longer relying on the abandoned POT-first default path
- the documented cipher path works again
- premium `141` extraction has been proven in real CLI runs
- one returned `141` URL has been proven to download completely
- player-version fingerprinted cache/manual refresh support has been added

But the project is **not done** against the real goals:

- reliability is still imperfect because of recent flaky `403` behavior
- `774` is not yet fully re-proven
- performance still misses the target badly

If someone wants to help, the most important remaining areas are:

1. intermittent music `403` reliability
2. cold `cipher_init_ms`
3. warm `n_transform_ms`
4. proof/coverage for `774`

---

## 18. Companion Docs

Related existing repo docs:

- `docs/TECHNICAL.md` — intended project architecture
- `docs/youtube-403-fix-learnings.md` — earlier technical learnings from the 403/cipher debugging work
