# =============================================================================
# ytx Makefile - High-Performance YouTube Stream Extractor
# =============================================================================
#
# Optimized for BULK MUSIC EXTRACTION (your TUI app use case)
#
# ARCHITECTURE OVERVIEW:
# ┌─────────────────────────────────────────────────────────────────────────────┐
# │                             ytx CLI                                         │
# ├──────────────────────────────────┬──────────────────────────────────────────┤
# │     Video Mode (ANDROID_VR)      │     Music Mode (WEB_REMIX + Cookies)     │
# │  • Direct URLs, no n-transform   │  • Requires n-parameter transformation  │
# │  • ~340ms per video              │  • Premium quality (itag 141, 256kbps)   │
# │  • Used occasionally             │  • YOUR MAIN USE CASE                    │
# └──────────────────────────────────┴──────────────────────────────────────────┘
#                                            │
#                                            ▼
# ┌─────────────────────────────────────────────────────────────────────────────┐
# │                        N-PARAMETER TRANSFORMATION                           │
# │                                                                             │
# │  BULK PERFORMANCE (with cached JS runner):                                  │
# │  ┌────────────────────────────────────────────────────────────────────────┐ │
# │  │  1st video:  ~700ms (cold start: fetch player.js + start Bun/Node)    │ │
# │  │  2nd video:  ~100ms (warm: reuse cached runner)                       │ │
# │  │  3rd video:  ~100ms (warm)                                            │ │
# │  │  ...                                                                  │ │
# │  │  100th video: ~100ms (warm)                                           │ │
# │  │                                                                       │ │
# │  │  TOTAL 100 videos: ~700ms + 99×100ms ≈ 10.6 seconds                   │ │
# │  │  vs naive approach: 100×700ms = 70 seconds (6.6x slower!)             │ │
# │  └────────────────────────────────────────────────────────────────────────┘ │
# └─────────────────────────────────────────────────────────────────────────────┘
#
# WHY GOJA CANNOT RUN THE ENTIRE player.js (and we need Bun/Node):
# ┌─────────────────────────────────────────────────────────────────────────────┐
# │  ANALYSIS RESULTS (from analyze_goja_full.go):                              │
# │                                                                             │
# │  1. PARSING: ✓ goja CAN parse the 2.6MB player.js (400ms)                  │
# │                                                                             │
# │  2. EXECUTION: ❌ FAILS because player.js uses ES2020+ features:           │
# │     • async/await         (6 occurrences)   - goja: ES5.1 only             │
# │     • Optional chaining   (10 occurrences)  - foo?.bar syntax              │
# │     • Private class fields(135 occurrences) - #privateField syntax         │
# │     • BigInt              (166 occurrences) - 123n literals                │
# │                                                                             │
# │  3. WHY NOT EXTRACT JUST THE N-FUNCTION?                                    │
# │     The n-function is only 39 bytes:                                        │
# │       function(S){return MP[z[7]](this,19,S)}                               │
# │                                                                             │
# │     But it calls MP[z[7]](), where:                                         │
# │     • MP is a 707KB function with 533 closure-scoped dependencies           │
# │     • z is an obfuscation array for method names                            │
# │     • 'this' refers to the IIFE closure context                             │
# │                                                                             │
# │     You CANNOT extract MP without running the entire 2.6MB IIFE first!      │
# │                                                                             │
# │  CONCLUSION:                                                                │
# │  We MUST use a full JavaScript runtime (Bun/Node) to:                       │
# │  1. Execute the entire player.js in a browser-like VM context               │
# │  2. Let the IIFE set up all closure variables (MP, z, etc.)                 │
# │  3. Then call the exposed n-function                                        │
# │                                                                             │
# │  Bun is preferred: 2x faster startup than Node.js                           │
# └─────────────────────────────────────────────────────────────────────────────┘
#
# OPTIMIZATION OPPORTUNITIES (10x engineer analysis):
# ┌─────────────────────────────────────────────────────────────────────────────┐
# │  CURRENT STATE (already optimized):                                         │
# │  ✓ Cached JS runner (Bun/Node process stays alive)                          │
# │  ✓ Compressed player.js cache (2.6MB → 848KB gzip)                          │
# │  ✓ Connection pooling (HTTP/2, keep-alive)                                  │
# │  ✓ Pre-compiled cipher (goja.Compile once, reuse)                           │
# │                                                                             │
# │  POTENTIAL FURTHER OPTIMIZATIONS:                                           │
# │  ┌────────────────────────────────────────────────────────────────────────┐ │
# │  │ 1. BATCH N-TRANSFORMS (not implemented yet)                            │ │
# │  │    Current: Send 1 n-param per IPC call                                │ │
# │  │    Better:  Send 10 n-params in one call, get 10 results back          │ │
# │  │    Benefit: Reduce IPC overhead (each call is ~1ms overhead)           │ │
# │  │                                                                        │ │
# │  │ 2. PARALLEL API CALLS (not implemented yet)                            │ │
# │  │    Current: Sequential YouTube API calls                               │ │
# │  │    Better:  Concurrent API calls with semaphore (limit 5)              │ │
# │  │    Benefit: Network latency is the bottleneck, parallelism helps       │ │
# │  │                                                                        │ │
# │  │ 3. WEBASSEMBLY QUICKJS (future consideration)                          │ │
# │  │    Use QuickJS compiled to WASM (github.com/aspect-build/aspect-cli)   │ │
# │  │    Pros: No external runtime dependency, faster startup                │ │
# │  │    Cons: Larger binary, WASM overhead, complex build                   │ │
# │  │                                                                        │ │
# │  │ 4. PRE-WARM DAEMON (for TUI apps)                                      │ │
# │  │    Keep a daemon running with hot JS runner                            │ │
# │  │    TUI connects via Unix socket                                        │ │
# │  │    Benefit: Instant response, no cold start                            │ │
# │  └────────────────────────────────────────────────────────────────────────┘ │
# └─────────────────────────────────────────────────────────────────────────────┘

