# Benchmarking Guide

`ytx` is performance-obsessed. We track several metrics to ensure we maintain our lead in extraction speed.

## Running Benchmarks

Use the `Makefile` targets to run standardized benchmarks:

```bash
# General benchmarks
make bench

# Bulk extraction benchmarks (50 tracks)
make bench-bulk

# Compare different JS runtimes
make bench-compare-runtime
```

## Key Metrics

### `cipher_init_ms`
The time taken to acquire a working cipher. 
- **Cold Target**: < 1500ms
- **Warm Target**: < 10ms

### `n_transform_ms`
The time taken to execute the N-parameter transformation.
- **Bun Target**: < 40ms
- **QuickJS Target**: < 100ms

### `total_ms`
The end-to-end time from input ID to playable URL output.
- **Cold Target**: < 1500ms
- **Warm Target**: < 500ms

## Interpreting Results

If `cipher_analyze_ms` (found in `cipher_detail`) is high on a cold start (> 500ms), it indicates that the **Tier 1 Fast Path** in `pkg/cipher_detect.go` has failed and the system is paying the cost of a full-file regex scan (Tier 3). Check `marker_miss` for clues.

If `n_transform_ms` is high, check if `ytx` is falling back to QuickJS instead of using Bun/Node, or if the JS runtime pre-spawning logic is failing.
