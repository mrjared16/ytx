package ytx

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mrjared16/ytx/internal/cache"
)

const visitorDataTTL = 30 * time.Minute

const (
	visitorDataSWRGrace = 2 * time.Hour
	cipherSWRGrace      = 18 * time.Hour
	backgroundWarmupTTL = 20 * time.Second
	defaultPOTokenTTL   = 10 * time.Minute
	stsWaitBudget       = 250 * time.Millisecond
)

// Extractor handles YouTube stream extraction
type Extractor struct {
	config           ClientConfig
	runtime          *Runtime
	cookies          []*http.Cookie
	httpClient       *http.Client
	sapisid          string
	cipher           *Cipher // lazy initialized, only for music mode
	cipherMu         sync.Mutex
	cacheManager     *cache.CacheManager // file-based persistent cache
	cipherRetried    atomic.Bool         // track if we've already retried with fresh cipher
	visitorData      string              // required since Jan 2025 for all API requests
	sessionIndex     string              // X-Goog-AuthUser header
	delegatedSID     string              // X-Goog-PageId header
	profile          bool                // enable profiling
	timings          Timings             // profiling timers
	fetchSubs        bool                // whether to include subtitles in output
	subLangs         []string            // subtitle languages to fetch (nil = default, empty = all)
	maxHeight        int                 // max video height (0 = no limit)
	audioFormat      AudioFormat         // preferred audio format (default: AudioFormatWebm)
	earlySTSOverride int                 // STS from early cipher signal (for API overlap)
	warnings         []string
	poMode           bool // explicit PO mode; off by default
	poChallenge      *POTokenChallenge
	poPlayerToken    string
	poURLToken       string
}

// defaultSubtitleLangs are fetched when --subs is used without --sub-langs
// Users can override via --sub-langs flag
var defaultSubtitleLangs = []string{"en"}

// ExtractorOption customizes shared services for a new extractor.
type ExtractorOption func(*Extractor) error

func WithRuntime(runtime *Runtime) ExtractorOption {
	return func(e *Extractor) error {
		if runtime == nil {
			return fmt.Errorf("runtime cannot be nil")
		}
		e.runtime = runtime
		return nil
	}
}

func WithHTTPClient(client *http.Client) ExtractorOption {
	return func(e *Extractor) error {
		if client == nil {
			return fmt.Errorf("http client cannot be nil")
		}
		e.httpClient = client
		return nil
	}
}

func WithCacheManager(cacheManager *cache.CacheManager) ExtractorOption {
	return func(e *Extractor) error {
		e.cacheManager = cacheManager
		return nil
	}
}

func WithPOMode(enabled bool) ExtractorOption {
	return func(e *Extractor) error {
		e.poMode = enabled
		return nil
	}
}

// NewExtractor creates a new extractor for the given mode
func NewExtractor(mode ClientMode, cookieFile string, opts ...ExtractorOption) (*Extractor, error) {
	config := GetClientConfig(mode)

	ext := &Extractor{
		config:  config,
		runtime: defaultRuntime,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				ForceAttemptHTTP2:   true,
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}

	for _, opt := range opts {
		if err := opt(ext); err != nil {
			return nil, err
		}
	}

	// Initialize file-based cache manager (graceful degradation if fails)
	if ext.cacheManager == nil {
		if cm, err := cache.NewCacheManager(); err == nil {
			ext.cacheManager = cm
		}
	}

	if ext.runtime == nil {
		ext.runtime = defaultRuntime
	}

	// Only load cookies if needed (music mode)
	if config.NeedsCookies {
		if cookieFile == "" {
			return nil, fmt.Errorf("music mode requires --cookies")
		}

		cookies, err := ParseNetscapeCookieFile(cookieFile)
		if err != nil {
			return nil, fmt.Errorf("failed to parse cookies: %w", err)
		}
		ext.cookies = cookies

		// Find SAPISID
		sapisid := FindCookie(cookies, "SAPISID", "__Secure-3PAPISID")
		if sapisid == "" {
			return nil, fmt.Errorf("SAPISID cookie not found - ensure you're logged in")
		}
		ext.sapisid = sapisid

	}

	return ext, nil
}

// SetProfile enables profiling for this extractor
func (e *Extractor) SetProfile(enabled bool) {
	e.profile = enabled
}

// SetFetchSubtitles enables subtitle extraction with optional language filter.
// If langs is nil or empty, uses default language (en).
// Use []string{"all"} to fetch all available languages.
func (e *Extractor) SetFetchSubtitles(langs []string) {
	e.fetchSubs = true
	e.subLangs = langs
}

// SetMaxHeight sets the maximum video height (e.g., 1080 for 1080p).
// Set to 0 to disable limit (default behavior).
func (e *Extractor) SetMaxHeight(height int) {
	e.maxHeight = height
}

func (e *Extractor) SetPreferWebm(enabled bool) {
	if enabled {
		e.audioFormat = AudioFormatWebm
	} else {
		e.audioFormat = AudioFormatM4A
	}
}

func (e *Extractor) SetAudioFormat(format AudioFormat) {
	e.audioFormat = format
}

func (e *Extractor) addWarning(warning string) {
	if warning == "" {
		return
	}
	for _, existing := range e.warnings {
		if existing == warning {
			return
		}
	}
	e.warnings = append(e.warnings, warning)
}

func (e *Extractor) addWarningf(format string, args ...any) {
	e.addWarning(fmt.Sprintf(format, args...))
}