# =============================================================================
# Configuration
# =============================================================================

BINARY_NAME := ytx
PKG := github.com/mrjared16/ytx
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')
LOCAL_GOOS := $(shell go env GOOS)
LOCAL_GOARCH := $(shell go env GOARCH)
ifeq ($(LOCAL_GOOS),windows)
RELEASE_EXT := .exe
else
RELEASE_EXT :=
endif

# Build flags for optimization
# -s: Omit symbol table (smaller binary, ~15% reduction)
# -w: Omit DWARF debugging info (smaller binary, ~10% reduction)
# -X: Inject version info at compile time
LDFLAGS := -s -w \
	-X 'main.Version=$(VERSION)' \
	-X 'main.BuildTime=$(BUILD_TIME)'

# Cache directory
CACHE_DIR := $(HOME)/.cache/ytx

# Test video IDs
TEST_VIDEO := dQw4w9WgXcQ
TEST_VIDEO_2 := hbl2Cuw75oE

# Troubleshooting defaults
DEBUG_VIDEO ?= $(TEST_VIDEO)
DEBUG_MODE ?= music
DEBUG_FIELD ?= url
DEBUG_COOKIE_FILE ?= $(HOME)/.config/ytx/cookies.txt

# =============================================================================
# Default Target
# =============================================================================

.PHONY: all
all: build

# =============================================================================
# BUILD TARGETS
# =============================================================================

## build: Build development binary with race detector
##   WHY: Race detector catches concurrency bugs (e.g., in cached JS runner)
##   COST: ~10x slower execution, 5-10x memory overhead
##   USE: Development only
.PHONY: build
build:
	@echo "Building $(BINARY_NAME) (development with race detector)..."
	go build -race -o $(BINARY_NAME) -v ./cmd/ytx
	@echo "Done: ./$(BINARY_NAME) ($(shell ls -lh $(BINARY_NAME) | awk '{print $$5}'))"

