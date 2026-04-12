# Troubleshooting Guide

When extraction fails or returns 403 Forbidden URLs, use this guide to diagnose and resolve the issue.

## 1. Baseline Verification
Before diving into `ytx` internals, verify the issue against `yt-dlp`:

```bash
# Check if yt-dlp works
yt-dlp -f 141 -g "VIDEO_ID"
# Probe the URL
curl -sI "$(yt-dlp -f 141 -g "VIDEO_ID")" | head -1
```

## 2. Diagnosing ytx
Use the built-in diagnostic tool to compare `ytx` results with `yt-dlp`:

```bash
go run cmd/diagnose/main.go VIDEO_ID
```

This tool will compare the signature (`s`) and N-parameter (`n`) transformations between both tools to pinpoint which transformation is broken.

## 3. Common Issues

### HTTP 403 Forbidden
This usually indicates that the Cipher or N-Transform is outdated.
- **Solution**: Run `ytx cache purge` and try again. If it still fails, the extraction patterns in `pkg/cipher_detect.go` may need an update.

### Advanced Diagnosis with `--profile`
If patterns are failing, run with `--profile` and inspect the `cipher_detail`:
- **sig_tier**: Tells you if it hit Tier 1 (fast) or fell back to Tier 3 (global).
- **marker_miss**: A list of exactly which markers failed. If you see `set("alr"`, it means Tier 1 is breaking.

### Missing Premium Audio (itag 141)
If `ytx music` returns itag 140 instead of 141:
- **Solution**: Ensure your cookies are valid and loaded. Check if the `signatureTimestamp` (STS) is being correctly extracted and sent in the API request.

## 4. Maintenance Checklist

When YouTube breaks the extraction logic, check these `yt-dlp` reference files for the latest fixes:

| Component | yt-dlp File | Description |
| :--- | :--- | :--- |
| **Function Discovery** | `yt.solver.core.js` | AST-based function finding logic. |
| **API Parameters** | `_video.py` | Implementation of `_extract_formats_and_subtitles`. |
| **Signature Timestamp** | `_video.py` | Implementation of `_extract_signature_timestamp`. |
