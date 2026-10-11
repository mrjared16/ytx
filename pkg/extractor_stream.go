package ytx

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// findBestAudioStream finds the highest quality audio stream.
//
// Music mode ranks pure audio quality FIRST across every usable candidate
// (direct URL or signature cipher), then prefers a direct URL only as an
// equal-quality tie-break — it never downgrades to a lower-quality direct
// format to skip deciphering, and an unusable pool (neither URL nor
// parseable cipher URL) returns nil explicitly, leaving the caller's error
// intact. Legacy video keeps its direct-first behavior: the video chain
// pre-partitions the pool, so the music-style mixing never applies there.
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
	if e.modeIsMusic() && e.audioFormat != AudioFormatM4A {
		// Music's default (webm) preference ranks 251 (Opus, higher
		// bitrate) ahead of 140 (AAC). This is a local, mode-scoped
		// adjustment: the shared AudioFormatPriority table and legacy
		// video keep the base order untouched.
		priorityItags = musicPriority(priorityItags)
	}

	var pool []Format
	if e.modeIsMusic() {
		// Music: usable candidates only. An entry with neither a direct
		// URL nor a strictly parseable cipher URL is not playable and is
		// excluded; an empty usable pool is an explicit nil so the caller
		// keeps an actionable error instead of a blind pick.
		for _, f := range audioFormats {
			if f.URL != "" || signatureCipherUsable(f.SignatureCipher) {
				pool = append(pool, f)
			}
		}
	} else {
		// Legacy video: the chain pre-partitions direct formats, and the
		// direct-first fallback semantics are preserved exactly.
		for _, f := range audioFormats {
			if f.URL != "" {
				pool = append(pool, f)
			}
		}
		if len(pool) == 0 {
			pool = audioFormats
		}
	}
	if len(pool) == 0 {
		return nil
	}

	// Rank quality: known itags by priority position (lower is better);
	// unknown itags rank after every known one, then the highest bitrate
	// wins; a direct URL breaks a full (rank+bitrate) tie.
	rankOf := func(f Format) int {
		for p, itag := range priorityItags {
			if f.Itag == itag {
				return p
			}
		}
		return len(priorityItags)
	}

	best := 0
	bestRank := rankOf(pool[0])
	bestBitrate := pool[0].Bitrate
	bestDirect := pool[0].URL != ""
	for i := 1; i < len(pool); i++ {
		rank := rankOf(pool[i])
		bitrate := pool[i].Bitrate
		direct := pool[i].URL != ""
		if rank < bestRank ||
			(rank == bestRank && (bitrate > bestBitrate || (bitrate == bestBitrate && direct && !bestDirect))) {
			best = i
			bestRank = rank
			bestBitrate = bitrate
			bestDirect = direct
		}
	}
	return &pool[best]
}

// musicPriority returns a copy of the given itag priority list with 251
// placed immediately ahead of 140, keeping every other position stable.
// It is a no-op (same backing list) when either itag is absent so no
// priority entry can be silently dropped.
func musicPriority(itags []int) []int {
	has140, has251 := false, false
	for _, itag := range itags {
		has140 = has140 || itag == 140
		has251 = has251 || itag == 251
	}
	if !has140 || !has251 {
		return itags
	}
	out := make([]int, 0, len(itags))
	for _, itag := range itags {
		if itag == 140 {
			out = append(out, 251)
		}
		if itag != 251 {
			out = append(out, itag)
		}
	}
	return out
}

// signatureCipherUsable reports whether a signature cipher carries a
// strictly parseable, non-empty base URL. It mirrors the exact contract
// getStreamURLContext enforces: malformed query escapes, missing or empty
// url values, and bare signatures like "s=abc" are not playable candidates.
func signatureCipherUsable(sigCipher string) bool {
	if sigCipher == "" {
		return false
	}
	params, err := url.ParseQuery(sigCipher)
	if err != nil {
		return false
	}
	return params.Get("url") != ""
}

// modeIsMusic reports whether this extractor is configured for music mode
// (the only mode with cookie-authenticated, cipher-bearing playback).
func (e *Extractor) modeIsMusic() bool {
	return e.config.NeedsCookies && e.config.NeedsCipher
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