## build-release: Build optimized production binary
##   WHY: Smaller binary, faster execution, no debug overhead
##   -trimpath: Removes local file paths from binary (reproducible builds)
##   -ldflags "-s -w": Strips symbols and debug info
##   CGO_ENABLED=1: Required for embedded QuickJS support
.PHONY: build-release
build-release:
	@echo "Building $(BINARY_NAME) (release)..."
	CGO_ENABLED=1 go build \
		-trimpath \
		-ldflags "$(LDFLAGS)" \
		-o $(BINARY_NAME) \
		./cmd/ytx
	@echo "Done: ./$(BINARY_NAME) ($(shell ls -lh $(BINARY_NAME) | awk '{print $$5}'))"

## build-fast: Quick build without race detector
##   WHY: Fast iteration during development
##   USE: When you're confident about concurrency
.PHONY: build-fast
build-fast:
	@echo "Building $(BINARY_NAME) (fast, no race detector)..."
	go build -o $(BINARY_NAME) ./cmd/ytx
	@echo "Done: ./$(BINARY_NAME)"

# =============================================================================
# TEST TARGETS
# =============================================================================

# Three test concerns:
#   1. make test              -> logical / local correctness
#   2. make test-regression   -> regression baseline and timing checks
#   3. make test-music-probe  -> explicit live YouTube probes

## test: Run logical/local tests
##   WHY: Fast default correctness check for code changes
##   SAFE: Probe tests are opt-in and skipped here
.PHONY: test
test:
	@echo "Running logical/local tests..."
	go test -v -timeout 180s ./pkg
	@echo "Done"

## test-regression: Run regression-focused tests
##   WHY: Verifies correctness against saved baseline and regression-sensitive helpers
##   OUTPUT: Golden comparison plus focused regression coverage
##   TESTS:
##     - TestExtractorRegression: Video mode extraction with timing
##     - TestNTransformIsolated: N-parameter transformation
##     - TestCacheIsolation: Verifies tests don't pollute system
.PHONY: test-regression
test-regression:
	@echo "Running regression tests with golden image..."
	go test -v -run 'TestExtractorRegression|TestNTransformIsolated|TestCacheIsolation' ./pkg -timeout 180s
	@echo "Done"

## test-regression-update: Rewrite regression golden image intentionally
##   USE: After validating an intentional change in extraction behavior/timings
.PHONY: test-regression-update
test-regression-update:
	@echo "Updating regression golden image..."
	UPDATE_GOLDEN=1 go test -v ./pkg -run '^TestExtractorRegression$$' -count=1 -timeout 180s
	@echo "Done"

## test-regression-perf: Fail on significant slowdown versus golden baseline
##   RULE: Fails only when total_ms exceeds baseline by > max(250ms, 35%)
##   USE: Manual performance gate after latency-sensitive changes
.PHONY: test-regression-perf
test-regression-perf:
	@echo "Running strict regression performance check..."
	STRICT_REGRESSION=1 go test -v ./pkg -run '^TestExtractorRegression$$' -count=1 -timeout 180s
	@echo "Done"

## test-music-probe: Run opt-in live YouTube-side probe tests (single + bulk)
##   REQUIRES: ~/.config/ytx/cookies.txt
##   NOTE: Explicit opt-in live probe against YouTube
.PHONY: test-music-probe
test-music-probe:
	@echo "Running live music probe tests..."
	RUN_LIVE_PROBE_TESTS=1 go test -v -run '^(TestMusicModeWithCookies|TestBulkMusicProbeRegression)$$' ./pkg -timeout 120s -count=1
	@echo "Done"

# =============================================================================
# BENCHMARK TARGETS (for your bulk extraction use case)
# =============================================================================

## bench: Run performance benchmarks
##   OUTPUT:
##     BenchmarkNTransform: ~5ms per call (with cached runner)
##     BenchmarkVideoExtraction: ~360ms per video
.PHONY: bench
bench:
	@echo "Running benchmarks..."
	go test -bench=. -benchmem -benchtime=3x ./pkg
	@echo "Done"

