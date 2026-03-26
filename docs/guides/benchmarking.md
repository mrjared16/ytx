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
- **Cold Target**: < 1200ms
- **Warm Target**: < 500ms

## Interpreting Results

If `cipher_init_ms` is high on a cold start (> 2500ms), it indicates a network bottleneck or inefficient pattern matching in `pkg/cipher.go`.

If `n_transform_ms` is high, check if `ytx` is falling back to QuickJS instead of using Bun/Node, or if the JS runtime pre-spawning logic is failing.
