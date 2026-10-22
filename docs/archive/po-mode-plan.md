# Explicit PO Mode Plan

> **ARCHIVED — historical planning document.** This describes the state and plan at the time it was written; it is **not** the current implementation. Retained for technical context. Original repository date: 2026-04-12.

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

(An earlier `docs/architecture/demo.md` reference was removed; that file no longer exists in the repository. The pipeline doc is the relevant current reference.)

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

The spike decides which one has the best response-time tradeoff.

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

### 1. Spike earliest PO bootstrap + runtime choice

### 2. Add explicit PO-mode models and disk cache layers

### 3. Implement in-process explicit --po coordinator and runtime lane

### 4. Integrate explicit --po mode into extraction and CLI

### 5. Add PO-mode warnings, probe guidance, and diagnostics

### 6. Add explicit --po live probe tests and collaborator docs

## Repo Touchpoints

### CLI

- `cmd/ytx/main.go`

### Extractor and runtime

- `pkg/extractor.go`
- `pkg/types.go`
- `pkg/cache.go`
- `pkg/runtime.go`
- `internal/jsengine/quickjs.go` (historical plan name: `pkg/quickjsrunner.go`)
- `internal/jsengine/subprocess.go` (historical plan name: `pkg/jsrunner.go`)
- `internal/jsengine/nrunner.mjs` (historical plan name: `pkg/nrunner.mjs`)
- future PO files (now `pkg/extractor_po.go`, `pkg/po_coordinator.go`, `pkg/potrunner.go`)

### Tests and probes

- `pkg/extractor_regression_test.go` (historical plan name: `pkg/extractor_test.go`)
- `pkg/extractor_setup_test.go`
- `Makefile`

### Docs

- `docs/architecture/extraction-pipeline.md`
- this file

## Collaboration Notes

When changing the plan:

1. update this document if cache strategy, runtime choice, or execution order changes
2. keep normal-mode and PO-mode probe coverage separate
3. record any discovered invalidation rule here, not only in code

## Open Questions

- Which runtime lane is fastest in practice for POT work: dedicated QuickJS or dedicated Bun?
- What exact bootstrap/challenge metadata is available early enough to start useful PO work before URL finalization?
- Which failures are reliable enough to trigger the user-facing suggestion to retry with `--po`?
