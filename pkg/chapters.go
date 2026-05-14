package ytx

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	chapterLineLeadingTimeRE  = regexp.MustCompile(`(?m)^\s*((?:\d+:)?\d{1,2}:\d{2})\b\W*\s(.+?)\s*$`)
	chapterLineTrailingTimeRE = regexp.MustCompile(`(?m)^\s*(.+?)\b\W*\s((?:\d+:)?\d{1,2}:\d{2})\s*$`)
	chapterTimestampRE        = regexp.MustCompile(`^(?:\d+:)?\d{1,2}:\d{2}$`)
)

type chapterCandidate struct {
	startTime float64
	title     string
}

type chapterPlayerOverlays struct {
	PlayerOverlayRenderer chapterPlayerOverlayRenderer `json:"playerOverlayRenderer"`
}

type chapterPlayerOverlayRenderer struct {
	DecoratedPlayerBarRenderer chapterDecoratedPlayerBarRenderer `json:"decoratedPlayerBarRenderer"`
}

type chapterDecoratedPlayerBarRenderer struct {
	DecoratedPlayerBarRenderer *chapterDecoratedPlayerBarRenderer `json:"decoratedPlayerBarRenderer,omitempty"`
	PlayerBar                  chapterPlayerBar                   `json:"playerBar"`
}

type chapterPlayerBar struct {
	ChapteredPlayerBarRenderer chapteredPlayerBarRenderer `json:"chapteredPlayerBarRenderer"`
}

type chapteredPlayerBarRenderer struct {
	Chapters []playerOverlayChapter `json:"chapters"`
}

type playerOverlayChapter struct {
	ChapterRenderer chapterRenderer `json:"chapterRenderer"`
}

type chapterRenderer struct {
	TimeRangeStartMillis int64       `json:"timeRangeStartMillis"`
	Title                chapterText `json:"title"`
}

type engagementPanel struct {
	EngagementPanelSectionListRenderer engagementPanelSectionListRenderer `json:"engagementPanelSectionListRenderer"`
}

type engagementPanelSectionListRenderer struct {
	Content engagementPanelContent `json:"content"`
}

type engagementPanelContent struct {
	MacroMarkersListRenderer macroMarkersListRenderer `json:"macroMarkersListRenderer"`
}

type macroMarkersListRenderer struct {
	Contents []macroMarkersListContent `json:"contents"`
}

type macroMarkersListContent struct {
	MacroMarkersListItemRenderer macroMarkersListItemRenderer `json:"macroMarkersListItemRenderer"`
}

type macroMarkersListItemRenderer struct {
	TimeDescription chapterText `json:"timeDescription"`
	Title           chapterText `json:"title"`
}

type chapterText struct {
	SimpleText string           `json:"simpleText,omitempty"`
	Runs       []chapterTextRun `json:"runs,omitempty"`
}

type chapterTextRun struct {
	Text string `json:"text,omitempty"`
}

func (t chapterText) PlainText() string {
	if t.SimpleText != "" {
		return t.SimpleText
	}

	var builder strings.Builder
	for _, run := range t.Runs {
		builder.WriteString(run.Text)
	}

	return builder.String()
}

func (r VideoResult) MarshalJSON() ([]byte, error) {
	payload := map[string]any{
		"video_url":  r.VideoURL,
		"audio_url":  r.AudioURL,
		"video_itag": r.VideoItag,
		"audio_itag": r.AudioItag,
		"width":      r.Width,
		"height":     r.Height,
		"title":      r.Title,
		"author":     r.Author,
	}

	if r.SubURL != "" {
		payload["sub_url"] = r.SubURL
	}
	if len(r.Subtitles) > 0 {
		payload["subtitles"] = r.Subtitles
	}
	if r.chaptersRequested || len(r.Chapters) > 0 {
		chapters := r.Chapters
		if chapters == nil {
			chapters = []Chapter{}
		}
		payload["chapters"] = chapters
	}
	if len(r.Warnings) > 0 {
		payload["warnings"] = r.Warnings
	}
	if r.Timings != nil {
		payload["timings"] = r.Timings
	}

	return json.Marshal(payload)
}

func extractChapters(playerResp *PlayerResponse) []Chapter {
	if playerResp == nil {
		return []Chapter{}
	}

	duration := videoDurationSeconds(playerResp.VideoDetails.LengthSeconds)
	for _, candidates := range [][]chapterCandidate{
		extractOverlayChapterCandidates(playerResp),
		extractEngagementPanelChapterCandidates(playerResp),
		extractDescriptionChapterCandidates(playerResp.VideoDetails.ShortDescription),
	} {
		chapters := normalizeChapterCandidates(candidates, duration)
		if len(chapters) > 0 {
			return chapters
		}
	}

	return []Chapter{}
}

func extractOverlayChapterCandidates(playerResp *PlayerResponse) []chapterCandidate {
	if len(playerResp.PlayerOverlays) == 0 {
		return nil
	}

	var playerOverlays chapterPlayerOverlays
	if err := json.Unmarshal(playerResp.PlayerOverlays, &playerOverlays); err != nil {
		return nil
	}

	overlays := playerOverlays.PlayerOverlayRenderer.DecoratedPlayerBarRenderer.chapterList()
	if len(overlays) == 0 {
		return nil
	}

	chapters := make([]chapterCandidate, 0, len(overlays))
	for _, overlayChapter := range overlays {
		renderer := overlayChapter.ChapterRenderer
		if renderer.TimeRangeStartMillis < 0 {
			continue
		}

		chapters = append(chapters, chapterCandidate{
			startTime: float64(renderer.TimeRangeStartMillis) / 1000,
			title:     renderer.Title.PlainText(),
		})
	}

	return chapters
}

