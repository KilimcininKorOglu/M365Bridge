# Changelog

All notable changes to M365Bridge will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).


## [1.5.0] - 2026-09-04

### Added
- Serve an image the model generates inside an ordinary chat answer. The link M365 returns can only be fetched with a designer token in `Authorization` plus a `fileToken` header, so no client could ever load it; every chat responder now rewrites it to `/v1/images/{ref}` and this gateway downloads the bytes itself. The reference is minted here and resolved from an in-memory store, never taken from the caller, so the route opens no SSRF surface
- Report that a picture is being generated while the turn waits for it. M365 announces the work about a minute before the address arrives, and the turn previously looked stalled. The state travels as `StreamChunk.Notice` and goes out as an SSE comment, so it enters no field contract, reaches no transcript, and an OpenAI or Anthropic client ignores it exactly as it ignores the keepalive
- Show that waiting state in the browser interface, in the reader's own language, and clear it by itself when the answer starts
- Read and write the account's Copilot personalization flags through `GET /v1/personalization` and `PATCH /v1/personalization`. M365 keeps a memory on the account rather than on a conversation, and it was measured reaching a brand-new conversation: content stored in an earlier session shaped a turn nobody asked to personalize. A write is verified by reading the flags back, because the endpoint answers 200 without moving the flag
- Offer that memory as a switch at the bottom of the interface sidebar. It is the operator's real M365 setting and reaches their web and mobile clients too, so nothing here changes it on its own, and a tenant that forbids personalization is reported rather than silently ignored

### Changed
- Ask the backend to merge pure deltas with the `feature.EnableMergingPureDeltas` variant. One long answer arrived as about 840 `writeAtCursor` deltas without it and about 130 with it, carrying the same bytes. Every other flag in the `variants` string was measured against the live backend and found inert
- Raise every Go declaration to 1.26.6, so `GOTOOLCHAIN=auto` cannot resolve an older toolchain and the workflows track the module floor through `go-version-file: go.mod` instead of a literal that drifts from it
- Count the snapshots `snapshotDelta` refuses and report the total on the turn's completion line. The refusals are correct and scale with the citations in the answer, but they were silent, so a turn that lost answer text looked exactly like a turn that lost nothing
- Annotate the directory open in `syncDir` as the proven-safe call it is, with the reason in the code, so the security gate reports zero rather than one permanent finding
- Pin that the middleware hands a streaming handler a working `http.Flusher`. Every SSE route asserts it and answers 500 when the assertion fails, so a middleware that wrapped the writer in a type without `Flush` would take all seven streaming routes down at once
- Document the Copilot memory switch in both READMEs

### Fixed
- Sync the directory after the atomic rename. Syncing a file commits its contents and not the name that points at them, so a power cut just after a successful write could lose it entirely rather than leave it torn
- Keep a generated image out of the snapshot baseline. The image markdown this package injects is never restated by a backend snapshot, so counting it into the baseline made the answer's first snapshot diverge and the opening words of the reply were dropped
- Verify a refresh token in the setup wizard by redeeming it, and stage it before the permanent file is written. Verifying through `Get` proved nothing, because `Get` returns a cached access token without reading the refresh token, so a placeholder passed whenever the previous run's cache was still warm
- Take the tenant and oid written to `data/.env` from the redeemed access token's claims rather than from the setup file. The oid takes no part in the token exchange, so nothing else in the wizard can tell a wrong one from a right one, and a wrong one surfaced much later as a failed chat request

## [1.4.9] - 2026-08-24

### Changed
- Keep every technical term in its original form in the Turkish documentation and in the Turkish interface catalog. `README.tr.md` translated terms the same document keeps in English elsewhere, so one term appeared under two names, and `web/src/locales/tr.json` showed the gate screen and the auth notice as `API anahtarı` while the same catalog kept `gateway`, `cookie` and `transcript`
- Rename the two `README.tr.md` headings that a link points at, so neither starts with `İ`. That letter lowercases to `i` plus the combining mark U+0307, which an anchor written with a plain `i` never reaches

