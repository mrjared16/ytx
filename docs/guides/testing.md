# Testing Guide

`ytx` uses three testing concerns. Pick the narrowest one that matches the change you made.

## 1. Logical tests — `make test`

Use this for normal development.

What it covers:
- local correctness
- isolated test behavior
- non-live test coverage in `./pkg`

What it avoids:
- explicit live stream probes against YouTube

Why:
- safest default workflow
- does not rely on repeatedly probing real stream URLs

## 2. Regression tests — `make test-regression`

Use this when touching extraction logic, cache behavior, cipher handling, or anything regression-sensitive.

What it covers:
- `TestExtractorRegression`
- `TestNTransformIsolated`
- `TestCacheIsolation`

Why:
- compares extraction behavior against the saved golden baseline
- keeps timing checks available without making normal `make test` runs carry all the regression intent

Related helpers:
- `make test-regression-update` — intentionally rewrite the golden image
- `make test-regression-perf` — fail on significant slowdown versus the golden baseline

## 3. YouTube-side probe tests — `make test-music-probe`

Use this only when you need direct evidence that extracted music URLs are streamable from YouTube.

What it covers:
- single-track music probe test
- 2-ID bulk music probe test

Requirements:
- `~/.config/ytx/cookies.txt`
- working Bun or Node runtime
- willingness to hit live YouTube endpoints

Why this is separate:
- these tests probe real stream URLs
- repeated runs increase unnecessary traffic to YouTube
- the repo intentionally keeps them behind an explicit make target

## Recommended workflow

- Most changes: `make test`
- Extraction or regression-sensitive changes: `make test-regression`
- Live streamability verification: `make test-music-probe`
