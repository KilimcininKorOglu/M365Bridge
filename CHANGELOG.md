# Changelog

All notable changes to M365Bridge will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).


## [1.3.6] - 2026-07-12

### Changed
- Move the project to Go 1.26 and adopt standard-library iterators (`slices.Backward`, `strings.SplitSeq`)
- Bump the Docker base images to `golang:1.26-alpine` and `alpine:3.24` and update the `gorilla/websocket` dependency
- Update pinned GitHub Actions to their latest releases via Dependabot

## [1.3.5] - 2026-07-12

### Added
- Harden the OpenAI Responses API for Codex: simulated tool-call retries, namespaced tools, ordered `response.failed` and `[DONE]` terminal events, and client-disconnect cancellation of upstream M365 streams

### Changed
- Modernize the codebase for the gopls modernize analyzer and enforce it as a CI job
- Harden the CI and release supply chain: pin GitHub Actions to commit SHAs, pin Docker base images by digest, add least-privilege workflow permissions, and add Dependabot

### Fixed
- Lowercase Responses tool policy error strings to satisfy Staticcheck
- Retry empty and required-tool Responses completions before failing
- Fix M365 chat routing for education tenants

## [1.3.1] - 2026-07-12

### Added
- Encrypt M365 web cookies with backward-compatible plaintext migration

### Changed
- Simplify the SSO authorization code exchange
- Add a context-window session continuity test
- Add a comprehensive tool-calling architecture guide
- Refine repository ignore rules

### Fixed
- Preserve provider tool names and Responses API call IDs

## [1.3.0] - 2026-07-11

### Added
- Add Claude Fable and GPT-5.6 reasoning model support
- Add opt-in built-in coding tools
- Add M365 conversation management support
- Add the Anthropic token counting endpoint
- Improve SSO extraction and broker redirect handling
- Support Anthropic system content blocks

### Changed
- Apply project-wide Go lint fixes
- Filter setup tokens by client ID
- Clean up API diagnostics

### Fixed
- Summarize broker authorization errors
- Isolate image generation conversations
- Reacquire expired designer broker tokens

## [1.2.2] - 2026-07-08

### Added
- Tool calling support for `/v1/completions` endpoint (simulated tool calls with `tools` field)
- Streaming support for `/v1/complete` endpoint (SSE events: `ping`, `completion` with delta text, final `completion` with `stop_reason`)

### Changed
- Consolidate `StreamChunk` and `ConversationStreamChunk` into a single `StreamChunk` type
- Consolidate `ChatStreamGen` to delegate to `ChatConversationStreamGen` (eliminates ~150 lines of duplicated WebSocket read loop logic)
- Remove shared state from `M365Client` for concurrent requests (per-request state via channel chunks, no mutex needed)
- `ChatConversation` now returns 6 values (added `conversationID` return value)
- `LastConversationID()`, `LastToolCalls()`, `LastThinking()` methods removed from `M365Client`
- CI: use CHANGELOG.md content for GitHub release body

### Fixed
- Make `/v1/models` endpoint public without auth requirement
- Merge system prompts into last message for M365 backend (system messages in earlier positions were silently dropped in multi-message conversations)

## [1.2.1] - 2026-07-07

### Added
- OpenAI Responses Compact API endpoint (`/v1/responses/compact`) for Codex remote compaction (streaming + non-streaming)

### Changed
- Documentation: add `/v1/responses/compact` endpoint docs to README, README.tr, AGENTS.md, and CLAUDE.md

## [1.2.0] - 2026-07-07

### Added
- Structured logging system (`pkg/logging`) with dual-writer (stdout + `data/proxy.log`) and leveled logging (DEBUG/INFO/WARN/ERROR/FATAL)
- OpenAI Images API endpoints (`/v1/images/generations`, `/v1/images/edits`) wrapping M365 DALL-E image generation
- Image generation support with server-side image download for both `url` and `b64_json` response formats
- Multiple image upload support for image edits (up to 16 images via repeated `image` form fields)
- OpenAI Responses API endpoint (`/v1/responses`)
- Generated image URL extraction from M365 WebSocket responses with markdown image link emission

### Fixed
- Client: extract generated image URLs from M365 WebSocket Progress messages (`contentOrigin: "ImageGeneration"`)

### Changed
- Documentation: fix model table formatting

## [1.1.0] - 2026-07-06

### Added
- Simulated tool calling mode for client-defined tools (OpenAI and Anthropic endpoints, streaming and non-streaming)
- Native Anthropic simulated mode with dedicated SSE handlers (`BuildSimulatedPromptAnthropic`/`ParseSimulatedResponseAnthropic`)
- Shell-routing for agentic coding loops (Claude Code, Droid CLI, Codex)
- Claude model support: `claude`, `claude-sonnet`, `claude-opus`, `claude-sonnet-4-20250514` (verified via tone test, routes to real Anthropic Claude Sonnet/Opus 4.6)
- Session ID embedded in model name via `:` separator (e.g. `gpt5.5-reasoning:my-session-001`)

### Changed
- Removed global `ToolCalling` configuration (`M365_TOOL_CALLING` env var and `Config.ToolCalling` field); tool calling is always enabled, `len(req.Tools) > 0` is the only gate
- Removed tool calling mode configuration (`M365_TOOL_CALLING_MODE` env var and `Config.ToolCallingMode` field); simulated mode is the only mode
- Removed fenced code block tool calling mode and all related functions (`ParseToolCalls`, `buildToolInstruction`, `injectToolDefs`, anti-confabulation retry logic)
- Strengthened and clarified tool use system instructions
- Updated documentation for tool calling and session isolation

## [1.0.3] - 2026-07-05

### Added
- SSO cookie-based re-authentication as fallback when 24h refresh token expires (AADSTS700084)
- SSO cookie capture during setup-wizard via `sso_cookies` field in setup.json
- Setup and token renewal process improvements

### Changed
- Docker setup documentation improved with single step-by-step flow

### Fixed
- SSO re-authentication reliability improvements (sso_reload=True, response_mode=fragment, correct redirect_uri, Origin header for SPA token exchange)

## [1.0.2] - 2026-07-05

### Changed
- Repository recreated to reset contributor history

## [1.0.1] - 2026-07-05

### Added
- Docker support with multi-stage Dockerfile and docker-compose.yml
- GitHub Actions CI workflow (cross-platform build for linux/darwin/windows amd64+arm64)
- GitHub Actions release workflow (6 platform binaries + multi-arch Docker image push to ghcr.io)
- Pre-built binary downloads from GitHub Releases
- .dockerignore for optimized Docker build context
- Version update skill for automated version bumping
- Prerequisites section and first-run expectations in README
- Model selection guide in README
- Anthropic SDK and image input Python examples in README
- .env example format in README

### Changed
- Project renamed from m365-copilot2api to M365Bridge
- Go module path changed to github.com/KilimcininKorOglu/M365Bridge
- Binary output moved to bin/ directory
- Encryption key storage moved from ~/.m365-copilot/ to data/tokens/
- .env file location moved from project root to data/.env
- Setup wizard output messages updated to use ./bin/m365-bridge paths
- Version field changed from const to var for ldflags override
- Version output changed from "M365 Copilot CLI" to "M365Bridge"
- README badges and Docker pull instructions added
- .gitignore updated for new project structure
