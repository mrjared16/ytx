package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	ytxpkg "github.com/mrjared16/ytx/pkg"
)

// Build information (set by Makefile)
var Version = "dev"
var BuildTime = ""

const usage = `ytx - Fast YouTube stream URL extractor

Usage:
    ytx video VIDEO_ID                     Extract video+audio URLs (for MPV)
    ytx music VIDEO_ID --cookies PATH      Extract 256kbps audio URL
    ytx music --bulk ID1,ID2,... --cookies PATH   Bulk extract (NDJSON output)

Modes:
    video   Fast extraction using ANDROID_VR client (no auth needed)
            Returns video and audio URLs for players like MPV

    music   Premium 256kbps audio using WEB_MUSIC client
            Requires Premium account cookies

Options:
    --cookies PATH   Path to Netscape-format cookies.txt (optional, defaults to ~/.config/ytx/cookies.txt)
    --bulk IDS       Comma-separated video IDs for bulk extraction

Output:
    video mode: {"video_url":"...","audio_url":"...","video_itag":137,"audio_itag":251}
    music mode: {"url":"...","itag":141,"bitrate":256000,"title":"..."}
    bulk mode:  NDJSON (one JSON per line, streamed)

Examples:
    ytx video hbl2Cuw75oE
    ytx music hbl2Cuw75oE
    ytx music hbl2Cuw75oE --cookies ~/cookies.txt
    ytx music --bulk hbl2Cuw75oE,dQw4w9WgXcQ --cookies ~/cookies.txt
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	mode := os.Args[1]

	switch mode {
	case "video":
		handleVideoMode()
	case "music":
		handleMusicMode()
	default:
		fmt.Fprintf(os.Stderr, "Unknown mode: %s\n\n", mode)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
}

func handleVideoMode() {
	if len(os.Args) < 3 {
		printError("MISSING_VIDEO_ID", "Usage: ytx video VIDEO_ID", "")
		os.Exit(1)
	}

	videoID := ytxpkg.ExtractVideoID(os.Args[2])
	if len(videoID) != 11 {
		printError("INVALID_VIDEO_ID", fmt.Sprintf("Invalid video ID: %s", os.Args[2]), "")
		os.Exit(1)
	}

	// Create extractor (no cookies needed for video mode)
	extractor, err := ytxpkg.NewExtractor(ytxpkg.ModeVideo, "")
	if err != nil {
		printError("INIT_FAILED", err.Error(), "")
		os.Exit(1)
	}

	result, err := extractor.ExtractVideo(videoID)
	if err != nil {
		errCode := categorizeError(err)
		printError(errCode, err.Error(), "")
		os.Exit(1)
	}

	output, _ := json.Marshal(result)
	fmt.Println(string(output))
}

func handleMusicMode() {
	args := os.Args[2:]

	var videoIDs []string
	var cookieFile string
	var isBulk bool

	// Parse arguments
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cookies":
			if i+1 < len(args) {
				cookieFile = args[i+1]
				i++
			}
		case "--bulk":
			if i+1 < len(args) {
				videoIDs = strings.Split(args[i+1], ",")
				isBulk = true
				i++
			}
		default:
			if !strings.HasPrefix(args[i], "-") && len(videoIDs) == 0 {
				videoIDs = []string{ytxpkg.ExtractVideoID(args[i])}
			}
		}
	}

	if len(videoIDs) == 0 {
		printError("MISSING_VIDEO_ID", "No video ID provided", "")
		os.Exit(1)
	}

	// Try --cookies flag first, then fallback to default config
	if cookieFile == "" {
		if ytxpkg.CookieFileExists() {
			defaultPath, _ := ytxpkg.GetDefaultCookiePath()
			cookieFile = defaultPath
		} else {
			printError("MISSING_COOKIES", "Music mode requires --cookies PATH or config file at ~/.config/ytx/cookies.txt", "")
			os.Exit(1)
		}
	}

	// Create extractor
	extractor, err := ytxpkg.NewExtractor(ytxpkg.ModeMusic, cookieFile)
	if err != nil {
		printError("BAD_COOKIES", err.Error(), "")
		os.Exit(1)
	}

	if isBulk {
		// Bulk mode - stream NDJSON
		extractor.BulkExtract(videoIDs)
	} else {
		// Single video
		videoID := videoIDs[0]
		if len(videoID) != 11 {
			printError("INVALID_VIDEO_ID", fmt.Sprintf("Invalid video ID: %s", videoID), "")
			os.Exit(1)
		}

		result, err := extractor.Extract(videoID)
		if err != nil {
			errCode := categorizeError(err)
			printError(errCode, err.Error(), "")
			os.Exit(1)
		}

		output, _ := json.Marshal(result)
		fmt.Println(string(output))
	}
}

func printError(code, message, debug string) {
	errResult := ytxpkg.ErrorResult{
		Error:   code,
		Message: message,
		Debug:   debug,
	}
	output, _ := json.Marshal(errResult)
	fmt.Fprintln(os.Stderr, string(output))
}

func categorizeError(err error) string {
	errStr := err.Error()

	switch {
	case contains(errStr, "SAPISID"):
		return "BAD_COOKIES"
	case contains(errStr, "not playable", "LOGIN_REQUIRED"):
		return "AUTH_FAILED"
	case contains(errStr, "PREMIUM", "premium"):
		return "PREMIUM_NEEDED"
	case contains(errStr, "not found", "unavailable"):
		return "VIDEO_NOT_FOUND"
	case contains(errStr, "cipher", "signature", "decrypt"):
		return "CIPHER_FAILED"
	default:
		return "API_ERROR"
	}
}

func contains(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if len(sub) > 0 && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
