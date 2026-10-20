package ytx

// videoClientProfile is an immutable innertube client pin used for video-mode
// (native, no cipher/PO/JS) extraction attempts.
type videoClientProfile struct {
	Name        string
	Version     string
	ClientID    int
	UserAgent   string
	OSName      string
	OSVersion   string
	DeviceMake  string
	DeviceModel string
	Platform    string
	// SDKVersion is the Android SDK version (androidSdkVersion), 0 when N/A.
	SDKVersion  int
	APIEndpoint string
	APIKey      string
	Origin      string
}

// Live-verified Sep 2026 (ytcui-dl-inspired revival pins).
// Centralized here so future YouTube breakage only requires updating this chain.
// API keys: Android-family clients share the public Android innertube key;
// Apple-family clients (VISIONOS/IOS) use the public iOS innertube key.
// Endpoint/origin are the standard youtubei player endpoint for all profiles.
// Deliberate ?key= choice: unlike ytcui-dl (which omits the key), this repo
// sends per-family keys because the youtubei player endpoint requires a key
// parameter and the VISIONOS live path was verified working with it.
var videoClientChain = []videoClientProfile{
	{
		Name:        "VISIONOS",
		Version:     "1.02",
		ClientID:    101,
		UserAgent:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_7_3) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15",
		OSName:      "visionOS",
		OSVersion:   "26.5.23O471",
		DeviceMake:  "Apple",
		DeviceModel: "RealityDevice17,1",
		APIEndpoint: "https://www.youtube.com/youtubei/v1/player",
		APIKey:      "AIzaSyB-63vPrdThhSEqEBirbVHzRNswu6nKC0E",
		Origin:      "https://www.youtube.com",
	},
	{
		Name:        "ANDROID_VR",
		Version:     "1.65.10",
		ClientID:    28,
		UserAgent:   "com.google.android.apps.youtube.vr.oculus/1.65.10 (Linux; U; Android 12L; en_US) gzip",
		OSName:      "Android",
		OSVersion:   "12L",
		DeviceMake:  "Oculus",
		DeviceModel: "Quest 3",
		Platform:    "MOBILE",
		SDKVersion:  32,
		APIEndpoint: "https://www.youtube.com/youtubei/v1/player",
		APIKey:      "AIzaSyA8eiZmM1FaDVjRy-df2KTyQ_vz_yYM39w",
		Origin:      "https://www.youtube.com",
	},
	{
		Name:        "ANDROID",
		Version:     "20.10.38",
		ClientID:    3,
		UserAgent:   "com.google.android.youtube/20.10.38 (Linux; U; Android 14; en_US) gzip",
		OSName:      "Android",
		OSVersion:   "14",
		DeviceMake:  "Google",
		Platform:    "MOBILE",
		SDKVersion:  34,
		APIEndpoint: "https://www.youtube.com/youtubei/v1/player",
		APIKey:      "AIzaSyA8eiZmM1FaDVjRy-df2KTyQ_vz_yYM39w",
		Origin:      "https://www.youtube.com",
	},
	{
		Name:        "IOS",
		Version:     "20.10.4",
		ClientID:    5,
		UserAgent:   "com.google.ios.youtube/20.10.4 (iPhone16,2; U; CPU iOS 18_3_2 like Mac OS X; en_US)",
		OSName:      "iOS",
		OSVersion:   "18.3.2.22D82",
		DeviceMake:  "Apple",
		DeviceModel: "iPhone16,2",
		Platform:    "MOBILE",
		APIEndpoint: "https://www.youtube.com/youtubei/v1/player",
		APIKey:      "AIzaSyB-63vPrdThhSEqEBirbVHzRNswu6nKC0E",
		Origin:      "https://www.youtube.com",
	},
}

// GetClientConfig returns the appropriate client configuration for the mode.
//
// ModeVideo returns the historical single-shot ANDROID_VR config unchanged:
// it is the configuration the public ExtractVideo contract is defined against
// and the config that audio-mode consumers share. The NATIVE video chain does
// not read this config at all - it uses the explicit videoClientChain profiles
// via callPlayerAPIWithProfile - so restoring ANDROID_VR here leaves the
// native profile chain completely unaffected.
func GetClientConfig(mode ClientMode) ClientConfig {
	switch mode {
	case ModeVideo:
		return ClientConfig{
			Name:         AndroidVRClientName,
			Version:      AndroidVRClientVersion,
			APIEndpoint:  AndroidVRAPIEndpoint,
			APIKey:       AndroidVRAPIKey,
			UserAgent:    AndroidVRUserAgent,
			Origin:       "https://www.youtube.com",
			NeedsCipher:  false,
			NeedsCookies: false,
			DeviceMake:   AndroidVRDeviceMake,
			DeviceModel:  AndroidVRDeviceModel,
			Platform:     AndroidVRPlatform,
			OSName:       AndroidVROSName,
			OSVersion:    AndroidVROSVersion,
			Headers: map[string]string{
				"X-Youtube-Client-Name":    "28",
				"X-Youtube-Client-Version": AndroidVRClientVersion,
			},
		}
	case ModeMusic:
		return ClientConfig{
			Name:         ClientName,
			Version:      ClientVersion,
			APIEndpoint:  APIEndpoint,
			APIKey:       ClientKey,
			UserAgent:    UserAgent,
			Origin:       Origin,
			NeedsCipher:  true,
			NeedsCookies: true,
			Headers: map[string]string{
				"X-Youtube-Client-Name":    "67",
				"X-Youtube-Client-Version": ClientVersion,
			},
		}
	default:
		// Default to music mode
		return GetClientConfig(ModeMusic)
	}
}
