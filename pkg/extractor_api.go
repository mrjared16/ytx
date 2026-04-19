package ytx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// callPlayerAPI makes the innertube player API call.
func (e *Extractor) callPlayerAPI(videoID string) (*PlayerResponse, error) {
	return e.callPlayerAPIContext(context.Background(), videoID)
}

func (e *Extractor) callPlayerAPIContext(ctx context.Context, videoID string) (*PlayerResponse, error) {
	client := InnertubeClient{
		HL:            "en",
		GL:            "US",
		ClientName:    e.config.Name,
		ClientVersion: e.config.Version,
		UserAgent:     e.config.UserAgent,
		TimeZone:      "UTC",
		UTCOffset:     0,
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

	if e.visitorData != "" {
		client.VisitorData = e.visitorData
	}

	var sts int
	if e.earlySTSOverride > 0 {
		sts = e.earlySTSOverride
	} else if e.cipher != nil {
		sts = e.cipher.SignatureTimestamp()
	}

	reqBody := InnertubeRequest{
		VideoID:        videoID,
		Context:        InnertubeContext{Client: client},
		ContentCheckOK: true,
		RacyCheckOK:    true,
		PlaybackContext: &PlaybackContext{
			ContentPlaybackContext: ContentPlaybackContext{
				HTML5Preference:    "HTML5_PREF_WANTS",
				SignatureTimestamp: sts,
			},
		},
	}
	if e.poMode && e.poPlayerToken != "" {
		reqBody.ServiceIntegrityDimensions = &ServiceIntegrityDimensions{PoToken: e.poPlayerToken}
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	apiURL := fmt.Sprintf("%s?key=%s&prettyPrint=false", e.config.APIEndpoint, e.config.APIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(jsonBody))
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
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body[:min(200, len(body))]))
	}

	var playerResp PlayerResponse
	if err := json.NewDecoder(resp.Body).Decode(&playerResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if playerResp.ResponseContext.VisitorData != "" && e.visitorData == "" {
		e.visitorData = playerResp.ResponseContext.VisitorData
	}

	return &playerResp, nil
}
