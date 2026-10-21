# YTX - YouTube Stream URL Extractor

[![Go Report Card](https://goreportcard.com/badge/github.com/mrjared16/ytx)](https://goreportcard.com/report/github.com/mrjared16/ytx)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Version](https://img.shields.io/badge/Go-1.25+-blue.svg)](https://golang.org)

Fast YouTube stream URL extractor written in Go. Designed for integration with music players (MPV, TUI apps) and streaming applications.

## Features

- **Video Mode**: Extract video + audio URLs with no authentication required; optional maximum resolution
- **Music Mode**: Extract the highest available authenticated audio format; requires a cookies file
- **Bulk Mode**: Extract multiple tracks sequentially and stream NDJSON results
- **Direct URLs**: Native video profiles return direct URLs without cipher, PO, or JS processing
- **NDJSON Output**: Streaming JSON output for bulk operations

Timing varies with network conditions, cache state, and JS runtime startup; this project does not publish a fixed latency SLO.

## Architecture

```
ytx/
├── cmd/
│   └── ytx/           # CLI entry point
├── pkg/               # Core packages
│   ├── client.go      # Video native profile chain and WEB_REMIX music config
│   ├── extractor.go   # Core extraction logic (API calls, stream selection)
│   ├── bulk.go        # Sequential bulk extraction with inter-track delay
│   ├── cipher.go      # Signature decryption (music mode only)
│   ├── auth.go        # SAPISIDHASH authentication for music access
│   ├── constants.go   # Client constants, API endpoints, itag priorities
│   ├── types.go       # Data structures for requests/responses
│   └── cache.go       # Persistent cipher cache
└── docs/             # Documentation
```

### Video and Music Paths

| Mode | Client | Auth | Cipher | Quality |
|------|--------|------|--------|---------|
| `video` | VISIONOS → ANDROID_VR → ANDROID → IOS (sequential fallback) | None | No | 1080p variants preferred, then 720p; otherwise highest available, subject to `--max-height`; direct audio |
| `music` | WEB_REMIX | SAPISIDHASH | Yes | Highest available supported audio (141/774 when offered; 251/140 fallbacks) |

Video mode tries each native client in order and keeps the first one that
returns direct adaptive streams whose end byte—and, when length discovery is
needed, start byte—probe successfully (HTTP 206 with matching Content-Range).
This validates reachability and byte access, not decoder or playback support.
Direct audio format and bitrate depend on the winning client and available
formats. The winning `client` and `user_agent` are included in the JSON output.
HTTP 429 or cancellation stops the fallback chain.

**Why separate video and music paths?**

- **Video path**: Guest native profiles return direct adaptive URLs without
  cipher, PO, or JS processing.
- **Music path**: `WEB_REMIX` uses authenticated cookies plus cipher and
  n-transform handling to select the highest available supported audio.

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

Examples use sample IDs; stream formats, bitrates, and URLs are illustrative
and vary by video, account, and network conditions.

### Video Mode (MPV Playback)

Extract video + audio URLs for players like MPV. No authentication required.

```bash
ytx video VIDEO_ID
ytx video VIDEO_ID --max-height 1080    # Limit to 1080p when available
ytx video VIDEO_ID --max-height 720     # Limit to 720p
ytx video VIDEO_ID --subs               # Include subtitles (en by default)
ytx video VIDEO_ID --chapters           # Include chapter metadata for mpv/players
ytx video VIDEO_ID --sub-langs all      # Include all available subtitles
ytx video VIDEO_ID --sub-langs en,es    # Include specific languages
```

**Example:**
```bash
ytx video VIDEO_ID_WITH_CHAPTERS --subs --chapters
```

**Illustrative output:**
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
  "client": "VISIONOS",
  "user_agent": "Mozilla/5.0 ...",
  "chapters": [
    {"start_time": 0, "end_time": 42.5, "title": "Intro"},
    {"start_time": 42.5, "end_time": 95.0, "title": "Verse 1"}
  ],
  "subtitles": [
    {"url": "https://...", "lang": "en", "name": "English"},
    {"url": "https://...", "lang": "en", "name": "English", "is_auto": true}
  ]
}
```

**MPV Integration:**
```bash
# Play video with separate audio track.
# Pass the returned user_agent so stream hosts see the winning client UA.
result=$(ytx video hbl2Cuw75oE)
video_url=$(echo "$result" | jq -r '.video_url')
audio_url=$(echo "$result" | jq -r '.audio_url')
ua=$(echo "$result" | jq -r '.user_agent // empty')
mpv "$video_url" --audio-file="$audio_url" ${ua:+--user-agent="$ua"}

# Play with subtitles
result=$(ytx video hbl2Cuw75oE --subs)
ua=$(echo "$result" | jq -r '.user_agent // empty')
mpv "$(echo $result | jq -r '.video_url')" \
    --audio-file="$(echo $result | jq -r '.audio_url')" \
    --sub-file="$(echo $result | jq -r '.sub_url')" \
    ${ua:+--user-agent="$ua"}

# Print chapter metadata for external player integration
result=$(ytx video MR6KSB6I_60 --chapters)
echo "$result" | jq '.chapters'

# Load all subtitle tracks
result=$(ytx video VIDEO_ID --subs)
mpv_args=("$(echo $result | jq -r '.video_url')")
mpv_args+=(--audio-file="$(echo $result | jq -r '.audio_url')")
if ua=$(echo $result | jq -r '.user_agent // empty'); [ -n "$ua" ]; then
  mpv_args+=(--user-agent="$ua")
fi
while IFS= read -r sub; do
  mpv_args+=(--sub-file="$sub")
done < <(echo $result | jq -r '.subtitles[].url')
mpv "${mpv_args[@]}"
```

### Music Mode (Highest Available Audio)

Extract the highest available supported audio. Music mode requires a cookies
file; higher-quality formats may require YouTube Premium and depend on the
track and account.

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

Extract multiple tracks sequentially in the order provided. Results stream as
NDJSON as each extraction completes.

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

Music mode requires a cookies file to:
1. Authenticate with the YouTube Music API
2. Request higher-quality audio formats when the account and track offer them
3. Generate the SAPISIDHASH authentication header

### Step 1: Export Cookies

Use a browser extension to export cookies in Netscape format:

**Recommended Extensions:**
- Chrome: [Get cookies.txt LOCALLY](https://chrome.google.com/webstore/detail/get-cookiestxt-locally/cclelndahbckbenkjhflpdbgdldlbecc)
- Firefox: [cookies.txt](https://addons.mozilla.org/en-US/firefox/addon/cookies-txt/)

**Export Steps:**
1. Log into [music.youtube.com](https://music.youtube.com) with your YouTube account (Premium may unlock higher formats when offered)
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
```

Common higher-quality formats are 141/774 when offered; 251/140 are valid
lower-quality fallbacks. A 140/251 result does not by itself prove bad cookies.
If a higher format is known to be available for the track but is missing,
re-check the cookies.

### Cookie Troubleshooting

| Error | Cause | Solution |
|-------|-------|----------|
| `SAPISID cookie not found` | Missing SAPISID in cookie file | Re-export cookies while logged in |
| `AUTH_FAILED` | Authentication required, or cookies invalid/expired | Re-export cookies if authenticated music still fails; guest video-mode `LOGIN_REQUIRED` is not cookie proof |
| `itag 140/251 instead of 141/774` | Higher format may not be offered for this track or account | Check a track where the higher format is known available; only then suspect cookies |
| `video not playable` | Region restriction or private video | Try different video |

### Cookie Security

- Keep `cookies.txt` private (contains your session)
- Don't commit to git (add to `.gitignore`)
- Cookies expire - re-export if errors occur
- Consider storing in `~/.config/ytx/cookies.txt`

## Error Handling

Single-extraction and command-level errors are returned as JSON to stderr.
Bulk per-track failures are emitted as NDJSON on stdout alongside successful
track results.

```json
{"error":"ERROR_CODE","message":"description"}
```

**Error Codes:**

| Code | Meaning |
|------|---------|
| `BAD_COOKIES` | Cookie file invalid or SAPISID missing |
| `AUTH_FAILED` | Authentication required, or cookies are invalid/expired. A guest video-mode `LOGIN_REQUIRED` result does not by itself prove bad cookies. |
| `PREMIUM_NEEDED` | Content requires Premium (rare) |
| `VIDEO_NOT_FOUND` | Video doesn't exist or is private |
| `CIPHER_FAILED` | Signature decryption failed |
| `API_ERROR` | Generic API error |

## Performance

Timing varies with network conditions, YouTube responses, cache state, and JS
runtime startup. This project does not publish a fixed latency SLO; cold and
warm runs have different costs. Use `--profile` for per-stage timings on your
machine.

### Persistent Cipher Cache

Cipher, player.js, and n-runtime artifacts are cached in the user cache
directory (`~/.cache/ytx` on typical Linux setups; `cipher.json` is the main
entry). Cipher entries expire after 6 hours.

- **Legacy signature path:** a failed signature decrypt may invalidate and
  refresh the cipher, then retry decryption with a fresh player response.
- **Modern URL-wrapper path:** fails closed instead of automatically retrying
  or purging the cache. Use `ytx cache purge` to clear cached data manually.

### Bulk Scheduling

Bulk mode processes IDs sequentially in the order provided, prefetches shared
visitor/cipher/n-runtime state once, and applies a randomized inter-track delay
(150ms base, ±100ms jitter). Each result is emitted as NDJSON when its
extraction completes. There is no fixed throughput or batch-size target.

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
    """Get the highest available music audio URL."""
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

# Optimized release binary
make build-release

# Local release bundle
make release
```

## Requirements

- Go 1.25+
- Bun preferred, with Node.js fallback, for music-mode n-parameter transforms
- For music mode: a Netscape-format cookies file; higher audio formats may require YouTube Premium and depend on the track and account

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
