package ytx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mrjared16/ytx/internal/cache"
)

func (e *Extractor) applyPOToken(streamURL string) string {
	if !e.poMode || (e.poPlayerToken == "" && e.poURLToken == "") {
		return streamURL
	}
	token := e.poURLToken
	if token == "" {
		token = e.poPlayerToken
	}
	parsedURL, err := url.Parse(streamURL)
	if err != nil {
		return streamURL
	}
	q := parsedURL.Query()
	if q.Get("pot") == "" {
		q.Set("pot", token)
		parsedURL.RawQuery = q.Encode()
	}
	return parsedURL.String()
}

func (e *Extractor) ensurePOTokenContext(ctx context.Context, videoID string) (string, string, error) {
	if !e.poMode {
		return "", "", nil
	}
	if videoID == "" {
		return "", "", fmt.Errorf("missing video id")
	}

	if e.cacheManager != nil {
		if cache, err := e.cacheManager.LoadPOToken(videoID, e.visitorData, e.sessionIndex); err == nil && cache.PlayerToken != "" && cache.URLToken != "" {
			return cache.PlayerToken, cache.URLToken, nil
		}
	}

	t0 := time.Now()
	challenge, err := e.loadPOTokenChallengeContext(ctx, videoID)
	if err != nil {
		return "", "", err
	}
	if e.profile {
		e.timings.POChallengeMs = time.Since(t0).Milliseconds()
	}

	e.poChallenge = challenge
	t1 := time.Now()
	playerToken, urlToken, ttl, err := e.mintPOTokenContext(ctx, challenge)
	if err != nil {
		return "", "", err
	}
	if e.profile {
		e.timings.POMintMs = time.Since(t1).Milliseconds()
	}
	if ttl <= 0 {
		ttl = defaultPOTokenTTL
	}

	if e.cacheManager != nil {
		_ = e.cacheManager.SavePOToken(&cache.POTokenCache{
			Token:        urlToken,
			PlayerToken:  playerToken,
			URLToken:     urlToken,
			VideoID:      videoID,
			VisitorData:  e.visitorData,
			SessionIndex: e.sessionIndex,
			ExpiresAt:    time.Now().Add(ttl),
		})
	}

	return playerToken, urlToken, nil
}

func (e *Extractor) loadPOTokenChallengeContext(ctx context.Context, videoID string) (*POTokenChallenge, error) {
	if challenge := e.extractWatchPagePOTokenChallengeContext(ctx, videoID); challenge != nil {
		return challenge, nil
	}
	return e.requestPOTokenChallengeViaAttGetContext(ctx, videoID)
}

