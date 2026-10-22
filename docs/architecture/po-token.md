# PO Token (Botguard) Engine

This document describes the explicit `--po` (Proof of Origin) token lane. It is not part of normal mode and it does not replace signature/n decoding.

## Overview

Some requests need a PO token in addition to cookies and a valid decoded stream URL. `--po` opts an extraction into minting that token. A successful mint is only one step: the signature and n transform must still succeed, and YouTube can still return 403 for other reasons (expired cookies, account restrictions, or YouTube-side blocking).

Normal mode performs no PO work.

## Mint flow

`--po` runs a token coordinator that:

1. Loads a cached token when the stored video/visitor/session identity validates and the entry is unexpired.
2. Otherwise loads a Botguard challenge. `loadPOTokenChallengeContext` first tries the music watch-page extractor; that helper is currently gated on the historical `WEB_MUSIC` config name, so the active `WEB_REMIX` configuration falls through to the attestation (`att/get`) request.
3. Calls the minting subprocess and waits for the result.
4. Sends the player token to the Innertube API in `ServiceIntegrityDimensions`.
5. Applies the URL token as the `pot` query parameter, when present.
6. Persists the result in `po_token.json` with an expiry.

### The subprocess is per mint, not a daemon

`mintPOTokenWithEngine` in `pkg/potrunner.go`:

- resolves a Bun or Node binary (`auto` prefers Bun, then Node; an explicit engine must exist). QuickJS is not supported for PO minting.
- writes the embedded `pkg/po_minter.mjs` to the ytx cache directory (or a temporary path)
- creates `exec.CommandContext(ctx, runtime, scriptPath)`
- sends one JSON request on stdin and reads one JSON response on stdout
- waits for the process and returns the player token, URL token, and TTL

There is **no persistent Bun/Node daemon**, no long-lived JSON challenge stream, and no sub-10 ms inter-track response guarantee. Bulk extraction is sequential; each mint that misses the token cache starts a new subprocess. A cached token avoids the mint entirely.

The process is cancellable through the caller's context.

### Runtime dependencies are not bundled

`pkg/po_minter.mjs` imports:

```js
import { BG, buildURL, GOOG_API_KEY, USER_AGENT } from 'bgutils-js';
import { JSDOM } from 'jsdom';
```

The repository has no `package.json`, lockfile, `node_modules`, or install step for these modules. The minter script is embedded in the Go binary, but those two modules must already be resolvable by the chosen Bun/Node runtime.

Implications:

- A fresh checkout is not guaranteed to have a working PO runtime.
- Bun is preferred when present, but that does not supply the JS modules by itself.
- Node fallback is conditional on module resolution and is not reliable on a clean machine without those dependencies.
- This document does not prescribe package versions or installation commands, because the repository does not pin or provision them.

## Caching

Tokens are stored in the ytx cache as `po_token.json`. Entries are keyed by video, visitor data, session index, and network key. On load, `LoadPOToken` looks for a matching entry and validates video, visitor data, session, and expiry; the network-key comparison is enforced only when a network key is supplied. Expired entries are not reused. The default in-process TTL fallback is 10 minutes when the mint does not return one.

A cached token skips the challenge fetch and mint, but not signature/n processing.

## Failure modes

- **Mint / runtime failure**: returned as an explicit PO error (`POFailureRuntimeMint`, or `POFailureTokenUnavailable` when the coordinator has no token). It happens before final URL publication.
- **Mint succeeds, final URL still 403**: the token did not address the actual cause. Investigate signature/n decoding, cookies, account, or YouTube-side blocking.
- **Missing modules / runtime**: the subprocess or import fails; this is an environment problem, not evidence that `--po` fixed a decoder bug.

`--po` is therefore not a universal 403 fix and should not be documented as one.

## Invariants

- **Explicit opt-in**: no PO challenge or mint work happens without `--po`.
- **Token identity**: entries are keyed by video, visitor data, session, and network; load-time validation checks video, visitor, session, and expiry, and enforces the network key only when one is supplied.
- **Separate failures**: mint errors and decoder errors are surfaced distinctly.
- **No hidden fallback**: if PO minting fails, extraction fails rather than appending an invalid token and hoping for the best.
