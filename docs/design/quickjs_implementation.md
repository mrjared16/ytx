# Replace goja with QuickJS + tdewolff/parse for N-Transform

## TL;DR

> **Quick Summary**: Fix YouTube 403 errors by replacing goja (ES5.1-only, broken) with QuickJS (ES2020+ via CGO) for JS execution, and tdewolff/parse for JS AST parsing. Unify both sig-decryption and n-transform on QuickJS, then remove goja entirely.
>
> **Deliverables**:
> - `pkg/quickjsrunner.go` — QuickJS-based JSEngine implementation
> - Modified `pkg/cipher.go` — sig-decryption + AST parsing on tdewolff/parse + QuickJS
> - Modified `pkg/jsengine.go` — QuickJS as default engine, subprocess behind explicit flag
> - Modified `Makefile` — CGO_ENABLED=1
> - goja dependency fully removed
>
> **Estimated Effort**: Medium (2-3 days)
> **Parallel Execution**: NO — sequential (each phase depends on prior)
> **Critical Path**: Task 0 (gate) → Task 1 → Task 2 → Task 3 → Task 4 → Task 5 → Task 6 → Task 7

---

## Context

### Original Request
Fix YouTube 403 errors caused by broken n-transform. YouTube changed the n-function from a self-contained ES5 function (~5 deps, ~2KB) to a thin wrapper (`function(S){return MP[z[7]](this,19,S)}`) delegating to a 707KB mega-function with 533 transitive deps using ES2020+ syntax (?., ??, ??=). goja is ES5.1-only and can't parse modern player.js.

### Research Findings
- **yt-dlp approach** (`.yt-dlp-ref/`): Bundled solver script keeps ENTIRE IIFE body (~2.6MB), finds n-function NAME only, wires `_result.n = (n) => nFunc(n)`, runs in Node/Deno/QuickJS subprocess. Works but slow (~300-500ms).
- **tdewolff/parse**: Go ES2020+ parser with full AST, `Scope.Declared`/`Scope.Undeclared` for dep tracking, `Walk()` for traversal. Parser only — no execution.
- **QuickJS via CGO** (`buke/quickjs-go`): ES2020+ native execution, ~1-5ms startup, ~20-50ms for 1.5MB eval. Only option that delivers CLI-fast n-transform.
- **esbuild→goja**: ~550-700ms cold start (goja compile is bottleneck). Not acceptable for CLI.

### Interview Decisions
- **Sig-function**: Move to QuickJS too (remove goja entirely — reduce redundant deps)
- **Subprocess fallback**: Keep, but only via explicit `--js-engine bun|node` flag (not auto-fallback)
- **Tests**: After implementation
- **Platform**: Linux only (CGO is trivial)

### Metis Review
**Identified Gaps** (addressed in plan):
- Browser mock completeness: Must port ALL 60+ stubs from `nrunner.mjs` to QuickJS runner
- IIFE patching technique: Must replicate `_exposed['funcName']=funcName` injection
- `atob`/`btoa`: QuickJS lacks Node's `Buffer.from()` — implement as Go-side helpers
- Phase 0 gate: Verify CGO compilation works BEFORE any refactoring
- URL/TextEncoder polyfills: QuickJS may need them (verify in Phase 0)

---

## Work Objectives

### Core Objective
Replace goja with QuickJS(CGO) + tdewolff/parse to execute modern ES2020+ YouTube player.js for n-transform, achieving ~50-100ms cold-start latency (vs 300-500ms subprocess).

### Concrete Deliverables
- `pkg/quickjsrunner.go` — NEW: QuickJS JSEngine implementation
- `pkg/cipher.go` — MODIFIED: tdewolff/parse for AST, QuickJS for sig execution, goja removed
- `pkg/jsengine.go` — MODIFIED: QuickJS as default engine type
- `Makefile` — MODIFIED: CGO_ENABLED=1
- `go.mod` — MODIFIED: +buke/quickjs-go, +tdewolff/parse/v2, -dop251/goja

### Definition of Done
- [ ] `CGO_ENABLED=1 go build ./cmd/ytx` succeeds
- [ ] `./ytx video dQw4w9WgXcQ` returns valid stream URLs (no 403)
- [ ] `grep -r "dop251/goja" --include="*.go" .` returns nothing
- [ ] `go vet ./...` clean
- [ ] Existing tests pass: `go test ./pkg/... -timeout 180s`

### Must Have
- QuickJS as primary engine (no LookPath needed — it's CGO, always available)
- All browser mocks from `nrunner.mjs` ported to QuickJS runner
- IIFE patching to expose n-function from closure
- Subprocess fallback via explicit `--js-engine bun|node` flag only
- Both sig-decryption and n-transform on QuickJS

### Must NOT Have (Guardrails)
- No new CLI flags beyond `--js-engine quickjs`
- No logging/metrics not present in current code
- No refactoring of `extractor.go`, `client.go`, or files not listed in phases
- No error wrapping or abstraction layers beyond current style
- No build tags or conditional compilation
- No modification to `jsrunner.go` or `nrunner.mjs` (subprocess stays intact)
- No modification to test data files

---

## Verification Strategy

> **UNIVERSAL RULE: ZERO HUMAN INTERVENTION**
>
> ALL tasks are verifiable by running commands. No human testing needed.

### Test Decision
- **Infrastructure exists**: YES (`extractor_test.go`, `cipher_sig_test.go`)
- **Automated tests**: Tests-after
- **Framework**: `go test`

### Agent-Executed QA Scenarios (MANDATORY — ALL tasks)

**Verification Tool by Deliverable Type:**

| Type | Tool | How Agent Verifies |
|------|------|-------------------|
| Go compilation | Bash | `CGO_ENABLED=1 go build ./...` |
| Unit execution | Bash | `go test -v -run 'TestName' ./pkg` |
| E2E extraction | Bash | `./ytx video <id>` + parse JSON output |
| Dep removal | Bash | `grep -r "dop251/goja"` returns empty |

---

## Execution Strategy

### Sequential Execution (No Parallelism)

Each phase depends on the prior. Phase 0 is a hard gate.

```
Task 0: CGO Gate          ← MUST PASS before anything
    ↓
Task 1: Cleanup           ← Discard experiments, revert dirty files
    ↓
Task 2: QuickJS Runner    ← NEW file: pkg/quickjsrunner.go
    ↓
Task 3: AST Parser Swap   ← cipher.go: goja/parser → tdewolff/parse
    ↓
Task 4: Sig Execution     ← cipher.go: goja.New() → QuickJS
    ↓
Task 5: Engine Factory     ← jsengine.go: QuickJS default
    ↓
Task 6: Remove goja       ← go.mod cleanup
    ↓
Task 7: Tests + E2E       ← Verify everything works
```