func extractEngagementPanelChapterCandidates(playerResp *PlayerResponse) []chapterCandidate {
	if len(playerResp.EngagementPanels) == 0 {
		return nil
	}

	var engagementPanels []engagementPanel
	if err := json.Unmarshal(playerResp.EngagementPanels, &engagementPanels); err != nil {
		return nil
	}

	for _, panel := range engagementPanels {
		contents := panel.EngagementPanelSectionListRenderer.Content.MacroMarkersListRenderer.Contents
		if len(contents) == 0 {
			continue
		}

		chapters := make([]chapterCandidate, 0, len(contents))
		for _, content := range contents {
			renderer := content.MacroMarkersListItemRenderer
			startTime, ok := parseChapterTimestamp(renderer.TimeDescription.PlainText())
			if !ok {
				continue
			}

			chapters = append(chapters, chapterCandidate{
				startTime: startTime,
				title:     renderer.Title.PlainText(),
			})
		}

		if len(chapters) > 0 {
			return chapters
		}
	}

	return nil
}

func extractDescriptionChapterCandidates(description string) []chapterCandidate {
	if description == "" {
		return nil
	}

	if chapters := validDescriptionChapterCandidates(extractDescriptionChapterMatches(chapterLineLeadingTimeRE.FindAllStringSubmatch(description, -1), true)); len(chapters) > 0 {
		return chapters
	}

	return validDescriptionChapterCandidates(extractDescriptionChapterMatches(chapterLineTrailingTimeRE.FindAllStringSubmatch(description, -1), false))
}

func extractDescriptionChapterMatches(matches [][]string, timeFirst bool) []chapterCandidate {
	chapters := make([]chapterCandidate, 0, len(matches))
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}

		timestamp := match[1]
		title := match[2]
		if !timeFirst {
			timestamp = match[2]
			title = match[1]
		}

		startTime, ok := parseChapterTimestamp(timestamp)
		if !ok {
			continue
		}

		chapters = append(chapters, chapterCandidate{
			startTime: startTime,
			title:     title,
		})
	}

	if len(chapters) == 0 {
		return nil
	}

	return chapters
}

func validDescriptionChapterCandidates(chapters []chapterCandidate) []chapterCandidate {
	if len(chapters) < 2 {
		return nil
	}

	sortedChapters := append([]chapterCandidate(nil), chapters...)
	sort.Slice(sortedChapters, func(i, j int) bool {
		return sortedChapters[i].startTime < sortedChapters[j].startTime
	})

	if sortedChapters[0].startTime != 0 {
		return nil
	}

	return chapters
}

func normalizeChapterCandidates(candidates []chapterCandidate, duration float64) []Chapter {
	if len(candidates) == 0 {
		return nil
	}

	sortedCandidates := append([]chapterCandidate(nil), candidates...)
	sort.Slice(sortedCandidates, func(i, j int) bool {
		return sortedCandidates[i].startTime < sortedCandidates[j].startTime
	})

	chapters := make([]Chapter, 0, len(sortedCandidates))
	lastStart := -1.0
	for _, candidate := range sortedCandidates {
		title := strings.TrimSpace(candidate.title)
		if candidate.startTime < 0 {
			continue
		}
		if duration > 0 && candidate.startTime > duration {
			continue
		}
		if lastStart >= 0 && candidate.startTime <= lastStart {
			continue
		}

		chapters = append(chapters, Chapter{
			StartTime: candidate.startTime,
			Title:     title,
		})
		lastStart = candidate.startTime
	}

	for i := range chapters {
		if i+1 < len(chapters) {
			chapters[i].EndTime = chapters[i+1].StartTime
			continue
		}
		if duration > chapters[i].StartTime {
			chapters[i].EndTime = duration
		}
	}

	return chapters
}

func parseChapterTimestamp(value string) (float64, bool) {
	timestamp := strings.TrimSpace(value)
	if !chapterTimestampRE.MatchString(timestamp) {
		return 0, false
	}

	parts := strings.Split(timestamp, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, false
	}

	if len(parts) == 2 {
		if !isValidClockComponent(parts[1]) {
			return 0, false
		}
	} else if !isValidClockComponent(parts[1]) || !isValidClockComponent(parts[2]) {
		return 0, false
	}

	total := 0
	for _, part := range parts {
		unit, err := strconv.Atoi(part)
		if err != nil {
			return 0, false
		}
		total = (total * 60) + unit
	}

	return float64(total), true
}

func isValidClockComponent(value string) bool {
	unit, err := strconv.Atoi(value)
	if err != nil {
		return false
	}

	return unit >= 0 && unit < 60
}

func videoDurationSeconds(lengthSeconds string) float64 {
	if lengthSeconds == "" {
		return 0
	}

	duration, err := strconv.ParseFloat(lengthSeconds, 64)
	if err != nil {
		return 0
	}

	return duration
}

func (r chapterDecoratedPlayerBarRenderer) chapterList() []playerOverlayChapter {
	current := &r
	for current != nil {
		chapters := current.PlayerBar.ChapteredPlayerBarRenderer.Chapters
		if len(chapters) > 0 {
			return chapters
		}
		current = current.DecoratedPlayerBarRenderer
	}

	return nil
}
