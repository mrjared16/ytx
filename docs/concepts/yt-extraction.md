# YouTube Extraction Concepts

Understanding how YouTube delivers streams is critical for maintaining and improving `ytx`.

## The YouTube Player Architecture

When a stream is requested, the process follows these standard steps:

1.  **Innertube API Call**: The client calls `/youtubei/v1/player` with a video ID and client context.
2.  **Streaming Data**: The response contains `adaptiveFormats`, listing available itags (quality levels).
3.  **Encrypted Parameters**: URLs in these formats are often protected by:
    -   **Signature (`s`)**: A scrambled string that must be decrypted using the player's cipher logic.
    -   **N-Parameter**: A throttling token that must be transformed to allow full-speed downloads.

## The Innertube API

The API request specifies the `clientName` and `clientVersion`. `ytx` uses specific clients to exploit certain behaviors:
-   **ANDROID_VR**: Often returns URLs without encryption.
-   **WEB_REMIX**: The music-specific client required for high-quality audio.

## The Cipher Problem

YouTube's player JavaScript (`base.js`, ~3MB) contains the functions for signature decryption and n-parameter transformation. These functions change frequently (often weekly).

### Tiered Fallback
YouTube intentionally adds "decoy" functions to break simple regex-based scrapers. `ytx` uses an **FCIS Tiered Detection** pipeline:
1.  **Fast Path**: Optimized regex search in small (8KB) windows near discriminating markers.
2.  **Global Path**: Full AST/Regex validation used as a backup if the fast path fails.

## Signature vs. N-Transform

| Feature | Signature (`s`) | N-Parameter |
| :--- | :--- | :--- |
| **Purpose** | Authentication / Authorization | Throttling / Rate Limiting |
| **Complexity** | Simple (Slice, Swap, Reverse) | High (Obfuscated, heavily nested) |
| **Execution** | Extracted snippet (~20KB) | Wrapper Mode (surgical injection) |
| **Frequency** | Rotates with player version | Rotates with player version |
