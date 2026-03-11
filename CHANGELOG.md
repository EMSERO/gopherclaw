# Changelog

All notable changes to GopherClaw will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/), and this project adheres to [Semantic Versioning](https://semver.org/).

## [0.1.0] - 2026-03-11

Initial public release.

### Added
- Multi-provider LLM router (Anthropic, OpenAI, GitHub Copilot, OpenRouter) with fallback chain
- Telegram, Discord, and Slack channel integrations with pairing, reactions, and debounce
- HTTP gateway with auth, WebSocket control UI, and per-IP rate limiting
- Multi-agent orchestration with task graph dispatcher
- Cron job scheduler with persistent storage, concurrent guard, and error backoff
- Browser tool (chromedp) with session pooling
- Eidetic semantic memory tools
- Background task queue with concurrency control
- Exec, web, file, and memory tools with Docker sandbox support
- Destructive command confirmation system with configurable blocklist
- SSRF protection with DNS preflight checks
- Webhook endpoint with HMAC-SHA256 validation
- JSONL session persistence with TTL, token pruning, and session reaper
- Atomic store writes for crash-safe JSON persistence
- System prompt caching and LLM-powered context compaction
- Per-session model overrides via `/model` command
- Self-update from GitHub Releases
- `gopherclaw init` setup wizard
- `gopherclaw --migrate` for OpenClaw migration
- Config hot-reload via fsnotify
- Skills (SKILL.md) and workspace docs injection
- Comprehensive test suite (78%+ coverage, fuzz tests, stress tests)
- CI pipeline with race detection, lint, and security scans
- GoReleaser multi-platform builds (Linux/macOS, amd64/arm64)
