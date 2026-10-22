# Extractor Breakage Playbook

Use this playbook when YouTube changes behavior and you need to decide whether the problem is:

- extraction logic failing before a URL is produced
- a PO token / attestation gate
- signature or n-transform (decoder) failure
- extracted URLs being blocked or invalid
- throttling or rate limiting during real downloads
- a performance change versus the last known-good baseline

The commands below map to existing `Makefile` targets and Go test names. Do not invent new targets for a one-off check.

## Fast path

```bash
# 1) Compare ytx with yt-dlp and check API/cookie/cipher assumptions
make diagnose DEBUG_VIDEO=dQw4w9WgXcQ

# 2) Extract a fresh URL and probe whether it is immediately usable
make probe-url DEBUG_MODE=music DEBUG_VIDEO=dQw4w9WgXcQ

# 3) Download the full stream to see if it is blocked, throttled, or rate-limited
make probe-download DEBUG_MODE=music DEBUG_VIDEO=dQw4w9WgXcQ

# 4) Compare current behavior against the saved regression golden image
make test-regression
```

`make test-regression` is a live test with a hard timing bound; see the regression section below before interpreting a failure.

The probe targets echo the full signed URL (`URL: ...`). Treat that as a short-lived credential and redact it before sharing logs or issues.

## Diagnostic commands

### `make diagnose`

Runs:

```bash
go run ./cmd/diagnose/main.go VIDEO_ID
```

This is the best first step when extraction breaks. It compares `ytx` with `yt-dlp` and prints:

- cookie / Premium auth status
- `yt-dlp` baseline URL
- `ytx` extracted URL
- direct API test with `signatureTimestamp`
- HTTP status for both extracted URLs

Interpretation:

- **yt-dlp fails too** → YouTube likely changed something fundamental
- **yt-dlp works, ytx fails** → bug is in `ytx`
- **ytx returns the wrong itag/quality** → check auth bootstrap data, headers, and `signatureTimestamp`
- **ytx URL returns 403** → could be a signature/n failure, a PO requirement, a PO mint failure, expired cookies, or YouTube-side blocking. Do not assume one cause.

### `make probe-url`

Extracts a fresh URL using the CLI, then performs a cheap HTTP Range probe.

The recipe's `curl` does not use `--fail`, so a transport-level success can still
be an HTTP 403. Read the printed `http=` status instead of relying on the
command exit code. A plain HEAD or ranged GET proves reachability only, not
decoder or playback correctness.

Default usage is music mode:

```bash
make probe-url DEBUG_MODE=music DEBUG_VIDEO=dQw4w9WgXcQ
```

By default it reads the `url` JSON field. For video mode, choose the field explicitly:

```bash
make probe-url DEBUG_MODE=video DEBUG_FIELD=audio_url DEBUG_VIDEO=dQw4w9WgXcQ
make probe-url DEBUG_MODE=video DEBUG_FIELD=video_url DEBUG_VIDEO=dQw4w9WgXcQ
```

Interpretation:

- **HTTP 206** → the URL is reachable and the probed range is readable; check `Content-Range` if you need byte-range evidence. This is **not** decoder or playback proof.
- **HTTP 200** → the host answered, but a range request got a full response; do not read it as range or decode success.
- **HTTP 403** → signature/n handling may be wrong, a PO token may be required, cookies may be stale, or YouTube is blocking the URL. A 403 alone cannot prove a player decoder bug.
- **timeout / connection error** → network issue or an invalid URL host/path

### `make probe-download`

Extracts a fresh URL, downloads the full stream to `/dev/null`, and prints transfer stats.

```bash
make probe-download DEBUG_MODE=music DEBUG_VIDEO=dQw4w9WgXcQ
```

This distinguishes:

- **Fails immediately with 403/4xx** → bad or blocked URL; could be decoder, PO, cookies, or YouTube-side
- **Starts, then resets / short download / curl error** → rate limiting or blocking mid-stream
- **Completes, but speed is extremely low** → likely throttling
- **Completes at normal speed** → extractor is probably fine; issue may be in the caller/player

Notes:

- `music` mode is the safest default because the stream is smaller.
- `video` mode full downloads can be large; use `DEBUG_FIELD=audio_url` unless you specifically need the video stream.

## Regression checks

### `make test-regression`

Runs:

```bash
go test -v -run 'TestExtractorRegression|TestNTransformIsolated|TestCacheIsolation' ./pkg -timeout 180s
```

`TestExtractorRegression` is live. It iterates video and (when cookies exist) music and music+PO, in cold and warm cache states, and compares the results with `pkg/testdata/golden_image.json`.

