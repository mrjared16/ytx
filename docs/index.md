# ytx Documentation

`ytx` is a Go YouTube stream extractor for video and authenticated music extraction.

## 🏛️ Architecture
- [**Overview**](architecture/overview.md) — Video and music client paths and the design principles.
- [**Extraction Pipeline**](architecture/extraction-pipeline.md) — Call chain, modern URL-wrapper path, and dated historical measurements.
- [**PO Token Engine**](architecture/po-token.md) — Explicit `--po` token minting, dependencies, and failure modes.

## 💡 Concepts
- [**YouTube Extraction 101**](concepts/yt-extraction.md) — Innertube, clients, cipher, signature, and n-transform.

## 🛠️ Internal Design
- [**JS Runtime Layer**](internal/js-runtime.md) — Modern Bun/Node URL wrappers vs the legacy QuickJS path.
- [**Persistent Caching**](internal/persistent-cache.md) — Cache version 7, identity checks, TTL/grace behavior, and artifacts.

## 📖 Guides
- [**Testing**](guides/testing.md) — Test target semantics, live coverage, regression bounds, and probe behavior.
- [**Troubleshooting**](guides/troubleshooting.md) — Diagnosing 403s, PO gates, and decoder failures.
- [**Benchmarking**](guides/benchmarking.md) — What the existing targets actually measure.

## 🗃️ Archive

These are historical records, not descriptions of the current implementation. Dates and original context are retained.

- [**PO Mode Implementation Plan**](archive/po-mode-plan.md) — Historical context for the PO mode project.
- [**Historical Learnings**](archive/youtube-403-fix-learnings.md) — Retrospective on the 403 Forbidden fix.
- [**Historical Help Request**](archive/music-mode-help-request.md) — Context for the music mode optimization project.
