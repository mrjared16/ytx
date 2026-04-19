// Diagnostic tool to compare ytx vs yt-dlp extraction
//
// When YouTube updates break extraction, run this to quickly identify
// which function is broken by comparing intermediate values side-by-side.
//
// Usage: go run cmd/diagnose/main.go [VIDEO_ID]

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	ytx "github.com/mrjared16/ytx/pkg"
)

func main() {
	videoID := "dQw4w9WgXcQ" // Default test video
	if len(os.Args) > 1 {
		videoID = os.Args[1]
	}

	cookieFile := os.ExpandEnv("$HOME/.config/ytx/cookies.txt")

	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Printf("DIAGNOSTIC: ytx vs yt-dlp comparison for %s\n", videoID)
	fmt.Println("=" + strings.Repeat("=", 79))

	// Step 0: Check cookie/premium status
	fmt.Println("\n[0] Cookie status:")
	cookieStatus := checkCookieStatus(cookieFile)
	fmt.Printf("    %s\n", cookieStatus)

	// Step 1: Get yt-dlp result as baseline
	fmt.Println("\n[1] yt-dlp baseline (format 141):")
	ytdlpURL, ytdlpErr := getYtdlpURL(videoID, cookieFile)
	if ytdlpErr != nil {
		fmt.Printf("    ERROR: %v\n", ytdlpErr)
		fmt.Println("    yt-dlp cannot extract - YouTube may have changed something")
	} else {
		fmt.Printf("    URL: %s...%s\n", ytdlpURL[:50], ytdlpURL[len(ytdlpURL)-30:])
		ytdlpParams := parseURLParams(ytdlpURL)
		fmt.Printf("    itag=%s, n=%s, sig=%s...\n",
			ytdlpParams["itag"],
			ytdlpParams["n"],
			truncate(ytdlpParams["sig"], 20))
	}

	// Step 2: Get ytx result
	fmt.Println("\n[2] ytx extraction:")
	ytxResult, ytxErr := getYtxResult(videoID)
	if ytxErr != nil {
		fmt.Printf("    ERROR: %v\n", ytxErr)
		classification := ytx.ClassifyPOFailure(ytxErr)
		fmt.Printf("    classification: %s\n", classification)
		if ytx.IsLikelyPORequiredError(ytxErr) {
			fmt.Println("    hint: likely PO-gated; retry with explicit --po")
		}
	} else {
		fmt.Printf("    itag=%d, bitrate=%d\n", ytxResult.Itag, ytxResult.Bitrate)
		ytxParams := parseURLParams(ytxResult.URL)
		fmt.Printf("    n=%s, sig=%s...\n",
			ytxParams["n"],
			truncate(ytxParams["sig"], 20))
	}

	// Step 3: Test API response directly
	fmt.Println("\n[3] Direct API test (with signatureTimestamp):")
	testAPIDirectly(videoID, cookieFile)

	// Step 4: Verify URLs work
	fmt.Println("\n[4] URL verification:")
	if ytdlpURL != "" {
		status := checkURL(ytdlpURL)
		fmt.Printf("    yt-dlp URL: HTTP %s\n", status)
	}
	if ytxResult != nil {
		status := checkURL(ytxResult.URL)
		fmt.Printf("    ytx URL:    HTTP %s\n", status)
	}

	// Summary
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("DIAGNOSIS:")
	fmt.Println(strings.Repeat("=", 80))

	if ytdlpErr != nil && ytxErr != nil {
		fmt.Println("  Both fail - YouTube likely changed something fundamental")
		fmt.Println("  Check yt-dlp commits for recent fixes")
	} else if ytdlpErr == nil && ytxErr != nil {
		if ytx.IsLikelyPORequiredError(ytxErr) {
			fmt.Println("  yt-dlp works, ytx likely hit PO gating")
			fmt.Println("  Retry ytx with explicit --po")
		} else {
			fmt.Println("  yt-dlp works, ytx fails - check ytx implementation")
		}
	} else if ytdlpErr == nil && ytxResult != nil {
		if ytxResult.Itag == 141 {
			fmt.Println("  Both return itag 141 - extraction working")
		} else {
			fmt.Println("  yt-dlp gets 141, ytx gets different itag")
			fmt.Println("  Likely issue: signatureTimestamp or API headers")
		}
	}
}

