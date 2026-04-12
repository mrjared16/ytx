package ytx

import "encoding/json"

// === INNERTUBE REQUEST ===

type InnertubeRequest struct {
	VideoID                    string                      `json:"videoId"`
	Context                    InnertubeContext            `json:"context"`
	ContentCheckOK             bool                        `json:"contentCheckOk"`
	RacyCheckOK                bool                        `json:"racyCheckOk"`
	PlaybackContext            *PlaybackContext            `json:"playbackContext,omitempty"`
	ServiceIntegrityDimensions *ServiceIntegrityDimensions `json:"serviceIntegrityDimensions,omitempty"`
}

type ServiceIntegrityDimensions struct {
	PoToken string `json:"poToken,omitempty"`
}

type BotGuardChallenge struct {
	BgChallenge    json.RawMessage `json:"bgChallenge,omitempty"`
	ChallengeToken string          `json:"challengeToken,omitempty"`
	EngagementType string          `json:"engagementType,omitempty"`
}

type POTokenChallenge struct {
	Source       string
	Webpage      json.RawMessage
	BGChallenge  json.RawMessage
	Challenge    BotGuardChallenge
	VideoID      string
	VisitorData  string
	SessionIndex string
}

type InnertubeContext struct {
	Client InnertubeClient `json:"client"`
}

type InnertubeClient struct {
	HL            string `json:"hl"`
	GL            string `json:"gl"`
	ClientName    string `json:"clientName"`
	ClientVersion string `json:"clientVersion"`
	UserAgent     string `json:"userAgent,omitempty"`
	TimeZone      string `json:"timeZone"`
	UTCOffset     int    `json:"utcOffsetMinutes"`
	DeviceMake    string `json:"deviceMake,omitempty"`
	DeviceModel   string `json:"deviceModel,omitempty"`
	Platform      string `json:"platform,omitempty"`
	OSName        string `json:"osName,omitempty"`
	OSVersion     string `json:"osVersion,omitempty"`
	VisitorData   string `json:"visitorData,omitempty"`
}

type PlaybackContext struct {
	ContentPlaybackContext ContentPlaybackContext `json:"contentPlaybackContext"`
}

type ContentPlaybackContext struct {
	HTML5Preference    string `json:"html5Preference"`
	SignatureTimestamp int    `json:"signatureTimestamp,omitempty"`
}

// === INNERTUBE RESPONSE ===

type PlayerResponse struct {
	ResponseContext   ResponseContext   `json:"responseContext"`
	PlayabilityStatus PlayabilityStatus `json:"playabilityStatus"`
	StreamingData     StreamingData     `json:"streamingData"`
	VideoDetails      VideoDetails      `json:"videoDetails"`
	Captions          *CaptionsRenderer `json:"captions,omitempty"`
}

type ResponseContext struct {
	VisitorData           string                 `json:"visitorData"`
	ServiceTrackingParams []ServiceTrackingParam `json:"serviceTrackingParams,omitempty"`
}

type ServiceTrackingParam struct {
	Service string          `json:"service,omitempty"`
	Params  []TrackingParam `json:"params,omitempty"`
}

type TrackingParam struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

type PlayabilityStatus struct {
	Status          string `json:"status"`
	Reason          string `json:"reason,omitempty"`
	PlayableInEmbed bool   `json:"playableInEmbed"`
}

type StreamingData struct {
	ExpiresInSeconds string   `json:"expiresInSeconds"`
	Formats          []Format `json:"formats"`
	AdaptiveFormats  []Format `json:"adaptiveFormats"`
}

type Format struct {
	Itag            int    `json:"itag"`
	URL             string `json:"url,omitempty"`
	SignatureCipher string `json:"signatureCipher,omitempty"`
	MimeType        string `json:"mimeType"`
	Bitrate         int    `json:"bitrate"`
	Width           int    `json:"width,omitempty"`
	Height          int    `json:"height,omitempty"`
	ContentLength   string `json:"contentLength,omitempty"`
	Quality         string `json:"quality"`
	AudioQuality    string `json:"audioQuality,omitempty"`
	AudioChannels   int    `json:"audioChannels,omitempty"`
	AudioSampleRate string `json:"audioSampleRate,omitempty"`
}

type VideoDetails struct {
	VideoID          string `json:"videoId"`
	Title            string `json:"title"`
	LengthSeconds    string `json:"lengthSeconds"`
	Author           string `json:"author"`
	ShortDescription string `json:"shortDescription"`
}

// === CAPTION TYPES ===

// CaptionsRenderer from playerResponse.captions
type CaptionsRenderer struct {
	PlayerCaptionsTracklistRenderer *CaptionTracklist `json:"playerCaptionsTracklistRenderer"`
}

// CaptionTracklist contains the list of caption tracks
type CaptionTracklist struct {
	CaptionTracks []CaptionTrack `json:"captionTracks"`
}

// CaptionTrack represents a single caption/subtitle track
type CaptionTrack struct {
	BaseURL      string      `json:"baseUrl"`
	LanguageCode string      `json:"languageCode"`
	Name         CaptionName `json:"name"`
	Kind         string      `json:"kind"` // "asr" for auto-generated
}