## bench-bulk: Simulate bulk music extraction (your TUI use case)
##   WHY: Measures real-world performance for your app
##   SHOWS: Cold start vs warm cache performance
.PHONY: bench-bulk
bench-bulk: build-fast
	@echo "=== Bulk Music Extraction Benchmark ==="
	@echo ""
	@echo "Cold start (first video - includes player.js fetch):"
	@time -f "  Time: %E (real), CPU: %P" ./$(BINARY_NAME) music $(TEST_VIDEO) 2>/dev/null | python3 -c "import json,sys; d=json.load(sys.stdin); print(f'  Result: itag={d[\"itag\"]}, bitrate={d[\"bitrate\"]}')"
	@echo ""
	@echo "Warm cache (subsequent videos - reuses JS runner):"
	@for i in 1 2 3; do \
		time -f "  Video $$i: %E" ./$(BINARY_NAME) music $(TEST_VIDEO) 2>/dev/null | head -c 1 > /dev/null; \
	done
	@echo ""
	@echo "TIP: For your TUI app, first extraction warms the cache."
	@echo "     Subsequent extractions reuse the cached JS runner (~100ms each)."

## bench-compare-runtime: Compare Bun vs Node.js performance
##   WHY: Validates that Bun is faster (2x startup time improvement)
.PHONY: bench-compare-runtime
bench-compare-runtime:
	@echo "=== JS Runtime Comparison ==="
	@echo ""
	@echo "Bun startup (10 invocations):"
	@time -f "  Total: %E" sh -c 'for i in $$(seq 1 10); do bun -e "console.log(1)" > /dev/null; done'
	@echo ""
	@echo "Node.js startup (10 invocations):"
	@time -f "  Total: %E" sh -c 'for i in $$(seq 1 10); do node -e "console.log(1)" > /dev/null; done'
	@echo ""
	@echo "Bun is typically 2x faster on startup."

# =============================================================================
# CACHE MANAGEMENT
# =============================================================================

## cache-info: Show cache status and contents
##   SHOWS:
##     cipher.json: Signature decryption code (~20KB)
##     player.js.gz: Compressed player.js for n-transform (~850KB)
##   TTL: 6 hours
.PHONY: cache-info
cache-info:
	@echo "=== Cache Information ==="
	@echo "Location: $(CACHE_DIR)"
	@echo ""
	@if [ -d "$(CACHE_DIR)" ]; then \
		echo "Contents:"; \
		ls -lh $(CACHE_DIR)/; \
		echo ""; \
		echo "Expiry:"; \
		cat $(CACHE_DIR)/cipher.json 2>/dev/null | python3 -c "import json,sys,datetime; d=json.load(sys.stdin); exp=d.get('expires_at',''); print(f'  Expires: {exp}'); print(f'  Created: {d.get(\"created_at\",\"\")}'); print(f'  Player URL: {d.get(\"player_url\",\"\")}')" 2>/dev/null || echo "  (unable to read)"; \
	else \
		echo "No cache found. Run 'make cache-warm' to pre-warm."; \
	fi

## cache-clear: Clear all cached data
##   USE: When YouTube updates player.js and extraction fails
.PHONY: cache-clear
cache-clear:
	@echo "Clearing cache..."
	rm -rf $(CACHE_DIR)
	@echo "Done"

## cache-warm: Pre-warm cache for faster first run
##   WHY: For your TUI app, warm cache before user starts searching
##   DOES: Fetches player.js and cipher, saves to cache
.PHONY: cache-warm
cache-warm: build-fast
	@echo "Warming cache..."
	@./$(BINARY_NAME) video $(TEST_VIDEO) > /dev/null 2>&1
	@echo "Cache warmed:"
	@ls -lh $(CACHE_DIR)/ 2>/dev/null || echo "  Error warming cache"

## cache-refresh: Force refresh current player.js/cipher cache and print fingerprint
.PHONY: cache-refresh
cache-refresh: build-fast
	@echo "Refreshing cache for $(TEST_VIDEO)..."
	@./$(BINARY_NAME) cache refresh $(TEST_VIDEO)

## cache-show: Print cached player metadata/fingerprint
.PHONY: cache-show
cache-show: build-fast
	@./$(BINARY_NAME) cache info

# =============================================================================
# ANALYSIS & DEBUGGING
# =============================================================================

