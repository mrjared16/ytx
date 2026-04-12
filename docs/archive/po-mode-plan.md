# Explicit PO Mode Plan

## Purpose

This plan exists so a new contributor can join the workstream without reconstructing prior chat history.

The feature is an **explicit `--po` mode** for `ytx music`, optimized for response time, while preserving the current normal mode unchanged.

## Requirements

- Normal mode must remain unchanged and do **zero** POT work.
- PO work must start **only** when the user passes `--po`.
- Speed/response time is the top priority for PO mode.
- Final player/video-bound PO token must be minted **per video**.
- If a normal-mode stream probe fails in a POT-like way, show a warning suggesting `--po`.
- Add PO-mode live probe coverage in tests/Make targets.

## Current Baseline

See:

- `docs/architecture/extraction-pipeline.md`
- `docs/architecture/demo.md`

Current extraction pipeline:

1. `fetchVisitorData`
2. `getCachedCipher`
3. early STS gate
4. `callPlayerAPI`
5. `findBestAudioStream`
6. `getStreamURL`
   - `DecryptSignature`
   - `TransformN`

Important invariant: normal mode should keep this pipeline unchanged.

## Architecture Decision

### Modes

- default: normal mode, no POT work
- `--po`: explicit PO mode, eager POT warmup

No smart auto-mode is planned.

### PO runtime lane

Use a **separate POT runtime lane** so POT work does not contend with the existing signature/n-transform runtime locks.

Candidate implementations:

1. dedicated QuickJS runtime/context
2. dedicated persistent Bun subprocess

The spike bead decides which one has the best response-time tradeoff.

### Cache strategy

Optimize for a short-lived CLI:

- prefer **cross-invocation disk caches** over in-process caches
- keep in-process hot state as a bonus for bulk mode only

#### Disk-first caches

1. interpreter raw JS
2. interpreter compiled bytecode
3. integrity token

#### In-process only

1. live minter/runtime state
2. bulk-mode hot state

### Build-time precompile

Allowed only for static JS owned by the project:

- browser stubs
- bridge/harness JS
- static helper scripts

Do **not** rely on build-time shipping of dynamic YouTube JS artifacts.

## Token Reuse Rules

Safe to reuse across requests/session windows (subject to cache key + invalidation):

- interpreter JS
- compiled bytecode
- integrity token
- bulk-mode in-memory minter state

Do **not** assume safe cross-video reuse for the final token appended to playback URLs.

Rule: mint the final player/video-bound token fresh per video.

## Planned Execution Order

### 1. `br-pc6` — Spike earliest PO bootstrap + runtime choice

### 2. `br-3es` — Add explicit PO-mode models and disk cache layers

### 3. `br-830` — Implement in-process explicit --po coordinator and runtime lane

### 4. `br-k92` — Integrate explicit --po mode into extraction and CLI

### 5. `br-1cb` — Add PO-mode warnings, probe guidance, and diagnostics

### 6. `br-zb5` — Add explicit --po live probe tests and collaborator docs

## Repo Touchpoints

### CLI

- `cmd/ytx/main.go`

### Extractor and runtime

- `pkg/extractor.go`
- `pkg/types.go`
- `pkg/cache.go`
- `pkg/runtime.go`
- `pkg/quickjsrunner.go`
- `pkg/jsrunner.go`
- future `pkg/pot*.go`

### Tests and probes

- `pkg/extractor_test.go`
- `Makefile`

### Docs

- `docs/architecture/extraction-pipeline.md`
- `docs/architecture/demo.md`
- this file

## Collaboration Notes

When changing the plan:

1. update the relevant bead description/comment if scope changes
2. update this file if cache strategy, runtime choice, or execution order changes
3. keep normal-mode and PO-mode probe coverage separate
4. record any discovered invalidation rule here, not only in code

## Open Questions

- Which runtime lane is fastest in practice for POT work: dedicated QuickJS or dedicated Bun?
- What exact bootstrap/challenge metadata is available early enough to start useful PO work before URL finalization?
- Which failures are reliable enough to trigger the user-facing suggestion to retry with `--po`?
