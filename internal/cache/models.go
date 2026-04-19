package cache

import "time"

const (
	cacheFileName        = "cipher.json"
	playerCacheFile      = "player.js.gz" // Compressed player.js for n-transform
	nRuntimeCacheFile    = "n_runtime.js"
	wrapperCodeCacheFile = "wrapper_code.bin"
	visitorCacheFile     = "visitor.json"
	poCacheFile          = "po_token.json"
	CacheTTL             = 6 * time.Hour
)

// CurrentCacheVersion is the current cache format version.
// Increment when cache structure changes to invalidate old caches.
const CurrentCacheVersion = 7

const POTokenCacheVersion = 2

// CipherCache represents the persisted cipher data.
type CipherCache struct {
	Version            int       `json:"version"`
	CreatedAt          time.Time `json:"created_at"`
	ExpiresAt          time.Time `json:"expires_at"`
	PlayerURL          string    `json:"player_url"` // e.g., /s/player/xxx/base.js
	PlayerFingerprint  string    `json:"player_fingerprint,omitempty"`
	BaseJSPath         string    `json:"basejs_path"` // Cached base.js path for fast refresh
	SigFunction        string    `json:"sig_function"`
	SigParam           int       `json:"sig_param"`
	SigUsesURLWrapper  bool      `json:"sig_uses_url_wrapper,omitempty"`
	NFunction          string    `json:"n_function"`
	SignatureTimestamp int       `json:"signature_timestamp"` // STS for API requests
	IsBytecode         bool      `json:"is_bytecode,omitempty"`
	WrapperBuildID     string    `json:"wrapper_build_id,omitempty"`
	JSCode             string    `json:"js_code,omitempty"`
}

// CacheManager handles persistent cipher caching.
type CacheManager struct {
	cacheDir string
}

type CacheInfo struct {
	CacheDir           string    `json:"cache_dir"`
	CachePath          string    `json:"cache_path"`
	PlayerCachePath    string    `json:"player_cache_path"`
	NRuntimeCachePath  string    `json:"n_runtime_cache_path"`
	Valid              bool      `json:"valid"`
	PlayerURL          string    `json:"player_url,omitempty"`
	BaseJSPath         string    `json:"basejs_path,omitempty"`
	PlayerFingerprint  string    `json:"player_fingerprint,omitempty"`
	SigFunction        string    `json:"sig_function,omitempty"`
	NFunction          string    `json:"n_function,omitempty"`
	SignatureTimestamp int       `json:"signature_timestamp,omitempty"`
	HasPlayerJS        bool      `json:"has_player_js"`
	HasNRuntimeJS      bool      `json:"has_n_runtime_js"`
	HasExtractedSig    bool      `json:"has_extracted_sig"`
	CreatedAt          time.Time `json:"created_at,omitempty"`
	ExpiresAt          time.Time `json:"expires_at,omitempty"`
}

// VisitorCache represents the persisted visitor data.
type VisitorCache struct {
	Data         string    `json:"data"`
	SessionIndex string    `json:"session_index"`
	DelegatedSID string    `json:"delegated_sid"`
	IsAuth       bool      `json:"is_auth"`
	AuthKey      string    `json:"auth_key,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// POTokenCache represents persisted PO token data for explicit --po mode.
type POTokenCache struct {
	Token        string    `json:"token,omitempty"`
	PlayerToken  string    `json:"player_token,omitempty"`
	URLToken     string    `json:"url_token,omitempty"`
	VideoID      string    `json:"video_id"`
	VisitorData  string    `json:"visitor_data,omitempty"`
	SessionIndex string    `json:"session_index,omitempty"`
	NetworkKey   string    `json:"network_key,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type poTokenCacheStore struct {
	Version int                     `json:"version"`
	Entries map[string]POTokenCache `json:"entries"`
}