Correctness fields (mode, cache state, itag, bitrate, status, n-transform behavior, and PO mode) are compared. Timing is **not** warning-only. For each result with a positive baseline:

```text
upper limit = baseline total_ms + 300ms
fail if actual total_ms > upper limit
```

`TimingToleranceMs = 300` lives in `pkg/extractor_setup_test.go`, and the comparison fails with the baseline and deviation in the message. Because this is a live network test, normal jitter can trip the bound. That is a known limitation of the current test, not proof that the extractor regressed.

### `make test-regression-perf`

The target exists but is not a distinct gate:

```bash
STRICT_REGRESSION=1 go test -v ./pkg -run '^TestExtractorRegression$' -count=1 -timeout 180s
```

`STRICT_REGRESSION` is not read anywhere in the Go tests. The command therefore has the same comparison behavior as the regression test: the same `baseline + 300ms` upper bound. There is no `max(250ms, 35%)` rule and no warning-only strict mode in the current code. Do not rely on this target to detect a slowdown that `make test-regression` would not also report.

### `make test-regression-update`

```bash
UPDATE_GOLDEN=1 go test -v ./pkg -run '^TestExtractorRegression$' -count=1 -timeout 180s
```

This rewrites the golden image and skips golden comparison. Run it only after deliberately validating a behavior change. It is not a fix for a failing live regression; using it to silence a failure hides the difference from future runs.

## Common workflows

### URL looks wrong

```bash
make diagnose DEBUG_VIDEO=dQw4w9WgXcQ
make probe-url DEBUG_MODE=music DEBUG_VIDEO=dQw4w9WgXcQ
```

Then compare the `n` and `sig` values between `ytx` and `yt-dlp`.

### URL is returned but playback fails

```bash
make probe-url DEBUG_MODE=music DEBUG_VIDEO=dQw4w9WgXcQ
make probe-download DEBUG_MODE=music DEBUG_VIDEO=dQw4w9WgXcQ
```

If `probe-url` reports a 2xx `http=` status (not merely curl exit 0) but `probe-download` is slow or unstable, suspect throttling / rate limiting rather than extraction.

### Suspected PO gate

Normal-mode failures can carry a hint to retry with `--po` when `IsLikelyPORequiredError` matches. PO mode is not guaranteed to fix a 403: it only supplies a token. If minting fails, the run returns a PO error before URL finalization. If minting succeeds but the final URL still fails, investigate the signature/n decoder or the account/cookies.

### A modern player fails signature or n handling

The modern URL-wrapper path is fail-closed. Check:

- `--js-engine auto|bun|node` (QuickJS is rejected for modern players)
- Bun/Node availability in `PATH`
- `cipher_detail`/warnings for wrapper ambiguity or explicit `url wrapper` errors

There is no automatic cache purge-and-retry for modern wrapper failures.

### Performance feels slower after a patch

```bash
make test-regression
```

Because the timing check is a hard `baseline + 300ms` bound, inspect the profiled stages:

- `visitor_data_ms`
- `player_api_ms`
- `cipher_init_ms` (includes Analysis + Fetch)
- `cipher_detail.sig_tier` (check for `global_fallback`)
- `cipher_detail.marker_miss` (JSON array of broken regex markers)
- `n_transform_ms` — legacy `TransformNContext` only; it is not instrumented on
  the modern URL-wrapper path (`getStreamURLModernContext` calls
  `TransformURLContext` directly)
- `total_ms`

These usually tell you whether the change is in auth/bootstrap, player API, cipher bootstrap, or JS runtime work. If `cipher_detail.sig_tier` is `global_fallback`, the Tier 1 fast path in `pkg/cipher_detect.go` did not match; inspect the reported markers. A modern wrapper may bypass the legacy tiers entirely.

On a modern URL-wrapper run, wrapper subprocess time has no dedicated timing
field. It is included in `total_ms`; `finalizeProfile` reports any remainder
not covered by the instrumented legacy fields as `other_ms`. Do not read a
missing/zero `n_transform_ms` as "no transform work" on that path.

## Related files

- `cmd/diagnose/main.go` — side-by-side ytx vs yt-dlp diagnostic tool
- `pkg/extractor_regression_test.go` — regression test and golden comparison
- `pkg/extractor_setup_test.go` — golden helpers, timing comparison, and live probe helpers
- `pkg/testdata/golden_image.json` — saved baseline
- `docs/guides/testing.md` — test target semantics
- `DEVELOPMENT.md` — broader architecture and debugging notes
