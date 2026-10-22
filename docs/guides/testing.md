# Testing Guide

`ytx` has three test concerns. Pick the narrowest one that matches the change, and do not treat any target as a fully offline guarantee unless the selected test is actually offline.

## 1. `make test`

The target runs:

```bash
go test -v -timeout 180s ./pkg
```

This is **all tests in `./pkg`**, not a local-only subset. In particular it includes `TestExtractorRegression`, which performs live extraction against YouTube for video mode and, when the test's cookie path exists, music and music+PO modes. `make test` does not run `./internal/...`.

Consequences:

- It requires network access for the regression test.
- The regression tests hardcode `$HOME/.config/ytx/cookies.txt` (via
  `os.UserHomeDir()`); they do not consult `XDG_CONFIG_HOME`. The CLI's default
  cookie path does honor XDG, so the two paths can differ. Music subcases skip
  when that fixed test path is missing; they still require Bun/Node when they run.
- A failure can come from YouTube behavior, cookies, missing runtimes, or the regression timing bound, not only from local logic.
- The suite is not guaranteed to be fully green in the current live environment. Do not add a skip or suppress a failure just to make the target pass.

Explicit live stream probes (`TestMusicModeWithCookies*`, the bulk probes) remain opt-in through `RUN_LIVE_PROBE_TESTS=1`; `make test` does not set it.

### Deterministic offline checks

For a local-only signal, run exact test names rather than the whole package. Real examples:

```bash
go test -v ./pkg -run '^TestFindSigFunctionName$' -count=1
go test -v ./pkg -run '^TestURLWrapperStructureEmptyGateDecoyRejected$' -count=1
go test -v ./internal/cache -run '^TestLoadVisitorDataAllowStaleWithinGrace$' -count=1
```

Discover names with:

```bash
go test -list '^Test' ./pkg
```

`go test -short ./pkg` skips the regression and bulk live suites, but it is not a blanket offline promise for every remaining test. Select offline tests explicitly when that distinction matters.

## 2. `make test-regression`

The target runs:

```bash
go test -v -run 'TestExtractorRegression|TestNTransformIsolated|TestCacheIsolation' ./pkg -timeout 180s
```

What it covers:

- `TestExtractorRegression` — live extraction plus golden-image comparison
- `TestNTransformIsolated` — n-transform behavior in an isolated cache
- `TestCacheIsolation` — proves tests do not write the system cache

`TestExtractorRegression` compares correctness fields and also enforces a timing upper bound:

```text
actual total_ms <= baseline total_ms + 300ms
```

There is no warning-only mode. `TimingToleranceMs` is `300` in `pkg/extractor_setup_test.go`, and the comparison fails when the upper bound is exceeded. Network jitter can therefore fail this target even when extraction is functionally correct; read the reported `baseline`/`deviation` before assuming a code regression.

Related targets:

- `make test-regression-update` — runs with `UPDATE_GOLDEN=1` and rewrites the golden image. Use only after verifying the new behavior is correct; it masks regressions by replacing the baseline.
- `make test-regression-perf` — currently invokes the same regression test with `STRICT_REGRESSION=1`. That environment variable is not read by the Go test, so this is **not** a distinct performance gate or a different tolerance. Do not describe it as a strict slowdown check.

## 3. `make test-music-probe`

The target runs four explicitly opt-in live tests:

```bash
RUN_LIVE_PROBE_TESTS=1 go test -v \
  -run '^(TestMusicModeWithCookies|TestMusicModeWithCookiesPO|TestBulkMusicProbeRegression|TestBulkMusicProbeRegressionPO)$' \
  ./pkg -timeout 120s -count=1
```

What it covers:

- single-track music mode, normal and PO
- two-ID bulk music mode, normal and PO

Requirements:

- `$HOME/.config/ytx/cookies.txt` (hardcoded by these tests; the CLI's default
  path honors `XDG_CONFIG_HOME`, the tests do not)
- a working Bun or Node runtime
- network access to live YouTube endpoints
- for PO, the `jsdom` and `bgutils-js` modules available to that runtime (see `docs/architecture/po-token.md`)

These tests probe real stream URLs and are intentionally behind an explicit target. They can still fail because of YouTube-side 403s or PO gating; a failure is evidence to investigate, not a reason to hide the test.

## Recommended workflow

- Local logic change: run the narrowest exact-name test that covers it.
- Extraction, cache, or cipher change: `make test-regression` knowing it is live and timing-sensitive.
- Need direct streamability evidence: `make test-music-probe`.
- `make test` is a broad package run with live regression coverage, not the offline default.
