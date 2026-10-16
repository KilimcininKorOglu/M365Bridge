# API Reference

M365Bridge provides complete HTTP compatibility for both OpenAI and Anthropic client protocols, alongside native session management, quota tracking, Copilot account personalization, and Model Context Protocol (MCP) support.

---

## Complete Endpoints Table

| Endpoint                          | Method | Protocol / Type           | Auth Required | Description                                             |
|-----------------------------------|--------|---------------------------|---------------|---------------------------------------------------------|
| `/v1/chat/completions`            | POST   | OpenAI                    | Yes           | Chat completions (streaming SSE and buffered)           |
| `/v1/completions`                 | POST   | OpenAI                    | Yes           | Legacy text completions                                 |
| `/v1/responses`                   | POST   | OpenAI                    | Yes           | Responses API (used by Codex CLI and modern agents)     |
| `/v1/responses/compact`           | POST   | OpenAI                    | Yes           | Responses Compact API for agent context compaction      |
| `/v1/messages`                    | POST   | Anthropic                 | Yes           | Messages API with native Anthropic SSE framing          |
| `/v1/messages/count_tokens`       | POST   | Anthropic                 | Yes           | Anthropic input token counter                           |
| `/v1/complete`                    | POST   | Anthropic                 | Yes           | Legacy Anthropic text completions                       |
| `/v1/models`                      | GET    | Dual (OpenAI & Anthropic) | No            | Unified model list matching both wire specifications    |
| `/v1/quota`                       | GET    | M365Bridge                | Yes           | Last observed Copilot conversation message counters     |
| `/v1/personalization`             | GET    | M365Bridge                | Yes           | Account-level Copilot memory and insights status        |
| `/v1/personalization`             | PATCH  | M365Bridge                | Yes           | Enable or disable account-level Copilot memory          |
| `/v1/sessions`                    | GET    | M365Bridge                | Yes           | List active session-to-conversation mappings            |
| `/v1/sessions/{id}`               | GET    | M365Bridge                | Yes           | Retrieve M365 conversation ID for a session             |
| `/v1/sessions/{id}`               | PUT    | M365Bridge                | Yes           | Bind a session ID to an existing M365 conversation      |
| `/v1/sessions/{id}`               | DELETE | M365Bridge                | Yes           | Delete conversation upstream and clear local mapping    |
| `/v1/sessions/{id}/messages`      | GET    | M365Bridge                | Yes           | Retrieve locally recorded transcript for a session      |
| `/v1/conversations`               | GET    | M365 Web                  | Yes           | List cloud conversations from M365 account              |
| `/v1/conversations`               | POST   | M365 Web                  | Yes           | Create a conversation with an initial prompt            |
| `/v1/conversations/{id}`          | PATCH  | M365 Web                  | Yes           | Rename a conversation (`{"name": "..."}`)               |
| `/v1/conversations/{id}`          | DELETE | M365 Web                  | Yes           | Delete upstream conversation and mapped sessions        |
| `/v1/conversations/{id}/messages` | GET    | M365 Web                  | Yes           | Fetch full turns directly from rendered M365 page       |
| `/v1/images/generations`          | POST   | OpenAI                    | Yes           | Generate image via Microsoft Designer prompt            |
| `/v1/images/edits`                | POST   | OpenAI                    | Yes           | Edit an existing image                                  |
| `/v1/images/{ref}`                | GET    | M365Bridge                | Yes           | Stream a generated image using internal proxy reference |
| `/mcp`                            | POST   | JSON-RPC 2.0              | Yes           | Model Context Protocol server endpoint                  |
| `/health`                         | GET    | System                    | No            | Liveness and health check                               |
| `/v1/health`                      | GET    | System                    | No            | Codex provider reachability probe                       |
| `/v1/auth`                        | GET    | System                    | No            | Discovery endpoint for web UI authentication gate       |
| `/v1/auth/verify`                 | POST   | System                    | No            | Verify credentials presented by the browser UI          |
| `/`                               | GET    | Web UI                    | No*           | Browser interface (may prompt for UI password)          |

---

## Dual Wire-Format Model Registry (`GET /v1/models`)

Both OpenAI and Anthropic clients discover models through `GET /v1/models`. The response is crafted to fulfill both schemas simultaneously:
- OpenAI fields: `object: "model"`, `id`, `created`, `owned_by`.
- Anthropic fields: `type: "model"`, `display_name`, `created_at`, `max_tokens`, `capabilities`.
- Pagination: `has_more: false`, with `first_id` and `last_id`.
Claude Code inspects this endpoint, filters for models beginning with `claude` or `anthropic`, and negotiates capabilities accordingly.

---

## Reasoning Intensity & Effort Control

Reasoning intensity is adjusted via the standard OpenAI-compatible `reasoning_effort` parameter (`none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`), Anthropic's `thinking` block, or Responses API's `reasoning.effort`. Fake model name suffixes (such as `-reasoning`) are no longer required to switch between reasoning levels:

