package ytx

// GetClientConfig returns the appropriate client configuration for the mode
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
