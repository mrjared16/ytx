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
    ytx video VIDEO_ID [options]           Extract video+audio URLs (for MPV)
    ytx music VIDEO_ID [options]           Extract 256kbps audio URL
    ytx music --bulk ID1,ID2,... [options] Bulk extract (NDJSON output)
    ytx cache purge                        Clear all cached data

Modes:
    video   Fast extraction using ANDROID_VR client (no auth needed)
            Returns video and audio URLs for players like MPV

    music   Premium 256kbps audio using WEB_MUSIC client
            Requires Premium account cookies

    cache   Cache management commands
            purge - Remove all cached cipher and player.js data

Options:
    --cookies PATH      Path to Netscape-format cookies.txt (optional, defaults to ~/.config/ytx/cookies.txt)
    --bulk IDS          Comma-separated video IDs for bulk extraction
    --js-engine ENGINE  bun|node|auto (default: auto, tries Bun → Node)
    --profile           Output timing breakdown for each extraction stage

Output:
    video mode: {"video_url":"...","audio_url":"...","video_itag":137,"audio_itag":251}
    music mode: {"url":"...","itag":141,"bitrate":256000,"title":"..."}
    bulk mode:  NDJSON (one JSON per line, streamed)

    With --profile:
    {"url":"...","timings":{"visitor_data_ms":200,"player_api_ms":150,"cipher_init_ms":400,"n_transform_ms":5,"total_ms":755,"js_engine":"bun"}}

Examples:
    ytx video hbl2Cuw75oE
    ytx music hbl2Cuw75oE
    ytx music hbl2Cuw75oE --cookies ~/cookies.txt
    ytx music --bulk hbl2Cuw75oE,dQw4w9WgXcQ --cookies ~/cookies.txt
    ytx music VIDEO_ID --js-engine bun --profile
    ytx cache purge
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
	case "cache":
		handleCacheCommand()
	default:
		fmt.Fprintf(os.Stderr, "Unknown mode: %s\n\n", mode)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
}

func handleVideoMode() {
	args := os.Args[2:]

	var videoID string
	var profile bool

	// Parse arguments
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--profile":
			profile = true
		default:
			if !strings.HasPrefix(args[i], "-") && videoID == "" {
				videoID = ytxpkg.ExtractVideoID(args[i])
			}
		}
	}

	if videoID == "" {
		printError("MISSING_VIDEO_ID", "Usage: ytx video VIDEO_ID [--profile]", "")
		os.Exit(1)
	}

	if len(videoID) != 11 {
		printError("INVALID_VIDEO_ID", fmt.Sprintf("Invalid video ID: %s", videoID), "")
		os.Exit(1)
	}

	// Create extractor (no cookies needed for video mode)
	extractor, err := ytxpkg.NewExtractor(ytxpkg.ModeVideo, "")
	if err != nil {
		printError("INIT_FAILED", err.Error(), "")
		os.Exit(1)
	}

	// Enable profiling if requested
	if profile {
		extractor.SetProfile(true)
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
	var jsEngine string
	var profile bool

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
		case "--js-engine":
			if i+1 < len(args) {
				jsEngine = args[i+1]
				i++
			}
		case "--profile":
			profile = true
		default:
			if !strings.HasPrefix(args[i], "-") && len(videoIDs) == 0 {
				videoIDs = []string{ytxpkg.ExtractVideoID(args[i])}
			}
		}
	}

	// Set JS engine if specified
	if jsEngine != "" {
		engineType, err := ytxpkg.ParseEngineType(jsEngine)
		if err != nil {
			printError("INVALID_ENGINE", err.Error(), "")
			os.Exit(1)
		}
		ytxpkg.SetEngineType(engineType)
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

	// Enable profiling if requested
	if profile {
		extractor.SetProfile(true)
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

func handleCacheCommand() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: ytx cache purge")
		os.Exit(1)
	}

	switch os.Args[2] {
	case "purge":
		if err := ytxpkg.PurgeCache(); err != nil {
			printError("CACHE_ERROR", err.Error(), "")
			os.Exit(1)
		}
		cacheDir, _ := ytxpkg.GetCacheDir()
		fmt.Printf("Cache purged: %s\n", cacheDir)
	default:
		fmt.Fprintf(os.Stderr, "Unknown cache command: %s\n", os.Args[2])
		fmt.Fprintln(os.Stderr, "Usage: ytx cache purge")
		os.Exit(1)
	}
}
