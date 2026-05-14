package ytx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestExtractChapters(t *testing.T) {
	tests := []struct {
		name       string
		playerJSON string
		want       []Chapter
	}{
		{
			name: "prefers player overlays",
			playerJSON: `{
				"videoDetails":{"lengthSeconds":"300","shortDescription":"Wrong 0:00"},
				"playerOverlays":{"playerOverlayRenderer":{"decoratedPlayerBarRenderer":{"decoratedPlayerBarRenderer":{"playerBar":{"chapteredPlayerBarRenderer":{"chapters":[
					{"chapterRenderer":{"timeRangeStartMillis":0,"title":{"simpleText":"Intro"}}},
					{"chapterRenderer":{"timeRangeStartMillis":120000,"title":{"simpleText":"Middle"}}}
				]}}}}}},
				"engagementPanels":[{"engagementPanelSectionListRenderer":{"content":{"macroMarkersListRenderer":{"contents":[
					{"macroMarkersListItemRenderer":{"timeDescription":{"simpleText":"0:00"},"title":{"simpleText":"Wrong Fallback"}}}
				]}}}}]
			}`,
			want: []Chapter{
				{StartTime: 0, EndTime: 120, Title: "Intro"},
				{StartTime: 120, EndTime: 300, Title: "Middle"},
			},
		},
		{
			name: "falls back to engagement panels",
			playerJSON: `{
				"videoDetails":{"lengthSeconds":"200"},
				"engagementPanels":[{"engagementPanelSectionListRenderer":{"content":{"macroMarkersListRenderer":{"contents":[
					{"macroMarkersListItemRenderer":{"timeDescription":{"simpleText":"0:00"},"title":{"runs":[{"text":"Intro"}]}}},
					{"macroMarkersListItemRenderer":{"timeDescription":{"simpleText":"1:30"},"title":{"simpleText":"Deep Dive"}}}
				]}}}}]
			}`,
			want: []Chapter{
				{StartTime: 0, EndTime: 90, Title: "Intro"},
				{StartTime: 90, EndTime: 200, Title: "Deep Dive"},
			},
		},
		{
			name: "falls back to description timestamps",
			playerJSON: `{
				"videoDetails":{"lengthSeconds":"240","shortDescription":"Intro 0:00\nScene One 1:05\nWrap-up 3:10"}
			}`,
			want: []Chapter{
				{StartTime: 0, EndTime: 65, Title: "Intro"},
				{StartTime: 65, EndTime: 190, Title: "Scene One"},
				{StartTime: 190, EndTime: 240, Title: "Wrap-up"},
			},
		},
		{
			name: "sorts out of order engagement panel chapters",
			playerJSON: `{
				"videoDetails":{"lengthSeconds":"180"},
				"engagementPanels":[{"engagementPanelSectionListRenderer":{"content":{"macroMarkersListRenderer":{"contents":[
					{"macroMarkersListItemRenderer":{"timeDescription":{"simpleText":"1:20"},"title":{"simpleText":"Middle"}}},
					{"macroMarkersListItemRenderer":{"timeDescription":{"simpleText":"0:00"},"title":{"simpleText":"Intro"}}}
				]}}}}]
			}`,
			want: []Chapter{
				{StartTime: 0, EndTime: 80, Title: "Intro"},
				{StartTime: 80, EndTime: 180, Title: "Middle"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractChapters(decodePlayerResponse(t, tt.playerJSON))
			assertChaptersEqual(t, got, tt.want)
		})
	}
}

func TestDescriptionChaptersRequireUsefulChapterList(t *testing.T) {
	playerResp := decodePlayerResponse(t, `{
		"videoDetails":{"lengthSeconds":"240","shortDescription":"See the answer at 1:05"}
	}`)

	chapters := extractChapters(playerResp)
	if len(chapters) != 0 {
		t.Fatalf("expected single timestamp mention to be ignored, got %+v", chapters)
	}
}

func TestVideoResultMarshalJSON(t *testing.T) {
	tests := []struct {
		name           string
		result         VideoResult
		wantContains   []string
		wantNotContain []string
	}{
		{
			name: "omits chapters when not requested",
			result: VideoResult{
				VideoURL:  "https://video.test",
				AudioURL:  "https://audio.test",
				VideoItag: 137,
				AudioItag: 140,
				Width:     1920,
				Height:    1080,
				Title:     "test",
				Author:    "author",
			},
			wantContains:   []string{"\"video_url\":\"https://video.test\"", "\"audio_url\":\"https://audio.test\"", "\"video_itag\":137", "\"audio_itag\":140", "\"width\":1920", "\"height\":1080", "\"title\":\"test\"", "\"author\":\"author\""},
			wantNotContain: []string{"\"chapters\""},
		},
		{
			name: "includes empty chapter array when requested",
			result: VideoResult{
				VideoURL:          "https://video.test",
				AudioURL:          "https://audio.test",
				VideoItag:         137,
				AudioItag:         140,
				Width:             1920,
				Height:            1080,
				Title:             "test",
				Author:            "author",
				Chapters:          []Chapter{},
				chaptersRequested: true,
			},
			wantContains: []string{"\"chapters\":[]"},
		},
		{
			name: "includes non-empty chapters without private request flag",
			result: VideoResult{
				VideoURL:  "https://video.test",
				AudioURL:  "https://audio.test",
				VideoItag: 137,
				AudioItag: 140,
				Width:     1920,
				Height:    1080,
				Title:     "test",
				Author:    "author",
				Chapters:  []Chapter{{StartTime: 0, Title: "Intro"}},
			},
			wantContains: []string{"\"chapters\":[{\"start_time\":0,\"title\":\"Intro\"}]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := json.Marshal(tt.result)
			if err != nil {
				t.Fatalf("marshal video result: %v", err)
			}

			got := string(payload)
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Fatalf("expected JSON to contain %q, got %s", want, got)
				}
			}
			for _, unwanted := range tt.wantNotContain {
				if strings.Contains(got, unwanted) {
					t.Fatalf("expected JSON to omit %q, got %s", unwanted, got)
				}
			}
		})
	}
}