### Fixed
- Correct two `README.tr.md` sentences that stated the wrong fact: the `M365_CLIENT_ID` row said the access tokens are arranged rather than issued to the client, and the broker refresh token step read as "returned by the refresher" where the token is rotated by it

## [1.4.8] - 2026-08-24

### Added
- Route a Codex `compaction_trigger` item in a `/v1/responses` input to `/v1/responses/compact`. Codex sends that item when it wants a conversation compacted; without the routing the request was answered as an ordinary turn, which consumed a message from the conversation quota and returned an answer instead of a summary. The item is also skipped during message conversion, so a stray trigger never reaches the prompt

### Changed
- Rewrite `README.md` and `README.tr.md` around one setup chapter

### Fixed
- Print the same browser snippet in the setup wizard that both READMEs document

## [1.4.7] - 2026-08-24

### Fixed
- Start a conversation in the browser interface when the gateway is reached over plain `http` on a LAN address. `crypto.randomUUID` carries `[SecureContext]` in the Web Cryptography IDL, so a browser outside a trusted origin does not carry it and naming a new session threw. The interface worked behind TLS and on `localhost`, which is why the failure only appeared on a deployment. The session id now comes from `getRandomValues`, the one member of `Crypto` an insecure context still offers, and falls back to a timestamp only where neither exists

## [1.4.6] - 2026-08-23

### Added
- Gate the browser interface with `M365_WEB_UI_PASSWORD`. An unset value leaves the interface open, as before. The password is one more credential the gateway accepts rather than a session of its own: the browser holds it in a cookie and sends it in the same `Authorization` header an API client sends its key in, so every credential stays on a header where a cross-site form cannot carry it
- Answer `GET /v1/auth` with the gate the browser interface must show (`none`, `password` or `api_key`), and `POST /v1/auth/verify` with whether an offered credential is accepted. Both are public, because the page that asks for a credential is served without one and cannot otherwise learn what to ask for. Without the verify route a wrong password would look like a right one whenever no API key is configured, since every route answers 200 then

### Changed
- `M365_WEB_UI_PASSWORD` and `M365_API_KEYS` are separate switches. A password with no key gates the interface and leaves the API open, which is what an empty key list means everywhere else in this gateway

### Fixed
- Compare every secret in constant time. The API key check returned as soon as two bytes differed, which let a caller measure how much of a guess was right

## [1.4.5] - 2026-08-23

### Added
- Translate the browser interface from JSON catalogs. Every file under `web/src/locales` is a language, so a fork adds one by dropping a file there and running `make ui`. The choice is stored in the `m365bridge_lang` cookie, English is the default, and a language the build does not carry resolves to English and rewrites the cookie
- Render an answer and its reasoning block as markdown, so a comparison table is a table and a citation is a link rather than a bare URL in the middle of a sentence. A user message stays literal
- Give every conversation its own address, `/c/{session id}`, so it can be reloaded onto, linked to, and reached with the browser's back and forward buttons
- Ask for a rename or a delete inside the page instead of through the browser's own dialogs
- Read the session id an agent client stamps on its request: `X-Claude-Code-Session-Id` from Claude Code and `Session-Id` from Codex, both below any session the caller names explicitly
- Accept `stop` on `/v1/chat/completions` and `/v1/completions`, and `stop_sequences` on `/v1/messages` and `/v1/complete`, on both the streaming and the non-streaming paths
- Report the stop sequence that ended a completion, as `null` rather than an empty string when the answer ended on its own
- Honour `parallel_tool_calls: false` and `disable_parallel_tool_use` by returning a single tool call

### Fixed
- Store a session's conversation before writing the end of a response, never after. A client that read the terminator and asked about its session at once was told the session does not exist, which made the browser interface show one conversation as two rows
- Delete a conversation on both sides whichever side starts, and clear every session bound to it, because more than one session can point at one conversation
- Keep each column's scrollbar inside its own column in the browser interface; a long conversation used to scroll the whole page and the sidebar list never scrolled on its own
- Report `created_at` on every Responses lifecycle event
- Read a bare url under an `image_url` or `input_image` block, which is the form several clients send
- Carry a `developer` message as instructions, the role OpenAI's reasoning models use in place of `system`
- Honour a `json_schema` response format
- End a completion at its stop sequence, including a sequence split across two upstream chunks
- Stop reading an answer that discusses sandboxes as a refusal
- Bound the request body every handler reads and every upstream response body this proxy reads
- Replace every state file in one step instead of truncating it, so a failed write cannot leave a half-written credential store
- Cut text on a rune boundary wherever a byte cap applies
- Report a response body that failed to encode instead of sending a truncated one