// CaptionName contains the display name for a caption track
type CaptionName struct {
	SimpleText string `json:"simpleText,omitempty"`
}

// Subtitle is the CLI output format for subtitles
type Subtitle struct {
	URL    string `json:"url"`
	Lang   string `json:"lang"`
	Name   string `json:"name"`
	IsAuto bool   `json:"is_auto,omitempty"`
}

// === CLI OUTPUT ===

// Timings captures duration of each stage for profiling
type Timings struct {
	VisitorDataMs   int64  `json:"visitor_data_ms,omitempty"`
	STSWaitMs       int64  `json:"sts_wait_ms,omitempty"`
	PlayerAPIMs     int64  `json:"player_api_ms,omitempty"`
	CipherInitMs    int64  `json:"cipher_init_ms,omitempty"`
	PlayerJSFetchMs int64  `json:"player_js_fetch_ms,omitempty"`
	CipherAnalyzeMs int64  `json:"cipher_analyze_ms,omitempty"`
	POChallengeMs   int64  `json:"po_challenge_ms,omitempty"`
	POMintMs        int64  `json:"po_mint_ms,omitempty"`
	CipherPrewarmMs int64  `json:"cipher_prewarm_ms,omitempty"`
	SigDecryptMs    int64  `json:"sig_decrypt_ms,omitempty"`
	NTransformMs    int64  `json:"n_transform_ms,omitempty"`
	OtherMs         int64  `json:"other_ms,omitempty"`
	TotalMs         int64  `json:"total_ms,omitempty"`
	JSEngine        string `json:"js_engine,omitempty"`

	CipherDetail *CipherAnalyzeDetail `json:"cipher_detail,omitempty"`
}

// CipherAnalyzeDetail breaks down cipher_analyze_ms into individual stages
// with the tier that was used (fast windowed vs slow global fallback).
// Populated on cold starts when --profile is enabled.
type CipherAnalyzeDetail struct {
	// Signature function detection
	SigMs   int64  `json:"sig_ms"`
	SigTier string `json:"sig_tier"` // "primary", "windowed_fallback", "global_fallback"
	SigName string `json:"sig_name,omitempty"`

	// URL transform wrapper detection (only when sig fails)
	WrapperMs   int64  `json:"wrapper_ms,omitempty"`
	WrapperTier string `json:"wrapper_tier"` // "windowed", "global_fallback", "skipped"

	// URL transform wrapper JS build (only when wrapper used)
	WrapperBuildMs   int64  `json:"wrapper_build_ms,omitempty"`
	WrapperBuildTier string `json:"wrapper_build_tier,omitempty"` // "windowed", "global_fallback"

	// N-function detection
	NFuncMs   int64  `json:"n_func_ms"`
	NFuncTier string `json:"n_func_tier"` // "windowed", "global_fallback"
	NFuncName string `json:"n_func_name,omitempty"`

	// Signature timestamp extraction
	StsMs int64 `json:"sts_ms"`

	// Diagnostics
	PlayerJSBytes int    `json:"player_js_bytes"`
	MarkerMiss    []string `json:"marker_miss,omitempty"` // which marker failed (when global fallback triggered)
}

type Result struct {
	URL      string   `json:"url"`
	Itag     int      `json:"itag"`
	Bitrate  int      `json:"bitrate"`
	MimeType string   `json:"mimeType,omitempty"`
	Title    string   `json:"title,omitempty"`
	Author   string   `json:"author,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Timings  *Timings `json:"timings,omitempty"`
}

type ErrorResult struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Debug   string `json:"debug,omitempty"`
}

// === CLIENT CONFIGURATION ===

type ClientMode string

const (
	ModeVideo ClientMode = "video"
	ModeMusic ClientMode = "music"
)

type ClientConfig struct {
	Name         string
	Version      string
	APIEndpoint  string
	APIKey       string
	UserAgent    string
	Origin       string
	NeedsCipher  bool
	NeedsCookies bool
	Headers      map[string]string
	// Device info (for IOS/Android clients)
	DeviceMake  string
	DeviceModel string
	Platform    string
	OSName      string
	OSVersion   string
}

// VideoResult extends Result with video URL
type VideoResult struct {
	VideoURL  string     `json:"video_url"`
	AudioURL  string     `json:"audio_url"`
	SubURL    string     `json:"sub_url,omitempty"`
	VideoItag int        `json:"video_itag"`
	AudioItag int        `json:"audio_itag"`
	Width     int        `json:"width"`
	Height    int        `json:"height"`
	Title     string     `json:"title"`
	Author    string     `json:"author"`
	Subtitles []Subtitle `json:"subtitles,omitempty"`
	Warnings  []string   `json:"warnings,omitempty"`
	Timings   *Timings   `json:"timings,omitempty"`
}

// BulkResult for streaming output
type BulkResult struct {
	ID      string `json:"id"`
	URL     string `json:"url,omitempty"`
	Itag    int    `json:"itag,omitempty"`
	Bitrate int    `json:"bitrate,omitempty"`
	Title   string `json:"title,omitempty"`
	Error   string `json:"error,omitempty"`
}
