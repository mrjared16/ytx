# Persistent Cache Architecture

To achieve sub-second warm-start times, `ytx` implements a robust file-based caching system.

## Cache Location
`~/.cache/ytx/cipher.json` (XDG-compliant)

## Cache Structure
The cache stores the following metadata for the current player version:

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

## Key Mechanisms

### 1. TTL-Based Invalidation
The cache has a default 6-hour TTL. After expiration, `ytx` will fetch a fresh `player.js` and re-derive the cipher.

### 2. Version Fingerprinting
The `player_url` acts as a version fingerprint. If YouTube rolls out a new player version, the URL changes, triggering an immediate cache refresh.

### 3. Fail-Forward Retry Logic
If a cached cipher fails (producing a 403 Forbidden on the final stream URL), `ytx` implements an automatic "fail-forward" strategy:
1. Invalidate the current cache.
2. Fetch the latest `player.js` immediately.
3. Re-extract the cipher and retry the decryption once.

This ensures reliability even when YouTube pushes a silent update mid-day.

## Management Commands

Users can manage the cache via the CLI:
- `ytx cache info`: Display current cache status and metadata.
- `ytx cache purge`: Delete the cache to force a fresh extraction.
- `ytx cache refresh <videoId>`: Force an update by performing a fresh extraction.
