package ytx

// === INNERTUBE REQUEST ===

type InnertubeRequest struct {
	VideoID         string           `json:"videoId"`
	Context         InnertubeContext `json:"context"`
	ContentCheckOK  bool             `json:"contentCheckOk"`
	RacyCheckOK     bool             `json:"racyCheckOk"`
	PlaybackContext *PlaybackContext `json:"playbackContext,omitempty"`
}

type InnertubeContext struct {
	Client InnertubeClient `json:"client"`
}

type InnertubeClient struct {
	HL                string `json:"hl"`
	GL                string `json:"gl"`
	ClientName        string `json:"clientName"`
	ClientVersion     string `json:"clientVersion"`
	UserAgent         string `json:"userAgent,omitempty"`
	TimeZone          string `json:"timeZone"`
	UTCOffset         int    `json:"utcOffsetMinutes"`
	DeviceMake        string `json:"deviceMake,omitempty"`
	DeviceModel       string `json:"deviceModel,omitempty"`
	Platform          string `json:"platform,omitempty"`
	OSName            string `json:"osName,omitempty"`
	OSVersion         string `json:"osVersion,omitempty"`
	VisitorData       string `json:"visitorData,omitempty"`
}

type PlaybackContext struct {
	ContentPlaybackContext ContentPlaybackContext `json:"contentPlaybackContext"`
}

type ContentPlaybackContext struct {
	HTML5Preference string `json:"html5Preference"`
}

// === INNERTUBE RESPONSE ===

type PlayerResponse struct {
	ResponseContext   ResponseContext   `json:"responseContext"`
	PlayabilityStatus PlayabilityStatus `json:"playabilityStatus"`
	StreamingData     StreamingData     `json:"streamingData"`
	VideoDetails      VideoDetails      `json:"videoDetails"`
}

type ResponseContext struct {
	VisitorData string `json:"visitorData"`
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

// === CLI OUTPUT ===

// Timings captures duration of each stage for profiling
type Timings struct {
	VisitorDataMs int64  `json:"visitor_data_ms,omitempty"`
	PlayerAPIMs   int64  `json:"player_api_ms,omitempty"`
	CipherInitMs  int64  `json:"cipher_init_ms,omitempty"`
	NTransformMs  int64  `json:"n_transform_ms,omitempty"`
	TotalMs       int64  `json:"total_ms,omitempty"`
	JSEngine      string `json:"js_engine,omitempty"`
}

type Result struct {
	URL      string   `json:"url"`
	Itag     int      `json:"itag"`
	Bitrate  int      `json:"bitrate"`
	MimeType string   `json:"mimeType,omitempty"`
	Title    string   `json:"title,omitempty"`
	Author   string   `json:"author,omitempty"`
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
	VideoURL  string   `json:"video_url"`
	AudioURL  string   `json:"audio_url"`
	VideoItag int      `json:"video_itag"`
	AudioItag int      `json:"audio_itag"`
	Width     int      `json:"width"`
	Height    int      `json:"height"`
	Title     string   `json:"title"`
	Author    string   `json:"author"`
	Timings   *Timings `json:"timings,omitempty"`
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
