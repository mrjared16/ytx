package ytx

// GetClientConfig returns the appropriate client configuration for the mode
func GetClientConfig(mode ClientMode) ClientConfig {
	switch mode {
	case ModeVideo:
		return ClientConfig{
			Name:         IOSClientName,
			Version:      IOSClientVersion,
			APIEndpoint:  IOSAPIEndpoint,
			APIKey:       IOSAPIKey,
			UserAgent:    IOSUserAgent,
			Origin:       "https://www.youtube.com",
			NeedsCipher:  false,
			NeedsCookies: false,
			DeviceMake:   IOSDeviceMake,
			DeviceModel:  IOSDeviceModel,
			Platform:     IOSPlatform,
			OSName:       IOSOSName,
			OSVersion:    IOSOSVersion,
			Headers: map[string]string{
				"X-Youtube-Client-Name":    "5",
				"X-Youtube-Client-Version": IOSClientVersion,
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
