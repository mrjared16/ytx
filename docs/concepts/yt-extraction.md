# YouTube Extraction Concepts

Understanding how YouTube delivers streams is necessary for maintaining `ytx`. This page describes the concepts; exact implementation details live in the architecture and internal docs.

## The standard extraction flow

1. **Innertube API call**: the client calls `/youtubei/v1/player` with a video ID and client context.
2. **Streaming data**: the response contains `adaptiveFormats`, listing available itags and either direct URLs or `signatureCipher` entries.
3. **Encrypted parameters**: protected formats may carry:
   - **Signature (`s`)**: a scrambled string decoded with the player's current cipher logic.
   - **N-parameter**: a throttling token that must be transformed to allow full-speed downloads.
4. **PO token (optional)**: an attestation token requested with `--po` when YouTube requires it. It is separate from signature/n decoding.

## Clients used by ytx

`ytx` does not use one client for everything.

### Video: native guest chain

Video extraction runs a sequential, guest-only chain:

```text
VISIONOS → ANDROID_VR → ANDROID → IOS
```

Profiles are tried in order. A profile must return playable adaptive direct URLs; before acceptance, `ytx` probes the final byte (using a `0-0` discovery range when the total length is unknown) and requires HTTP 206 with an exact matching `Content-Range`. This is range-readability evidence, not decode proof, and this path does no cipher/PO/JS work.

### Music: `WEB_REMIX`

Music uses the `WEB_REMIX` client, cookies, and SAPISIDHASH authentication. It is the path that uses cipher/n handling, with `WEB_REMIX`'s endpoint and key rather than the video client's.

## The cipher problem

YouTube's player JavaScript (commonly called `player.js` or `base.js`) contains the current signature and n logic. The payload is large and changes frequently, so `ytx` detects the relevant shape and caches the derived state rather than hard-coding a function name.

### Detection ladder

YouTube adds decoys and changes minified names. `ytx` uses an FCIS ladder:

1. **Tier 0 — modern URL wrapper**: bounded marker/structural recognition. If validated, a single Bun/Node URL transform produces `sig`, `n`, and `alr`. Ambiguous candidates fail closed.
2. **Tier 1 — fast windowed regex**: scans a small window around discriminating markers.
3. **Tier 2 — wrapper signature detection**: structural validation of a wrapper shape.
4. **Tier 3 — global fallback**: broader full-file scanning for legacy players when the fast paths miss.

Modern players do not fall back to QuickJS. On legacy players, signature decryption always uses the embedded QuickJS wrapper context, while the legacy n-transform is engine-selected (Bun/Node or embedded QuickJS).

## Signature vs n-transform

| Feature | Signature (`s`) | N-Parameter |
| :--- | :--- | :--- |
| **Purpose** | Authentication / authorization | Throttling / rate limiting |
| **Shape** | Often a short call chain | Heavily obfuscated transform |
| **Modern execution** | Same URL-wrapper subprocess call as n | Same URL-wrapper subprocess call as n |
| **Legacy execution** | Embedded QuickJS (signature decryption always; engine selection does not apply) | Engine-selected Bun/Node or embedded QuickJS |
| **Frequency** | Rotates with the player version | Rotates with the player version |

## Cache and STS identity

The `signatureTimestamp` (STS) must belong to the same player version used for cipher work. When a cached player bundle is present, the persistent cache validates its full-content fingerprint and, when available, the STS before reuse; bundle-less legacy caches use their legacy gates. See `docs/internal/persistent-cache.md`.

## PO token is not a decoder

A PO token can satisfy an attestation gate. It cannot repair a broken signature or n transform. A run can mint PO successfully and still fail on decoding, and a 403 can have causes unrelated to either. See `docs/architecture/po-token.md`.