## analyze-goja: Analyze why goja cannot run player.js
##   SHOWS: ES2020+ features that goja doesn't support
##   EDUCATIONAL: Explains the technical constraints
.PHONY: analyze-goja
analyze-goja:
	@echo "Analyzing goja compatibility with player.js..."
	@if [ -f "analyze_goja_full.go" ]; then \
		go run analyze_goja_full.go; \
	else \
		echo "Analysis script not found. See Makefile comments for explanation."; \
	fi

## diagnose: Compare ytx with yt-dlp and direct API behavior
##   USE: First command to run when extraction breaks
.PHONY: diagnose
diagnose:
	@echo "Running extractor diagnosis for $(DEBUG_VIDEO)..."
	go run ./cmd/diagnose/main.go $(DEBUG_VIDEO)

## probe-url: Extract a fresh URL and perform a quick HTTP probe
##   DEFAULTS: DEBUG_MODE=music DEBUG_FIELD=url
##   VIDEO MODE: set DEBUG_FIELD=audio_url or DEBUG_FIELD=video_url
.PHONY: probe-url
probe-url: build-fast
	@set -e; \
	if [ "$(DEBUG_MODE)" = "music" ]; then \
		JSON=$$(./$(BINARY_NAME) music $(DEBUG_VIDEO) --cookies "$(DEBUG_COOKIE_FILE)" --profile); \
	else \
		JSON=$$(./$(BINARY_NAME) video $(DEBUG_VIDEO) --profile); \
	fi; \
	URL=$$(printf '%s' "$$JSON" | python3 -c 'import json,sys; data=json.load(sys.stdin); key=sys.argv[1]; value=data.get(key, ""); assert value, f"missing field: {key}"; print(value)' "$(DEBUG_FIELD)"); \
	echo "Probing $(DEBUG_MODE)/$(DEBUG_FIELD) for $(DEBUG_VIDEO)..."; \
	echo "URL: $$URL"; \
	curl -L --silent --show-error --output /dev/null --range 0-0 \
		--write-out "http=%{http_code} bytes=%{size_download} total=%{time_total}s speed=%{speed_download}B/s\n" \
		"$$URL"

## probe-download: Extract a fresh URL and download the full stream to /dev/null
##   USE: Distinguish invalid URL vs throttling vs rate limiting/blocking
.PHONY: probe-download
probe-download: build-fast
	@set -e; \
	if [ "$(DEBUG_MODE)" = "music" ]; then \
		JSON=$$(./$(BINARY_NAME) music $(DEBUG_VIDEO) --cookies "$(DEBUG_COOKIE_FILE)" --profile); \
	else \
		JSON=$$(./$(BINARY_NAME) video $(DEBUG_VIDEO) --profile); \
	fi; \
	URL=$$(printf '%s' "$$JSON" | python3 -c 'import json,sys; data=json.load(sys.stdin); key=sys.argv[1]; value=data.get(key, ""); assert value, f"missing field: {key}"; print(value)' "$(DEBUG_FIELD)"); \
	echo "Downloading $(DEBUG_MODE)/$(DEBUG_FIELD) for $(DEBUG_VIDEO) to /dev/null..."; \
	echo "URL: $$URL"; \
	curl -L --fail --silent --show-error --output /dev/null \
		--write-out "http=%{http_code} bytes=%{size_download} total=%{time_total}s speed=%{speed_download}B/s\n" \
		"$$URL"

## profile-cpu: CPU profiling for optimization
.PHONY: profile-cpu
profile-cpu:
	go test -cpuprofile=cpu.prof -bench=BenchmarkNTransform -benchtime=10x ./pkg
	@echo "Profile: cpu.prof"
	@echo "View: go tool pprof -http=:8080 cpu.prof"

## profile-mem: Memory profiling
.PHONY: profile-mem
profile-mem:
	go test -memprofile=mem.prof -bench=BenchmarkNTransform -benchtime=10x ./pkg
	@echo "Profile: mem.prof"
	@echo "View: go tool pprof -http=:8080 mem.prof"

# =============================================================================
# DEVELOPMENT
# =============================================================================

