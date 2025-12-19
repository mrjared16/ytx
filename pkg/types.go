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
	HL            string `json:"hl"`
	GL            string `json:"gl"`
	ClientName    string `json:"clientName"`
	ClientVersion string `json:"clientVersion"`
	UserAgent     string `json:"userAgent,omitempty"`
	TimeZone      string `json:"timeZone"`
	UTCOffset     int    `json:"utcOffsetMinutes"`
}

type PlaybackContext struct {
	ContentPlaybackContext ContentPlaybackContext `json:"contentPlaybackContext"`
}

type ContentPlaybackContext struct {
	HTML5Preference string `json:"html5Preference"`
}

// === INNERTUBE RESPONSE ===

type PlayerResponse struct {
	PlayabilityStatus PlayabilityStatus `json:"playabilityStatus"`
	StreamingData     StreamingData     `json:"streamingData"`
	VideoDetails      VideoDetails      `json:"videoDetails"`
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

type Result struct {
	URL      string `json:"url"`
	Itag     int    `json:"itag"`
	Bitrate  int    `json:"bitrate"`
	MimeType string `json:"mimeType,omitempty"`
	Title    string `json:"title,omitempty"`
	Author   string `json:"author,omitempty"`
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
}

// VideoResult extends Result with video URL
type VideoResult struct {
	VideoURL  string `json:"video_url"`
	AudioURL  string `json:"audio_url"`
	VideoItag int    `json:"video_itag"`
	AudioItag int    `json:"audio_itag"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Title     string `json:"title"`
	Author    string `json:"author"`
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
