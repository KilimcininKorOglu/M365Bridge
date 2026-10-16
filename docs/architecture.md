# Architecture & Security

M365Bridge is an architectural bridge connecting Microsoft 365 Copilot's proprietary enterprise WebSocket protocol with modern, standard HTTP interfaces.

---

## Architectural Data Flow

```text
OpenAI / Anthropic Client (Claude Code, Cursor, SDKs)
  │
  ▼
pkg/servers (HTTP Adapter & Protocol Translators)
  ├── Sessions, Stop Sequences, Token Estimation
  └── Simulated Tool Calling & Built-in Coding Tools
  │
  ▼
pkg/payload (Constructs M365 JSON Payload & Annotations)
  │
  ▼
pkg/client (Per-request SignalR WebSocket over TLS)
  │
  ▼
wss://substrate.office.com/m365Copilot/Chathub
  │
  ▼
pkg/client StreamChunk Event Loop
  ├── Answer accumulator (writeAtCursor vs snapshot delta deduplication)
  ├── Image reference translation
  └── Reasoning / thinking block parsing
  │
  ▼
pkg/servers Streaming SSE / JSON Response
```

### Upstream Protocol (SignalR)
- **Per-Turn Connection**: M365 Copilot communicates over a stateful SignalR WebSocket protocol framed by ASCII Record Separator characters (`\x1e`).
- **Connection Isolation**: M365Bridge opens a fresh, isolated WebSocket connection for each request and closes it upon turn completion or client cancellation.
- **Answer Accumulation**: Copilot sends answer content over two interleaved channels: incremental cursor writes (`writeAtCursor`) and full snapshot restatements (`messages[].text`). M365Bridge employs an `answerAccumulator` with strict delta analysis to prevent answer duplication or raw private-use citation markers (`\ue200cite`) from reaching the client.

---

## Security Model

### Cryptography & Credential Storage
- **AES-256-GCM**: Sensitive tokens and cookies stored under `data/tokens/` are encrypted at rest with authenticated AES-256-GCM.
- **Key Management**: The master encryption key is generated automatically and saved to `data/tokens/encryption.key` with strict filesystem permissions (`0600`).
- **Zero-Storage Transcripts**: Conversation history is maintained in upstream cloud memory. Local transcripts are only saved if `M365_ENABLE_WEB_UI=1` is set, and are bounded by entry and byte limits.

### Atomic File System Operations
All state files (configuration, token caches, session registries, transcripts) are updated via `atomicfile.Write`:
1. Writes content to an ephemeral temporary file in the target directory.
2. Flushes and syncs data to physical storage (`fsync`).
3. Renames the temporary file over the destination atomically.
4. Syncs the parent directory.
This prevents corrupted or half-written credential files in the event of unexpected process termination or power loss.

### Constant-Time Comparisons
API key and Web UI password verifications use cryptographic constant-time checks (`crypto/subtle.ConstantTimeCompare`), preventing byte-by-byte timing attacks.

---

## Project Structure

```
cmd/cli/main.go            # Application entry point and subcommand router
pkg/
  atomicfile/              # Atomic write-sync-rename operations
  auth/auth.go             # TokenManager, OAuth 2.0 Device Code, token caching
  auth/sso.go              # MSAL broker token flow for Microsoft Designer
  client/client.go         # M365Client, SignalR WebSocket transport
  client/conversations.go  # Upstream conversation listing, renaming, deletion
  client/history.go        # Cloud conversation turn extraction
  client/citations.go      # Markdown citation link resolver
  client/errors.go         # UpstreamError classification
  codingtools/             # Optional local developer tools manager
  crypto/crypto.go         # AES-256-GCM authenticated cipher operations
  logging/                 # Unified application logger
  models/models.go         # Model registry, configuration, versioning
  payload/payload.go       # Copilot payload builders, timezones, attachments
  servers/
    api.go                 # Main HTTP server: OpenAI & Anthropic routes, SSE
    auth.go                # Web UI password verification & public gates
    cli.go                 # CLI interactive session server
    errors.go              # Normalized OpenAI error responses
    mcp.go                 # Model Context Protocol JSON-RPC 2.0 server
    sessions.go            # Session-to-conversation mapping API
    stopsequence.go        # Streaming stop sequence truncation writer
    transcripts.go         # Local conversation history manager
    webui.go               # HTTP file server for embedded Web UI
  setup/login.go           # RFC 8628 Device Code login & PKCE fallback
  textcut/                 # Rune-boundary safe text cutting
  toolcalling/             # Simulated tool calling engine & parser
  webui/embed.go           # Go embed directive packaging compiled UI bundle
web/                       # React 19 + TypeScript + Vite frontend project
docs/                      # Documentation guides and screenshots
data/                      # Runtime state (gitignored): tokens/, transcripts/, cache/
```

---

## Dependencies & Footprint

M365Bridge maintains a minimal dependency profile with zero headless browsers and no external CGO requirements:

| Dependency                      | Purpose                           |
|---------------------------------|-----------------------------------|
| `github.com/google/uuid`        | Session IDs and request tracking  |
| `github.com/gorilla/websocket`  | SignalR WebSocket transport       |
| `github.com/pkoukk/tiktoken-go` | Local BPE token counting          |
| `github.com/dlclark/regexp2`    | Regular expressions for tokenizer |
