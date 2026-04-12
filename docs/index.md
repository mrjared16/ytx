# ytx Documentation

`ytx` is a high-performance YouTube stream extractor written in Go, specifically optimized for TUI music players and low-latency playback.

## 🏛️ Architecture
- [**Overview**](architecture/overview.md) — The dual-client strategy and performance philosophy.
- [**Extraction Pipeline**](architecture/extraction-pipeline.md) — Detailed technical flow and timing profiles.
- [**PO Token Engine**](architecture/po-token.md) — Technical details of the Botguard bypass and optimizations.

## 💡 Concepts
- [**YouTube Extraction 101**](concepts/yt-extraction.md) — Understanding Innertube, Cipher, and N-Transform.

## 🛠️ Internal Design
- [**JS Runtime Layer**](internal/js-runtime.md) — How we execute and extract JavaScript transformations.
- [**Persistent Caching**](internal/persistent-cache.md) — Performance through intelligent caching.

## 📖 Guides
- [**Testing**](guides/testing.md) — The three test concerns: logical, regression, and live YouTube probes.
- [**Troubleshooting**](guides/troubleshooting.md) — Diagnosing and fixing extraction breaks.
- [**Benchmarking**](guides/benchmarking.md) — Measuring performance and interpreting metrics.

## 🗃️ Archive
- [**PO Mode Implementation Plan**](archive/po-mode-plan.md) — Historical context for the PO mode project.
- [**Historical Learnings**](archive/youtube-403-fix-learnings.md) — Retrospective on the 403 Forbidden fix.
- [**Historical Help Request**](archive/music-mode-help-request.md) — Context for the music mode optimization project.
