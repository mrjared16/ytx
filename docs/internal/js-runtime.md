# JS Runtime Layer

`ytx` has two JavaScript execution paths. Which one applies depends on the player shape that was detected, not on a global "fastest engine" preference.

## Modern URL-wrapper protocol (primary)

Modern players carry a validated URL-wrapper family such as the `set("alr","yes")` shape. For these players `ytx` detects the wrapper relationship with bounded marker discovery (`urlWrapperDetect`), then uses a single subprocess URL transform to obtain the signature, the transformed `n`, and the `alr` marker. This path does **not** fall back to QuickJS, bytecode compilation, or global n-function discovery. Ambiguous wrapper candidates fail closed instead of guessing.

Runtime selection for this path is handled by `newURLWrapperRunner` in `pkg/runtime_engine.go`:

- `auto` (the default) tries **Bun first**, then **Node.js**.
- Explicit `bun` and `node` are respected.
- `quickjs` is rejected: the URL-wrapper protocol requires a Bun/Node runtime.
- If neither Bun nor Node is available, the extraction returns an explicit error.
- Explicit `--js-engine` values are parsed by `internal/jsengine/engine.go`; `auto`, `quickjs`, `bun`, and `node` are the accepted names.

The Bun/Node process is a per-runner subprocess, not a long-lived daemon. The runner writes the prepared player bundle to a temporary file, starts the runtime, and exchanges JSON over stdin/stdout. It is created lazily and closed when the cipher or engine cache is rotated. The legacy pre-spawn mechanism is not consumed by modern players and is explicitly discarded.

Relevant source:

- `pkg/cipher_urlwrapper.go` — wrapper discovery and validation
- `pkg/cipher_signature.go` — modern signature call and strict error handling
- `pkg/runtime_engine.go` — engine selection and URL-wrapper runner lifecycle
- `internal/jsengine/subprocess.go` — Bun/Node process and JSON protocol
- `internal/jsengine/nrunner.mjs` — subprocess script used by the runner

## Legacy engine selection

Legacy players still use the pre-wrapper signature/n discovery paths. For those players `auto` resolves in this order:

1. Bun, if present in `PATH`
2. Node.js, by looking up the `node` binary only (`newAutoEngine`)
3. The embedded QuickJS runner

For legacy `auto`, a machine with only a `nodejs` binary and no `node` is not
discovered and falls through to QuickJS. The `nodejs` binary name is used by
the subprocess lookup (`lookupRuntime`) for an explicit `--js-engine node` and
for the modern URL-wrapper path after `node` is missing. It is not a universal
alias used by legacy `auto`.

An explicit `quickjs` selection is available for the legacy path. QuickJS is an in-process CGO runtime using `github.com/buke/quickjs-go`; it is no longer the primary modern engine. Engine selection applies to the legacy n-transform: legacy signature decryption always runs in the embedded QuickJS wrapper context (`ensureWrapperContext`), regardless of the selected engine.

Relevant source:

- `pkg/runtime_engine.go` — `newAutoEngine` fallback order
- `internal/jsengine/engine.go` — engine types and interface
- `internal/jsengine/quickjs.go` — embedded QuickJS runner
- `internal/jsengine/subprocess.go` — Bun/Node runner
- `internal/jsengine/stubs.go` — minimal browser globals for the embedded runtime

## Component roles

### `Cipher` (`pkg/cipher.go`)

The orchestrator. It:

- fetches and fingerprints `player.js`
- recognizes the modern URL-wrapper protocol before legacy detection
- locates legacy signature/n functions via regex and structural checks
- extracts wrapper/runtime code with `internal/jsextract`
- manages the persistent cipher cache and signature timestamp (STS)

### `internal/jsextract`

Legacy and wrapper-source extraction lives here, not in a package-level `quickjsrunner.go`/`jsrunner.go`/`nrunner.mjs`. It parses player JavaScript with `goja/parser`, indexes definitions, and resolves the dependency closure needed by the target function.

For the modern protocol, the runner receives a prepared full bundle and calls the validated wrapper; surgical extraction is a legacy-path optimization, not the primary modern flow.

## Performance notes

Do not infer fixed latencies from this document. Both paths depend on the player payload, hardware, process startup, and YouTube's current JavaScript. Benchmarks in `docs/guides/benchmarking.md` describe how to measure the current behavior. The Makefile's benchmark comments are local developer guidance, not guarantees.