### Changed
- Resolve every request's session through one order, so `/v1/completions`, `/v1/messages` and `/v1/complete` read `session_id` and `user` from the body like the other endpoints
- Cut text on a character boundary from one place rather than at each call site
- Drop the unused scoped token exchange from the authentication layer
- Remove the dependabot configuration
- Rewrite both READMEs against the running service: the Go floor, the package list, the Anthropic SDK base URL, the capability tree shape, the MCP tool arguments, the image backend name, `413 request_too_large`, and what an unserved model name returns

## [1.4.4] - 2026-08-20

### Added
- Keep a Responses turn inside an unfinished Codex goal as `commentary` rather than `final_answer`, so the client continues the turn until an `update_goal` output reports `complete` or `blocked`

### Fixed
- Stream a custom tool's input over `response.custom_tool_call_input.delta` and `.done` instead of `response.function_call_arguments`, and name it by the item id the surrounding `output_item` events announced
- Carry an `input_image` block through the Responses input converter; the image was flattened away before the upload step the path already had
- Read a Responses `input_image` url as the bare string the API sends, and accept the `input_text` and `output_text` block names

### Changed
- Keep the `custom` declaration when one tool name is declared both freeform and as a function, because a freeform body is not the JSON a client parses in function-call arguments

## [1.4.3] - 2026-08-20

### Changed
- Print every environment variable the binary reads in `--help`, grouped by what it affects, instead of naming four and deferring the rest to the README
- Read the documented defaults from the same constants `LoadConfig` applies, so a changed default cannot leave a stale value in the usage text
- Document `M365_TENANT_ID`, `M365_USER_OID`, `M365_CLIENT_ID`, `M365_API_KEYS`, `M365_API_KEY` and `TZ` as environment variables in both READMEs; the two the process exits without were absent
- State in `--help` and in both READMEs that every flag is optional, and which defaults `serve` and `setup-wizard` apply when none is given
- List `--help` itself in the usage text

## [1.4.2] - 2026-08-20

### Fixed
- Keep the conversation a built-in coding tool loop created when the loop ends on an error, instead of orphaning it and starting a new one on the next turn
- Compare coding tool calls by signature rather than by raw arguments, so a repeat that differs only in JSON key order or whitespace no longer runs the tool again

## [1.4.1] - 2026-08-20

### Added
- Serve a browser interface at `/` from files embedded in the binary, gated on `M365_ENABLE_WEB_UI`
- Add the chat interface with a conversation sidebar, streaming answers and model selection
- Keep the composer in place while the conversation scrolls and move the model picker into it
- Record the turns of a session under `data/transcripts` and read them back over `GET /v1/sessions/{id}/messages`
- Bind a session to a conversation that already exists with `PUT /v1/sessions/{id}`
- Read a conversation this gateway never carried from the M365 conversation page
- Import an upstream conversation into a session over `GET /v1/conversations/{id}/messages`
- Offer to load the history of a conversation started in the M365 web or mobile client
- Document the whole command surface in `--help`, including both subcommands and the environment variables

### Changed
- Set `ReadHeaderTimeout` and `IdleTimeout` on the HTTP server, and tighten the log file, the written coding-tool file and their directories to owner-only
- State on every remaining static-analysis finding why the code is safe, so a real one cannot hide in the noise
- Cover the coding-tool workspace escape and the written file mode with tests
- Describe the web interface and reading a conversation held upstream in both READMEs
- Ignore the Go build cache directory

### Fixed
- Strip the backend's raw citation markers from answer text on every channel
- Check the error every discarded call returns, and report a failed cache directory instead of proceeding without one
- Restrict a log file an earlier build left readable

