# Extractor Breakage Playbook

Use this playbook when YouTube changes behavior and you need to quickly decide whether the problem is:

- extraction logic failing before a URL is produced
- extracted URLs being blocked or invalid
- throttling or rate limiting during real downloads
- a performance regression versus the last known-good baseline

The commands below are wired into the `Makefile` so you do not have to remember long invocations.

## Fast path

For a broken extraction, run these in order:

```bash
# 1) Compare ytx with yt-dlp and check API/cookie/cipher assumptions
make diagnose DEBUG_VIDEO=dQw4w9WgXcQ

# 2) Extract a fresh URL and probe whether it is immediately usable
make probe-url DEBUG_MODE=music DEBUG_VIDEO=dQw4w9WgXcQ

# 3) Download the full stream to see if it is blocked, throttled, or rate-limited
make probe-download DEBUG_MODE=music DEBUG_VIDEO=dQw4w9WgXcQ

# 4) Compare current behavior against the saved regression golden image
make test-regression

# 5) Fail if extraction is significantly slower than the golden baseline
make test-regression-perf
```

## Diagnostic commands

### `make diagnose`

Runs:

```bash
go run cmd/diagnose/main.go VIDEO_ID
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
- **ytx URL returns 403** → likely cipher / signature / n-transform breakage

### `make probe-url`

Extracts a fresh URL using the CLI, then performs a cheap HTTP probe.

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

- **HTTP 200 / 206** → URL is probably structurally valid
- **HTTP 403** → signature or `n` handling is usually wrong, or YouTube is actively blocking the URL
- **timeout / connection error** → network issue or an invalid URL host/path

### `make probe-download`

Extracts a fresh URL, downloads the full stream to `/dev/null`, and prints transfer stats.

```bash
make probe-download DEBUG_MODE=music DEBUG_VIDEO=dQw4w9WgXcQ
```

This is the quickest way to distinguish these cases:

- **Fails immediately with 403/4xx** → bad URL or blocked URL, usually requires a new extraction fix
- **Starts, then resets / short download / curl error** → rate limiting or blocking mid-stream
- **Completes, but speed is extremely low** → likely throttling
- **Completes at normal speed** → extractor is probably fine; issue may be in the caller/player

Notes:

- `music` mode is the safest default because the stream is smaller.
- `video` mode full downloads can be large; use `DEBUG_FIELD=audio_url` unless you specifically need the video stream.

## Regression checks

### `make test-regression`

Runs the regression suite against the saved golden image.

The golden now includes both cache states:

- `video/cold`
- `video/warm`
- `music/cold`
- `music/warm`

The test always checks correctness fields (mode, cache state, itag, bitrate, status, n-transform behavior). Timing drift is logged as a warning by default because network variance is expected.

### `make test-regression-perf`

Runs the same regression suite in strict performance mode:

```bash
STRICT_REGRESSION=1 go test -v ./pkg -run '^TestExtractorRegression$' -count=1
```

This mode fails only on **significant** slowdowns, not small timing jitter.

Current rule:

- fail when `total_ms` is slower than baseline by more than `max(250ms, 35%)`

Use it as a manual guardrail after performance-sensitive changes.

### `make test-regression-update`

Updates the golden image after an intentional change to extraction behavior or timing:

```bash
UPDATE_GOLDEN=1 go test -v ./pkg -run '^TestExtractorRegression$' -count=1
```

Only run this after you are confident the new behavior is correct.

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

If `probe-url` succeeds but `probe-download` is slow or unstable, suspect throttling / rate limiting rather than extraction.

### Performance feels slower after a patch

```bash
make test-regression
make test-regression-perf
```

If strict perf fails, inspect the profiled stages in the output:

- `visitor_data_ms`
- `player_api_ms`
- `cipher_init_ms`
- `n_transform_ms`
- `total_ms`

These usually tell you whether the regression is in auth/bootstrap, player API, cipher bootstrap, or JS runtime work.

## Related files

- `cmd/diagnose/main.go` — side-by-side ytx vs yt-dlp diagnostic tool
- `pkg/extractor_test.go` — regression suite and golden comparison
- `pkg/testdata/golden_image.json` — saved warm/cold baseline
- `DEVELOPMENT.md` — broader architecture and debugging notes
