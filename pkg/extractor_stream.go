package ytx

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// findBestAudioStream finds the highest quality audio stream.
func (e *Extractor) findBestAudioStream(formats []Format) *Format {
	var audioFormats []Format
	for _, f := range formats {
		if strings.HasPrefix(f.MimeType, "audio/") {
			audioFormats = append(audioFormats, f)
		}
	}

	if len(audioFormats) == 0 {
		return nil
	}

	var priorityItags []int
	if itags, ok := AudioFormatPriority[e.audioFormat]; ok {
		priorityItags = itags
	} else {
		priorityItags = AudioFormatPriority[AudioFormatWebm]
	}

	selectPreferred := func(candidates []Format) *Format {
		for _, targetItag := range priorityItags {
			for i := range candidates {
				if candidates[i].Itag == targetItag {
					return &candidates[i]
				}
			}
		}

		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].Bitrate > candidates[j].Bitrate
		})
		return &candidates[0]
	}

	var directURLFormats []Format
	for _, f := range audioFormats {
		if f.URL != "" {
			directURLFormats = append(directURLFormats, f)
		}
	}
	if len(directURLFormats) > 0 {
		return selectPreferred(directURLFormats)
	}

	return selectPreferred(audioFormats)
}

// findBestVideoStream finds the highest quality video stream within maxHeight limit.
func (e *Extractor) findBestVideoStream(formats []Format) *Format {
	var videoFormats []Format
	for _, f := range formats {
		if strings.HasPrefix(f.MimeType, "video/") && f.Height > 0 {
			if e.maxHeight > 0 && f.Height > e.maxHeight {
				continue
			}
			videoFormats = append(videoFormats, f)
		}
	}

	if len(videoFormats) == 0 {
		return nil
	}

	for _, targetItag := range VideoItags {
		for i := range videoFormats {
			if videoFormats[i].Itag == targetItag {
				return &videoFormats[i]
			}
		}
	}

	sort.Slice(videoFormats, func(i, j int) bool {
		return videoFormats[i].Height > videoFormats[j].Height
	})

	return &videoFormats[0]
}

var (
	videoIDRegex      = regexp.MustCompile(`(?:v=|/)([a-zA-Z0-9_-]{11})`)
	validVideoIDRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{11}$`)
)

// ExtractVideoID extracts the video ID from a URL or returns the ID if already valid.
func ExtractVideoID(input string) string {
	input = strings.TrimSpace(input)

	if len(input) == 11 && validVideoIDRegex.MatchString(input) {
		return input
	}

	matches := videoIDRegex.FindStringSubmatch(input)
	if len(matches) >= 2 {
		return matches[1]
	}

	return input
}

// extractSubtitles extracts and filters subtitles from player response captions.
func extractSubtitles(captions *CaptionsRenderer, langs []string) []Subtitle {
	if captions == nil || captions.PlayerCaptionsTracklistRenderer == nil {
		return nil
	}

	tracks := captions.PlayerCaptionsTracklistRenderer.CaptionTracks
	if len(tracks) == 0 {
		return nil
	}

	fetchAll := false
	if len(langs) == 0 {
		langs = defaultSubtitleLangs
	} else {
		for _, l := range langs {
			if l == "all" {
				fetchAll = true
				break
			}
		}
	}

	var langSet map[string]bool
	if !fetchAll {
		langSet = make(map[string]bool, len(langs))
		for _, l := range langs {
			langSet[l] = true
		}
	}

	subtitles := make([]Subtitle, 0, len(tracks))
	for _, track := range tracks {
		if track.BaseURL == "" {
			continue
		}

		if !fetchAll && !langSet[track.LanguageCode] {
			continue
		}

		u, err := url.Parse(track.BaseURL)
		if err != nil {
			continue
		}
		q := u.Query()
		q.Set("fmt", "vtt")
		u.RawQuery = q.Encode()

		name := track.Name.SimpleText
		if name == "" {
			name = track.LanguageCode
		}

		subtitles = append(subtitles, Subtitle{
			URL:    u.String(),
			Lang:   track.LanguageCode,
			Name:   name,
			IsAuto: track.Kind == "asr",
		})
	}

	sort.Slice(subtitles, func(i, j int) bool {
		if subtitles[i].IsAuto != subtitles[j].IsAuto {
			return !subtitles[i].IsAuto
		}
		return subtitles[i].Lang < subtitles[j].Lang
	})

	return subtitles
}
