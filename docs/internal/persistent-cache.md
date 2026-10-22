# Persistent Cache Architecture

`ytx` uses a versioned file cache for the player-derived cipher state. The cache is an optimization and an identity-checked artifact store; it is not a promise that a cached stream URL remains usable.

## Cache location

The cache is `filepath.Join(os.UserCacheDir(), "ytx")` (`NewCacheManager` in
`internal/cache/persistence.go`), unless a directory is injected for tests. On
Unix, `os.UserCacheDir()` uses an absolute `$XDG_CACHE_HOME` when set and
otherwise `$HOME/.cache`; other platforms use Go's platform cache root. If
`os.UserCacheDir()` fails, the code falls back to `$HOME/.cache/ytx`.

The Makefile's `CACHE_DIR` is a separate, fixed default (`$(HOME)/.cache/ytx`).
`make cache-info` and `make cache-clear` operate on that Makefile path only, so
they do not follow a custom `XDG_CACHE_HOME`; use `ytx cache info` to see the
path the CLI actually uses.

Current format version: **7** (`internal/cache/models.go`).

## Cache files

| File | Contents |
| :--- | :--- |
| `cipher.json` | Cipher metadata (version, timestamps, player URL, fingerprint, STS, sig/n fields, wrapper flags) |
| `player.js.gz` | Gzipped full player bundle; authoritative source for modern wrapper revalidation |
| `n_runtime.js` | Prepared legacy n-transform runtime |
| `wrapper_code.bin` | Legacy wrapper runtime bytecode |
| `cipher_code.bin` | Legacy raw JS/bytecode payload (not written for wrapper-mode caches) |
| `visitor.json` | Visitor data/session cache |
| `po_token.json` | PO token entries (versioned store) |

The large JS payload is deliberately kept out of `cipher.json`; it is stored separately or, for wrapper-mode caches, represented by `player.js.gz`.

## Metadata shape

Version-1 caches used the old flat shape below. The example timestamps are the original historical values and are retained, but this shape is no longer accepted:

```json
{
  "version": 1,
  "created_at": "2024-12-19T16:00:00Z",
  "expires_at": "2024-12-19T22:00:00Z",
  "player_url": "/s/player/abc123/base.js",
  "sig_function": "Xva",
  "sig_param": 96,
  "n_function": "nma",
  "js_code": "var z='...'.split(';');..."
}
```

Current caches use version 7. Representative fields (timestamps are RFC3339 values that vary per cache):

```json
{
  "version": 7,
  "created_at": "<RFC3339>",
  "expires_at": "<RFC3339>",
  "player_url": "/s/player/abc123/player_ias.vflset/en_US/base.js",
  "player_fingerprint": "<full-content digest>",
  "basejs_path": "/s/player/abc123/",
  "sig_function": "Xva",
  "sig_param": 96,
  "sig_uses_url_wrapper": true,
  "n_function": "",
  "signature_timestamp": 20438,
  "wrapper_build_id": "..."
}
```

Legacy caches may still carry `sig_uses_url_wrapper: false`, a non-empty `n_function`, `is_bytecode`, and extracted JS payloads. Modern wrapper caches discard cached `n=xp` and wrapper bytecode and rebuild from the cached full player bundle instead.

## Identity validation

A cache is only reused when its identity matches the player it describes:

- `CipherCache.Version` must equal `CurrentCacheVersion`.
- When `player.js.gz` is present, the full-content fingerprint computed from the loaded bundle must equal `PlayerFingerprint`.
- A positive STS derived from the bundle must equal the stored `SignatureTimestamp`.
- URL-wrapper (music) caches additionally require positive stored and derived STS values; a missing/stale STS is not accepted for this mode.
- Old sampled-region fingerprints do not match the full digest, so they migrate with one explicit refetch.
- Bundle-less legacy caches keep the legacy compatibility gates.

The bundle is loaded once and the same bytes feed both identity validation and cipher hydration, avoiding mixed-version state.

## TTL and stale-while-revalidate

The 6-hour fresh window and 18-hour stale-while-revalidate window describe the
persistent **disk** loader:

- `CacheTTL` is **6 hours**. A disk cache is fresh until `expires_at`.
- `cipherSWRGrace` is **18 hours** in `pkg/extractor.go`.
- An expired disk cache can still be used for up to 18 hours past expiry, provided it is version-compatible and passes identity validation. It is returned immediately while a background refresh is started.
- After that grace window, the disk cache is invalidated and a fresh player fetch runs.
- For the disk loader, the maximum serveable lifetime is roughly 24 hours from creation, and only the first 6 hours are "fresh". A stale hit is always paired with asynchronous refresh.

The in-memory cache has separate timing (`pkg/extractor_cipher.go`):

- A memory entry is returned as fresh until its stored expiry.
- After expiry, it can still be returned for up to 18 hours from its `updated` timestamp while an asynchronous refresh runs.
- `updated` is reset whenever a cipher is hydrated from disk or stored after a fetch, so a memory entry can outlive the disk loader's roughly 24-hour creation-age window. The 24-hour figure is therefore not a universal age cap.

Visitor data has its own TTL and a separate 2-hour stale-while-revalidate grace; it is not tied to the cipher TTL.

## Failure and retry behavior

A final stream URL returning HTTP 403 does **not** universally invalidate the cache or trigger a retry. The actual behavior is narrower:

- **Legacy signature path**: `DecryptSignature` errors enter `decryptWithRetryContext`. The cache is invalidated, a fresh cipher is fetched, and decryption is retried once (or twice for a wrapper-signature form). A successful first try persists artifacts; a retried one does not, to avoid poisoning the cache with a different player variant.
- **Modern URL-wrapper path**: failures are fail-closed. The modern path bypasses `decryptWithRetry`; a wrapper/subprocess/validation error is returned explicitly. There is no automatic purge-and-retry.
- **Cache hydration ambiguity**: if a cached wrapper player is ambiguous, it is invalidated and refetched rather than guessed at.
- **PO mode**: mint failure is a token error, not a cipher-cache invalidation. A successful mint can still be followed by a decoder failure.

There is no implemented "pending cache decryption" feature, and none should be documented as available.

## Manual management

```bash
ytx cache info                 # metadata, fingerprint, artifact presence
ytx cache purge                # remove cipher and artifact files
ytx cache refresh VIDEO_ID     # purge, then fetch and persist fresh artifacts
```

`Purge` removes `cipher.json`, `cipher_code.bin`, `player.js.gz`, `n_runtime.js`, `wrapper_code.bin`, and `po_token.json`. It intentionally **retains** `visitor.json`; use a separate full reset if visitor data itself must be discarded. `cache refresh` goes through the same purge and therefore also leaves the visitor cache in place.

Deleting only `cipher.json` is not equivalent to a full purge: the player/runtime artifacts would remain and may be revalidated or reused depending on the mode.