func (e *Extractor) extractWatchPagePOTokenChallengeContext(ctx context.Context, videoID string) *POTokenChallenge {
	if e.config.Name != "WEB_MUSIC" {
		return nil
	}
	watchURL := fmt.Sprintf("https://music.youtube.com/watch?v=%s", videoID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, watchURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", e.config.UserAgent)
	req.Header.Set("Cookie", BuildCookieHeader(e.cookies))
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	bgChallenge := extractBGChallengeFromWatchPage(body)
	if len(bgChallenge) == 0 {
		return nil
	}

	return &POTokenChallenge{
		Source:       "watch",
		Webpage:      append([]byte(nil), body...),
		BGChallenge:  append([]byte(nil), bgChallenge...),
		VideoID:      videoID,
		VisitorData:  e.visitorData,
		SessionIndex: e.sessionIndex,
	}
}

func (e *Extractor) requestPOTokenChallengeViaAttGetContext(ctx context.Context, videoID string) (*POTokenChallenge, error) {
	client := InnertubeClient{
		HL:            "en",
		GL:            "US",
		ClientName:    e.config.Name,
		ClientVersion: e.config.Version,
		UserAgent:     e.config.UserAgent,
		TimeZone:      "UTC",
		UTCOffset:     0,
		VisitorData:   e.visitorData,
	}

	if e.config.DeviceMake != "" {
		client.DeviceMake = e.config.DeviceMake
	}
	if e.config.DeviceModel != "" {
		client.DeviceModel = e.config.DeviceModel
	}
	if e.config.Platform != "" {
		client.Platform = e.config.Platform
	}
	if e.config.OSName != "" {
		client.OSName = e.config.OSName
	}
	if e.config.OSVersion != "" {
		client.OSVersion = e.config.OSVersion
	}

	payload := map[string]any{
		"videoId":        videoID,
		"context":        map[string]any{"client": client},
		"engagementType": "ENGAGEMENT_TYPE_UNBOUND",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	attEndpoint := strings.Replace(e.config.APIEndpoint, "/player", "/att/get", 1)
	apiURL := fmt.Sprintf("%s?key=%s&prettyPrint=false", attEndpoint, e.config.APIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", e.config.UserAgent)
	req.Header.Set("Origin", e.config.Origin)
	req.Header.Set("Referer", e.config.Origin+"/")
	for k, v := range e.config.Headers {
		req.Header.Set(k, v)
	}
	if e.config.NeedsCookies && e.sapisid != "" {
		sapisidhash := GenerateSAPISIDHASH(e.sapisid, e.config.Origin)
		req.Header.Set("Authorization", sapisidhash)
		req.Header.Set("X-Origin", e.config.Origin)
		req.Header.Set("Cookie", BuildCookieHeader(e.cookies))
		if e.sessionIndex != "" {
			req.Header.Set("X-Goog-AuthUser", e.sessionIndex)
		}
		req.Header.Set("X-Youtube-Bootstrap-Logged-In", "true")
	}
	if e.visitorData != "" {
		req.Header.Set("X-Goog-Visitor-Id", e.visitorData)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, NewPOError(POFailureAttGetChallenge, "att/get request failed", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, NewPOError(POFailureAttGetChallenge, "failed reading att/get response", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, NewPOError(POFailureAttGetChallenge, fmt.Sprintf("att/get returned %d: %s", resp.StatusCode, string(respBody[:min(200, len(respBody))])), nil)
	}

	var raw any
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, NewPOError(POFailureAttGetChallenge, "failed to parse att/get response", err)
	}

	challenge := extractBGChallenge(raw)
	if challenge == nil {
		return nil, NewPOError(POFailureAttGetChallenge, "att/get response missing bgChallenge", nil)
	}
	return &POTokenChallenge{Source: "att/get", BGChallenge: challenge.BgChallenge, Challenge: *challenge, VideoID: videoID, VisitorData: e.visitorData, SessionIndex: e.sessionIndex}, nil
}

func (e *Extractor) mintPOTokenContext(ctx context.Context, challenge *POTokenChallenge) (string, string, time.Duration, error) {
	playerToken, urlToken, ttl, err := mintPOTokenWithEngine(ctx, e.runtime.GetEngineType(), challenge)
	if err != nil {
		return "", "", 0, NewPOError(POFailureRuntimeMint, "failed to mint po token", err)
	}
	return playerToken, urlToken, ttl, nil
}

func extractBGChallengeFromWatchPage(body []byte) json.RawMessage {
	if len(body) == 0 {
		return nil
	}

	if match := ytAtRRegex.FindSubmatch(body); len(match) >= 2 {
		quoted := string(match[1])
		if strings.HasPrefix(quoted, "'") {
			quoted = `"` + strings.Trim(strings.TrimPrefix(quoted, "'"), "'") + `"`
		}
		if rawJSON, err := strconv.Unquote(quoted); err == nil {
			var container map[string]any
			if err := json.Unmarshal([]byte(rawJSON), &container); err == nil {
				if raw, ok := container["bgChallenge"]; ok {
					if encoded, err := json.Marshal(raw); err == nil {
						return encoded
					}
				}
			}
		}
	}

	if match := ytAtNRegex.FindSubmatch(body); len(match) >= 2 {
		var container map[string]any
		if err := json.Unmarshal(match[1], &container); err == nil {
			if raw, ok := container["bgChallenge"]; ok {
				if encoded, err := json.Marshal(raw); err == nil {
					return encoded
				}
			}
		}
	}

	return nil
}

func extractPOToken(v any) string {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if strings.EqualFold(k, "poToken") {
				if s, ok := child.(string); ok {
					return s
				}
			}
			if tok := extractPOToken(child); tok != "" {
				return tok
			}
		}
	case []any:
		for _, child := range t {
			if tok := extractPOToken(child); tok != "" {
				return tok
			}
		}
	}
	return ""
}

func extractBGChallenge(v any) *BotGuardChallenge {
	switch t := v.(type) {
	case map[string]any:
		if raw, ok := t["bgChallenge"]; ok {
			data, err := json.Marshal(raw)
			if err != nil {
				return nil
			}
			challenge := &BotGuardChallenge{BgChallenge: data}
			if m, ok := raw.(map[string]any); ok {
				if s, ok := m["engagementType"].(string); ok {
					challenge.EngagementType = s
				}
				if s, ok := m["challengeToken"].(string); ok {
					challenge.ChallengeToken = s
				}
			}
			return challenge
		}
		for _, child := range t {
			if challenge := extractBGChallenge(child); challenge != nil {
				return challenge
			}
		}
	case []any:
		for _, child := range t {
			if challenge := extractBGChallenge(child); challenge != nil {
				return challenge
			}
		}
	}
	return nil
}

func extractPOTokenTTL(v any) time.Duration {
	seconds := extractTTLSeconds(v)
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func extractTTLSeconds(v any) int {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if strings.EqualFold(k, "expiresInSeconds") || strings.EqualFold(k, "ttlSeconds") {
				switch vv := child.(type) {
				case float64:
					return int(vv)
				case int:
					return vv
				case string:
					n, _ := strconv.Atoi(vv)
					if n > 0 {
						return n
					}
				}
			}
			if n := extractTTLSeconds(child); n > 0 {
				return n
			}
		}
	case []any:
		for _, child := range t {
			if n := extractTTLSeconds(child); n > 0 {
				return n
			}
		}
	}
	return 0
}
