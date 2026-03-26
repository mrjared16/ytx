# AGENTS.md
Guidance for coding agents working in `/home/user/workspace/projects/pytubefix/ytx`.

## Project
- Language: Go
- Module: `github.com/mrjared16/ytx`
- Go version: `1.25.5`
- Main library package: `./pkg`
- Main CLI: `./cmd/ytx`
- Diagnostic tool: `go run cmd/diagnose/main.go VIDEO_ID`
- Music-mode n-transform runtime: Bun preferred, Node.js fallback

## Key paths
- `pkg/` — extractor, cipher, auth, cache, JS runtime logic
- `cmd/ytx/` — primary CLI entrypoint
- `cmd/diagnose/` — compare `ytx` vs `yt-dlp`
- `docs/` — docs
- `Makefile` — canonical build/test/lint/benchmark/cache/release commands
- `DEVELOPMENT.md` — architecture, debugging, and regression notes

## Editor-specific rule files
Checked repository-local editor/agent rules:
- No `.cursor/rules/` directory found
- No `.cursorrules` file found
- No `.github/copilot-instructions.md` file found

## Setup
Install deps and verify JS runtime:
```bash
make deps
```
Notes:
- `make deps` runs `go mod download` and `go mod tidy`
- Music mode requires `bun` or `node`
- Some tests/features require `~/.config/ytx/cookies.txt`

## Build, format, vet, lint
Prefer Make targets:
```bash
make build
make build-fast
make build-release
make fmt
make vet
make lint
```
Useful raw commands:
```bash
go build -race -o ytx ./cmd/ytx
go build -o ytx ./cmd/ytx
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w ..." -o ytx ./cmd/ytx
```
Run `make fmt` before finishing edits.

## Test commands
Canonical targets:
```bash
make test
make test-short
make test-regression
make test-music
make test-all
```
Common raw commands:
```bash
go test -v -timeout 180s ./pkg
go test -v -run '^TestCacheIsolation$' -timeout 30s ./pkg
go test -v -run '^TestExtractorRegression$' ./pkg/...
UPDATE_GOLDEN=1 go test -v ./pkg/... -run '^TestExtractorRegression$'
```

## Running a single test
Preferred pattern:
```bash
go test -v ./pkg -run '^TestName$' -count=1
```
Examples:
```bash
go test -v ./pkg -run '^TestCacheIsolation$' -count=1
go test -v ./pkg -run '^TestNTransformIsolated$' -count=1
go test -v ./pkg -run '^(TestCacheIsolation|TestNTransformIsolated)$' -count=1
go test -list Test ./pkg
```
Verified in this repo: `go test -v -run '^TestCacheIsolation$' -count=1 ./pkg`

## Regression, diagnostics, benchmarks
- Main regression test: `TestExtractorRegression`
- Golden file: `pkg/testdata/golden_image.json`
- Update goldens intentionally with `UPDATE_GOLDEN=1`
- Keep timing assertions tolerant of network variance
- When extraction breaks, use `go run cmd/diagnose/main.go VIDEO_ID`
- Useful extras: `make bench`, `make bench-bulk`, `make bench-compare-runtime`, `make analyze-goja`, `make profile-cpu`, `make profile-mem`

## Code style guidelines
### Formatting
- Follow `gofmt` exactly
- Do not hand-align code that `gofmt` will rewrite
- Keep files `gofmt`-clean and `go vet`-clean
- Keep comments concise; use sentence comments above exported items and important logic blocks

### Imports
- Use standard Go grouping: stdlib, blank line, third-party/local
- Let `gofmt` sort imports
- Alias imports only when needed for clarity or collision avoidance
- Existing command code uses `ytx "github.com/mrjared16/ytx/pkg"`

### Naming
- Exported identifiers: PascalCase
- Unexported identifiers: lowerCamelCase
- Preserve acronym casing already used here: `URL`, `API`, `JS`, `TTL`, `HTTP`, `ID`
- Test names should be explicit and behavior-oriented, e.g. `TestCacheIsolation`

### Types and data modeling
- Prefer concrete structs with explicit JSON tags for request/response models
- Keep wire-format structs close to YouTube payloads; avoid premature abstraction
- Use pointer fields only when optionality/omission matters
- Reuse existing domain types before adding new ones

### Error handling
- Return errors; do not `panic` in normal library code
- Wrap errors with context using `fmt.Errorf("...: %w", err)`
- Keep messages specific and actionable
- For expected filesystem cases, use patterns like `os.IsNotExist(err)`
- If optional degradation is intentional, make it explicit in code/comments

### Concurrency and side effects
- Keep concurrency bounded and easy to reason about
- Prefer typed result structs for goroutine/channel communication, as in `Extractor.Extract`
- Avoid hidden global state except documented caches
- Do not break cache isolation or test isolation

### Testing conventions
- Use `t.TempDir()` for filesystem isolation
- Use `t.Helper()` in test helpers
- Use `t.Run()` for scenarios/subtests
- Skip environment-dependent tests cleanly when cookies or JS runtime are unavailable
- Review golden-file changes carefully

### Repository-specific guidance
- Prefer Make targets when an equivalent exists
- Preserve the dual-mode design: video mode vs music mode
- Music mode depends on cookies, SAPISIDHASH auth, and n-parameter transformation
- `signatureTimestamp` in playback context is critical; do not remove it casually
- When YouTube changes behavior, consult `DEVELOPMENT.md` and compare with `yt-dlp` before large refactors
- Keep Bun preferred but preserve Node fallback unless the runtime strategy is being changed intentionally

## Agent workflow guidance
- Make minimal, local changes first
- After edits, run formatting plus the most relevant focused test
- For `pkg/` logic changes, prefer a single targeted `go test -run '^TestName$' ./pkg -count=1` before broader suites
- If you touch regression-sensitive behavior, run `TestExtractorRegression` or explain why not
- If you touch music-mode extraction, consider cookie-dependent tests and/or `cmd/diagnose`

<!-- codebase-memory-mcp:start -->
# Codebase Knowledge Graph (codebase-memory-mcp)

This project uses codebase-memory-mcp to maintain a knowledge graph of the codebase.
ALWAYS prefer MCP graph tools over grep/glob/file-search for code discovery.

## Priority Order
1. `search_graph` — find functions, classes, routes, variables by pattern
2. `trace_call_path` — trace who calls a function or what it calls
3. `get_code_snippet` — read specific function/class source code
4. `query_graph` — run Cypher queries for complex patterns
5. `get_architecture` — high-level project summary

## When to fall back to grep/glob
- Searching for string literals, error messages, config values
- Searching non-code files (Dockerfiles, shell scripts, configs)
- When MCP tools return insufficient results

## Examples
- Find a handler: `search_graph(name_pattern=".*OrderHandler.*")`
- Who calls it: `trace_call_path(function_name="OrderHandler", direction="inbound")`
- Read source: `get_code_snippet(qualified_name="pkg/orders.OrderHandler")`
<!-- codebase-memory-mcp:end -->
