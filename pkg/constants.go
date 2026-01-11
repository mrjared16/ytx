package ytx

// === YOUTUBE MUSIC CLIENT CONSTANTS ===
// UPDATE THESE WHEN YOUTUBE BREAKS THE EXTRACTOR

const (
	// Client identification for YouTube Music Web
	ClientName    = "WEB_REMIX"
	ClientVersion = "1.20251216.01.00" // Check music.youtube.com network tab for current version
	ClientKey     = "AIzaSyC9XL3ZjWddXya6X74dJoCTL-WEYFDNX30"

	// API endpoint
	APIEndpoint = "https://music.youtube.com/youtubei/v1/player"
	Origin      = "https://music.youtube.com"

	// User agent
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	// Base.js URL pattern (for cipher)
	PlayerJSURLBase = "https://www.youtube.com"
)

// AudioFormat represents preferred audio container format
type AudioFormat int

const (
	AudioFormatWebm AudioFormat = iota // default: 774 (webm/opus) first
	AudioFormatM4A                     // 141 (m4a/aac) first
)

// AudioFormatPriority maps format preference to itag priority order
var AudioFormatPriority = map[AudioFormat][]int{
	AudioFormatWebm: {774, 141, 140, 251},
	AudioFormatM4A:  {141, 774, 140, 251},
}

// PremiumAudioItags - legacy, use AudioFormatPriority instead
var PremiumAudioItags = []int{
	141, // 256kbps AAC
	774, // 256kbps AAC (alternative)
	140, // 128kbps AAC (fallback)
	251, // 160kbps Opus (fallback)
}

// === WEB CLIENT (for fetching visitorData) ===
const (
	WEBClientName    = "WEB"
	WEBClientVersion = "2.20241126.01.00"
	WEBAPIEndpoint   = "https://www.youtube.com/youtubei/v1/player"
	WEBAPIKey        = "AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8"
	WEBUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// === ANDROID_VR CLIENT (for video mode - works with visitorData) ===
const (
	AndroidVRClientName    = "ANDROID_VR"
	AndroidVRClientVersion = "1.60.19"
	AndroidVRAPIEndpoint   = "https://www.youtube.com/youtubei/v1/player"
	AndroidVRAPIKey        = "AIzaSyA8eiZmM1FaDVjRy-df2KTyQ_vz_yYM39w"
	AndroidVRUserAgent     = "com.google.android.apps.youtube.vr.oculus/1.60.19 (Linux; U; Android 12L; eureka-user Build/SQ3A.220605.009.A1) gzip"
	AndroidVRDeviceMake    = "Oculus"
	AndroidVRDeviceModel   = "Quest 3"
	AndroidVRPlatform      = "MOBILE"
	AndroidVROSName        = "Android"
	AndroidVROSVersion     = "12L"
)

// Video itags - prioritized order (1080p)
var VideoItags = []int{
	137, // 1080p AVC
	248, // 1080p VP9
	399, // 1080p AV1
	136, // 720p AVC (fallback)
}

// Standard audio itags (IOS)
var StandardAudioItags = []int{
	140, // 128kbps AAC (best compatibility)
	251, // 160kbps Opus
	250, // 70kbps Opus
}
