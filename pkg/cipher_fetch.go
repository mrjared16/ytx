package ytx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Base.js URL pattern
var basejsPattern = regexp.MustCompile(`/s/player/[\w-]+/[\w./-]+/base\.js`)

func httpGetBytes(client *http.Client, url string) ([]byte, error) {
	return httpGetBytesContext(context.Background(), client, url)
}

func httpGetBytesContext(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	return io.ReadAll(resp.Body)
}

func fetchPlayerJS(videoID string, httpClient *http.Client, playerBaseURL string) (string, []byte, error) {
	return fetchPlayerJSContext(context.Background(), videoID, httpClient, playerBaseURL)
}

func playerPageURLs(videoID, playerBaseURL string, embedFirst bool) []string {
	pageURLs := make([]string, 0, 4)
	addPageURL := func(base string, first string, second string) {
		pageURLs = append(pageURLs,
			fmt.Sprintf(first, base, videoID),
			fmt.Sprintf(second, base, videoID),
		)
	}

	if embedFirst {
		addPageURL(playerBaseURL, "%s/embed/%s?hl=en", "%s/watch?v=%s")
		if playerBaseURL != PlayerJSURLBase {
			addPageURL(PlayerJSURLBase, "%s/embed/%s?hl=en", "%s/watch?v=%s")
		}
		return pageURLs
	}

	addPageURL(playerBaseURL, "%s/watch?v=%s", "%s/embed/%s?hl=en")
	if playerBaseURL != PlayerJSURLBase {
		addPageURL(PlayerJSURLBase, "%s/watch?v=%s", "%s/embed/%s?hl=en")
	}
	return pageURLs
}

func fetchPlayerJSContext(ctx context.Context, videoID string, httpClient *http.Client, playerBaseURL string) (string, []byte, error) {
	pageURLs := playerPageURLs(videoID, playerBaseURL, true)

	return fetchPlayerJSFromPageURLsContext(ctx, httpClient, playerBaseURL, pageURLs)
}

func fetchPlayerJSWatchFirstContext(ctx context.Context, videoID string, httpClient *http.Client, playerBaseURL string) (string, []byte, error) {
	return fetchPlayerJSFromPageURLsContext(ctx, httpClient, playerBaseURL, playerPageURLs(videoID, playerBaseURL, false))
}

func fetchPlayerJSFromPageURLsContext(ctx context.Context, httpClient *http.Client, playerBaseURL string, pageURLs []string) (string, []byte, error) {
	var lastErr error
	for _, pageURL := range pageURLs {
		body, err := httpGetBytesContext(ctx, httpClient, pageURL)
		if err != nil {
			lastErr = err
			continue
		}

		playerPath := preferredPlayerPathFromPage(body)
		if playerPath == "" {
			lastErr = fmt.Errorf("unable to find base.js URL in %s", pageURL)
			continue
		}

		pageBaseURL := playerBaseURL
		if parsedPageURL, parseErr := url.Parse(pageURL); parseErr == nil {
			pageBaseURL = parsedPageURL.Scheme + "://" + parsedPageURL.Host
		}

		playerJS, err := httpGetBytesContext(ctx, httpClient, pageBaseURL+playerPath)
		if err != nil {
			lastErr = fmt.Errorf("failed to fetch player JS from %s: %w", pageURL, err)
			continue
		}

		return playerPath, playerJS, nil
	}

	if lastErr == nil {
		lastErr = errors.New("unable to fetch player JS")
	}

	return "", nil, lastErr
}

func preferredPlayerPathFromPage(body []byte) string {
	matches := basejsPattern.FindAllString(string(body), -1)
	if len(matches) == 0 {
		return ""
	}

	seen := make(map[string]struct{}, len(matches))
	paths := make([]string, 0, len(matches))
	for _, match := range matches {
		if _, ok := seen[match]; ok {
			continue
		}
		seen[match] = struct{}{}
		paths = append(paths, match)
	}

	best := paths[0]
	bestScore := playerPathPreferenceScore(best)
	for _, path := range paths[1:] {
		score := playerPathPreferenceScore(path)
		if score > bestScore || (score == bestScore && len(path) < len(best)) {
			best = path
			bestScore = score
		}
	}

	return best
}

func playerPathPreferenceScore(path string) int {
	score := 100
	if strings.Contains(path, "player_embed") {
		score += 50
	}
	if strings.Contains(path, "/embed") {
		score -= 10
	}
	if strings.Contains(path, "player_ias") {
		score -= 50
	}
	return score
}