func TestExtractVideoContextFetchChaptersOptIn(t *testing.T) {
	for _, tt := range []struct {
		name         string
		configure    func(*Extractor)
		wantContains string
		wantOmit     string
	}{
		{
			name: "emits empty chapters when requested and none exist",
			configure: func(e *Extractor) {
				e.SetFetchChapters(true)
			},
			wantContains: "\"chapters\":[]",
		},
		{
			name:      "omits chapters when not requested",
			configure: func(*Extractor) {},
			wantOmit:  "\"chapters\"",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ext, err := NewExtractor(ModeVideo, "", WithHTTPClient(fakeVideoHTTPClient(t, playerVideoResponseWithoutChapters())))
			if err != nil {
				t.Fatalf("NewExtractor returned error: %v", err)
			}
			ext.visitorData = "VISITOR_TEST"
			tt.configure(ext)

			result, err := ext.ExtractVideoContext(t.Context(), "MR6KSB6I_60")
			if err != nil {
				t.Fatalf("ExtractVideoContext returned error: %v", err)
			}

			payload, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal video result: %v", err)
			}
			got := string(payload)
			if tt.wantContains != "" && !strings.Contains(got, tt.wantContains) {
				t.Fatalf("expected JSON to contain %q, got %s", tt.wantContains, got)
			}
			if tt.wantOmit != "" && strings.Contains(got, tt.wantOmit) {
				t.Fatalf("expected JSON to omit %q, got %s", tt.wantOmit, got)
			}
		})
	}
}

func TestResultMarshalJSONOmitsChapters(t *testing.T) {
	payload, err := json.Marshal(Result{
		URL:     "https://audio.test",
		Itag:    141,
		Bitrate: 256000,
		Title:   "test",
		Author:  "author",
	})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}

	got := string(payload)
	if strings.Contains(got, "\"chapters\"") {
		t.Fatalf("expected music Result JSON to omit chapters, got %s", got)
	}
}

func TestParseChapterTimestampRejectsInvalidClockValues(t *testing.T) {
	tests := []string{"1:99", "99:99", "1:70:00", "1:20:70"}
	for _, timestamp := range tests {
		t.Run(timestamp, func(t *testing.T) {
			if _, ok := parseChapterTimestamp(timestamp); ok {
				t.Fatalf("expected %q to be rejected", timestamp)
			}
		})
	}
}

func decodePlayerResponse(t *testing.T, body string) *PlayerResponse {
	t.Helper()

	var playerResp PlayerResponse
	if err := json.Unmarshal([]byte(body), &playerResp); err != nil {
		t.Fatalf("decode player response: %v", err)
	}

	return &playerResp
}

func assertChaptersEqual(t *testing.T, got, want []Chapter) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("chapter count mismatch: got %d want %d (%+v)", len(got), len(want), got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chapter %d mismatch: got %+v want %+v", i, got[i], want[i])
		}
	}
}

func fakeVideoHTTPClient(t *testing.T, responseBody string) *http.Client {
	t.Helper()

	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Helper()
			if req.Method == http.MethodPost && req.URL.Host == "www.youtube.com" && req.URL.Path == "/youtubei/v1/player" {
				return jsonHTTPResponse(http.StatusOK, responseBody), nil
			}

			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL.String())
		}),
	}
}

func playerVideoResponseWithoutChapters() string {
	return `{
		"responseContext":{"visitorData":"VISITOR_TEST"},
		"playabilityStatus":{"status":"OK"},
		"streamingData":{"adaptiveFormats":[
			{"itag":137,"url":"https://video.test/videoplayback","mimeType":"video/mp4; codecs=\"avc1.640028\"","bitrate":4000000,"width":1920,"height":1080,"quality":"hd1080"},
			{"itag":140,"url":"https://audio.test/videoplayback","mimeType":"audio/mp4; codecs=\"mp4a.40.2\"","bitrate":128000,"quality":"tiny","audioQuality":"AUDIO_QUALITY_MEDIUM"}
		]},
		"videoDetails":{"videoId":"MR6KSB6I_60","title":"test","lengthSeconds":"120","author":"author","shortDescription":""}
	}`
}
