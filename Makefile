# =============================================================================
# ytx Makefile - YouTube Stream Extractor
# =============================================================================
#
# ARCHITECTURE NOTES:
# - Video mode: guest-only native chain VISIONOS -> ANDROID_VR -> ANDROID -> IOS.
#   Direct-URL formats are checked with near-end byte-range probes; this proves
#   range readability, not signature/n decode correctness.
# - Music mode: WEB_REMIX with cookies. Modern URL-wrapper players require Bun
#   or Node. For the legacy n-transform, auto resolves Bun -> Node (the "node"
#   binary) -> embedded QuickJS. Legacy signature decryption always uses
#   embedded QuickJS. QuickJS is not a modern URL-wrapper fallback.
# - PO mode: explicit --po. Each mint starts one short-lived Bun/Node subprocess;
#   there is no persistent daemon. jsdom/bgutils-js are external dependencies.
# - Cipher/player disk cache: version 7, 6-hour fresh TTL plus an 18-hour
#   stale-while-revalidate grace from expires_at. Visitor and PO caches have
#   separate TTLs. The in-memory runtime cache is process-scoped and resets its
#   updated timestamp on disk hydration, so that window is not universal.
#   `cache purge` retains visitor.json; `make cache-clear` removes the Makefile
#   CACHE_DIR only (the CLI may use a different os.UserCacheDir path).
# - Bulk extraction is sequential per video ID with a shared warmup and a
#   randomized inter-track delay. One `ytx music --bulk` process shares its
#   extractor/runtime; separate CLI invocations reuse only the on-disk cache.
#   No fixed throughput or audio-quality promise.
#
# MEASURE PERFORMANCE LOCALLY:
# Network, player changes, cache state, runtime startup, and account access all
# affect timings and available formats. The benchmark targets measure the
# current environment; their output is not a cross-machine guarantee.

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

# Makefile cache path (used by the targets below only). The CLI resolves its own
# path via os.UserCacheDir()/XDG_CACHE_HOME; set CACHE_DIR to match a custom path.
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
##   WHY: Race detector catches concurrency bugs (e.g., in the cached JS engine)
##   COST: Slower execution and higher memory use than a normal build
##   USE: Development only
.PHONY: build
build:
	@echo "Building $(BINARY_NAME) (development with race detector)..."
	go build -race -o $(BINARY_NAME) -v ./cmd/ytx
	@echo "Done: ./$(BINARY_NAME) ($(shell ls -lh $(BINARY_NAME) | awk '{print $$5}'))"

## build-release: Build optimized production binary
##   WHY: Smaller distributable binary with production build flags
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

# Test concerns:
#   1. make test              -> full ./pkg suite, including live extraction
#   2. make test-regression   -> focused golden-image regression coverage
#   3. make test-music-probe  -> explicit live YouTube-side probe tests
#
# The default test target is not a local-only unit-test command. Extraction
# tests use the network and can skip cookie-gated music cases when the test's
# hardcoded $HOME/.config/ytx/cookies.txt is absent (the CLI may use XDG).

## test: Run the package test suite, including live extraction
##   WHY: Default correctness check for code changes
##   NOTE: Includes TestExtractorRegression; network/cookie conditions apply
.PHONY: test
test:
	@echo "Running full ./pkg tests (live regression included; network required)..."
	go test -v -timeout 180s ./pkg
	@echo "Done"

## test-regression: Run focused regression tests
##   WHY: Verifies behavior against the saved golden image
##   OUTPUT: Golden comparison plus focused regression coverage
##   BOUND: TestExtractorRegression fails when total_ms > baseline + 300ms
##   TESTS:
##     - TestExtractorRegression: Video plus cookie-gated music/PO extraction
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

