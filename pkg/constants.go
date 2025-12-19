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

// Premium audio itags - prioritized order
var PremiumAudioItags = []int{
	141, // 256kbps AAC
	774, // 256kbps AAC (alternative)
	140, // 128kbps AAC (fallback)
	251, // 160kbps Opus (fallback)
}

// === IOS CLIENT (no cipher needed) - DEFAULT for video mode ===
const (
	IOSClientName    = "IOS"
	IOSClientVersion = "19.45.4"
	IOSAPIEndpoint   = "https://www.youtube.com/youtubei/v1/player"
	IOSAPIKey        = "AIzaSyB-63vPrdThhKuerbB2N_l7Kwwcxj6yUAc"
	IOSUserAgent     = "com.google.ios.youtube/19.45.4 (iPhone16,2; U; CPU iOS 18_1_0 like Mac OS X;)"
	IOSDeviceMake    = "Apple"
	IOSDeviceModel   = "iPhone16,2"
	IOSPlatform      = "MOBILE"
	IOSOSName        = "iPhone"
	IOSOSVersion     = "18.1.0.22B83"
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