func getYtdlpURL(videoID, cookieFile string) (string, error) {
	cmd := exec.Command("yt-dlp",
		"--cookies", cookieFile,
		"-f", "141",
		"-g",
		fmt.Sprintf("https://music.youtube.com/watch?v=%s", videoID))
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func getYtxResult(videoID string) (*ytx.Result, error) {
	ext, err := ytx.NewExtractor(ytx.ModeMusic, os.ExpandEnv("$HOME/.config/ytx/cookies.txt"))
	if err != nil {
		return nil, err
	}
	return ext.Extract(videoID)
}

func testAPIDirectly(videoID, cookieFile string) {
	cookies, err := ytx.ParseNetscapeCookieFile(cookieFile)
	if err != nil {
		fmt.Printf("    Cookie error: %v\n", err)
		return
	}

	sapisid := ytx.FindCookie(cookies, "SAPISID", "__Secure-3PAPISID")
	client := &http.Client{Timeout: 30 * time.Second}

	// Get visitorData
	watchURL := fmt.Sprintf("https://music.youtube.com/watch?v=%s", videoID)
	req, err := http.NewRequest(http.MethodGet, watchURL, nil)
	if err != nil {
		fmt.Printf("    Failed to build watch request: %v\n", err)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Cookie", ytx.BuildCookieHeader(cookies))
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("    Watch request failed: %v\n", err)
		return
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		fmt.Printf("    Failed reading watch response: %v\n", err)
		return
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("    Watch returned %d: %s\n", resp.StatusCode, truncate(string(body), 120))
		return
	}

	visitorDataRegex := regexp.MustCompile(`"visitorData"\s*:\s*"([^"]+)"`)
	var visitorData string
	if m := visitorDataRegex.FindSubmatch(body); len(m) >= 2 {
		visitorData = string(m[1])
	}

	// Get signatureTimestamp from player.js
	embedResp, err := client.Get(fmt.Sprintf("https://www.youtube.com/embed/%s?hl=en", videoID))
	if err != nil {
		fmt.Printf("    Embed request failed: %v\n", err)
		return
	}
	embedBody, err := io.ReadAll(embedResp.Body)
	embedResp.Body.Close()
	if err != nil {
		fmt.Printf("    Failed reading embed response: %v\n", err)
		return
	}
	if embedResp.StatusCode != http.StatusOK {
		fmt.Printf("    Embed returned %d: %s\n", embedResp.StatusCode, truncate(string(embedBody), 120))
		return
	}

	basejsPattern := regexp.MustCompile(`/s/player/[\w-]+/[\w./-]+/base\.js`)
	playerPath := basejsPattern.FindString(string(embedBody))
	if playerPath == "" {
		fmt.Println("    Failed to locate player path in embed response")
		return
	}
	playerResp, err := client.Get("https://www.youtube.com" + playerPath)
	if err != nil {
		fmt.Printf("    Player JS request failed: %v\n", err)
		return
	}
	playerJS, err := io.ReadAll(playerResp.Body)
	playerResp.Body.Close()
	if err != nil {
		fmt.Printf("    Failed reading player JS response: %v\n", err)
		return
	}
	if playerResp.StatusCode != http.StatusOK {
		fmt.Printf("    Player JS returned %d: %s\n", playerResp.StatusCode, truncate(string(playerJS), 120))
		return
	}

	stsPattern := regexp.MustCompile(`["']?signatureTimestamp["']?\s*[=:]\\s*(\d+)`)
	stsMatch := stsPattern.FindSubmatch(playerJS)
	var sts int
	if len(stsMatch) >= 2 {
		fmt.Sscanf(string(stsMatch[1]), "%d", &sts)
	}
	fmt.Printf("    signatureTimestamp: %d\n", sts)

	// Make API call
	origin := "https://music.youtube.com"
	sapisidhash := ytx.GenerateSAPISIDHASH(sapisid, origin)

	reqBody := map[string]interface{}{
		"videoId": videoID,
		"context": map[string]interface{}{
			"client": map[string]interface{}{
				"hl":            "en",
				"gl":            "US",
				"clientName":    "WEB_REMIX",
				"clientVersion": "1.20251216.01.00",
				"visitorData":   visitorData,
			},
		},
		"contentCheckOk": true,
		"racyCheckOk":    true,
		"playbackContext": map[string]interface{}{
			"contentPlaybackContext": map[string]interface{}{
				"html5Preference":    "HTML5_PREF_WANTS",
				"signatureTimestamp": sts,
			},
		},
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		fmt.Printf("    Failed to marshal API request: %v\n", err)
		return
	}
	apiURL := "https://music.youtube.com/youtubei/v1/player?key=AIzaSyC9XL3ZjWddXya6X74dJoCTL-WEYFDNX30&prettyPrint=false"

	apiReq, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(jsonBody))
	if err != nil {
		fmt.Printf("    Failed to build API request: %v\n", err)
		return
	}
	apiReq.Header.Set("Content-Type", "application/json")
	apiReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	apiReq.Header.Set("Origin", origin)
	apiReq.Header.Set("Referer", origin+"/")
	apiReq.Header.Set("X-Youtube-Client-Name", "67")
	apiReq.Header.Set("X-Youtube-Client-Version", "1.20251216.01.00")
	apiReq.Header.Set("Authorization", sapisidhash)
	apiReq.Header.Set("X-Origin", origin)
	apiReq.Header.Set("Cookie", ytx.BuildCookieHeader(cookies))
	apiReq.Header.Set("X-Youtube-Bootstrap-Logged-In", "true")
	apiReq.Header.Set("X-Goog-Visitor-Id", visitorData)

	apiResp, err := client.Do(apiReq)
	if err != nil {
		fmt.Printf("    API request failed: %v\n", err)
		return
	}
	respBody, err := io.ReadAll(apiResp.Body)
	apiResp.Body.Close()
	if err != nil {
		fmt.Printf("    Failed reading API response: %v\n", err)
		return
	}
	if apiResp.StatusCode != http.StatusOK {
		fmt.Printf("    API returned %d: %s\n", apiResp.StatusCode, truncate(string(respBody), 120))
		return
	}

	var result struct {
		StreamingData struct {
			AdaptiveFormats []struct {
				Itag     int    `json:"itag"`
				MimeType string `json:"mimeType"`
				Bitrate  int    `json:"bitrate"`
			} `json:"adaptiveFormats"`
		} `json:"streamingData"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		fmt.Printf("    Failed to parse API response: %v\n", err)
		return
	}

	fmt.Printf("    Audio itags: ")
	has141 := false
	for _, f := range result.StreamingData.AdaptiveFormats {
		if strings.HasPrefix(f.MimeType, "audio/") {
			fmt.Printf("%d(%dkbps) ", f.Itag, f.Bitrate/1000)
			if f.Itag == 141 {
				has141 = true
			}
		}
	}
	if has141 {
		fmt.Printf("✓")
	} else {
		fmt.Printf("✗ NO 141")
	}
	fmt.Println()
}

func parseURLParams(url string) map[string]string {
	params := make(map[string]string)
	if idx := strings.Index(url, "?"); idx != -1 {
		url = url[idx+1:]
	}
	for _, part := range strings.Split(url, "&") {
		if idx := strings.Index(part, "="); idx != -1 {
			params[part[:idx]] = part[idx+1:]
		}
	}
	return params
}

func checkURL(url string) string {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Head(url)
	if err != nil {
		return fmt.Sprintf("ERROR: %v", err)
	}
	defer resp.Body.Close()
	return fmt.Sprintf("%d", resp.StatusCode)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// checkCookieStatus verifies YouTube cookie authentication and premium status
func checkCookieStatus(cookieFile string) string {
	// Check if cookie file exists
	if _, err := os.Stat(cookieFile); os.IsNotExist(err) {
		return "NO COOKIE FILE - non-auth"
	}

	// Check for required auth cookies
	cookies, err := ytx.ParseNetscapeCookieFile(cookieFile)
	if err != nil {
		return fmt.Sprintf("COOKIE PARSE ERROR: %v", err)
	}

	sapisid := ytx.FindCookie(cookies, "SAPISID", "__Secure-3PAPISID")
	if sapisid == "" {
		return "MISSING SAPISID - non-auth"
	}

	// Check for premium by looking at YouTube response
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, "https://www.youtube.com/", nil)
	if err != nil {
		return fmt.Sprintf("REQUEST BUILD ERROR: %v", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Cookie", ytx.BuildCookieHeader(cookies))

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("REQUEST ERROR: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("REQUEST ERROR: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("RESPONSE READ ERROR: %v", err)
	}
	bodyStr := string(body)

	if strings.Contains(bodyStr, "YOUTUBE_PREMIUM_LOGO") {
		return "PREMIUM ✓ (authenticated with YouTube Premium)"
	} else if strings.Contains(bodyStr, "topbarLogoRenderer") {
		return "NON-PREMIUM (authenticated but no Premium subscription)"
	}
	return "NON-AUTH (cookies may be expired)"
}
