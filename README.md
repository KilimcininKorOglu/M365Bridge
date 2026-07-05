# M365Bridge

[![CI](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/ci.yml)
[![Release](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/release.yml/badge.svg)](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/release.yml)
[![Version](https://img.shields.io/github/v/release/KilimcininKorOglu/M365Bridge)](https://github.com/KilimcininKorOglu/M365Bridge/releases)
[![Docker](https://img.shields.io/badge/docker-ghcr.io-blue)](https://github.com/KilimcininKorOglu/M365Bridge/pkgs/container/m365bridge)

**English** | **[Türkçe](README.tr.md)**

A Go implementation that converts Microsoft 365 Copilot's WebSocket interface to OpenAI/Anthropic compatible HTTP API.

## Architecture

Your App -> M365Bridge -> substrate.office.com (SignalR) -> M365 Copilot Backend

## Prerequisites

- **Go 1.22+** installed ([download](https://go.dev/dl/))
- **git** for cloning this repository
- A **Microsoft 365 Copilot license** (business or enterprise account with Copilot access) tested a copilot chat (basic) account
- A browser logged into [https://m365.cloud.microsoft](https://m365.cloud.microsoft) (for setup wizard token extraction)

## Features

- Text chat with streaming/non-streaming output
- Multimodal image input (OpenAI `image_url` and Anthropic `image` content blocks; PNG, JPEG, GIF, WebP)
- Multi-turn conversation support via ConversationId tracking
- Session isolation (per-session M365 conversations)
- Thinking/reasoning content extraction (`reasoning_content` for OpenAI, `thinking` blocks for Anthropic)
- OpenAI-compatible API endpoints
- Anthropic-compatible API endpoints (dedicated SSE handlers)
- API key authentication (`M365_API_KEYS` / `M365_API_KEY`)
- max_tokens enforcement across all endpoints (tiktoken BPE)
- CLI interface for interactive use
- Single binary with subcommand routing

## Installation

```bash
git clone https://github.com/KilimcininKorOglu/M365Bridge
cd M365Bridge
go mod download
go build -o bin/m365-bridge ./cmd/cli
```

### Pre-built Binaries

Download the latest binary for your platform from [GitHub Releases](https://github.com/KilimcininKorOglu/M365Bridge/releases):

| Platform                    | File                            |
|-----------------------------|---------------------------------|
| Linux amd64                 | `m365-bridge-linux-amd64`       |
| Linux arm64                 | `m365-bridge-linux-arm64`       |
| macOS amd64 (Intel)         | `m365-bridge-darwin-amd64`      |
| macOS arm64 (Apple Silicon) | `m365-bridge-darwin-arm64`      |
| Windows amd64               | `m365-bridge-windows-amd64.exe` |
| Windows arm64               | `m365-bridge-windows-arm64.exe` |

```bash
# Example: Linux amd64
wget https://github.com/KilimcininKorOglu/M365Bridge/releases/latest/download/m365-bridge-linux-amd64
chmod +x m365-bridge-linux-amd64
./m365-bridge-linux-amd64 serve --port 8000
```

### Docker

The easiest way to run M365Bridge is with Docker. The pre-built image is available on GitHub Container Registry.

**Option 1: Docker Compose (recommended)**

Create a `docker-compose.yml` file:

```yaml
services:
  m365bridge:
    image: ghcr.io/kilimcininkoroglu/m365bridge:latest
    container_name: m365bridge
    ports:
      - "8230:8000"
    volumes:
      - ./data:/app/data
    restart: unless-stopped
```

Start the server:

```bash
docker compose up -d
```

The API will be available at `http://localhost:8230`.

**Option 2: Docker run**

```bash
docker run -d \
  --name m365bridge \
  -p 8230:8000 \
  -v $(pwd)/data:/app/data \
  --restart unless-stopped \
  ghcr.io/kilimcininkoroglu/m365bridge:latest
```

**Option 3: Build from source**

If you want to build the image yourself instead of using the pre-built one:

```bash
docker compose up --build -d
```

**Notes:**

- The `data/` directory stores tokens, cache, and configuration. It is created automatically on first run.
- Port `8230` (host) maps to port `8000` (container). Change the host port in `docker-compose.yml` or the `-p` flag if needed.
- The container starts with `serve --port 8000` by default.
- After starting the container, run the setup wizard to configure authentication: `docker exec -it m365bridge ./bin/m365-bridge setup-wizard`

## Configuration

Run the setup wizard to configure authentication:

```bash
./bin/m365-bridge setup-wizard
```

The wizard will guide you through:
1. Logging into https://m365.cloud.microsoft in your browser
2. Running a JavaScript snippet in the browser console to intercept the refresh token
3. Saving the JSON output to `data/setup.json`
4. Encrypting and storing the refresh token
5. Saving environment variables to `data/.env` file
6. (Optional) Capturing SSO cookies for automatic token renewal after 24-hour expiry

The wizard reads a JSON file (default: `data/setup.json`) containing `oid`, `tenant`, and `refresh_token` fields. It encrypts the refresh token with AES-256-GCM, stores the encryption key in `data/tokens/encryption.key`, and writes environment variables to `data/.env`.

> **Note on token expiry:** Microsoft SPA refresh tokens expire after 24 hours (AADSTS700084). Without SSO cookies, you must re-run the setup wizard daily. To enable automatic renewal, capture SSO cookies during setup (see Step 2 below). SSO cookies last weeks/months and allow the server to silently re-authenticate when the refresh token expires.

### Manual Setup

If you prefer to extract the configuration manually instead of using the wizard:

1. Open https://m365.cloud.microsoft and log in
2. Press F12 to open DevTools, go to Console
3. Paste and run the JavaScript snippet below. It intercepts MSAL token refresh requests to capture the plaintext refresh token (MSAL.js v3 encrypts refresh tokens in localStorage, so direct localStorage extraction does not work). It also clears cached access tokens to force a token refresh, which triggers the interceptor.

<details>
<summary>Click to expand the JavaScript interceptor snippet</summary>

```javascript
(() => {
const k = Object.keys(localStorage).find(k => k.startsWith('msal.') && k.includes('|'));
if (!k) return 'NOT_FOUND';
const p = k.split('|')[1].split('.');
const oid = p[0], tenant = p[1];

const origFetch = window.fetch;
window.fetch = async function(...args) {
  const resp = await origFetch.apply(this, args);
  const url = typeof args[0] === 'string' ? args[0] : (args[0] && args[0].url) || '';
  if (url.includes('login.microsoftonline.com') && url.includes('oauth2/v2.0/token')) {
    try {
      const clone = resp.clone();
      const data = await clone.json();
      if (data.refresh_token) {
        console.log('===== COPY THE COMPLETE JSON LINE BELOW =====');
        console.log(JSON.stringify({oid, tenant, refresh_token: data.refresh_token}));
      }
    } catch(e) {}
  }
  return resp;
};

const origXHROpen = XMLHttpRequest.prototype.open;
const origXHRSend = XMLHttpRequest.prototype.send;
XMLHttpRequest.prototype.open = function(method, url) {
  this._url = url;
  return origXHROpen.apply(this, arguments);
};
XMLHttpRequest.prototype.send = function(body) {
  this.addEventListener('load', function() {
    if (this._url && this._url.includes('oauth2/v2.0/token')) {
      try {
        const data = JSON.parse(this.responseText);
        if (data.refresh_token) {
          console.log('===== COPY THE COMPLETE JSON LINE BELOW =====');
          console.log(JSON.stringify({oid, tenant, refresh_token: data.refresh_token}));
        }
      } catch(e) {}
    }
  });
  return origXHRSend.apply(this, arguments);
};

const keys = Object.keys(localStorage);
let cleared = 0;
for (const key of keys) {
  if (key.includes('accesstoken') || key.includes('idtoken')) {
    localStorage.removeItem(key);
    cleared++;
  }
}

window.dispatchEvent(new Event('load'));
if (window.msal) {
  try {
    const accounts = window.msal.getAllAccounts();
    if (accounts.length > 0) {
      window.msal.acquireTokenSilent({
        account: accounts[0],
        scopes: ['https://substrate.office.com/sydney/.default']
      }).catch(() => {});
    }
  } catch(e) {}
}

return 'Interceptors installed and ' + cleared + ' access tokens cleared. MSAL should refresh automatically. Watch the console for the JSON output.';
})()
```

</details>

4. Watch the console for: `===== COPY THE COMPLETE JSON LINE BELOW =====`
5. Copy the JSON output (it contains `oid`, `tenant`, and `refresh_token`)
6. Save it to `data/setup.json`
7. Run the wizard to verify and save the configuration:

```bash
./bin/m365-bridge setup-wizard
```

The wizard will encrypt the refresh token with AES-256-GCM and save environment variables to `data/.env`.

### Capturing SSO Cookies (Optional, Recommended)

SSO cookies enable automatic token renewal after the 24-hour refresh token expiry. Without them, you must re-run the setup wizard every 24 hours.

1. In your browser DevTools, go to **Application** > **Cookies** > `https://login.microsoftonline.com`
2. Find and copy the values of these cookies:
   - `ESTSAUTH`
   - `ESTSAUTHPERSISTENT`
3. Add them to `data/setup.json` under the `sso_cookies` array:

```json
{
  "oid": "your-oid",
  "tenant": "your-tenant",
  "refresh_token": "your-refresh-token",
  "sso_cookies": [
    {"name": "ESTSAUTH", "value": "cookie-value-here"},
    {"name": "ESTSAUTHPERSISTENT", "value": "cookie-value-here"}
  ]
}
```

4. Run the setup wizard again to save the SSO cookies:

```bash
./bin/m365-bridge setup-wizard
```

The wizard will encrypt the SSO cookies with AES-256-GCM and store them in `data/tokens/sso_cookies.json`. When the refresh token expires, the server automatically uses the SSO cookies to silently re-authenticate via `oauth2/v2.0/authorize?prompt=none` and obtain a fresh access and refresh token.

### Environment Variables

| Variable         | Required | Description                                                              |
|------------------|----------|--------------------------------------------------------------------------|
| `M365_TENANT_ID` | Yes      | Azure AD tenant ID                                                       |
| `M365_USER_OID`  | Yes      | User object ID                                                           |
| `M365_CLIENT_ID` | No       | Azure AD app client ID (defaults to the M365 Copilot web app client ID)  |
| `M365_API_KEYS`  | No       | Comma-separated API keys for proxy auth (takes precedence over singular) |
| `M365_API_KEY`   | No       | Single API key for proxy auth (backward compatible)                      |

When `M365_API_KEYS` or `M365_API_KEY` is set, all `/v1/*` endpoints require `Authorization: Bearer <key>` header. When both are empty, no auth is enforced.

The setup wizard creates `data/.env` automatically. If you need to create it manually, here is the expected format:

```env
M365_TENANT_ID=your-tenant-id
M365_USER_OID=your-user-oid
M365_API_KEYS=key1,key2
```

`M365_CLIENT_ID` defaults to the M365 Copilot web app's registered client ID. You only need to override this if you are using a custom Azure AD app registration.

## Usage

### CLI Flags

| Flag            | Type   | Default | Description                                                              |
|-----------------|--------|---------|--------------------------------------------------------------------------|
| `-i`            | bool   | false   | Interactive mode (multi-turn conversation)                               |
| `--model`       | string | `auto`  | Model to use: `auto`, `quick`, `reasoning`, `gpt5.5`, `gpt5.5-reasoning` |
| `--reasoning`   | bool   | false   | Use reasoning mode                                                       |
| `--no-stream`   | bool   | false   | Disable streaming, print full response at once                           |
| `--list-models` | bool   | false   | List all available models and exit                                       |
| `--version`     | bool   | false   | Show version and exit                                                    |

Positional argument (if no flag consumes it): the query text for single query mode.

### Subcommand: serve

Starts the HTTP API server.

| Flag        | Type | Default | Description           |
|-------------|------|---------|-----------------------|
| `--port`    | int  | 8000    | Port to listen on     |
| `--version` | bool | false   | Show version and exit |

### Subcommand: setup-wizard

Runs the browser-based setup wizard. Reads JSON from file containing `oid`, `tenant`, and `refresh_token`.

| Flag     | Type   | Default           | Description             |
|----------|--------|-------------------|-------------------------|
| `--file` | string | `data/setup.json` | Path to setup JSON file |

### Examples

```bash
# Single query
./bin/m365-bridge "your question"

# Interactive mode
./bin/m365-bridge -i

# Specify model with reasoning
./bin/m365-bridge --model gpt5.5-reasoning "your question"

# Non-streaming
./bin/m365-bridge --no-stream "your question"

# List models
./bin/m365-bridge --list-models

# Start API server
./bin/m365-bridge serve --port 8000

# Run setup wizard with custom file
./bin/m365-bridge setup-wizard --file /path/to/setup.json
```

### API Server

```bash
# Start API server on port 8000
./bin/m365-bridge serve --port 8000

# Test with curl (no auth)
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"Hello"}]}'

# Test with curl (with API key)
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{"model":"gpt5.5","messages":[{"role":"user","content":"Hello"}]}'

# Streaming with session isolation
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -H "X-Session-Id: my-session-1" \
  -d '{"model":"gpt5.5","stream":true,"messages":[{"role":"user","content":"Hello"}]}'
```

### First Run

When you start the server for the first time:

1. The server reads `data/.env` from the current working directory
2. It loads the encrypted refresh token from `data/tokens/rt_90day.txt`
3. It performs a token refresh (exchanges refresh token for an access token). This takes 1-2 seconds
4. On success, you will see: `Starting API server on port 8000`
5. The first request may take slightly longer as it opens a WebSocket connection to `substrate.office.com`

If the refresh token is missing or expired, the server will attempt SSO cookie re-authentication if `data/tokens/sso_cookies.json` exists. If SSO cookies are also missing or expired, the server will fail to start with a token refresh error. Re-run `./bin/m365-bridge setup-wizard` to extract fresh tokens and cookies.

### Session Isolation

Each session maps to a unique M365 conversation. Session ID is resolved in priority order:

1. `session_id` field in request body
2. `user` field in request body
3. `X-Session-Id` header
4. `hash(api_key + first_user_message)` (when auth is on) or `hash(first_user_message)` (when auth is off)

The hash fallback allows standard OpenAI clients (like Claude Code) that cannot send custom headers to have separate conversations automatically, as long as their first user message differs.

### Python Client (OpenAI SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8000/v1",
    api_key="your-api-key",  # required if M365_API_KEYS is set
)
resp = client.chat.completions.create(
    model="gpt5.5",
    messages=[{"role": "user", "content": "Hello"}]
)
print(resp.choices[0].message.content)
```

### Python Client (Anthropic SDK)

```python
from anthropic import Anthropic

client = Anthropic(
    base_url="http://127.0.0.1:8000/v1",
    api_key="your-api-key",  # required if M365_API_KEYS is set
)
resp = client.messages.create(
    model="gpt5.5",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello"}]
)
print(resp.content[0].text)
```

### Image Input Example

```python
from openai import OpenAI
import base64

client = OpenAI(
    base_url="http://127.0.0.1:8000/v1",
    api_key="your-api-key",
)

with open("image.png", "rb") as f:
    img_b64 = base64.b64encode(f.read()).decode()

resp = client.chat.completions.create(
    model="gpt5.5",
    messages=[{
        "role": "user",
        "content": [
            {"type": "text", "text": "What is in this image?"},
            {"type": "image_url", "image_url": {"url": f"data:image/png;base64,{img_b64}"}},
        ],
    }],
)
print(resp.choices[0].message.content)
```

## API Endpoints

| Endpoint                    | Description                                         |
|-----------------------------|-----------------------------------------------------|
| `POST /v1/chat/completions` | OpenAI Chat Completions (streaming + non-streaming) |
| `POST /v1/completions`      | OpenAI text completion (streaming + non-streaming)  |
| `POST /v1/messages`         | Anthropic Messages format (dedicated SSE handlers)  |
| `POST /v1/complete`         | Anthropic Complete (FIM)                            |
| `GET /v1/models`            | Model list                                          |
| `GET /health`               | Health check (no auth required)                     |

## Models

All model selection is via the `tone` field sent to the M365 backend. The `Override` field is empty for all models.

| Key                | Tone              | OpenAI ID         |
|--------------------|-------------------|-------------------|
| `auto`             | Magic             | gpt-4-auto        |
| `quick`            | Chat              | gpt-4-quick       |
| `reasoning`        | Magic             | gpt-4-reasoning   |
| `gpt5.5`           | Gpt_5_5_Chat      | gpt-5.5           |
| `gpt5.5-reasoning` | Gpt_5_5_Reasoning | gpt-5.5-reasoning |

### Which model should I use?

| Use case                                     | Model              |
|----------------------------------------------|--------------------|
| General purpose, let backend decide          | `auto`             |
| Fast responses, simple questions             | `quick`            |
| Complex reasoning, multi-step problems       | `reasoning`        |
| GPT-5.5 chat (latest conversational model)   | `gpt5.5`           |
| GPT-5.5 with deep thinking (shows reasoning) | `gpt5.5-reasoning` |

`gpt5.5-reasoning` produces `reasoning_content` output containing the model's thinking process. All model identifiers route to the same GPT-5 backend; the `tone` field controls response behavior.

## Project Structure

```
cmd/cli/main.go          # Single entry point, subcommand router
pkg/
  auth/auth.go           # TokenManager, token refresh, AES-encrypted refresh token storage
  auth/sso.go            # SSO cookie-based re-authentication (fallback for 24h token expiry)
  client/client.go       # M365Client, WebSocket (SignalR) communication
  crypto/crypto.go       # AES-256-GCM encryption for refresh tokens
  models/models.go       # Version, ModelRegistry, Config, LoadConfig, LookupModel
  payload/payload.go     # Request payload builders, URL builder, locale/timezone helpers
  servers/
    api.go               # HTTP API server, all endpoints, max_tokens, token counting, session isolation
    cli.go               # CLI server, interactive mode
  setup/wizard.go        # Browser-based setup wizard (JS snippet, token verify, data/.env save)
go.mod                   # Module: github.com/KilimcininKorOglu/M365Bridge, Go 1.22
data/                    # Runtime data (gitignored): tokens/, setup.json, cache/
```

## Dependencies

| Dependency                      | Purpose                                                               |
|---------------------------------|-----------------------------------------------------------------------|
| `github.com/google/uuid`        | UUID generation for SIDs and request IDs                              |
| `github.com/gorilla/websocket`  | WebSocket client for SignalR                                          |
| `github.com/pkoukk/tiktoken-go` | BPE token counting (cl100k_base) for usage and max_tokens enforcement |
| `golang.org/x/net`             | publicsuffix list for SSO cookie jar                                |

## Security

- Refresh tokens encrypted with AES-256-GCM before storage
- SSO cookies encrypted with AES-256-GCM before storage (`data/tokens/sso_cookies.json`)
- Encryption key stored in `data/tokens/encryption.key`
- Access tokens cached in `data/tokens/token_cache.json` (disk-persisted, ~1h expiry with 60s buffer)
- Background token refresher proactively refreshes access token every 30 minutes in `serve` mode
- SSO cookie auto-renewal silently re-authenticates when refresh token expires (24h SPA limit)
- No credentials stored in code or repository
- `data/` directory is gitignored (contains tokens, cache, setup.json)
- API key authentication protects all `/v1/*` endpoints when configured

## Image Input Support

The proxy supports multimodal image input via OpenAI and Anthropic API formats:

- **OpenAI**: `content` array with `{"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}` blocks
- **Anthropic**: `content` array with `{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "..."}}` blocks

Images are uploaded to the M365 backend via `POST https://substrate.office.com/m365Copilot/UploadFile` and attached to the WebSocket message as `messageAnnotations`. Supported formats: PNG, JPEG, GIF, WebP.

## Unimplemented Features

- File upload
- Code interpreter

## Disclaimer

This project is for learning and research purposes only. It explores publicly observable network communication protocols.

By using this project, you confirm that:
- You have legitimate Microsoft 365 Copilot authorization
- It is for personal learning and research, not commercial use
- You understand the risks of using unofficial interfaces
- You accept all consequences

This project does not:
- Crack encryption or bypass authentication
- Access or leak others' data
- Interfere with Microsoft services
- Have any association with Microsoft Corporation

## License

Research Only
