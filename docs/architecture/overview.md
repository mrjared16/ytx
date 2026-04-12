# Architecture Overview

`ytx` is designed with a single goal: **minimal latency for single-video extraction.** 

## The Dual-Client Strategy

Unlike generic extractors that use a "one-size-fits-all" approach, `ytx` selects the optimal YouTube client based on the requested mode:

| Client Mode | Internal Client | Advantage |
| :--- | :--- | :--- |
| **Video** | `ANDROID_VR` | Returns direct URLs. skip cipher decryption entirely (~220ms total). |
| **Music** | `WEB_REMIX` | Access to premium 256kbps audio (itag 141). Requires cipher but optimized. |

## Performance Philosophy

`ytx` achieves its speed through several key architectural decisions:

1.  **Native Go Execution**: Core logic is implemented in Go, eliminating Python/Node overhead.
2.  **FCIS Tiered Detection**: Using a "Functional Core / Imperative Shell" strategy with windowed regex scanning to analyze the 1.6MB `player.js` in ~100ms.
3.  **HTTP/2 Connection Pooling**: Reusing TCP connections for API calls and script fetching.
4.  **PO Token Optimization**: Persistent Botguard challenge caching to eliminate the 4s attestation penalty on repeat runs.

## High-Level Flow

```mermaid
graph TD
    A[Request: Video ID] --> B{Client Mode?}
    B -- Video --> C[ANDROID_VR API]
    C --> D[Direct URL - Result]
    B -- Music --> E[Tiered Detection Pipeline]
    E -- Tier 1 --> F[Fast Window Scan]
    F -- Fail --> G[Tier 2/3 Fallback]
    G --> H[Signature Decryption]
    H --> I[N-Transform]
    I --> J[Final URL - Result]
```

## Performance Comparison (Cold Start)

| Tool | Video Mode | Music Mode |
| :--- | :--- | :--- |
| yt-dlp | ~1200ms | ~1500ms |
| pytubefix| ~3000ms | ~2800ms |
| **ytx** | **~220ms** | **~1400ms** |

*Note: Warm starts for ytx Music Mode drop to **~350-450ms**.*