## [1.4.0] - 2026-08-20

### Added
- Expose Copilot through a JSON-RPC 2.0 Model Context Protocol server on `/mcp`
- Publish the OpenAI and the Anthropic model schema from a single `/v1/models` route
- Advertise the Codex catalog fields, owner, input budget and tool support in the model list
- Publish three measured tones and state thinking support per model entry
- Accept `max` reasoning effort and describe every preset
- Expose the session to conversation mapping over `/v1/sessions` and `/v1/sessions/{id}`
- Store the session id and a timestamp in the context cache
- Add the `/v1/health` reachability probe
- Surface the M365 conversation quota and report throttling
- Add context window configuration and defaults
- Report usage on the Anthropic Complete endpoint and on the streaming completions route
- Commit stream headers before the upstream turn and keep every SSE stream alive during upstream silence
- Carry the upstream HTTP status out of failed requests
- Report an M365 content refusal as a distinct error
- Add the evidence ledger for client-driven tool loops, and cap the tool rounds one turn may drive
- Put settled tool results into the simulation prompt and stop forwarding a tool call whose result is already settled
- Replace a completion report that no tool result backs
- Accept a progress note for a running client tool
- Validate tool call arguments against their JSON schema and enforce `tool_choice` when parsing the model response
- Reject tool results that answer no declared tool call
- Re-ask when the backend answers a tool request with prose, and claim an unfenced grammar tool body as a call
- Widen the sandbox refusal detector to more phrasings
- Stop routing a declared `web_search` to the client
- Keep tool call structure after text flattening
- Accept the Responses reasoning block and emit custom tools as `custom_tool_call` in Responses output
- Fetch caller-supplied remote image URLs
- Log file and audio content blocks instead of dropping them silently

### Changed
- Answer an empty Responses probe without an upstream turn
- Accumulate streamed text with `strings.Builder`
- Drop values no caller reads
- Restrict generated-image downloads to allowed hosts
- Document the error contract, the tool contract, the session routes, usage reporting, SSE resilience and the setup that does not use Docker

### Fixed
- Never forward M365's own tool calls, and keep the backend's own tool messages out of the answer
- Stop a re-encoded snapshot from repeating the answer
- Report a turn the backend ended without an answer, and a turn that ends with no answer and no verdict
- Reject a model this service does not serve instead of folding it into the auto entry
- Serve the reasoning model from a tone that reasons
- Print the CLI model list in a stable order
- Classify upstream failures instead of reporting them all as 500
- Put the error category in `type` and the machine-readable string in `code`
- Drop a streaming turn when its client disconnects, and bound SSE writes to a gone client
- Count tokens with `o200k_base` and report the source
- Count Anthropic prompt tokens like every other endpoint
- Report usage from the buffered coding-tool responders
- Bill tool choice framing only when tools were declared
- Reject a repeated tool call id and make tool call IDs unique
- Bound the tool result text the ledger carries
- Recognize every exit-code wording as a failure
- Keep backend calls suppressed for a `web_search`-only request
- Claim a grammar body wrapped in a valid envelope, and withhold an unparseable transport envelope from the answer
- Replace the unverified claim on the buffered streams too
- Match the backend's observed Turkish refusal wording
- Identify the proxy when fetching a remote image
- Accept the API key from `x-api-key` as well as `Authorization`
- Include the `signature` field on Anthropic thinking blocks
- Serialize refresh token redemption
- Preserve tool schemas and namespaces


## [1.3.7] - 2026-07-14

### Added
- Live-stream filtered reasoning/thinking on the OpenAI chat, OpenAI Responses, and Anthropic streaming endpoints
- Re-ask the backend once when simulated tool calls drop required arguments, applied across all endpoints
- Log the parsed tool-call count in the Anthropic simulated parser for parity with the OpenAI parser

### Changed
- Unify the simulated thinking/transport-envelope filter across all endpoints

### Fixed
- Stream Anthropic `tool_use` input as `input_json_delta` on both the direct and buffered streaming paths so SDK clients accumulate arguments correctly
- Drop simulated tool calls that are missing required arguments before emitting them

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
