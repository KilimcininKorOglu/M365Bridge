# Configuration Guide

M365Bridge is configured through environment variables or a `data/.env` file. Variables defined directly in the host or container environment take precedence over values in `data/.env`. Run `./m365-bridge --help` at any time to inspect all supported variables with their current default values.

---

## Environment Variables

### Core & Authentication
| Variable         | Default                                | Description                                                                                                        |
|------------------|----------------------------------------|--------------------------------------------------------------------------------------------------------------------|
| `M365_TENANT_ID` | *Required* | Microsoft Entra ID Directory (tenant) ID (UUID). Automatically populated by `m365-bridge login`. |
| `M365_USER_OID`  | *Required*                             | User Object ID (UUID) within the tenant. Automatically extracted from token claims during login.                   |
| `M365_CLIENT_ID` | `c0ab8ce9-e9a0-42e7-b064-33d422df41f1` | Microsoft OAuth client ID used for Sydney/Chathub tokens.                                                          |
| `TZ`             | System timezone                        | Timezone sent with each Copilot turn (e.g. `Europe/Istanbul`, `UTC`). Falls back to `/etc/localtime`, then UTC.    |

### API Security & Access Control
| Variable        | Default | Description                                                                                                                                        |
|-----------------|---------|----------------------------------------------------------------------------------------------------------------------------------------------------|
| `M365_API_KEYS` | *Unset* | Comma-separated list of bearer tokens required for all `/v1/*` routes and `/mcp`. If unset, the API is publicly accessible without authentication. |
| `M365_API_KEY`  | *Unset* | A single API key fallback used when `M365_API_KEYS` is not defined.                                                                                |

### Context Window & Token Limits
| Variable                 | Default   | Description                                                                                                                              |
|--------------------------|-----------|------------------------------------------------------------------------------------------------------------------------------------------|
| `M365_CONTEXT_WINDOW`    | `1000000` | Advertised context window token count in `GET /v1/models` so client harnesses (Claude Code, Cursor) do not prematurely truncate prompts. |
| `M365_MAX_OUTPUT_TOKENS` | `1000000` | Advertised maximum completion output token ceiling in `GET /v1/models`.                                                                  |

### Tool Calling & Agentic Loops
| Variable                 | Default | Description                                                                                                                   |
|--------------------------|---------|-------------------------------------------------------------------------------------------------------------------------------|
| `M365_MAX_TOOL_ROUNDS`   | `32`    | Maximum consecutive tool-calling rounds permitted per user turn before returning `HTTP 409 tool_round_limit` (hard cap: 512). |
| `M365_ENABLE_WEB_SEARCH` | `1`     | Enables Copilot's built-in `BingWebSearch` on every turn (`1` = enabled, `0` = disabled).                                     |

### Built-in Coding Tools (Local Execution)
> [!CAUTION]
> Local coding tools grant the model file system access and shell execution capabilities within `M365_WORKSPACE_DIR`. Always require API keys (`M365_API_KEYS`) when enabled.
| Variable                        | Default   | Description                                                                                                      |
|---------------------------------|-----------|------------------------------------------------------------------------------------------------------------------|
| `M365_ENABLE_CODE_TOOLS`        | `0`       | Master switch for local coding tools execution (`1` to enable, `0` to disable).                                  |
| `M365_AUTO_EXPOSE_TOOLS`        | `0`       | Set to `1` to automatically expose built-in coding tool schemas to models even if the client did not send tools. |
| `M365_WORKSPACE_DIR`            | `.`       | Absolute or relative directory path that strictly confines file and Git operations.                              |
| `M365_CODE_TOOL_TIMEOUT`        | `30s`     | Execution timeout for commands and test runs (Go duration format, e.g., `15s`, `2m`).                            |
| `M365_CODE_TOOL_MAX_OUTPUT`     | `1048576` | Maximum bytes captured from shell command outputs (1 MiB default). Output exceeding this is truncated.           |
| `M365_CODE_TOOL_MAX_READ_BYTES` | `1048576` | Maximum file size in bytes returned by the `read_file` tool.                                                     |
| `M365_CODE_TOOL_MAX_ITERATIONS` | `10`      | Maximum multi-step model and tool loop iterations per request.                                                   |

### Multimodal & Image Generation
| Variable                    | Default                | Description                                                                                                      |
|-----------------------------|------------------------|------------------------------------------------------------------------------------------------------------------|
| `M365_IMAGE_HOST_ALLOWLIST` | `.officeapps.live.com` | Comma-separated list of trusted hosts permitted to serve generated images. Prefixes with a dot match subdomains. |

### Web UI & Transcripts
| Variable               | Default | Description                                                                                                    |
|------------------------|---------|----------------------------------------------------------------------------------------------------------------|
| `M365_ENABLE_WEB_UI`   | `1`     | Serves the browser interface at `/` and records transcripts in `data/transcripts`. Set to `0` to disable both. |
| `M365_WEB_UI_PASSWORD` | *Unset* | Optional password gate for the Web UI. If unset, the browser UI opens without credentials.                     |

---

## Web UI Configuration & Customization

The web interface is embedded directly inside the Go binary (`pkg/webui/dist`) and requires no secondary Node.js process or external static asset hosting.

### Web UI Security Matrix
`M365_WEB_UI_PASSWORD` and `M365_API_KEYS` operate as independent security layers:
- **Neither set**: The web UI and all API endpoints are open without authentication.
- **Password set, API keys unset**: Web UI prompts for the password. API routes remain accessible without keys.
- **API keys set, Password unset**: Web UI prompts for one of the valid API keys.
- **Both set**: Web UI prompts for the password, while the API accepts either valid API keys or the web UI password.

The web UI credential is kept in an HTTP-only/secure cookie and transmitted via the standard `Authorization: Bearer <token>` header, preventing CSRF vulnerabilities.

### Transcripts & Privacy
- When `M365_ENABLE_WEB_UI=1`, turn history is recorded in `data/transcripts/{session_id}.json`.
- When set to `0`, `/` returns `HTTP 404`, transcript logging is disabled, and `GET /v1/sessions/{id}/messages` returns `404 transcripts_disabled`.

### Language Support
The interface supports English (`en`) and Turkish (`tr`). Language preference is persisted via the `m365bridge_lang` cookie.
To add a new language:
1. Copy `web/src/locales/en.json` to `web/src/locales/<lang_code>.json`.
2. Translate the values and update `$label`.
3. Rebuild the frontend using `make ui`.

### Developing & Rebuilding the Web UI
- For rapid UI development with hot module reloading:
  ```bash
  cd web && npm install && npm run dev
  ```
  The Vite dev server runs at `http://localhost:5173` and proxies `/v1` requests to `http://localhost:8230`.
- To compile the production bundle into Go source:
  ```bash
  make ui
  make build
  ```
