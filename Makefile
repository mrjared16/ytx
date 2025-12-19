# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod

# Binary name
BINARY_NAME=ytx
BINARY_UNIX=$(BINARY_NAME)_unix

# Version and build info
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME=$(shell date -u '+%Y-%m-%d_%H:%M:%S')
LDFLAGS=-ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME)"

.PHONY: all build clean test deps help install uninstall

all: test build

build: 
	$(GOBUILD) $(LDFLAGS) -o $(BINARY_NAME) -v ./cmd/ytx

test:
	$(GOTEST) -v ./...

clean: 
	$(GOCLEAN)
	rm -f $(BINARY_NAME)
	rm -f $(BINARY_UNIX)

deps:
	$(GOMOD) download
	$(GOMOD) tidy

# Cross compilation
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BINARY_UNIX) -v ./cmd/ytx

# Install to system
install: build
	@if [ "$(shell id -u)" -eq 0 ]; then \
		echo "Installing to /usr/local/bin..."; \
		install -Dm 755 $(BINARY_NAME) /usr/local/bin/$(BINARY_NAME); \
		echo "YTX installed to /usr/local/bin/ytx"; \
	else \
		echo "Installing to ~/.local/bin..."; \
		mkdir -p ~/.local/bin; \
		install -m 755 $(BINARY_NAME) ~/.local/bin/$(BINARY_NAME); \
		echo "YTX installed to ~/.local/bin/ytx"; \
		echo "Make sure ~/.local/bin is in your PATH"; \
	fi
	@echo ""
	@echo "🍪 COOKIE SETUP FOR MUSIC MODE:"
	@echo "   To use music mode (256kbps audio), you need YouTube Premium cookies:"
	@echo "   1. Install browser extension: 'Get cookies.txt LOCALLY' (Chrome)"
	@echo "   2. Visit https://music.youtube.com and login with Premium"
	@echo "   3. Export cookies as 'cookies.txt' (Netscape format)"
	@echo "   4. Use: ytx music VIDEO_ID --cookies cookies.txt"
	@echo ""
	@echo "⚡ QUICK TEST:"
	@echo "   Video mode: ytx video hbl2Cuw75oE"
	@echo "   Music mode: ytx music hbl2Cuw75oE --cookies cookies.txt"

# Uninstall from system
uninstall:
	@if [ -f /usr/local/bin/$(BINARY_NAME) ]; then \
		rm -f /usr/local/bin/$(BINARY_NAME); \
		echo "Removed /usr/local/bin/$(BINARY_NAME)"; \
	fi
	@if [ -f ~/.local/bin/$(BINARY_NAME) ]; then \
		rm -f ~/.local/bin/$(BINARY_NAME); \
		echo "Removed ~/.local/bin/$(BINARY_NAME)"; \
	fi

# Development targets
run:
	$(GOCMD) run .

fmt:
	$(GOCMD) fmt ./...

vet:
	$(GOCMD) vet ./...

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed, skipping lint"; \
	fi

# Release targets
release: clean test build
	mkdir -p release
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o release/$(BINARY_NAME)-linux-amd64 ./cmd/ytx
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o release/$(BINARY_NAME)-darwin-amd64 ./cmd/ytx
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o release/$(BINARY_NAME)-windows-amd64.exe ./cmd/ytx

help:
	@echo "Available targets:"
	@echo "  all          - Run tests and build"
	@echo "  build        - Build the binary"
	@echo "  test         - Run tests"
	@echo "  clean        - Clean build artifacts"
	@echo "  deps         - Download and tidy dependencies"
	@echo "  install      - Build and install to system (/usr/local/bin or ~/.local/bin)"
	@echo "  uninstall    - Remove from system"
	@echo "  run          - Run the application"
	@echo "  fmt          - Format Go code"
	@echo "  vet          - Run go vet"
	@echo "  lint         - Run golangci-lint (if available)"
	@echo "  build-linux  - Cross compile for Linux"
	@echo "  release      - Build release binaries for multiple platforms"
	@echo "  help         - Show this help"
	@echo ""
	@echo "Example usage:"
	@echo "  make install    # Build and install ytx"
	@echo "  ytx video dQw4w9WgXcQ    # Extract video URLs"
	@echo "  ytx music ID --cookies ~/cookies.txt  # Extract audio URLs"