## test-regression-perf: Run TestExtractorRegression with STRICT_REGRESSION=1
##   ACTUAL: STRICT_REGRESSION is not read; there is no separate strict gate.
##   RULE: The regression test allows golden total_ms + 300ms (TimingToleranceMs).
##   USE: Not a stricter performance gate; compare benchmark results separately.
.PHONY: test-regression-perf
test-regression-perf:
	@echo "Running regression with STRICT_REGRESSION=1 (not read by Go tests)..."
	STRICT_REGRESSION=1 go test -v ./pkg -run '^TestExtractorRegression$$' -count=1 -timeout 180s
	@echo "Done"

## test-music-probe: Run opt-in live YouTube-side probe tests (single + bulk)
##   REQUIRES: $HOME/.config/ytx/cookies.txt (hardcoded by tests), Bun or Node, network.
##   PO CASES: Also need jsdom and bgutils-js resolvable by that runtime.
##   NOTE: Explicit opt-in live probe against YouTube
.PHONY: test-music-probe
test-music-probe:
	@echo "Running live music probe tests..."
	RUN_LIVE_PROBE_TESTS=1 go test -v -run '^(TestMusicModeWithCookies|TestMusicModeWithCookiesPO|TestBulkMusicProbeRegression|TestBulkMusicProbeRegressionPO)$$' ./pkg -timeout 120s -count=1
	@echo "Done"

# =============================================================================
# BENCHMARK TARGETS (for your bulk extraction use case)
# =============================================================================

## bench: Run package tests plus all benchmarks
##   NOTE: No -run filter, so normal tests run first, including live regression.
##   OUTPUT: Environment-dependent timings (benchtime=3x); not a fixed SLA.
.PHONY: bench
bench:
	@echo "Running benchmarks..."
	go test -bench=. -benchmem -benchtime=3x ./pkg
	@echo "Done"

## bench-bulk: Run one music extraction, then three more sequentially
##   NOTE: No cache purge; the "cold" run is cold only when cache is empty.
##   SCOPE: One video ID, not a 50-track throughput test; no fixed ms/track.
##   PROCESSES: Each run is a separate ytx process; only disk cache is reused.
##   SHOWS: First-run itag/bitrate; repeats are silent; timing is environment-dependent.
.PHONY: bench-bulk
bench-bulk: build-fast
	@echo "=== Bulk Music Extraction Benchmark ==="
	@echo ""
	@echo "First run (cold only if disk cache is empty; may fetch player.js):"
	@time -f "  Time: %E (real), CPU: %P" ./$(BINARY_NAME) music $(TEST_VIDEO) 2>/dev/null | python3 -c "import json,sys; d=json.load(sys.stdin); print(f'  Result: itag={d[\"itag\"]}, bitrate={d[\"bitrate\"]}')"
	@echo ""
	@echo "Subsequent runs (separate processes; reuse valid disk cache):"
	@for i in 1 2 3; do \
		time -f "  Video $$i: %E" ./$(BINARY_NAME) music $(TEST_VIDEO) 2>/dev/null | head -c 1 > /dev/null; \
	done
	@echo ""
	@echo "TIP: The first run can populate disk cache for later processes."
	@echo "     No fixed per-track timing or cross-process JS runner is promised."

## bench-compare-runtime: Compare Bun and Node.js process startup
##   NOTE: Measures trivial startup only; speedup is environment-dependent.
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
	@echo "Startup difference varies by machine and runtime version; measure here."

# =============================================================================
# CACHE MANAGEMENT
# =============================================================================

