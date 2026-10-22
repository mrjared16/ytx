# Benchmarking Guide

`ytx` performance depends on network latency, player size, cache state, the selected client, and Bun/Node process startup. Measure your own run; do not treat old numbers or Makefile comments as guarantees.

## Running benchmarks

Use the existing `Makefile` targets:

```bash
# General benchmarks: all benchmarks in ./pkg at -benchtime=3x
make bench

# Rebuild, then run one cold and three disk-cache-warm music extractions of the same ID
make bench-bulk

# Compare Bun and Node process startup with ten invocations each
make bench-compare-runtime
```

### What `make bench-bulk` actually does

It is a small CLI exercise, not a 50-track throughput benchmark and not a
reliable timing report:

1. Builds `ytx` with `build-fast`.
2. Runs `./ytx music $(TEST_VIDEO)` once as the first invocation.
3. Runs the same `TEST_VIDEO` three more times as repeat invocations, each in a separate process.
4. Prints only the first run's itag/bitrate summary (parsed from its JSON
   stdout).

The recipe sends each run's stderr to `/dev/null`. When the external GNU
`time` is in use, its report goes to that stderr, so the elapsed line is
discarded; the three repeats also send stdout to `head -c 1 > /dev/null`, so
they print nothing. The timing prefix assumes a `time` that accepts `-f`
(GNU-style); a shell builtin without that option can fail before `ytx` runs.
There is therefore **no reliable per-run elapsed-time report** from
`make bench-bulk`, and its first line can be shell-dependent. For real
measurement, run `ytx music TEST_VIDEO --profile` for stage timings or run the
CLI under a shell-native `time` without redirecting stderr. Do not add a new
target for this.

It does not purge the cache first, so the first run is only cold if the cache was already empty.

`TEST_VIDEO` defaults to `dQw4w9WgXcQ`. It does not generate a 50-ID batch and does not exercise a batch n-transform. On an empty cache, the first run pays player fetch/analysis/runtime startup; the next three reuse the persistent on-disk artifacts (cipher, player bundle, runtime) across separate CLI invocations. No in-process cache state is shared between those invocations, and each is still a separate extraction with network calls.

### What `make bench` measures

`go test -bench=. -benchmem -benchtime=3x ./pkg` runs the package benchmarks, including `BenchmarkNTransform` and `BenchmarkVideoExtraction`. Because `-run` is not set, it also runs the package's tests first, including live `TestExtractorRegression`; this target is not offline. For benchmark-only runs, use the same command with `-run '^$'`. A few iterations are enough to spot a large change, not enough to characterize production latency.

## Metrics

### `cipher_init_ms`

Time spent waiting for the cipher after the API work has started. It includes cache lookup/restore or player fetch plus detection and runtime preparation, depending on the path. The value is highly cache- and player-dependent; there is no current fixed target.

### `n_transform_ms`

Legacy-only counter: time spent around `Cipher.TransformNContext` in
`unthrottleContext`. It is not updated by the modern URL-wrapper path
(`getStreamURLModernContext`), which calls `Cipher.TransformURLContext`
directly. Modern wrapper subprocess time is therefore not reported here; it is
only part of `total_ms`, and any uninstrumented remainder is reported as
`other_ms` by `finalizeProfile`. For legacy runs, Bun/Node or QuickJS may
execute the transform; process startup dominates short transforms, so compare
like-for-like runs rather than expecting a stable millisecond figure.

### `total_ms`

End-to-end extraction time from input ID to returned URL. Report it with the cache state, mode, client, runtime, player URL/fingerprint, and network conditions, otherwise the number is not comparable.

## Interpreting results

If `cipher_detail.sig_tier` is `global_fallback`, Tier 1 windowed detection missed its markers and the legacy path paid for a broader scan. Check `marker_miss` for the exact clues. This diagnostic does not apply when Tier 0 recognizes a modern URL-wrapper player, which skips legacy detection.

If legacy `n_transform_ms` is high, check:

- whether the legacy run is using Bun/Node or embedded QuickJS
- whether Bun/Node startup is the cost, especially on the first invocation
- whether the legacy runner is being recreated per call instead of reused from the engine cache

Because `n_transform_ms` is legacy-only, it cannot explain modern URL-wrapper latency; use `total_ms` and `other_ms` for that path instead.

## Reporting benchmarks

Always include:

- commit or `git describe --always --dirty`
- command/target
- mode and client
- cold/warm state and whether the cache directory was fresh
- JS engine/runtime and version output
- player URL/fingerprint if available
- whether cookies or `--po` were used
- network conditions

Do not publish a single timing table as a general performance claim. Historical measurements may be useful context only when labeled with their measurement date and environment.
