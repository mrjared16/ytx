# JS Runtime Layer

`ytx` uses a hybrid JavaScript execution model designed to balance performance, compatibility, and zero-dependency requirements.

## Overview

YouTube uses complex JavaScript transformations for signature decryption and n-parameter transformation. `ytx` handles these using multiple execution strategies:

| Task | Engine | Rationale | Performance |
| :--- | :--- | :--- | :--- |
| **Signature Decryption** | QuickJS (CGO) | Low startup, ES2020+ support. | ~1ms |
| **N-Transform** | Bun / Node.js | JIT-accelerated for large scripts. | ~30ms |
| **Fallback** | QuickJS | Zero-dependency, embedded runtime. | ~80ms |

## Component Roles

### `Cipher` (`pkg/cipher.go`)
The orchestrator. It handles:
- Locating function names via regex.
- Extracting code via AST analysis.
- Managing the persistent cipher cache.

### `QuickJSRunner` (`pkg/quickjsrunner.go`)
An embedded CGO-based runner using QuickJS. It includes `browserStubsJS` to emulate a minimal browser environment (`window`, `document`, `navigator`). This runner is the primary engine for signature decryption.

### `SubprocessRunner` (`pkg/jsrunner.go`)
Manages external JS runtimes (Bun/Node.js). It minimizes overhead by:
- **Pre-spawning**: Starting the process in parallel with the `player.js` download.
- **IPC**: Communication via stdin/stdout using a JSON protocol.

## Surgical AST Extraction

To avoid executing the entire 3MB `player.js` for signatures, `ytx` performs a surgical extraction:

1.  **Parsing**: Uses `goja/parser` to build an AST.
2.  **Indexing**: Maps all function and variable definitions.
3.  **Dependency Resolution**: Recursively identifies the minimal closure of code required by the target function.
4.  **Result**: Reduces the JS payload from ~3MB to ~20KB, making evaluation nearly instantaneous.
