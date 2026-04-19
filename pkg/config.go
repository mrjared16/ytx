package ytx

import (
	"os"
	"path/filepath"
)

const (
	// DefaultCookieFileName is the default cookie file name
	DefaultCookieFileName = "cookies.txt"
)

// GetConfigDir returns the XDG config directory for ytx
func GetConfigDir() (string, error) {
	// Check for XDG_CONFIG_HOME first
	if configHome := os.Getenv("XDG_CONFIG_HOME"); configHome != "" {
		return filepath.Join(configHome, "ytx"), nil
	}

	// Fallback to ~/.config
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".config", "ytx"), nil
}

// GetDefaultCookiePath returns the default cookie file path
func GetDefaultCookiePath() (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", err
	}

	// Create config directory if it doesn't exist
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return "", err
	}

	return filepath.Join(configDir, DefaultCookieFileName), nil
}

// CookieFileExists checks if the default cookie file exists
func CookieFileExists() bool {
	exists, err := CookieFileExistsWithError()
	if err != nil {
		return false
	}
	return exists
}

// CookieFileExistsWithError checks if the default cookie file exists and returns underlying errors.
func CookieFileExistsWithError() (bool, error) {
	path, err := GetDefaultCookiePath()
	if err != nil {
		return false, err
	}

	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
