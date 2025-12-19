package ytx

import (
	"bufio"
	"crypto/sha1"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// GenerateSAPISIDHASH generates the Authorization header for YouTube Music API.
// Formula: SAPISIDHASH timestamp_SHA1(timestamp + " " + SAPISID + " " + origin)
func GenerateSAPISIDHASH(sapisid, origin string) string {
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	hashInput := fmt.Sprintf("%s %s %s", timestamp, sapisid, origin)
	hash := sha1.Sum([]byte(hashInput))
	return fmt.Sprintf("SAPISIDHASH %s_%x", timestamp, hash)
}

// ParseNetscapeCookieFile parses a Netscape-format cookies.txt file.
// Format: domain \t include_subdomains \t path \t secure \t expires \t name \t value
func ParseNetscapeCookieFile(path string) ([]*http.Cookie, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open cookie file: %w", err)
	}
	defer file.Close()

	var cookies []*http.Cookie
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and empty lines
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}

		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue // Invalid line
		}

		// Parse expiry timestamp
		expiry, _ := strconv.ParseInt(fields[4], 10, 64)

		cookies = append(cookies, &http.Cookie{
			Domain:  strings.TrimPrefix(fields[0], "."),
			Path:    fields[2],
			Secure:  fields[3] == "TRUE",
			Expires: time.Unix(expiry, 0),
			Name:    fields[5],
			Value:   fields[6],
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading cookie file: %w", err)
	}

	return cookies, nil
}

// FindCookie finds a cookie by name from the list, trying multiple names.
func FindCookie(cookies []*http.Cookie, names ...string) string {
	for _, name := range names {
		for _, c := range cookies {
			if c.Name == name {
				return c.Value
			}
		}
	}
	return ""
}

// BuildCookieHeader builds the Cookie header string from cookies.
func BuildCookieHeader(cookies []*http.Cookie) string {
	var parts []string
	for _, c := range cookies {
		parts = append(parts, fmt.Sprintf("%s=%s", c.Name, c.Value))
	}
	return strings.Join(parts, "; ")
}
