# Tool Calling & Coding Tools

Microsoft 365 Copilot does not natively execute user-defined tools. M365Bridge bridges this gap with an advanced **Simulated Tool Calling** layer, allowing coding assistants like **Claude Code**, **Codex**, **Cursor**, and custom agents to declare and execute functions transparently.

---

## Simulated Tool Calling Architecture

1. **Request Serialization**: The incoming client request (`tools` array in OpenAI or Anthropic format) is parsed and formatted into structured prompt instructions.
2. **Model Invocation**: M365 Copilot generates a structured JSON response enclosed in a markdown block.
3. **Parsing & Validation**: M365Bridge parses the output, validates arguments against the tool's JSON Schema, enforces tool contracts, and packages calls into standard OpenAI `tool_calls` or Anthropic `tool_use` blocks.
4. **Execution**: The client application executes the tool locally and posts the result back in the next conversation turn.

This operates across `/v1/chat/completions`, `/v1/messages`, and `/v1/responses`, in both streaming and non-streaming modes.

---

## Tool Call Validation & Enforcement

- **Schema Validation**: Arguments are checked against declared types, enums, required fields, and nested structures. Invalid calls are dropped and retried once with corrective instructions.
- **Contract Pruning**: When `additionalProperties: false` is set, extraneous properties are quietly pruned rather than failing the request.
- **`tool_choice` Enforcement**:
  - `"none"`: Suppresses tool generation entirely.
  - `"required"` / `"any"`: Guarantees that at least one tool call is emitted.
  - Named function: Re-prompts if the model calls an unpinned tool.
- **Parallel Tool Control**:
  - `parallel_tool_calls: false` (OpenAI) and `tool_choice.disable_parallel_tool_use: true` (Anthropic) ensure only a single tool call is forwarded per turn.
- **Unique Call IDs**: All tool calls are issued unique `call_<uuid>` IDs, preventing client duplicate-ID rejections.
- **M365 Built-in Filtering**: Internal backend tools (`code_interpreter`, `BingWebSearch`, `trigger_plugin`) are intercepted and omitted so clients never receive unexecutable internal actions.

---

## Agent Loops (Claude Code & Codex)

Autonomous agents drive multi-turn loops by resending complete message history. M365Bridge manages these loops cleanly:
- **Round Ceiling**: Enforces `M365_MAX_TOOL_ROUNDS` (default `32`, max `512`). Turns exceeding this limit return `HTTP 409 tool_round_limit`.
- **Loop Stalling Prevention**: If an agent repeats identical tool calls and arguments three times with identical outputs, the duplicate call is pruned to break infinite loops.
- **Result Compaction**: Intermediate tool execution results in history are compacted with byte-range markers to prevent context window explosion.
- **Self-Claim Verification**: If a model falsely claims in prose that it "already executed the command on your machine" without emitting a tool call, the gateway detects this phrasing (in English, Turkish, and Chinese) and forces a re-prompt.

---

## Built-in Local Coding Tools (Opt-in)

M365Bridge can directly execute a curated set of developer tools on the server filesystem.

> [!CAUTION]
> Enabling local coding tools permits remote file manipulation and shell command execution inside `M365_WORKSPACE_DIR`. Always configure `M365_API_KEYS` and never expose this mode to untrusted networks.

### Activation & Settings
Enable in `data/.env`:
```env
M365_ENABLE_CODE_TOOLS=1
M365_WORKSPACE_DIR=/path/to/project
M365_CODE_TOOL_TIMEOUT=30s
M365_CODE_TOOL_MAX_OUTPUT=1048576
```

### Available Built-in Tools
| Tool            | Description                                           |
|-----------------|-------------------------------------------------------|
| `list_files`    | List directory contents within workspace              |
| `read_file`     | Read file contents (subject to byte limit)            |
| `write_file`    | Create or update files within workspace               |
| `search_files`  | Grep/search for regex patterns in workspace files     |
| `git_status`    | Inspect Git working directory status                  |
| `git_diff`      | View unstaged or staged Git diffs                     |
| `git_log`       | Read recent commit history                            |
| `shell_command` | Execute arbitrary shell commands in workspace         |
| `apply_patch`   | Apply unified Git diff patches                        |
| `run_tests`     | Run test suites with strict timeout and output limits |

---

## Model Context Protocol (MCP) Server

M365Bridge exposes Copilot as a Model Context Protocol server at `/mcp` using JSON-RPC 2.0 (protocol revision `2025-06-18`).

### Supported MCP Tools
- `ask_copilot`: Submit a prompt to Copilot and receive a text answer.
- `describe_image`: Submit an image (via data URI) along with a prompt.

### MCP Example
```bash
curl -s -X POST http://localhost:8230/mcp \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/call",
    "params": {
      "name": "ask_copilot",
      "arguments": {"prompt": "Explain the Raft consensus algorithm"}
    }
  }'
```
> [!NOTE]
> Simulated tool calling is deliberately excluded from `/mcp`. MCP clients already possess native tool execution engines; nesting prompt-based tool simulation would introduce competing loops.