- **OpenAI Chat Completions (`/v1/chat/completions`)**: Accepts `reasoning_effort: "<level>"` or `reasoning: {"effort": "<level>"}`.
- **Anthropic Messages (`/v1/messages`)**: Accepts `thinking: {"type": "enabled"|"adaptive", "budget_tokens": <n>}`, `output_config: {"effort": "<level>"}`, or `reasoning_effort: "<level>"`.
- **Responses API (`/v1/responses`)**: Accepts `reasoning: {"effort": "<level>"}`.

When reasoning effort is `medium` or higher (or thinking is enabled), requests targeting base models (`gpt-5.5`, `gpt-5.4`, `gpt-5.3`, `gpt-5.2`, `gpt-5.6`, `claude-sonnet`, etc.) automatically route to the corresponding reasoning tone upstream. Setting effort to `low`, `minimal`, or `none` (or thinking disabled) guarantees execution on the non-reasoning fast chat tone.

---

## Responses API & Compact API (`/v1/responses`)

### Streaming Event Sequence
Streaming requests on `/v1/responses` emit typed SSE events:
`response.created` -> `response.in_progress` -> `response.output_item.added` -> `response.output_text.delta` -> `response.output_text.done` -> `response.completed`.

### Context Compaction (`/v1/responses/compact`)
Designed for remote agent context compaction (such as Codex CLI):
1. Flattens conversation message history into an internal summary prompt.
2. Copilot summarizes the conversation.
3. Returns a structured `compaction` item with `encrypted_content`.

---

## Token Counting & Usage Reporting

Because the M365 SignalR backend does not return token usage figures, M365Bridge calculates accurate token counts locally:
- **Tokenizer**: Tiktoken BPE tokenizer using `o200k_base` (matching GPT-5 class models), with fallback to `cl100k_base`.
- **Reasoning Tokens**: Reported explicitly under `reasoning_tokens`.
- **Usage Metadata**: Every response includes `usage_source: "tiktoken_o200k_base_estimate"`.

Example OpenAI usage block:
```json
{
  "prompt_tokens": 42,
  "completion_tokens": 17,
  "reasoning_tokens": 6,
  "total_tokens": 59,
  "usage_source": "tiktoken_o200k_base_estimate"
}
```

---

## Stop Sequences

Supported on all endpoints (`stop` for OpenAI, `stop_sequences` for Anthropic):
- **Immediate Streaming Cut**: A specialized streaming buffer (`stopSequenceWriter`) buffers trailing bytes and terminates the stream the exact rune a stop sequence matches.
- Matched sequences are omitted from the final content.
- Anthropic reports `stop_reason: "stop_sequence"` and names the matched string.

---

## Error Handling & Status Classification

All errors are normalized into standard OpenAI error structures:
```json
{
  "error": {
    "message": "Descriptive error message",
    "type": "invalid_request_error | authentication_error | rate_limit_error | server_error",
    "code": "machine_readable_code"
  }
}
```

### Upstream Error Classification Table
| HTTP Status | Machine Code               | Cause                                                  |
|-------------|----------------------------|--------------------------------------------------------|
| `401`       | `upstream_auth_failed`     | Credentials expired, revoked, or failed to refresh     |
| `403`       | `insufficient_permissions` | Microsoft tenant policy rejected the request           |
| `429`       | `rate_limit_exceeded`      | Upstream rate limit hit; includes `Retry-After` header |
| `429`       | `upstream_throttled`       | Conversation message quota has been exhausted          |
| `409`       | `tool_round_limit`         | Agent turn exceeded `M365_MAX_TOOL_ROUNDS`             |
| `404`       | `model_not_found`          | Requested model is not in `/v1/models`                 |
| `502`       | `upstream_error`           | Upstream SignalR rejected turn or unreachable          |
| `502`       | `upstream_unavailable`     | WebSocket handshake failed or dropped                  |
| `502`       | `upstream_turn_failed`     | Upstream completed turn without returning content      |
| `502`       | `upstream_content_blocked` | Content filtered by Microsoft safety policies          |
| `504`       | `upstream_timeout`         | Turn timed out waiting for upstream SignalR response   |

---

## Quota Tracking & Account Memory

### Conversation Message Quota (`GET /v1/quota`)
The Copilot backend enforces a ceiling on the number of messages per conversation (typically 600) and reports current usage in stream update frames. `GET /v1/quota` exposes the most recently observed counters:
```json
{
  "object": "quota",
  "available": true,
  "exhausted": false,
  "used": 8,
  "max": 600,
  "headroom": 592
}
```

### Copilot Personalization & Memory (`/v1/personalization`)
Microsoft 365 Copilot stores a cross-conversation memory on your account. To maintain clean conversation isolation, M365Bridge allows reading and toggling this flag:
- `GET /v1/personalization`: Read current account memory flags.
- `PATCH /v1/personalization` with `{"memory_enabled": false}`: Disables cross-conversation memory on the account.
