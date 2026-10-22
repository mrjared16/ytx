# YTX - YouTube Stream URL Extractor

[![Go Report Card](https://goreportcard.com/badge/github.com/mrjared16/ytx)](https://goreportcard.com/report/github.com/mrjared16/ytx)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Version](https://img.shields.io/badge/Go-1.25.5%2B-blue.svg)](https://golang.org)

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
│   ├── ytx/             # CLI entry point (modes, options, JSON output)
│   └── diagnose/        # ytx vs yt-dlp diagnostic comparison
├── pkg/                 # Core extraction package
│   ├── extractor.go     # Extractor setup and options
│   ├── extractor_extract.go  # Extraction orchestration, video client chain, range probes
│   ├── extractor_stream.go   # Audio stream selection and quality ranking
│   ├── extractor_cipher.go   # Cipher cache selection and refresh
│   ├── extractor_visitor.go  # Visitor data and session state
│   ├── client.go        # Innertube client profiles and WEB_REMIX config
│   ├── auth.go          # SAPISIDHASH authentication
│   ├── cipher*.go       # Signature/n-transform and URL-wrapper handling
│   ├── extractor_po.go  # Opt-in PO-token extraction path
│   ├── po_*.go          # PO failure classification and coordination
│   ├── runtime*.go      # JS runtime selection and caching
│   ├── bulk.go          # Sequential NDJSON bulk extraction
│   └── types.go         # Wire and output types
├── internal/
│   ├── cache/           # Persistent cipher/player/runtime/visitor/PO cache
│   ├── jsengine/        # Bun/Node subprocess and embedded QuickJS engines
│   └── jsextract/       # Player.js function extraction helpers
└── docs/                # Additional documentation
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
  n-transform handling to select the highest available supported audio. It
  ranks audio quality first across usable candidates and only prefers a
  direct URL as an equal-quality tie-break, so it does not downgrade to a
  lower-quality direct format just to skip deciphering.

Modern URL-wrapper players require Bun or Node. `--js-engine` accepts `auto`
(default), `bun`, `node`, or `quickjs`; `quickjs` is legacy-only and cannot run
the modern URL-wrapper protocol.

## Installation

```bash
# Install from source
git clone https://github.com/mrjared16/ytx.git
cd ytx
make install

# Or install the CLI package directly (requires Go 1.25.5+ and a C compiler)
go install github.com/mrjared16/ytx/cmd/ytx@latest
```

`make install` builds the release binary with `CGO_ENABLED=1` and installs to
`/usr/local/bin` when run as root, otherwise to `~/.local/bin` (make sure that
directory is on your `PATH`). `go install` writes to `GOBIN` if set, otherwise
to `$(go env GOPATH)/bin`; that location is separate from `make install`.

A C compiler is required because the embedded QuickJS fallback uses cgo. Bun and
Node.js are runtime dependencies for music-mode n-transform/URL-wrapper work,
not a replacement for the build toolchain.

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
if ! result=$(ytx video hbl2Cuw75oE); then
  printf 'extraction failed\n' >&2
  exit 1
fi
video_url=$(printf '%s' "$result" | jq -r '.video_url')
audio_url=$(printf '%s' "$result" | jq -r '.audio_url')
ua=$(printf '%s' "$result" | jq -r '.user_agent // empty')
mpv_args=("$video_url" "--audio-file=$audio_url")
if [ -n "$ua" ]; then
  mpv_args+=(--user-agent="$ua")
fi
mpv "${mpv_args[@]}"

# Play with subtitles
if ! result=$(ytx video hbl2Cuw75oE --subs); then
  printf 'extraction failed\n' >&2
  exit 1
fi
video_url=$(printf '%s' "$result" | jq -r '.video_url')
audio_url=$(printf '%s' "$result" | jq -r '.audio_url')
sub_url=$(printf '%s' "$result" | jq -r '.sub_url')
ua=$(printf '%s' "$result" | jq -r '.user_agent // empty')
mpv_args=("$video_url" "--audio-file=$audio_url" "--sub-file=$sub_url")
if [ -n "$ua" ]; then
  mpv_args+=(--user-agent="$ua")
fi
mpv "${mpv_args[@]}"

# Print chapter metadata for external player integration
if ! result=$(ytx video MR6KSB6I_60 --chapters); then
  printf 'extraction failed\n' >&2
  exit 1
fi
printf '%s\n' "$result" | jq '.chapters'

# Load all subtitle tracks
if ! result=$(ytx video VIDEO_ID --subs); then
  printf 'extraction failed\n' >&2
  exit 1
fi
video_url=$(printf '%s' "$result" | jq -r '.video_url')
audio_url=$(printf '%s' "$result" | jq -r '.audio_url')
ua=$(printf '%s' "$result" | jq -r '.user_agent // empty')
mpv_args=("$video_url" "--audio-file=$audio_url")
if [ -n "$ua" ]; then
  mpv_args+=(--user-agent="$ua")
fi
while IFS= read -r sub; do
  mpv_args+=(--sub-file="$sub")
done < <(printf '%s' "$result" | jq -r '.subtitles[].url')
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

`--po` opts into the PO-token extraction path. It can help when YouTube
returns a PO/bot-gating response, but challenge availability and token minting
are not guaranteed, so `--po` is not a general 403 fix.

**Illustrative output:**
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
ytx music --bulk dQw4w9WgXcQ,oHg5SJYRHA0,9bZkp7q19f0 --cookies ~/cookies.txt
```

**Output (NDJSON - one JSON per line, in input order):**
```json
{"id":"dQw4w9WgXcQ","url":"https://...","itag":141,"bitrate":258454,"title":"Never Gonna Give You Up"}
{"id":"oHg5SJYRHA0","url":"https://...","itag":141,"bitrate":258454,"title":"RickRoll'D"}
{"id":"9bZkp7q19f0","url":"https://...","itag":141,"bitrate":258454,"title":"Gangnam Style"}
```

A failed track emits `id` plus an `error` message and no `url`/`itag` fields.
Per-track failures are data on stdout; they do not make the bulk command exit
nonzero. Setup failures (bad cookies, invalid engine, missing IDs) print one
JSON error on stderr and exit nonzero.

**Parsing in your application:**
```bash
# Read each line as it arrives; skip failed tracks instead of passing null URLs on.
ytx music --bulk dQw4w9WgXcQ,oHg5SJYRHA0,9bZkp7q19f0 --cookies cookies.txt |
while IFS= read -r line; do
  id=$(printf '%s' "$line" | jq -r '.id')
  err=$(printf '%s' "$line" | jq -r '.error // empty')
  if [ -n "$err" ]; then
    printf 'skipping %s: %s\n' "$id" "$err" >&2
    continue
  fi
  url=$(printf '%s' "$line" | jq -r '.url')
  title=$(printf '%s' "$line" | jq -r '.title')
  printf 'Buffering: %s\n' "$title"
  mpv --no-video "$url" || printf 'playback failed for %s\n' "$id" >&2
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
# XDG_CONFIG_HOME is honored; otherwise this is ~/.config/ytx
mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/ytx"
cp cookies.txt "${XDG_CONFIG_HOME:-$HOME/.config}/ytx/cookies.txt"
chmod 600 "${XDG_CONFIG_HOME:-$HOME/.config}/ytx/cookies.txt"

# Now just run without --cookies flag
ytx music hbl2Cuw75oE
```

**Method 2: Explicit Path**
```bash
# Use --cookies flag for custom location
ytx music hbl2Cuw75oE --cookies ~/cookies.txt
```

**Priority:** `--cookies` flag takes priority over default config. The default
path is `$XDG_CONFIG_HOME/ytx/cookies.txt`, falling back to
`~/.config/ytx/cookies.txt` when `XDG_CONFIG_HOME` is unset.

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
- `SAPISID` or `__Secure-3PAPISID` — ytx validates that at least one of these
  exists because it is used for SAPISIDHASH.
- `SID`, `HSID`, `SSID`, `__Secure-1PSID`, `__Secure-3PSID` — commonly present
  session cookies that help maintain an authenticated session. ytx does not
  validate these individually, so they are not documented as locally mandatory.

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

- Keep `cookies.txt` private (contains your session); `chmod 600` is recommended
- Don't commit to git (add to `.gitignore`)
- Cookies expire - re-export if errors occur
- Prefer `$XDG_CONFIG_HOME/ytx/cookies.txt` (or `~/.config/ytx/cookies.txt`)
- Do not paste cookie contents, signed media URLs, or other session secrets into
  logs, bug reports, or retained files; signed URLs are short-lived credentials

## Error Handling

Single-extraction failures and cache-command failures are returned as one JSON
object on stderr:

```json
{"error":"ERROR_CODE","message":"description"}
```

Bulk per-track failures are emitted as NDJSON on stdout alongside successful
track results, with an `id` and an `error` message but no `url`/`itag`. They do
not change the bulk process exit status. Usage and dispatch errors (no
arguments, unknown mode, missing/unknown cache subcommand) are plain text on
stderr, not JSON.

**Extraction Error Codes:**

| Code | Meaning |
|------|---------|
| `BAD_COOKIES` | Cookie file could not be parsed, or no `SAPISID`/`__Secure-3PAPISID` was found |
| `AUTH_FAILED` | Login/auth required, `not playable`/`LOGIN_REQUIRED`, or invalid/expired cookies. A private or unplayable video can land here depending on YouTube's reason, so this is not by itself proof of bad cookies. |
| `PREMIUM_NEEDED` | Error text indicates Premium is required |
| `VIDEO_NOT_FOUND` | Not found/unavailable, or the video client chain was exhausted with mostly unplayable results. Private videos may instead classify as `AUTH_FAILED`. |
| `CIPHER_FAILED` | Error text indicates a cipher, signature, or decrypt failure |
| `API_ERROR` | Other extraction/API failures, including cancellation and rate limiting |

**CLI Error Codes:**

| Code | Meaning |
|------|---------|
| `MISSING_VIDEO_ID` | No video ID supplied (single mode or cache refresh) |
| `INVALID_VIDEO_ID` | Extracted ID is not 11 characters |
| `INVALID_MAX_HEIGHT` | `--max-height` is not a positive integer |
| `MISSING_COOKIES` | Music mode has no `--cookies` path and no default cookie file |
| `INVALID_ENGINE` | `--js-engine` value is not `auto`, `bun`, `node`, or `quickjs` |
| `INIT_FAILED` | Video-mode extractor initialization failed |
| `CACHE_ERROR` | Cache purge/info/refresh operation failed |

## Performance

Timing varies with network conditions, YouTube responses, cache state, and JS
runtime startup. This project does not publish a fixed latency SLO; cold and
warm runs have different costs. Use `--profile` for per-stage timings on your
machine.

### Persistent Cipher Cache

Cipher, player.js, n-runtime, URL-wrapper bytecode, visitor, and PO-token
artifacts live in `os.UserCacheDir()/ytx` (on Linux, `$XDG_CACHE_HOME/ytx` when
set, otherwise `~/.cache/ytx`; `cipher.json` is the main cipher entry). The persistent **disk** entry is fresh for 6 hours and may then be
served stale for up to 18 more hours (stale-while-revalidate) while a refresh
runs in the background; for the disk loader that is roughly 24 hours from
creation. The **in-memory** entry is different: after its expiry it can be
served from memory for up to 18 hours after its last `updated` time, which is
reset whenever a cipher is hydrated or stored. So 24 hours is not a universal
age cap across both cache layers, and it is not a guarantee that the cached
player remains valid if YouTube rotates it sooner.

- **Signature retry:** a failed signature decrypt may invalidate and refresh
  the cipher, then retry decryption with a fresh player response.
- **Modern URL-wrapper path:** transforms fail closed (no legacy QuickJS
  fallback and no silent untransformed URL). A failed n-transform is not
  automatically purged and retried; automatic purge-and-retry recovery for
  this path is not implemented.
- **Manual reset:** `ytx cache purge` removes cached cipher, player.js,
  n-runtime, wrapper bytecode, and PO-token data. It retains `visitor.json`
  and is not a guaranteed fix for a YouTube-side change.

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

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        return Err(format!("ytx exited {}: {}", output.status, stderr.trim()).into());
    }

    let result: MusicResult = serde_json::from_slice(&output.stdout)?;
    Ok(result)
}
```

### Shell Script (Playlist Player)

```bash
#!/usr/bin/env bash
set -euo pipefail
COOKIES="${XDG_CONFIG_HOME:-$HOME/.config}/ytx/cookies.txt"

# Get playlist video IDs (comma-separated)
IDS="dQw4w9WgXcQ,oHg5SJYRHA0,9bZkp7q19f0"

# Extract and play each track; skip errored lines and failed URLs.
ytx music --bulk "$IDS" --cookies "$COOKIES" | while IFS= read -r line; do
    id=$(printf '%s' "$line" | jq -r '.id')
    err=$(printf '%s' "$line" | jq -r '.error // empty')
    if [ -n "$err" ]; then
        printf 'skipping %s: %s\n' "$id" "$err" >&2
        continue
    fi

    url=$(printf '%s' "$line" | jq -r '.url')
    title=$(printf '%s' "$line" | jq -r '.title')
    if [ -z "$url" ] || [ "$url" = "null" ]; then
        printf 'no playable URL for %s\n' "$id" >&2
        continue
    fi

    printf 'Now playing: %s\n' "$title"
    if ! mpv --no-video "$url"; then
        printf 'playback failed for %s\n' "$id" >&2
    fi
done
```

### Python Integration

```python
import json
import subprocess
import sys


def _run_json(args: list[str]) -> dict:
    result = subprocess.run(args, capture_output=True, text=True)
    if result.returncode != 0:
        raise RuntimeError(result.stderr.strip() or f"ytx exited {result.returncode}")
    return json.loads(result.stdout)


def get_video_urls(video_id: str) -> dict:
    """Get video + audio URLs for MPV playback."""
    return _run_json(["ytx", "video", video_id])


def get_music_url(video_id: str, cookies: str) -> dict:
    """Get the highest available music audio URL."""
    return _run_json(["ytx", "music", video_id, "--cookies", cookies])


def get_bulk_urls(video_ids: list[str], cookies: str):
    """Stream successful bulk results; stderr is inherited, not piped."""
    proc = subprocess.Popen(
        ["ytx", "music", "--bulk", ",".join(video_ids), "--cookies", cookies],
        stdout=subprocess.PIPE, text=True,
    )
    completed = False
    try:
        for line in proc.stdout:
            line = line.strip()
            if not line:
                continue
            item = json.loads(line)
            if item.get("error"):
                print(f"skipping {item.get('id')}: {item['error']}", file=sys.stderr)
                continue
            yield item
        completed = True
    finally:
        # Only stop the child when the consumer stopped early or raised.
        # On normal stdout EOF, wait for the child to finish and report its
        # own exit status instead of racing it with SIGTERM.
        if not completed and proc.poll() is None:
            proc.terminate()
        returncode = proc.wait()
        if proc.stdout is not None:
            proc.stdout.close()
        if completed and returncode != 0:
            raise RuntimeError(f"ytx bulk exited with status {returncode}")
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

- Go 1.25.5+ and a C compiler (cgo for the embedded QuickJS fallback)
- Bun preferred, with Node.js fallback, for music-mode n-parameter transforms
  and mandatory for the modern URL-wrapper protocol; `auto` may fall back to
  embedded QuickJS for legacy n-transform only
- For music mode: a Netscape-format cookies file; higher audio formats may
  require YouTube Premium and depend on the track and account

## Troubleshooting

### HTTP 403 on Music Mode

A 403 is not proof of one specific cause. Signed stream URLs expire, cookies
can expire or be rotated, YouTube may require PO/bot-gating, and a player
change can break cipher/n-transform handling. Check in this order:

1. Re-run the extraction. A URL captured earlier may simply be stale.
2. Compare with yt-dlp using a format that exists for the track. Format 141 is
   not offered for every track, so `-f 141` alone can fail on availability:

   ```bash
   # yt-dlp chooses an available audio format; prints only the HTTP status.
   if ytdlp_url=$(yt-dlp -f bestaudio/best -g "https://music.youtube.com/watch?v=VIDEO_ID"); then
     curl -s -o /dev/null -w 'yt-dlp URL: HTTP %{http_code}\n' -I "$ytdlp_url"
   else
     printf 'yt-dlp extraction failed\n' >&2
   fi
   ```

3. Probe ytx without printing or writing the signed URL. A HEAD request proves
   reachability only; a ranged GET adds byte-access proof. Neither proves
   decoder or playback support.

   ```bash
   if result=$(ytx music VIDEO_ID); then
     url=$(printf '%s' "$result" | jq -r '.url')
     curl -s -o /dev/null -w 'HEAD: HTTP %{http_code}\n' -I "$url"
     curl -s -o /dev/null -w 'range GET: HTTP %{http_code}\n' -H 'Range: bytes=0-0' "$url"
   else
     printf 'ytx extraction failed\n' >&2
   fi
   ```

   Expect `206` for the ranged GET (with a matching `Content-Range` when
   inspected). A `200`/`206` only means the host served bytes.

4. If errors mention bot/PO gating, retry with explicit `--po`. Token minting
   depends on challenge availability and can fail, so this is opt-in and not
   guaranteed to resolve a 403.

5. If both tools fail on the same track, suspect a YouTube-side change and
   follow the debugging guide. `ytx cache purge` clears cached cipher,
   player.js, n-runtime, wrapper bytecode, and PO data (visitor data remains),
   but it is a manual reset, not an automatic recovery guarantee.

Do not paste signed URLs or cookie values into logs, issues, or retained
files; they are short-lived session credentials.

### Diagnose Tool

Use the built-in diagnostic tool for side-by-side comparison:

```bash
go run cmd/diagnose/main.go VIDEO_ID
```

It checks local cookie status, compares ytx with yt-dlp, and performs HEAD
requests for URL reachability. The current tool asks yt-dlp for format `141`;
that format may not be offered for every track, and a successful HEAD check is
not playback or decoder proof.

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
