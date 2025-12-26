# YTX - YouTube Stream URL Extractor

[![Go Report Card](https://goreportcard.com/badge/github.com/mrjared16/ytx)](https://goreportcard.com/report/github.com/mrjared16/ytx)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Version](https://img.shields.io/badge/Go-1.25+-blue.svg)](https://golang.org)

Fast YouTube stream URL extractor written in Go. Designed for integration with music players (MPV, TUI apps) and streaming applications.

## Features

- **Video Mode**: Extract video + audio URLs (**~240ms**) - no authentication required, configurable max resolution
- **Music Mode**: Extract 256kbps premium audio (~1100ms) - requires YouTube Premium cookies
- **Bulk Mode**: Extract multiple tracks with pipelined requests (3 tracks in ~1100ms)
- **Direct URLs**: No cipher required for video mode (ANDROID_VR client)
- **NDJSON Output**: Streaming JSON output for bulk operations

## Architecture

```
ytx/
├── cmd/
│   └── ytx/           # CLI entry point
├── pkg/               # Core packages
│   ├── client.go      # Client configuration factory (ANDROID_VR vs WEB_MUSIC)
│   ├── extractor.go   # Core extraction logic (API calls, stream selection)
│   ├── bulk.go        # Pipelined bulk extraction with rate limiting
│   ├── cipher.go      # Signature decryption (music mode only)
│   ├── auth.go        # SAPISIDHASH authentication for premium access
│   ├── constants.go   # Client constants, API endpoints, itag priorities
│   ├── types.go       # Data structures for requests/responses
│   └── cache.go       # Persistent cipher cache
└── docs/             # Documentation
```

### Dual-Client Architecture

| Mode | Client | Auth | Cipher | Speed | Quality |
|------|--------|------|--------|-------|---------|
| `video` | ANDROID_VR | None | No | **~260ms** | 1080p video + 160kbps audio |
| `music` | WEB_MUSIC | SAPISIDHASH | Yes | **~512ms (warm)** | 256kbps audio (itag 141/774) |

**Why two clients?**

- **ANDROID_VR**: Returns direct URLs (no cipher), but max 160kbps audio
- **WEB_MUSIC**: Requires cipher decryption, but provides premium 256kbps audio

## Installation

```bash
# Install from source
git clone https://github.com/mrjared16/ytx.git
cd ytx
make install

# Or go install (requires Go 1.25+)
go install github.com/mrjared16/ytx@latest
```

## Usage

### Video Mode (MPV Playback)

Extract video + audio URLs for players like MPV. No authentication required.

```bash
ytx video VIDEO_ID
ytx video VIDEO_ID --max-height 1080    # Limit to 1080p (saves bandwidth)
ytx video VIDEO_ID --max-height 720     # Limit to 720p
ytx video VIDEO_ID --subs               # Include subtitles (en by default)
ytx video VIDEO_ID --sub-langs all      # Include all available subtitles
ytx video VIDEO_ID --sub-langs en,es    # Include specific languages
```

**Example:**
```bash
ytx video hbl2Cuw75oE --subs
```

**Output:**
```json
{
  "video_url": "https://...",
  "audio_url": "https://...",
  "sub_url": "https://...",
  "video_itag": 137,
  "audio_itag": 140,
  "width": 1920,
  "height": 1080,
  "title": "Rick Astley - Never Gonna Give You Up",
  "author": "Rick Astley",
  "subtitles": [
    {"url": "https://...", "lang": "en", "name": "English"},
    {"url": "https://...", "lang": "en", "name": "English", "is_auto": true}
  ]
}
```

**MPV Integration:**
```bash
# Play video with separate audio track
result=$(ytx video hbl2Cuw75oE)
video_url=$(echo "$result" | jq -r '.video_url')
audio_url=$(echo "$result" | jq -r '.audio_url')
mpv "$video_url" --audio-file="$audio_url"

# Play with subtitles
result=$(ytx video hbl2Cuw75oE --subs)
mpv "$(echo $result | jq -r '.video_url')" \
    --audio-file="$(echo $result | jq -r '.audio_url')" \
    --sub-file="$(echo $result | jq -r '.sub_url')"

# Load all subtitle tracks
result=$(ytx video VIDEO_ID --subs)
mpv_args=("$(echo $result | jq -r '.video_url')")
mpv_args+=(--audio-file="$(echo $result | jq -r '.audio_url')")
while IFS= read -r sub; do
  mpv_args+=(--sub-file="$sub")
done < <(echo $result | jq -r '.subtitles[].url')
mpv "${mpv_args[@]}"
```

### Music Mode (Premium 256kbps)

Extract premium quality audio. Requires YouTube Premium account cookies.

```bash
ytx music VIDEO_ID --cookies PATH
```

**Example:**
```bash
ytx music hbl2Cuw75oE --cookies ~/cookies.txt
```

**Output:**
```json
{
  "url": "https://...",
  "itag": 141,
  "bitrate": 258454,
  "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"",
  "title": "Never Gonna Give You Up",
  "author": "Rick Astley"
}
```

### Bulk Mode (Playlists/Albums)

Extract multiple tracks with pipelined requests. Results stream as NDJSON.

```bash
ytx music --bulk ID1,ID2,ID3 --cookies PATH
```

**Example:**
```bash
ytx music --bulk hbl2Cuw75oE,dQw4w9WgXcQ,oHg5SJYRHA0 --cookies ~/cookies.txt
```

**Output (NDJSON - one JSON per line):**
```json
{"id":"dQw4w9WgXcQ","url":"https://...","itag":141,"bitrate":258454,"title":"Never Gonna Give You Up"}
{"id":"oHg5SJYRHA0","url":"https://...","itag":141,"bitrate":258454,"title":"RickRoll'D"}
{"id":"9bZkp7q19f0","url":"https://...","itag":141,"bitrate":258454,"title":"Gangnam Style"}
```

**Parsing in your application:**
```bash
# Read each line as it arrives (streaming)
ytx music --bulk id1,id2,id3 --cookies cookies.txt | while read line; do
  url=$(echo "$line" | jq -r '.url')
  title=$(echo "$line" | jq -r '.title')
  echo "Buffering: $title"
  # Start buffering/playing...
done
```

## Cookie Configuration

### Why Cookies?

Music mode requires YouTube Premium account cookies to:
1. Authenticate with YouTube Music API
2. Access premium audio quality (256kbps, itag 141/774)
3. Generate SAPISIDHASH authentication header

### Step 1: Export Cookies

Use a browser extension to export cookies in Netscape format:

**Recommended Extensions:**
- Chrome: [Get cookies.txt LOCALLY](https://chrome.google.com/webstore/detail/get-cookiestxt-locally/cclelndahbckbenkjhflpdbgdldlbecc)
- Firefox: [cookies.txt](https://addons.mozilla.org/en-US/firefox/addon/cookies-txt/)

**Export Steps:**
1. Log into [music.youtube.com](https://music.youtube.com) with your Premium account
2. Click: extension icon
3. Select "Export" or "Download cookies.txt"
4. Save as `cookies.txt`

### Step 2: Cookie File Location

YTX supports two methods for cookie configuration:

**Method 1: Default Config (Recommended)**
```bash
# Place cookies in XDG-compliant location
mkdir -p ~/.config/ytx
cp cookies.txt ~/.config/ytx/cookies.txt

# Now just run without --cookies flag
ytx music hbl2Cuw75oE
```

**Method 2: Explicit Path**
```bash
# Use --cookies flag for custom location
ytx music hbl2Cuw75oE --cookies ~/cookies.txt
```

**Priority:** `--cookies` flag takes priority over default config.

### Step 3: Cookie File Format

The file must be in Netscape cookie format:

```
# Netscape HTTP Cookie File
.youtube.com	TRUE	/	TRUE	1234567890	SAPISID	ABC123...
.youtube.com	TRUE	/	TRUE	1234567890	__Secure-3PAPISID	ABC123...
.youtube.com	TRUE	/	TRUE	1234567890	SID	DEF456...
.youtube.com	TRUE	/	TRUE	1234567890	HSID	GHI789...
.youtube.com	TRUE	/	TRUE	1234567890	SSID	JKL012...
```

**Required Cookies:**
- `SAPISID` or `__Secure-3PAPISID` (essential for SAPISIDHASH auth)
- `SID`, `HSID`, `SSID` (session cookies)
- `__Secure-1PSID`, `__Secure-3PSID` (secure session)

### Step 4: Verify Cookies

```bash
# Test default config location
ytx music hbl2Cuw75oE

# Test explicit path
ytx music hbl2Cuw75oE --cookies ~/cookies.txt

# Should return itag 141 or 774 (256kbps)
# If you get itag 140 (128kbps), cookies may be invalid or not Premium
```
# Netscape HTTP Cookie File
.youtube.com	TRUE	/	TRUE	1234567890	SAPISID	ABC123...
.youtube.com	TRUE	/	TRUE	1234567890	__Secure-3PAPISID	ABC123...
.youtube.com	TRUE	/	TRUE	1234567890	SID	DEF456...
.youtube.com	TRUE	/	TRUE	1234567890	HSID	GHI789...
.youtube.com	TRUE	/	TRUE	1234567890	SSID	JKL012...
```

**Required Cookies:**
- `SAPISID` or `__Secure-3PAPISID` (essential for SAPISIDHASH auth)
- `SID`, `HSID`, `SSID` (session cookies)
- `__Secure-1PSID`, `__Secure-3PSID` (secure session)

### Step 3: Verify Cookies

```bash
# Test that cookies work
ytx music dQw4w9WgXcQ --cookies ~/cookies.txt

# Should return itag 141 or 774 (256kbps)
# If you get itag 140 (128kbps), cookies may be invalid or not Premium
```

### Cookie Troubleshooting

| Error | Cause | Solution |
|-------|-------|----------|
| `SAPISID cookie not found` | Missing SAPISID in cookie file | Re-export cookies while logged in |
| `AUTH_FAILED` | Cookies expired or invalid | Export fresh cookies |
| `itag 140 instead of 141` | Not a Premium account | Use YouTube Premium account |
| `video not playable` | Region restriction or private video | Try different video |

### Cookie Security

- Keep `cookies.txt` private (contains your session)
- Don't commit to git (add to `.gitignore`)
- Cookies expire - re-export if errors occur
- Consider storing in `~/.config/ytx/cookies.txt`

## Error Handling

All errors are returned as JSON to stderr:

```json
{"error":"ERROR_CODE","message":"description"}
```

**Error Codes:**

| Code | Meaning |
|------|---------|
| `BAD_COOKIES` | Cookie file invalid or SAPISID missing |
| `AUTH_FAILED` | Authentication failed (expired cookies) |
| `PREMIUM_NEEDED` | Content requires Premium (rare) |
| `VIDEO_NOT_FOUND` | Video doesn't exist or is private |
| `CIPHER_FAILED` | Signature decryption failed |
| `API_ERROR` | Generic API error |

## Performance

| Operation | Time | Notes |
|-----------|------|-------|
| Video mode (single) | **~260ms** | No cipher, direct URLs |
| Music mode (cold) | **~1134ms** | First call, cipher fetched |
| Music mode (warm) | **~512ms** | Cipher cached to disk |
| Bulk (4 tracks, warm) | **~393ms** | Batched + pipelined (~98ms/track) |
| Bulk (10 tracks, est) | **~980ms** | 2 batches × ~490ms |
| Bulk (50 tracks, est) | **~4.9s** | 5 batches × ~980ms |

### Persistent Cipher Cache

The cipher (extracted from YouTube's base.js) is cached to disk at `~/.cache/ytx/cipher.json`. This eliminates the ~1500ms cipher fetch overhead on subsequent calls.

**Cache behavior:**
- **TTL:** 6 hours (cipher rarely changes)
- **Fail-forward:** If decryption fails, cache is invalidated and refreshed automatically
- **Size:** ~20KB (only extracted functions, not the full 2.3MB base.js)

```
First call:    ~1800ms (fetch cipher, save to disk)
Second call:    ~300ms (load from disk ~10ms + API ~290ms)
Cache stale:   ~1800ms (one-time refresh)
```

### Intelligent Batching

Bulk mode uses bucket-based pipelining to maximize throughput while staying under rate limits:

```
┌────────────────────────────────────────────────────────┐
│  Bucket 1: [id1...id10]  → 50ms stagger, parallel      │
│  Wait 200ms                                            │
│  Bucket 2: [id11...id20] → 50ms stagger, parallel      │
│  ...                                                   │
└────────────────────────────────────────────────────────┘
```

| Parameter | Default | Rationale |
|-----------|---------|-----------|
| Batch size | 10 | Safe concurrent window for premium accounts |
| Intra-batch delay | 50ms | Stagger within batch |
| Inter-batch delay | 200ms | Breathing room between batches |

Premium accounts have higher rate limits - this configuration is tuned for authenticated users.

## Integration Examples

### Rust TUI App

```rust
use std::process::Command;
use serde::Deserialize;

#[derive(Deserialize)]
struct MusicResult {
    url: String,
    itag: u32,
    bitrate: u32,
    title: String,
}

fn get_stream_url(video_id: &str, cookies: &str) -> Result<MusicResult, Box<dyn std::error::Error>> {
    let output = Command::new("ytx")
        .args(["music", video_id, "--cookies", cookies])
        .output()?;

    let result: MusicResult = serde_json::from_slice(&output.stdout)?;
    Ok(result)
}
```

### Shell Script (Playlist Player)

```bash
#!/bin/bash
COOKIES="$HOME/.config/ytx/cookies.txt"

# Get playlist video IDs (comma-separated)
IDS="dQw4w9WgXcQ,oHg5SJYRHA0,9bZkp7q19f0"

# Extract and play each track
ytx music --bulk "$IDS" --cookies "$COOKIES" | while read -r line; do
    url=$(echo "$line" | jq -r '.url')
    title=$(echo "$line" | jq -r '.title')

    echo "Now playing: $title"
    mpv --no-video "$url"
done
```

### Python Integration

```python
import subprocess
import json

def get_video_urls(video_id: str) -> dict:
    """Get video + audio URLs for MPV playback."""
    result = subprocess.run(
        ["ytx", "video", video_id],
        capture_output=True, text=True
    )
    return json.loads(result.stdout)

def get_music_url(video_id: str, cookies: str) -> dict:
    """Get premium 256kbps audio URL."""
    result = subprocess.run(
        ["ytx", "music", video_id, "--cookies", cookies],
        capture_output=True, text=True
    )
    return json.loads(result.stdout)

def get_bulk_urls(video_ids: list, cookies: str):
    """Stream bulk results as generator."""
    proc = subprocess.Popen(
        ["ytx", "music", "--bulk", ",".join(video_ids), "--cookies", cookies],
        stdout=subprocess.PIPE, text=True
    )
    for line in proc.stdout:
        yield json.loads(line)
```

## Development

```bash
# Build
make build

# Test
make test

# Lint
make lint

# Cross-compile
make build-linux

# Release builds
make release
```

## Requirements

- Go 1.25+
- Bun or Node.js (for n-parameter transform in music mode)
- For music mode: YouTube Premium account and Netscape-format cookies file

## Troubleshooting

### HTTP 403 on Music Mode

If URLs return 403, the cipher/n-transform may be broken due to YouTube updates.

```bash
# Quick check: compare with yt-dlp
yt-dlp -f 141 -g "https://music.youtube.com/watch?v=VIDEO_ID"  # Should work
./ytx music VIDEO_ID | jq -r '.url' | xargs curl -sI | head -1  # If 403, broken
```

### Diagnose Tool

Use the built-in diagnostic tool for side-by-side comparison:

```bash
go run cmd/diagnose/main.go VIDEO_ID
```

This checks cookie status, compares ytx vs yt-dlp, and verifies URLs work.

See [DEVELOPMENT.md](DEVELOPMENT.md#when-youtube-breaks-things) for debugging guide.

### yt-dlp Reference

When extraction breaks, check yt-dlp's fixes in:
- `yt_dlp/extractor/youtube/jsc/_builtin/vendor/yt.solver.core.js` - Function finder
- `yt_dlp/extractor/youtube/_video.py` - Main extraction logic

## License

MIT

## Contributing

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request