## cache-info: Show cache location, contents, and cipher metadata
##   SHOWS:
##     cipher.json: Player/cipher metadata; cached file sizes vary
##     player.js.gz: Gzip-compressed player bundle; cached file sizes vary
##   PATH: $(CACHE_DIR) is the Makefile default; the CLI uses os.UserCacheDir().
##   TTL: Cipher/player disk cache: 6 hours fresh plus 18 hours from expires_at.
##        Visitor/PO caches use separate TTLs; in-memory state is process-scoped.
.PHONY: cache-info
cache-info:
	@echo "=== Cache Information ==="
	@echo "Location (Makefile CACHE_DIR; CLI may use os.UserCacheDir/XDG_CACHE_HOME): $(CACHE_DIR)"
	@echo ""
	@if [ -d "$(CACHE_DIR)" ]; then \
		echo "Contents:"; \
		ls -lh $(CACHE_DIR)/; \
		echo ""; \
		echo "Expiry:"; \
		cat $(CACHE_DIR)/cipher.json 2>/dev/null | python3 -c "import json,sys,datetime; d=json.load(sys.stdin); exp=d.get('expires_at',''); print(f'  Expires: {exp}'); print(f'  Created: {d.get(\"created_at\",\"\")}'); print(f'  Player URL: {d.get(\"player_url\",\"\")}')" 2>/dev/null || echo "  (unable to read)"; \
	else \
		echo "No cache found. Run 'make cache-refresh' to fetch player/cipher artifacts."; \
	fi

## cache-clear: Remove the Makefile CACHE_DIR (not necessarily the CLI cache)
##   USE: When YouTube updates player.js and extraction fails
##   PATH: Fixed $(CACHE_DIR); the CLI honors os.UserCacheDir()/XDG_CACHE_HOME.
##   NOTE: Deletes visitor.json too; `ytx cache purge` retains visitor data.
.PHONY: cache-clear
cache-clear:
	@echo "Clearing cache..."
	rm -rf $(CACHE_DIR)
	@echo "Done"

## cache-warm: Warm guest video-mode visitor state
##   WHY: Warms only the guest video path's visitor cache
##   DOES: Runs `ytx video`; it does not fetch player.js/cipher or music auth state.
##   MUSIC CACHE: Use `make cache-refresh` or `ytx cache refresh VIDEO_ID`.
.PHONY: cache-warm
cache-warm: build-fast
	@echo "Warming guest video-mode visitor state..."
	@./$(BINARY_NAME) video $(TEST_VIDEO) > /dev/null 2>&1
	@echo "Guest video cache attempt complete (music cipher not warmed):"
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

## analyze-goja: Run the optional legacy goja compatibility script
##   NOTE: analyze_goja_full.go is not tracked; absent script prints guidance.
##   SCOPE: Legacy runtime diagnostics, not the modern Bun/Node URL-wrapper path.
.PHONY: analyze-goja
analyze-goja:
	@echo "Running optional legacy goja compatibility script..."
	@if [ -f "analyze_goja_full.go" ]; then \
		go run analyze_goja_full.go; \
	else \
		echo "Analysis script not found; see Makefile comments (legacy diagnostic only)."; \
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
	@which bun > /dev/null && echo "✓ Bun found (preferred)" || (which node > /dev/null && echo "✓ Node.js found (Bun preferred when available)" || echo "⚠ Install Bun or Node.js for music mode")

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
##   NOTE: Runtime and bulk sections below are qualitative, with no fixed
##         timing promise; use the benchmark targets to measure this machine.
##   NOTE: analyze-goja is a legacy diagnostic, not the current runtime path.
.PHONY: help
help:
	@echo "ytx - YouTube Stream Extractor"
	@echo "Video extraction plus cookie-authenticated music and sequential bulk mode"
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
	@echo "BULK MUSIC BEHAVIOR:"
	@echo "  ytx music --bulk is sequential in ONE process with a shared extractor/runtime."
	@echo "  Separate CLI invocations each start a new process; only disk cache is reused."
	@echo "  make bench-bulk does one first run plus three separate CLI repeats of one video."
	@echo ""
	@echo "JS RUNTIME (CURRENT):"
	@echo "  Modern URL-wrapper players require Bun or Node; QuickJS is rejected."
	@echo "  Legacy n-transform auto: Bun, then Node (node binary), then embedded QuickJS."
	@echo "  Legacy signature always uses embedded QuickJS; analyze-goja is legacy-only."
