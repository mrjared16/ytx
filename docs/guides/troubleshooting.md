# Troubleshooting Guide

When extraction fails or returns a 403 Forbidden URL, identify the failing stage before changing code. A 403 is a symptom, not a diagnosis.

## 1. Baseline verification

Before diving into `ytx` internals, verify the issue against `yt-dlp`:

```bash
# Check whether yt-dlp can extract an available audio format
yt-dlp -f bestaudio/best -g "VIDEO_ID"
# Probe the URL without printing it
if ytdlp_url=$(yt-dlp -f bestaudio/best -g "VIDEO_ID"); then
  curl -s -o /dev/null -w 'HTTP %{http_code}\n' -I "$ytdlp_url"
else
  printf 'yt-dlp extraction failed\n' >&2
fi
```

Format 141 is not available for every track, and it may require the right
account/Premium access. A `yt-dlp -f 141` failure alone is therefore not a
universal baseline or proof that YouTube changed something fundamental. If
`yt-dlp` also fails with an available format, suspect a broader YouTube,
network, or account problem.

## 2. Diagnosing ytx

```bash
go run ./cmd/diagnose/main.go VIDEO_ID
# or
make diagnose DEBUG_VIDEO=VIDEO_ID
```

The diagnostic compares `ytx` and `yt-dlp`, prints cookie/Premium status, runs a direct API test with `signatureTimestamp`, and checks the returned URLs.

## 3. Common issues

### HTTP 403 Forbidden

Do not assume this is only an outdated cipher. Possible causes:

- signature or n transform is wrong
- a PO token is required, or a PO mint failed
- cookies/SAPISIDHASH are expired or lack access
- YouTube is blocking the request/IP or the returned URL
- a stale cache entry passed an old gate but no longer matches the player

First re-run with a deliberate cache purge:

```bash
ytx cache purge
ytx music VIDEO_ID --profile
```

`cache purge` removes cipher/player/PO artifacts but retains `visitor.json`. If the failure persists, inspect the warnings and cipher detail before editing detection patterns.

### `--po` did not fix 403

PO mode only supplies a token. Check whether minting succeeded:

- missing Bun/Node → runtime error before URL finalization
- missing `jsdom`/`bgutils-js` → mint subprocess import failure
- successful mint but still 403 → investigate signature/n, cookies, account, or YouTube-side blocking

See `docs/architecture/po-token.md` for the dependencies and failure split.

### Modern wrapper errors

Modern players require Bun or Node. QuickJS is rejected for this path. Check:

```bash
which bun
which node
ytx music VIDEO_ID --js-engine auto --profile
```

An ambiguous wrapper is fail-closed: a cached ambiguous player is invalidated and refetched, while a fresh ambiguous player returns an explicit error. Neither path silently uses legacy QuickJS. A modern wrapper failure also does not trigger an automatic cache purge-and-retry for a final 403.

### Advanced diagnosis with `--profile`

Inspect `cipher_detail`:

- **sig_tier**: legacy values are `primary`, `windowed`, `wrapper` (Tier 2),
  `global_fallback`, and the refetch variants `refetch`/`refetch_wrapper`; the
  modern protocol reports `urlwrapper`. `global_fallback` means the fast path missed.
- **marker_miss**: markers that failed to match
- **wrapper_tier**: separate field for wrapper recognition (`protocol`,
  `windowed`, `global_fallback`, `ambiguous`, or `skipped`), when present

If a modern wrapper was recognized, the legacy tier diagnostics may be skipped by design.

### Missing Premium audio (itag 141)

If `ytx music` returns a lower-quality itag:

- ensure cookies are valid and loaded
- verify `signatureTimestamp` is present and belongs to the same player
- check whether the account actually offers the requested format
- inspect the cache fingerprint/STS identity

The highest valid format depends on the account and track; a particular bitrate is not guaranteed.

## 4. Maintenance references

When YouTube changes the player, compare with `yt-dlp` before a large refactor:

| Component | Reference |
| :--- | :--- |
| Function discovery / wrapper shape | `pkg/cipher_detect.go`, `pkg/cipher_urlwrapper.go` |
| Modern transform runner | `internal/jsengine/subprocess.go`, `pkg/runtime_engine.go` |
| Signature timestamp | `pkg/cipher.go`, `pkg/cipher_signature.go` |
| API request construction | `pkg/extractor_api.go`, `pkg/types.go` |
| Regression behavior | `pkg/extractor_regression_test.go`, `pkg/extractor_setup_test.go` |

See `docs/extractor-breakage-playbook.md` for command-oriented diagnosis.