## fmt: Format code
.PHONY: fmt
fmt:
	go fmt ./...

## vet: Run go vet
.PHONY: vet
vet:
	go vet ./...

## lint: Run golangci-lint
.PHONY: lint
lint:
	@which golangci-lint > /dev/null || (echo "Install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest" && exit 1)
	golangci-lint run ./...

## deps: Download and tidy dependencies
.PHONY: deps
deps:
	go mod download
	go mod tidy
	@echo "Checking JS runtime..."
	@which bun > /dev/null && echo "✓ Bun found (preferred)" || (which node > /dev/null && echo "✓ Node.js found (Bun is 2x faster)" || echo "⚠ Install Bun or Node.js for music mode")

# =============================================================================
# INSTALLATION
# =============================================================================

## install: Build and install to system
.PHONY: install
install: build-release
	@if [ "$$(id -u)" -eq 0 ]; then \
		install -Dm 755 $(BINARY_NAME) /usr/local/bin/$(BINARY_NAME); \
		echo "Installed to /usr/local/bin/$(BINARY_NAME)"; \
	else \
		mkdir -p ~/.local/bin; \
		install -m 755 $(BINARY_NAME) ~/.local/bin/$(BINARY_NAME); \
		echo "Installed to ~/.local/bin/$(BINARY_NAME)"; \
	fi

## uninstall: Remove from system
.PHONY: uninstall
uninstall:
	rm -f /usr/local/bin/$(BINARY_NAME) ~/.local/bin/$(BINARY_NAME)
	@echo "Uninstalled"

# =============================================================================
# RELEASE
# =============================================================================

## release: Build release binary for current local platform
.PHONY: release
release: clean
	@mkdir -p release
	CGO_ENABLED=1 GOOS=$(LOCAL_GOOS) GOARCH=$(LOCAL_GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o release/$(BINARY_NAME)-$(LOCAL_GOOS)-$(LOCAL_GOARCH)$(RELEASE_EXT) ./cmd/ytx
	@ls -lh release/

# =============================================================================
# CLEANUP
# =============================================================================

## clean: Remove build artifacts
.PHONY: clean
clean:
	rm -f $(BINARY_NAME)
	rm -rf release/
	rm -f cpu.prof mem.prof
	@echo "Cleaned"

## clean-all: Remove everything including cache
.PHONY: clean-all
clean-all: clean cache-clear

# =============================================================================
# HELP
# =============================================================================

## help: Show this help
.PHONY: help
help:
	@echo "ytx - YouTube Stream Extractor"
	@echo "Optimized for bulk music extraction in TUI applications"
	@echo ""
	@echo "USAGE: make [target]"
	@echo ""
	@echo "BUILD:"
	@grep -E '^## (build|install|release)' $(MAKEFILE_LIST) | sed 's/^## /  /' | cut -d: -f1
	@echo ""
	@echo "TEST:"
	@grep -E '^## test' $(MAKEFILE_LIST) | sed 's/^## /  /' | cut -d: -f1
	@echo ""
	@echo "BENCHMARK:"
	@grep -E '^## bench' $(MAKEFILE_LIST) | sed 's/^## /  /' | cut -d: -f1
	@echo ""
	@echo "CACHE:"
	@grep -E '^## cache' $(MAKEFILE_LIST) | sed 's/^## /  /' | cut -d: -f1
	@echo ""
	@echo "ANALYSIS:"
	@grep -E '^## (analyze|profile|diagnose|probe)' $(MAKEFILE_LIST) | sed 's/^## /  /' | cut -d: -f1
	@echo ""
	@echo "BULK MUSIC PERFORMANCE:"
	@echo "  First video:  ~700ms (cold start)"
	@echo "  Subsequent:   ~100ms (cached JS runner)"
	@echo "  100 videos:   ~10.6s total (vs 70s without caching)"
	@echo ""
	@echo "WHY BUN/NODE.JS IS REQUIRED:"
	@echo "  player.js uses ES2020+ features (async/await, ?., #fields)"
	@echo "  goja only supports ES5.1"
	@echo "  Run 'make analyze-goja' for detailed analysis"
