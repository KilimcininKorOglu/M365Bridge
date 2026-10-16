# Sessions & Conversations

In Microsoft 365 Copilot, conversation history is managed on the server by Microsoft's backend under a unique `ConversationId`. Rather than repeatedly re-uploading entire multi-turn transcripts, M365Bridge maintains a persistent mapping between client-facing session identifiers and upstream M365 conversations.

---

## Session Resolution Order

For every incoming chat or completion request, M365Bridge determines the active session ID using the following strict priority sequence:

1. **Model Name Suffix**: `model:sessionID` (e.g. `gpt5.5-reasoning:project-alpha`).
2. **`previous_response_id`**: In `/v1/responses` request bodies.
3. **`session_id`**: Field in request JSON bodies.
4. **`user`**: Field in request JSON bodies.
5. **`X-Session-Id`**: Header.
6. **Agent Headers**:
   - `X-Claude-Code-Session-Id` (automatically sent by Claude Code).
   - `session-id` (automatically sent by OpenAI Codex CLI).
7. **Deterministic Hash Fallback**:
   - `hash(api_key + first user message)` when auth is enabled.
   - `hash(first user message)` when auth is disabled.

This priority guarantees that autonomous tools like Claude Code and Codex stay mapped to their own isolated conversations out of the box with zero configuration, while allowing developers to explicitly override session routing using headers or model suffixes.

---

## Session Naming via Model Suffix

Any client can pin its conversation session by appending a colon and session name to the model string:

```json
{
  "model": "gpt5.5-reasoning:frontend-refactor-branch",
  "messages": [{"role": "user", "content": "Let's continue reviewing the PR"}]
}
```

The gateway extracts `gpt5.5-reasoning` as the model tone and routes the request into `frontend-refactor-branch`.

---

## System Instructions & Developer Roles

Because M365 Copilot stores past turns in its cloud database and accepts only the latest turn over WebSocket, system instructions sent in earlier turns would be lost if not explicitly forwarded.

- **Prefixing Strategy**: All `system` and `developer` role messages in the request are aggregated and cleanly prefixed to the latest user turn.
- **Anthropic Compatibility**: Top-level `system` strings or arrays of text blocks are merged identically.
- **History Hygiene**: System instructions are excluded from the recorded chat history so they do not pollute conversation transcripts.

---

## Session Management Endpoints

M365Bridge provides administrative endpoints to inspect and manage conversation mappings:

### 1. List Sessions (`GET /v1/sessions`)
Returns a list of all active session mappings stored in the local cache (`data/cache/`):
```json
{
  "sessions": [
    {
      "session_id": "project-alpha",
      "conversation_id": "a1b2c3d4-...",
      "last_updated": 1729051200
    }
  ]
}
```

### 2. Inspect a Session (`GET /v1/sessions/{id}`)
Retrieves the associated M365 conversation ID and timestamps for a specific session.

### 3. Rebind a Session (`PUT /v1/sessions/{id}`)
Points a local session to an existing M365 cloud conversation:
```bash
curl -X PUT http://localhost:8230/v1/sessions/my-session \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{"conversation_id": "target-m365-conversation-uuid"}'
```

### 4. Delete a Session (`DELETE /v1/sessions/{id}`)
Deletes the conversation from Microsoft's cloud servers and removes the local mapping:
- To delete only the local session mapping while keeping the conversation intact on Microsoft servers, append `?local_only=true`.
