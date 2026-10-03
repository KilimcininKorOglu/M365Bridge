# M365Bridge

[![CI](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/ci.yml)
[![Release](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/release.yml/badge.svg)](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/release.yml)
[![Version](https://img.shields.io/github/v/release/KilimcininKorOglu/M365Bridge)](https://github.com/KilimcininKorOglu/M365Bridge/releases)
[![Docker](https://img.shields.io/badge/docker-ghcr.io-blue)](https://github.com/KilimcininKorOglu/M365Bridge/pkgs/container/m365bridge)
[![Go](https://img.shields.io/badge/Go-1.26.6%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![OpenAI Compatible](https://img.shields.io/badge/API-OpenAI%20Compatible-412991)](#api-endpoints)
[![Anthropic Compatible](https://img.shields.io/badge/API-Anthropic%20Compatible-D97757?logo=anthropic&logoColor=white)](#api-endpoints)

**English** | **[Türkçe](README.tr.md)**

M365Bridge turns a Microsoft 365 Copilot subscription into an OpenAI-compatible and Anthropic-compatible HTTP API. Point any client that speaks either protocol at this service and it works: Claude Code, Codex, Cursor, Cline, the OpenAI and Anthropic SDKs, or your own code.

![The browser interface answering a question with sources](docs/webui-en.png)

## How it works

```
Your client  ->  M365Bridge  ->  substrate.office.com (SignalR)  ->  M365 Copilot
```

Copilot has no public API. It talks to its own web client over a SignalR WebSocket. M365Bridge signs in with your account, speaks that WebSocket protocol upstream, and presents the familiar HTTP endpoints downstream. Everything ships as one binary: the API server, a browser interface, an interactive CLI, and the setup wizard.

## Requirements

- A **Microsoft 365 Copilot license**. A business or enterprise account with Copilot access. A Copilot Chat (basic) account has also been tested.
- A **browser signed in** to [m365.cloud.microsoft](https://m365.cloud.microsoft). You will collect credentials from it once during setup.
- **Docker**, or **Go 1.26.6+** if you build from source. Go 1.21 and newer also work, because they download the 1.26.6 toolchain on the first build, unless `GOTOOLCHAIN` is set to `local`. The patch level is part of the requirement: every release before 1.26.6 carries standard-library vulnerabilities this service reaches through its own HTTP and TLS paths.

## Features

- Text chat, streaming and non-streaming
- Image input on both protocols, and image generation through Microsoft Designer
- Multi-turn conversations, with each session mapped to its own M365 conversation
- Reasoning content exposed as `reasoning_content` (OpenAI) and `thinking` blocks (Anthropic)
- Tool calling for client-defined tools on both protocols, streaming and non-streaming
- OpenAI endpoints, including the Responses API and its compaction route
- Anthropic endpoints with dedicated SSE handlers
- Model Context Protocol server on `/mcp`
- Optional built-in coding tools the gateway runs locally
- Stop sequences on every chat endpoint, applied as the answer streams
- API key authentication, and a separate password for the browser interface
- `max_tokens` enforcement using tiktoken BPE counting
- Conversation quota reporting on `/v1/quota`
- Browser interface compiled into the binary, in English and Turkish
- Interactive and one-shot CLI modes

## Installation

Choose one of the three options below. All three leave you with the same thing: a running service that has not yet been connected to your Microsoft account. [Connecting your account](#connecting-your-microsoft-365-account) comes next and is identical for all three.

### Option A: Docker

The shortest path. The published image needs no build.

Create `docker-compose.yml`:

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

Start it:

```bash
docker compose up -d
```

The service listens on `http://localhost:8230`. Host port `8230` maps to container port `8000`; change the left half of the mapping if that port is taken. The `./data` volume holds your credentials, configuration and cache, so keep it.

If you prefer plain `docker run`:

```bash
docker run -d \
  --name m365bridge \
  -p 8230:8000 \
  -v "$(pwd)/data:/app/data" \
  --restart unless-stopped \
  ghcr.io/kilimcininkoroglu/m365bridge:latest
```

To build the image from a source checkout instead of pulling it, use `docker compose up --build -d`.

### Option B: Pre-built binary

Download the binary for your platform from [Releases](https://github.com/KilimcininKorOglu/M365Bridge/releases):

| Platform                    | File                            |
|-----------------------------|---------------------------------|
| Linux amd64                 | `m365-bridge-linux-amd64`       |
| Linux arm64                 | `m365-bridge-linux-arm64`       |
| macOS Intel                 | `m365-bridge-darwin-amd64`      |
| macOS Apple Silicon         | `m365-bridge-darwin-arm64`      |
| Windows amd64               | `m365-bridge-windows-amd64.exe` |
| Windows arm64               | `m365-bridge-windows-arm64.exe` |

```bash
mkdir m365bridge && cd m365bridge
curl -L -o m365-bridge \
  https://github.com/KilimcininKorOglu/M365Bridge/releases/latest/download/m365-bridge-linux-amd64
chmod +x m365-bridge
mkdir data
```

The binary resolves every runtime path relative to the current working directory, so it looks for `data/` where you launch it. Always run it from the directory that contains `data/`. Starting it elsewhere produces a missing-token error even when setup succeeded.

### Option C: Build from source

```bash
git clone https://github.com/KilimcininKorOglu/M365Bridge
cd M365Bridge
go build -o bin/m365-bridge ./cmd/cli
mkdir -p data
```

On Windows, build to `bin\m365-bridge.exe`. The same working-directory rule as Option B applies: run every command from the repository root.

## Connecting your Microsoft 365 account

This is the only setup chapter. Every installation option uses it unchanged.

You will collect three things from your browser, write them into one JSON file, and hand that file to the setup wizard. The wizard encrypts everything and writes the configuration the service reads on every start.

> Collect all three. The refresh token alone expires after **24 hours**, and a setup without cookies stops working the next day. This is the single most common reason for "it worked yesterday and now it asks me to sign in again".

### Step 1: Get the refresh token

1. Open [m365.cloud.microsoft](https://m365.cloud.microsoft) and sign in.
2. Press **F12** to open DevTools and switch to the **Console** tab.
3. Paste the snippet below and press Enter.

<details>
<summary>Show the console snippet</summary>

```javascript
(async () => {
// 1. Get oid/tenant from the signed-in account
let oid, tenant;
for (const key of Object.keys(localStorage)) {
  if (!key.includes('active-account-filters')) continue;
  try {
    const val = JSON.parse(localStorage.getItem(key));
    if (val?.homeAccountId?.includes('.')) { [oid, tenant] = val.homeAccountId.split('.'); break; }
  } catch(e) {}
}
if (!oid) {
  const mk = Object.keys(localStorage).find(k => k.startsWith('msal.') && k.includes('|'));
  if (mk) { const p = mk.split('|')[1]; if (p?.includes('.')) [oid, tenant] = p.split('.'); }
}
if (!oid || !tenant) return 'ERROR: No signed-in account found. Log in to m365.cloud.microsoft and run this again.';

// 2. Watch every token exchange for the one this gateway uses
const targetClientID = '4765445b-32c6-49b0-83e6-1d93765276ca';
const origFetch = window.fetch;
let done;
const captured = new Promise(resolve => { done = resolve; });
window.fetch = async function(...args) {
  const resp = await origFetch.apply(this, args);
  const url = typeof args[0] === 'string' ? args[0] : args[0]?.url || '';
  if (url.includes('oauth2/v2.0/token')) {
    try {
      let bodyStr = '';
      const init = args[1];
      if (typeof init?.body === 'string') bodyStr = init.body;
      else if (init?.body instanceof URLSearchParams) bodyStr = init.body.toString();
      else if (init?.body instanceof ArrayBuffer || ArrayBuffer.isView(init?.body)) bodyStr = new TextDecoder().decode(init.body);
      else if (args[0] instanceof Request) bodyStr = await args[0].clone().text();
      // The sign-in exchange puts a broker id in client_id and carries the real
      // target in brk_client_id, so both are accepted.
      const params = new URLSearchParams(bodyStr);
      const isTarget = params.get('client_id') === targetClientID
                    || params.get('brk_client_id') === targetClientID;
      if (isTarget) {
        const data = await resp.clone().json();
        if (data.refresh_token) {
          console.log('===== COPY THE COMPLETE JSON BELOW =====');
          console.log(JSON.stringify({oid, tenant, refresh_token: data.refresh_token}, null, 2));
          done(true);
        }
      }
    } catch(e) {}
  }
  return resp;
};

// 3. Make the app ask for a token
// The page keeps its MSAL instance out of reach of the console, so the refresh
// cannot be requested directly. Moving to another route makes the app request
// one; the original page is restored afterwards.
const startPath = location.pathname;
let moved = false;
for (const href of ['/search', '/library', '/teach', '/chat/all', '/chat']) {
  if (href === startPath) continue;
  const link = document.querySelector('a[href="' + href + '"]');
  if (link) { link.click(); moved = true; break; }
}

// 4. Wait for the exchange, then put everything back
const ok = await Promise.race([captured, new Promise(r => setTimeout(() => r(false), 20000))]);
if (moved) history.back();
window.fetch = origFetch;

return ok
  ? 'Done. Copy the JSON printed above.'
  : 'No token exchange seen; the app is still using a token it refreshed a moment ago. Reload the page and run this again.';
})()
```

</details>

The snippet navigates to another page and back on its own to make the app request a token. Wait a few seconds for this line to appear:

```
===== COPY THE COMPLETE JSON BELOW =====
```

Copy the JSON printed under it. It looks like this:

```json
{
  "oid": "your-oid",
  "tenant": "your-tenant",
  "refresh_token": "your-refresh-token"
}
```

If the snippet reports that it saw no token exchange, the app is still using a token it refreshed moments ago. Reload the page and run it again.

### Step 2: Get the login cookies

These two cookies let the service sign itself back in when the 24-hour refresh token expires. Without them you must repeat Step 1 every day.

The console snippet cannot read them. They belong to `login.microsoftonline.com` rather than to the page you just ran it on, and they are `HttpOnly`, so no script on any page can reach them. Copy them by hand:

1. Open [login.microsoftonline.com](https://login.microsoftonline.com) in the same browser.
2. Press **F12**, then go to **Application** > **Cookies** > `https://login.microsoftonline.com`.
3. Copy the values of these two cookies:
   - `ESTSAUTH`
   - `ESTSAUTHPERSISTENT`

### Step 3: Get the M365 web cookies

These power the conversation list in the browser interface, and the rename and delete operations. Chat works without them; the sidebar falls back to local sessions alone and tells you so.

1. Open [m365.cloud.microsoft](https://m365.cloud.microsoft).
2. Press **F12**, then go to **Application** > **Cookies** > `https://m365.cloud.microsoft`.
3. Copy every cookie listed there. The service sends them back as one `Cookie` header, so copying all of them is both simplest and most reliable.

### Step 4: Write data/setup.json

Create `data/setup.json` next to the `data/` directory your installation uses. Under Docker this is the directory the compose file mounts.

```json
{
  "oid": "your-oid",
  "tenant": "your-tenant",
  "refresh_token": "your-refresh-token",
  "sso_cookies": [
    {"name": "ESTSAUTH", "value": "...", "domain": "login.microsoftonline.com"},
    {"name": "ESTSAUTHPERSISTENT", "value": "...", "domain": "login.microsoftonline.com"},
    {"name": "first-m365-cookie", "value": "...", "domain": "m365.cloud.microsoft"},
    {"name": "second-m365-cookie", "value": "...", "domain": "m365.cloud.microsoft"}
  ]
}
```

**Every cookie entry needs its `domain` field.** The wizard routes each cookie by that field: `login.microsoftonline.com` becomes the sign-in credential, `m365.cloud.microsoft` becomes the web-client credential. A cookie with no `domain` matches neither and is discarded. The wizard still prints how many cookies it read, so a file missing this field looks like it worked and silently produces a setup that dies after 24 hours.

### Step 5: Run the setup wizard

**Docker:**

```bash
docker exec -it m365bridge ./bin/m365-bridge setup-wizard
```

**Binary, Linux and macOS:**

```bash
./m365-bridge setup-wizard
```

**Binary, Windows PowerShell:**

```powershell
.\m365-bridge.exe setup-wizard
```

The wizard prints the browser instructions again for reference, then reads `data/setup.json`, encrypts the refresh token and both cookie sets with AES-256-GCM, redeems the refresh token against Microsoft, and writes `data/.env`.

The redemption is the verification. The token is staged in a file of its own until Microsoft answers, so a value that cannot be redeemed never replaces a token that works, and a refresh token shorter than 100 characters is refused before anything is stored: it is the example text left in place of a real value.

`tenant` and `oid` are checked before anything is stored too. A value that is not a GUID, and a GUID whose every digit is the same character such as `22222222-2222-2222-2222-222222222222`, is refused with the field named, because Entra issues no such id and a value of that shape is filler someone typed in place of a real one. What lands in `data/.env` afterwards is not what the file said either: the ids are read from the claims of the access token Microsoft just returned. That is the only check the `oid` can get, since it takes no part in the token exchange, and a wrong one otherwise breaks the install much later, on an ordinary chat request.

Read its output rather than only its exit. A successful run with all three credentials prints all of these:

```
SSO cookies encrypted and saved
M365 web cookies encrypted and saved
Refresh token redeemed, encrypted and saved
```

If the first two lines are missing while the wizard still reports cookies as captured, your `domain` fields are missing or misspelled. Fix `data/setup.json` and run the wizard again.

Pass `--file` to read the JSON from another path:

```bash
./m365-bridge setup-wizard --file /path/to/setup.json
```

### Step 6: Start the service and check it

Docker users already have it running. Restart the container so it picks up the new configuration:

```bash
docker compose restart
```

For a binary installation, start the server:

```bash
./m365-bridge serve --port 8000
```

Then ask it a question:

```bash
curl http://localhost:8230/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt5.5","messages":[{"role":"user","content":"Hello"}]}'
```

Use the port your installation listens on: `8230` for the Docker setup above, or whatever you passed to `--port`. Open the same address in a browser to reach the interface.

### Keeping the connection alive

Microsoft issues single-page-application refresh tokens with a **24-hour** lifetime. What happens next depends on what you collected:

| What you provided | Result |
|---|---|
| Refresh token only | Stops working after 24 hours. Repeat Steps 1, 4 and 5 every day. |
| Refresh token and login cookies | The service signs itself back in silently. Runs for weeks. |

The login cookies themselves expire eventually, and a tenant policy or a password change ends them sooner. When the service starts reporting authentication errors again, repeat Steps 1 to 5. Nothing else needs to be reset.

### If something goes wrong

| Symptom | Cause |
|---|---|
| The wizard reports a missing or invalid `refresh_token` | `data/setup.json` is missing, empty, or holds the placeholder text instead of the real JSON. |
| The wizard says the `refresh_token` is too short to be a real token | The example value is still in `data/setup.json`. A real one runs to thousands of characters. |
| The wizard says `tenant` or `oid` is not a GUID, or is filler text | That field in `data/setup.json` was never replaced with the value the browser console printed. |
| Every request answers `upstream_auth_failed` and the log shows `AADSTS90002: Tenant not found` | `data/.env` holds a placeholder tenant. The `tid` and `oid` claims of the token in `data/tokens/token_cache.json` carry the real ones. |
| The wizard reports cookies as captured but saves none | Cookie entries have no `domain` field. See Step 4. |
| The server exits with a token refresh error | The refresh token expired and there are no usable login cookies. Repeat Steps 1 to 5. |
| The server reports a missing token even after setup succeeded | You launched it from a different directory. Run it where `data/` lives. |
| The sidebar shows no conversation names | The M365 web cookies are missing or expired. Repeat Step 3. |
| The server exits complaining about `M365_TENANT_ID` | The wizard never wrote `data/.env`, which means it failed before its last step. |

## Usage

### Command line

```bash
# One question, one answer
./m365-bridge "your question"

# Interactive session
./m365-bridge -i

# Choose a model
./m365-bridge --model gpt5.5-reasoning "your question"

# Print the whole answer at once instead of streaming it
./m365-bridge --no-stream "your question"

# List the models this build serves
./m365-bridge --list-models

# Start the HTTP server
./m365-bridge serve --port 8000
```

Every flag is optional. With none, `serve` listens on port 8000 and `setup-wizard` reads `data/setup.json`.

| Flag            | Type   | Default | Description                                          |
|-----------------|--------|---------|------------------------------------------------------|
| `-i`            | bool   | false   | Interactive mode, multi-turn                         |
| `--model`       | string | `auto`  | Model key; see [Models](#models)                     |
| `--reasoning`   | bool   | false   | Use the reasoning variant                            |
| `--no-stream`   | bool   | false   | Print the full answer at once                        |
| `--list-models` | bool   | false   | List the models and exit                             |
| `--version`     | bool   | false   | Print the version and exit                           |
| `--help`        | bool   | false   | Print every flag and environment variable, and exit  |

Anything left over on the command line is the question itself.

`serve` takes `--port` (default `8000`) and `--version`. `setup-wizard` takes `--file` (default `data/setup.json`).

### HTTP

```bash
# No API key configured
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"Hello"}]}'

# With an API key
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{"model":"gpt5.5","messages":[{"role":"user","content":"Hello"}]}'

# Streaming, with a named session
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -H "X-Session-Id: my-session-1" \
  -d '{"model":"gpt5.5","stream":true,"messages":[{"role":"user","content":"Hello"}]}'
```

### Python, OpenAI SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8000/v1",
    api_key="your-api-key",  # required only when M365_API_KEYS is set
)
resp = client.chat.completions.create(
    model="gpt5.5",
    messages=[{"role": "user", "content": "Hello"}],
)
print(resp.choices[0].message.content)
```

### Python, Anthropic SDK

```python
from anthropic import Anthropic

client = Anthropic(
    base_url="http://127.0.0.1:8000",
    api_key="your-api-key",
)
resp = client.messages.create(
    model="gpt5.5",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello"}],
)
print(resp.content[0].text)
```

The Anthropic SDK appends `/v1/messages` itself, so its base URL stops at the host.

### Sending an image

```python
from openai import OpenAI
import base64

client = OpenAI(base_url="http://127.0.0.1:8000/v1", api_key="your-api-key")

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

### What happens on the first request

1. The server reads `data/.env` from the current working directory.
2. It loads the encrypted refresh token from `data/tokens/rt_90day.txt`.
3. It exchanges that token for an access token, which takes a second or two.
4. It reports `Starting API server on port 8000 (no API key required)`, or `(API key required, N key(s) configured)` when keys are set.
5. The first chat request takes slightly longer than the rest, because it opens the WebSocket to `substrate.office.com`.

If the refresh token is missing or expired, the server tries the login cookies in `data/tokens/sso_cookies.json`. If those are gone too, it stops with a token refresh error and you should repeat the setup steps.

## Configuration

Configuration comes from `data/.env`. A variable set in the process environment overrides the file. `m365-bridge --help` prints every variable with its current default.

The setup wizard writes the first two.

| Variable         | Default                                | Description                                                                     |
|------------------|----------------------------------------|---------------------------------------------------------------------------------|
| `M365_TENANT_ID` | required                               | Directory (tenant) ID. The service exits without it.                            |
| `M365_USER_OID`  | required                               | Object ID of the signed-in user. The service exits without it.                  |
| `M365_CLIENT_ID` | `4765445b-32c6-49b0-83e6-1d93765276ca` | OAuth client the access tokens are issued to. Change it only for a tenant that blocks the default. |
| `M365_API_KEYS`  | unset                                  | Comma-separated keys a client must present. Unset leaves every `/v1/*` route and `/mcp` open. |
| `M365_API_KEY`   | unset                                  | A single key, read only when `M365_API_KEYS` is unset.                          |
| `TZ`             | system zone                            | Timezone sent with each turn. Falls back to `/etc/localtime`, then UTC.         |

The remaining variables are documented in the section describing the behaviour they change: [the browser interface](#browser-interface), [the advertised context window](#advertised-context-window), [tool loops](#client-driven-tool-loops), [built-in coding tools](#built-in-coding-tools-opt-in) and [image generation](#image-generation).

## Browser interface

Open the server's root address in a browser, `http://localhost:8230/` under the Docker setup above. The interface is compiled into the binary, so there is no asset directory to serve and no second process to run.

It lists your conversations in a sidebar, streams answers as they arrive, and lets you pick a model from `GET /v1/models`. You can create, rename and delete conversations from it. Answers are rendered as markdown, so a comparison table arrives as a table and a citation as a link rather than a bare URL mid-sentence. The reasoning block behind **Show the thinking** is rendered the same way, because the backend writes that in markdown too. Your own messages are shown exactly as you typed them. Rename and delete ask inside the page rather than through the browser's own dialogs.

Everything the page needs is compiled in. It loads no font, script or stylesheet from anywhere else, so it works on a machine with no route to the internet beyond the M365 backend itself.

### Addresses

Each conversation has its own address, `/c/{session id}`. Opening a conversation writes that address, so you can reload onto it, link to it, and reach it with the browser's back and forward buttons. An address naming a conversation the gateway no longer holds falls back to the empty pane.

### What the sidebar shows

Two sources are merged. `GET /v1/conversations` supplies the names and needs the M365 web cookies from Step 3. `GET /v1/sessions` supplies the session ids that make a conversation continuable. A conversation present in both appears as one row.

Without the cookies the first call fails, the sidebar falls back to local mappings alone, and says so. A conversation only M365 knows about is marked as such, and gets a session id bound to it the moment you open it. That is what makes a conversation you started in the M365 web or mobile client continuable here.

### Language

The interface ships in English and Turkish. The picker sits next to the name in the sidebar, and English is the default.

Each language is one JSON file under `web/src/locales`, named by its language code. Nothing in the code names a file; the build compiles the directory in. To add a language, copy `en.json`, translate its values, save it as `de.json` or similar, and run `make ui`. The `$label` entry names the language in its own language and is what the picker shows. A file that translates only part of the catalog falls through to English for the rest, so a partial translation is usable rather than broken.

The choice is stored in the `m365bridge_lang` cookie. A browser with no cookie, and a cookie naming a language this build does not carry, are treated the same way: English, written back so the stored value and the shown language cannot disagree.

### Transcripts

The backend tracks history by conversation ID and never replays it, so the gateway keeps its own record of the turns it carried, one file per session under `data/transcripts`. This is the only place message content reaches disk. Entries per session, bytes per message and files in the store are all bounded.

A conversation started outside this gateway has no record, so its history is empty when you open it. The interface says so and offers to fetch it, which calls `GET /v1/conversations/{id}/messages`. Deleting a session deletes its transcript, and so does a turn that produced nothing, since both start a new conversation under that id.

### Settings

| Variable               | Default | Description                                                                                    |
|------------------------|---------|------------------------------------------------------------------------------------------------|
| `M365_ENABLE_WEB_UI`   | `1`     | Serves the interface at `/` and records transcripts. `0`, `false`, `off` or `no` disables both. |
| `M365_WEB_UI_PASSWORD` | unset   | Password the interface asks for. Unset opens it to anyone who can reach it.                     |

Turning the interface off makes `/` return 404 and stops the recording, which is what a deployment that only proxies wants. `GET /v1/sessions/{id}/messages` then answers `404 transcripts_disabled`.

### Password

Set `M365_WEB_UI_PASSWORD` and the interface asks for it before it draws anything. Leave it unset and the interface opens with no login.

The password is one more credential the gateway accepts, not a session of its own. The browser holds it in a cookie and sends it in the same `Authorization` header an API client sends its key in. Every credential therefore stays on a header, where a cross-site form cannot carry it, and the interface reaches the authenticated routes without a session mechanism that would need CSRF protection of its own.

Two public routes exist so the interface can learn what to ask for, because the page itself is served without a credential:

| Endpoint               | Description                                                              |
|------------------------|--------------------------------------------------------------------------|
| `GET /v1/auth`         | Reports which gate to show: `{"mode": "none" \| "password" \| "api_key"}` |
| `POST /v1/auth/verify` | Answers whether the credential in the request header is accepted         |

Neither returns a secret. The credential travels in a header rather than a body, so it stays out of anything that records a payload, and the log records only that a credential was rejected.

`M365_WEB_UI_PASSWORD` and `M365_API_KEYS` are separate switches:

- **Neither set**: the interface opens with no login, and every route is open.
- **Password only**: the interface asks for the password. The API stays open, because an empty key list means open everywhere else in this gateway. Set `M365_API_KEYS` as well if the API must be closed too.
- **Keys only**: the interface asks for an API key, because without one its every data call is refused.
- **Both**: the interface asks for the password, and the API accepts either the password or a key.

### Rebuilding the interface

The sources live in `web/`, and the build output is committed at `pkg/webui/dist` because `go:embed` reads it at compile time. After changing anything under `web/`:

```bash
make ui      # builds in a node container and copies the output into pkg/webui/dist
make up      # rebuilds the image and restarts the container
```

For faster iteration, run the Vite dev server instead. It serves the interface and forwards `/v1` to a gateway on `http://localhost:8230`:

```bash
cd web && npm install && npm run dev
```

Finish with `make ui` so the committed build matches the sources.

The interface uses React, `react-markdown` with `remark-gfm` for answers, and SweetAlert2 for its dialogs. All of them are bundled into the committed output, so the served page fetches nothing at runtime.

## Sessions and conversations

Each session maps to one M365 conversation. The session ID is resolved in this order:

1. The part after the colon in the model name, `model:sessionID`
2. `previous_response_id` in the request body, on `/v1/responses` only
3. `session_id` in the request body
4. `user` in the request body
5. The `X-Session-Id` header
6. The `X-Claude-Code-Session-Id` header (Claude Code) or `session-id` (Codex)
7. `hash(api_key + first user message)` when auth is on, or `hash(first user message)` when it is off

Every endpoint uses this one order.

Claude Code and Codex each stamp their own session on every request under a header name neither can be told to change. Rule 6 reads both, so both clients keep one conversation per session with no configuration. It ranks below the rules above it because a client writes that header on its own, while everything above is a value the caller set deliberately.

Codex also sends `thread-id` carrying the same value as `session-id`, so reading it would only answer for a request that already carries `session-id`. Its `x-codex-turn-metadata` header is never read: the `installation_id` inside stays the same across every session on one machine, so keying a conversation on it would merge unrelated sessions into one.

The hash fallback covers any other client, as long as its first user message differs.

### Naming a session in the model

You can embed a session ID in the model name with a colon:

```
model: "gpt5.5-reasoning:my-session-001"
```

This is equivalent to sending `X-Session-Id: my-session-001` or `session_id` in the body. The model key is read before the colon and the session ID after it. Claude Code and Codex are already covered by rule 6, so reach for this when you want to name the session yourself, or for a client that sends no session header at all.

### Managing sessions

`GET /v1/sessions` lists the mappings, newest first. Entries written before the mapping carried its session ID cannot be listed, because the cache file name is a hash of the key. They are reported as a `legacy_entries` count and join the list after their next turn rewrites them.

`DELETE /v1/sessions/{id}` deletes the upstream M365 conversation and then clears the mapping, so the next turn on that session ID starts fresh. The mapping is kept when the upstream delete fails, so you can retry. Deleting the conversation needs the M365 web cookies; add `?local_only=true` to clear only the mapping and leave the conversation in place, which is what a deployment without those cookies needs.

Deleting is paired in both directions. `DELETE /v1/conversations/{id}` clears every session mapped to that conversation along with its transcript, because more than one session can point at one conversation.

`PUT /v1/sessions/{id}` takes `{"conversation_id": "..."}` and points a session at a conversation that already exists. The chat path only ever resolves a session to a conversation, so without this route a conversation started elsewhere could not be continued through the gateway. Rebinding an existing session is allowed.

### System instructions

The M365 backend keeps conversation history itself and receives only the latest turn, so an instruction sent in an earlier message would never reach it. Every `system` message in the request is therefore collected and prefixed to that turn, and kept out of the flattened history where it would otherwise read as a past conversation line.

`developer` is treated identically. OpenAI renamed the role for its reasoning models and both names remain valid, so a client sending either reaches the model the same way.

Anthropic's top-level `system` field is accepted as a string or as an array of text blocks, and becomes the same prefixed instruction.

## API endpoints

| Endpoint                              | Description                                            |
|---------------------------------------|--------------------------------------------------------|
| `POST /v1/chat/completions`           | OpenAI Chat Completions, streaming and non-streaming   |
| `POST /v1/completions`                | OpenAI text completion                                 |
| `POST /v1/responses`                  | OpenAI Responses API                                   |
| `POST /v1/responses/compact`          | OpenAI Responses Compact, for Codex remote compaction  |
| `POST /v1/messages`                   | Anthropic Messages, with dedicated SSE handlers        |
| `POST /v1/messages/count_tokens`      | Anthropic input token counting                         |
| `POST /v1/complete`                   | Anthropic Complete                                     |
| `POST /v1/images/generations`         | Generate an image from text                            |
| `POST /v1/images/edits`               | Edit an existing image                                 |
| `GET /v1/images/{ref}`                | An image generated in a chat answer                    |
| `GET /v1/conversations`               | List M365 conversations; needs the M365 web cookies    |
| `POST /v1/conversations`              | Create a conversation with an initial message          |
| `PATCH /v1/conversations/{id}`        | Rename a conversation with `{"name": "..."}`           |
| `DELETE /v1/conversations/{id}`       | Delete a conversation and clear its session mappings   |
| `GET /v1/conversations/{id}/messages` | Read the turns of a conversation held upstream         |
| `GET /v1/models`                      | Model list, in both wire formats                       |
| `GET /v1/quota`                       | Last observed M365 conversation message quota          |
| `GET /v1/personalization`             | The account's Copilot memory settings                  |
| `PATCH /v1/personalization`           | Turns the account's Copilot memory on or off           |
| `GET /v1/sessions`                    | List the session-to-conversation mappings              |
| `GET /v1/sessions/{id}`               | Read one session's conversation ID                     |
| `PUT /v1/sessions/{id}`               | Bind a session to an existing conversation             |
| `GET /v1/sessions/{id}/messages`      | Read the recorded turns of a session                   |
| `DELETE /v1/sessions/{id}`            | Delete the conversation and clear the mapping          |
| `POST /mcp`                           | Model Context Protocol server, JSON-RPC 2.0            |
| `GET /v1/health`                      | Reachability probe for Codex, no auth                  |
| `GET /v1/auth`                        | Which gate the interface must show, no auth            |
| `POST /v1/auth/verify`                | Whether a credential is accepted, no auth              |
| `GET /health`                         | Health check, no auth                                  |
| `GET /`                               | Browser interface; the page itself needs no auth       |

`GET /v1/sessions/{id}/messages` returns what the gateway recorded for that session. It answers `404 transcripts_disabled` when `M365_ENABLE_WEB_UI` is off, and an empty list for a conversation started elsewhere.

`GET /v1/conversations/{id}/messages` reads the turns of a conversation this gateway never carried. The backend keeps history under the conversation ID and offers no action that returns it, so this recovers the history from the conversation page the M365 web client renders, which needs the M365 web cookies. It costs a page download and a walk of a serialization this project does not control, so nothing calls it automatically. Add `?session_id=...` to store the result under that session and bind the session to the conversation, which is what the interface does behind its "load history" button. Without it the response is returned and nothing is written. A page carrying no readable turn answers `502` rather than an empty conversation, because a caller cannot tell an empty conversation from a failed read.

## Error responses

Every endpoint reports failures in the OpenAI error shape. `type` is the category a client branches on, and `code` is the specific machine-readable reason:

```json
{"error": {"message": "M365 rate limit reached for this chat request; retry after the interval in the Retry-After header", "type": "rate_limit_error", "code": "rate_limit_exceeded"}}
```

`type` is one of `invalid_request_error`, `authentication_error`, `rate_limit_error` or `server_error`. For a request the proxy rejects on its own, `code` is the status slug, such as `bad_request` or `method_not_allowed`. A body over 32 MiB is the one exception with a name of its own, `413 request_too_large`, so a client can tell a request it must shrink from one that was simply malformed.

A failed backend request is classified rather than reported as a generic `500`:

| Status | `code`                     | Cause                                                        |
|--------|----------------------------|--------------------------------------------------------------|
| `401`  | `upstream_auth_failed`     | The stored credentials are missing or could not be refreshed |
| `403`  | `insufficient_permissions` | M365 refused the request for the configured account          |
| `429`  | `rate_limit_exceeded`      | M365 throttled the request; a `Retry-After` header is sent   |
| `429`  | `upstream_throttled`       | The conversation message quota is exhausted                  |
| `409`  | `tool_round_limit`         | One turn drove more tool rounds than `M365_MAX_TOOL_ROUNDS`  |
| `404`  | `model_not_found`          | The requested model is not in `GET /v1/models`               |
| `502`  | `upstream_error`           | M365 rejected the request or was unreachable                 |
| `502`  | `upstream_unavailable`     | The WebSocket handshake failed or the connection dropped     |
| `502`  | `upstream_turn_failed`     | M365 ended the turn without producing an answer              |
| `502`  | `upstream_content_blocked` | M365 declined the request instead of answering it            |
| `503`  | `upstream_unavailable`     | M365 reported itself unavailable                             |
| `504`  | `upstream_timeout`         | M365 did not answer in time                                  |

A failure with no evidence of an upstream cause still reports `500` with `internal_error`, so a bug in the proxy is not presented as a backend outage. Error messages are fixed text; the transport error, including request URLs and credential file paths, stays in the server log.

Once a stream has opened the status is already sent, so the same classification travels in the body. OpenAI-shaped routes put an `error` object on the data line and then `[DONE]`. `/v1/messages` and `/v1/complete` send an `error` event, and `/v1/responses` sends `response.failed`. No route writes the failure as assistant content, which a client would otherwise store as the answer.

## Models

Model selection travels in the `tone` field the M365 backend reads. GPT-5.x keys route to the GPT-5 backend. Claude tones return Claude answers, though M365 does not expose the underlying model identity in its SignalR metadata.

| Key                        | Tone              | OpenAI ID         | Thinking | Backend |
|----------------------------|-------------------|-------------------|----------|---------|
| `auto`                     | Magic             | gpt-4-auto        | No       | GPT-5   |
| `quick`                    | Chat              | gpt-4-quick       | No       | GPT-5   |
| `reasoning`                | Gpt_5_2_Reasoning | gpt-4-reasoning   | Yes      | GPT-5   |
| `gpt5.2`                   | Gpt_5_2_Chat      | gpt-5.2           | No       | GPT-5   |
| `gpt5.2-reasoning`         | Gpt_5_2_Reasoning | gpt-5.2-reasoning | Yes      | GPT-5   |
| `gpt5.3`                   | Gpt_5_3_Chat      | gpt-5.3           | No       | GPT-5   |
| `gpt5.4`                   | Gpt_5_4_Chat      | gpt-5.4           | No       | GPT-5   |
| `gpt5.4-reasoning`         | Gpt_5_4_Reasoning | gpt-5.4-reasoning | Yes      | GPT-5   |
| `gpt5.5`                   | Gpt_5_5_Chat      | gpt-5.5           | No       | GPT-5   |
| `gpt5.5-reasoning`         | Gpt_5_5_Reasoning | gpt-5.5-reasoning | Yes      | GPT-5   |
| `gpt5.6`                   | Gpt_5_6_Chat      | gpt-5.6           | No       | GPT-5   |
| `gpt5.6-reasoning`         | Gpt_5_6_Reasoning | gpt-5.6-reasoning | Yes      | GPT-5   |
| `gpt6sol`                  | Gpt_6_Sol_Reasoning | gpt-6-sol       | No       | GPT-6   |
| `claude`                   | Claude_Sonnet     | claude-sonnet-4.6 | No       | Claude  |
| `claude-sonnet`            | Claude_Sonnet     | claude-sonnet-4.6 | No       | Claude  |
| `claude-opus`              | Claude_Opus       | claude-opus-4.6   | Yes      | Claude  |
| `claude-sonnet-4-20250514` | Claude_Sonnet     | claude-sonnet-4.6 | No       | Claude  |

### Which one to use

| What you want                             | Model              |
|-------------------------------------------|--------------------|
| General purpose, let the backend decide   | `auto`             |
| Fast answers to simple questions          | `quick`            |
| Complex reasoning, multi-step problems    | `gpt5.5-reasoning` |
| The newest reasoning model                | `gpt6sol`          |
| Plain chat on a recent model              | `gpt5.5`           |
| Claude Sonnet 4.6                         | `claude-sonnet`    |
| Claude Opus 4.6, the most capable         | `claude-opus`      |

A reasoning model produces thinking content alongside its answer. OpenAI endpoints expose it as `reasoning_content`, and Anthropic endpoints as a `thinking` block before the `text` block. `claude-opus` produces it as well; `claude-sonnet` does not. `gpt5.6-reasoning` advertises the capability but has not been observed emitting it. Every advertised capability comes from measured behaviour rather than from the name of the tone.

### Model names this gateway does not serve

A model name outside the registry is answered with `404 model_not_found`, never with a different entry, so a caller is never answered by a tone it did not ask for.

The registry carries the vendor names agent clients send, so `claude-sonnet-4-20250514` resolves. `gpt-4o` and `o1` do not, and return `404`. `GET /v1/models` lists every id that is served.

A request with no model at all defaults to `gpt5.5-reasoning`, the reasoning tone that is reliable for tool calling, rather than to `auto`. This covers an empty `model` field, a missing one, and a bare `:session-id` suffix.

### Advertised context window

Each entry in `GET /v1/models` advertises `context_window` and `max_output_tokens` so client harnesses do not pre-truncate prompts or output. These are hints for the client only; M365 enforces its own limits regardless.

| Variable                 | Default   | Description                                            |
|--------------------------|-----------|--------------------------------------------------------|
| `M365_CONTEXT_WINDOW`    | `1000000` | Advertised context window token count in `/v1/models`. |
| `M365_MAX_OUTPUT_TOKENS` | `1000000` | Advertised maximum output token count in `/v1/models`. |

### Model list fields

`GET /v1/models` lists each model once, keyed by its advertised id and sorted, so aliases such as `claude` and `claude-sonnet` do not appear twice. Each entry carries:

| Field               | Description                                                                                     |
|---------------------|-------------------------------------------------------------------------------------------------|
| `owned_by`          | `anthropic-via-microsoft-365` for the Claude tones, `microsoft-365` for the rest.                 |
| `context_window`    | The advertised window, from `M365_CONTEXT_WINDOW`.                                                |
| `max_output_tokens` | The advertised output budget, from `M365_MAX_OUTPUT_TOKENS`.                                      |
| `max_input_tokens`  | The window minus the output budget, or the full window when the output budget is not smaller.     |
| `supports_tools`    | Always `true`; every model reaches caller-defined tools through the simulated tool calling layer. |

The response also carries `reasoning_effort_presets`, each an `{effort, description}` pair naming an effort value the Responses API accepts.

Each entry additionally carries the model-catalog fields Codex CLI reads, which plain OpenAI clients ignore: `base_instructions`, `model_messages`, `default_reasoning_level`, `apply_patch_tool_type`, `shell_type`, `tool_mode`, `truncation_policy`, `supports_parallel_tool_calls`, and the verbosity and reasoning-summary defaults. Every capability appears both at the top level and under `capabilities`, because OpenAI-compatible clients disagree on where to look for it.

#### Both wire formats in one response

The route answers OpenAI and Anthropic clients at once, because both protocols reach the proxy on the same path. Each entry is a valid OpenAI model object and a valid Anthropic `ModelInfo` at the same time. The two field sets do not collide, so each client reads only what it knows.

| Field           | Protocol  | Description                                           |
|-----------------|-----------|-------------------------------------------------------|
| `object`        | OpenAI    | Always `model`.                                       |
| `created`       | OpenAI    | Unix seconds.                                         |
| `owned_by`      | OpenAI    | The vendor behind the tone.                           |
| `shutdown_date` | OpenAI    | Always `null`; no model is scheduled to retire.       |
| `type`          | Anthropic | Always `model`.                                       |
| `display_name`  | Anthropic | Human-readable name, such as `Claude Sonnet 4.6`.     |
| `created_at`    | Anthropic | The same instant as `created`, in RFC 3339.           |
| `max_tokens`    | Anthropic | The output ceiling, matching `max_output_tokens`.     |

The list itself carries `object` and `data` for OpenAI, and `has_more`, `first_id` and `last_id` for Anthropic. The whole registry fits in one page, so `has_more` is always `false` and the cursors are the first and last advertised id.

`capabilities` holds Anthropic's capability tree alongside the flat OpenAI-style entries: `batch`, `citations`, `code_execution`, `context_management`, `effort`, `image_input`, `pdf_input`, `structured_outputs` and `thinking`. Every node carries a `supported` boolean, and three nest one level further: `effort` names each accepted value (`low`, `medium`, `high`, `xhigh`, `max`), `thinking` names its `types` (`enabled`, `adaptive`), and `context_management` names each dated strategy. The values state what the proxy actually does, so most read `false`. `effort` is `true` only for a model with a `-reasoning` variant to route to, and `thinking` only for a tone measured to emit chain-of-thought content.

Claude Code discovers gateway models through this route. It reads only the Anthropic format and only adds ids beginning with `claude` or `anthropic`, which is why the Claude tones keep such ids.

### Conversation quota

M365 enforces a per-conversation message ceiling and reports the counters on its update frames. Every turn logs them, for example `ConvStream throttling: used=8 max=600 headroom=592`.

`GET /v1/quota` returns the last observed counters. The backend only sends them while a turn is in flight, so the values reflect the most recent chat request rather than a live lookup, and they belong to whichever conversation produced that request:

```json
{"object":"quota","available":true,"exhausted":false,"used":8,"max":600,"headroom":592}
```

Counters the proxy does not recognize are returned under `extra` instead of being dropped. When a request produces an empty upstream response and the last counters show the ceiling was reached, the proxy answers `429` with code `upstream_throttled` rather than a generic empty-response error. Start a new session to continue.

### Copilot memory

M365 Copilot keeps a memory on the account, and it reaches every turn this proxy serves whatever conversation the turn belongs to. Measured against the live backend, a brand-new conversation listed content from unrelated earlier sessions and applied a writing-style preference stored by one of them. Conversation isolation runs through the session-to-conversation mapping, and this setting passes underneath it.

`GET /v1/personalization` reports the account's settings, and `PATCH /v1/personalization` with `{"memory_enabled": false}` changes the memory switch. The browser interface shows the same switch at the bottom of its sidebar.

```json
{"object":"personalization","memory_enabled":false,"insights_from_history_enabled":false,
 "custom_instruction_enabled":true,"graph_content_enabled":false,"personalization_allowed_by_tenant":true}
```

Turning memory off also turns off insights from conversation history; the backend moves the two together. The write is verified with a read, so the answer is the state the account reports rather than the value that was asked for.

**This is your real M365 account setting.** It applies to the M365 web and mobile Copilot too, and nothing here changes it on its own: the proxy only reports it until someone moves the switch. A tenant that forbids personalization answers `409 personalization_disabled_by_tenant`.

### Token usage

Prompt and completion token counts are estimates produced locally, because the M365 backend reports no usage. The encoder is `o200k_base`, the encoding of the GPT-5 class models the backend serves, with `cl100k_base` as a fallback and a character-based estimate when neither vocabulary can be fetched. Every `usage` object names which one produced the numbers:

```json
{"prompt_tokens": 42, "completion_tokens": 17, "reasoning_tokens": 6, "total_tokens": 59, "usage_source": "tiktoken_o200k_base_estimate"}
```

`usage_source` and `reasoning_tokens` are non-standard fields; the standard fields keep their meaning and position. `reasoning_tokens` counts the thinking content and reads `0` for a tone that emits none. Every endpoint reports usage, streaming and non-streaming alike, including `/v1/complete`, whose own format defines no usage object.

The Anthropic endpoints report the same counts under their own field names, with the same two extra fields:

```json
{"input_tokens": 42, "output_tokens": 17, "reasoning_tokens": 6, "usage_source": "tiktoken_o200k_base_estimate"}
```

A streaming `/v1/messages` turn splits that object the way the Anthropic wire format does: `message_start` carries the input side, `message_delta` carries the output side, and both name their source. A streaming `/v1/complete` turn reports usage on its final `completion` event, because the earlier events carry deltas.

`/v1/chat/completions` and `/v1/completions` accept the OpenAI `stream_options` object. `{"include_usage": false}` withholds the usage object from a streaming turn. Leaving `stream_options` out keeps it, which differs from OpenAI's own default of `false`: this proxy has always reported usage on every streaming turn and clients here read it. Prompt tokens are counted from the message roles and contents, the serialized tool definitions and the `tool_choice` value, plus a fixed per-message and per-tool framing allowance. The `tool_choice` allowance applies only when the request declared tools, so the same turn costs the same on every endpoint.

### Stop sequences

A stop sequence ends the answer where the caller said it ends. Every chat endpoint accepts one under its own protocol's name:

| Endpoint               | Field            | Shape                        |
|------------------------|------------------|------------------------------|
| `/v1/chat/completions` | `stop`           | A string or an array of them |
| `/v1/completions`      | `stop`           | A string or an array of them |
| `/v1/messages`         | `stop_sequences` | An array of strings          |
| `/v1/complete`         | `stop_sequences` | An array of strings          |

The answer is cut just before the earliest sequence that appears in it, and the sequence itself is removed, so a caller that frames a turn does not read the frame back. With several sequences the answer ends at whichever arrives first, not at whichever was listed first. An empty sequence is ignored rather than matching at offset zero.

The OpenAI endpoints report the ordinary `finish_reason: "stop"`, the same as an answer that ended on its own. The Anthropic endpoints report `stop_reason: "stop_sequence"` and name the sequence that fired: `/v1/messages` in `stop_sequence`, `/v1/complete` in `stop`. Both fields stay `null` when the answer ended on its own, so a client testing for null is not misled by an empty string. `max_tokens` still wins when it is reached first, and the reported reason becomes `max_tokens`.

A streamed answer is cut as it is produced, not afterwards. A sequence can straddle two upstream chunks, so the deltas pass through a writer that holds back the tail which could still complete one, released on a character boundary. A request sending no stop sequence holds nothing back and receives every chunk as it arrives.

## MCP server

`POST /mcp` exposes M365 Copilot to Model Context Protocol clients over JSON-RPC 2.0, protocol revision `2025-06-18`. It supports `initialize`, `tools/list`, `tools/call` and `ping`; lifecycle notifications are acknowledged with `202` and no body. The route requires an API key when one is configured.

| Tool             | Arguments                                  | Description                                |
|------------------|--------------------------------------------|--------------------------------------------|
| `ask_copilot`    | `prompt` (required), `model`               | One stateless Copilot turn returning text  |
| `describe_image` | `image_url` (required, data URI), `prompt` | Asks Copilot about an inline image         |

```bash
curl -s -X POST http://localhost:8000/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask_copilot","arguments":{"prompt":"Summarize the CAP theorem"}}}'
```

Copilot is deliberately a leaf in the MCP role. The simulated tool calling the `/v1` endpoints use is **not** offered through MCP: an MCP client already has a real, schema-enforced tool mechanism, and nesting the prompt-based emulation inside it would create two competing tool loops. Every MCP call is an independent turn with no conversation continuity.

## Tool calling

M365 does not natively run tools a caller defines. M365Bridge fills the gap with **simulated tool calling**, so Claude Code's Read, Bash and Write, Codex's tools, and your own definitions all work.

1. The client sends a request carrying a `tools` array, in OpenAI function or Anthropic tool schema form.
2. M365Bridge serializes that request into the prompt it sends to Copilot.
3. Copilot answers with a full response JSON in a ```` ```json ```` block.
4. M365Bridge parses it and emits OpenAI `tool_calls` or Anthropic `tool_use` content blocks.
5. The client runs the tool and returns the result in its next message.

This works on `/v1/chat/completions`, `/v1/messages` and `/v1/responses`, streaming and non-streaming. Requests without `tools` are unaffected and need no configuration.

### Example, OpenAI

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "model": "gpt5.5-reasoning",
    "messages": [{"role": "user", "content": "Run: echo hello"}],
    "tools": [{
      "type": "function",
      "function": {
        "name": "bash",
        "description": "Run a shell command",
        "parameters": {
          "type": "object",
          "properties": {"command": {"type": "string"}},
          "required": ["command"]
        }
      }
    }],
    "tool_choice": "required"
  }'
```

```json
{
  "choices": [{
    "finish_reason": "tool_calls",
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "call_001",
        "type": "function",
        "function": {"name": "bash", "arguments": "{\"command\": \"echo hello\"}"}
      }]
    }
  }]
}
```

### Example, Anthropic

```bash
curl http://127.0.0.1:8000/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "model": "gpt5.5-reasoning",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Run: echo hello"}],
    "tools": [{
      "name": "bash",
      "description": "Run a shell command",
      "input_schema": {
        "type": "object",
        "properties": {"command": {"type": "string"}},
        "required": ["command"]
      }
    }],
    "tool_choice": {"type": "any"}
  }'
```

```json
{
  "content": [{
    "type": "tool_use",
    "id": "call_0e46d749-f182-419e-865f-abcb9c200de9",
    "name": "bash",
    "input": {"command": "echo hello"}
  }],
  "stop_reason": "tool_use"
}
```

### How calls are validated

- Arguments are validated against the declared JSON schema: `type`, `enum`, `required`, nested `properties` and array `items`. A call that violates the contract is dropped, and the proxy re-asks once carrying the rejection reason, so agent clients never receive an unexecutable call. This works best for single-step calls. Sustained multi-round agent loops, such as Claude Code's `/init` or its sub-agent tasks, depend on the backend model's own tool-use reliability and are not guaranteed.
- Under `additionalProperties: false`, arguments the schema does not declare are removed rather than rejected, so one stray field does not cost a round trip.
- `tool_choice` is enforced when the response is parsed, not merely requested in the prompt. Under `"none"` no call is forwarded. When a specific function is pinned, a call to any other tool is dropped and re-asked.
- `parallel_tool_calls: false` (OpenAI) and `tool_choice.disable_parallel_tool_use: true` (Anthropic) are enforced the same way: at most one call is forwarded per turn, and the rest are dropped rather than reordered, because the model emits them in the order it wants them run and the next round can ask for the following one. Leaving the field out allows parallel calls, which is the default in both protocols.
- Every tool call id is a fresh `call_<uuid>`. The backend's own ids repeat across turns, which clients reject as duplicates.
- A tool result whose `tool_call_id` (OpenAI), `tool_use_id` (Anthropic) or `call_id` (Responses) is missing, or names a call the same request never declared, is rejected with HTTP 400. A request declaring no tool calls at all skips the id check, so a client that trimmed its history is not blocked.

### How difficult answers are handled

- When the backend answers a tool request with prose denying the tools exist, claiming to have run the work in its own sandbox, or stating it cannot reach the caller's machine, the proxy re-asks once with an explicit instruction. These phrasings are recognized in English, Chinese and Turkish. An ordinary text answer passes through untouched.
- When Copilot runs its own server-side tools, such as web search or the code interpreter, and returns plain text instead of a simulated JSON payload, the response is returned as a normal completion with `finish_reason: "stop"`.
- When M365 raises a tool call for one of its own built-ins (`search`, `code_interpreter`, `trigger_plugin`, `invoke_action`), that call is dropped and the turn ends on `stop`. This holds even when the request declares no tools: the client never declared those names and cannot execute them, and the answer already carries the search results inline.
- An unparseable tool-calling envelope is withheld instead of being forwarded as the assistant message. An answer that was nothing but envelope becomes a short notice.
- When M365 refuses the request itself rather than answering it, non-streaming endpoints return `502 upstream_content_blocked`, so the refusal is not mistaken for an answer. A streaming turn has already opened its response, so it is logged instead.
- `tool_result` messages (OpenAI) and `tool_use` / `tool_result` blocks (Anthropic) in history are converted to plain text before being sent to M365, which does not understand tool roles.
- Streaming endpoints buffer the full response before parsing tool calls, because the JSON may span several chunks. While that buffer fills, the stream writes a keepalive frame every ten idle seconds so the connection does not look dead.

### Client-driven tool loops

Agent clients such as Claude Code and Codex drive the tool loop themselves and resend the whole call and result history on every request. The proxy holds no state between those requests, so it rebuilds the evidence of the current turn from the incoming history. A turn starts at the last user message carrying no tool result, which keeps the Anthropic shape, where every result arrives as a user message, from looking like a new turn.

| Variable                 | Default | Description                                                                                        |
|--------------------------|---------|----------------------------------------------------------------------------------------------------|
| `M365_MAX_TOOL_ROUNDS`   | `32`    | Tool rounds one user turn may drive before HTTP 409. Capped at `512`.                              |
| `M365_ENABLE_WEB_SEARCH` | `1`     | Declares the M365 `BingWebSearch` built-in on every turn. `0`, `false`, `off` or `no` withholds it. |

- Exceeding the cap returns `409 tool_round_limit` and reports the round count. HTTP 409 is not a status the Anthropic SDK expects, but an explicit refusal is better than answering forever while the client asks for one more round.
- Completed calls and their results are restated in the prompt as final evidence, so the model answers from a result it already has instead of asking for it again. When the same call has failed the same way more than once, the prompt also asks for a change of approach.
- A tool call repeating a name and arguments whose result is already in the turn is dropped on the third identical attempt. The first repeat passes, because reading a file back after writing it, or re-running the tests after a change, are ordinary. A call demanded through `tool_choice` is always forwarded, and a drop never triggers the corrective re-ask, because re-asking would produce the same call again.
- Each restated result is compacted to a head and a tail around a marker naming the removed size, so a long build log does not grow the prompt on every round.
- A tool call id declared twice, or answered twice, is rejected with HTTP 400: nothing can tell which call a later result belongs to.
- A reply that only announces which tool it means to use, naming a declared tool in a short sentence without a code fence, is re-asked once. If the retry stays an announcement, the answer text is replaced so the client is not left waiting for a call that never comes.
- A `function_call_progress` input item lets a long-running client tool report intermediate status. It reaches the model as context but never answers the pending call and never starts a new turn.
- A grammar-constrained tool (`"type": "custom"`, such as Codex code mode's `exec`) takes a raw body rather than JSON arguments. When the backend emits that body unfenced, either as a lone `{"input": "..."}` object or as bare source, it is claimed as a `custom_tool_call` on `/v1/responses` instead of being forwarded as escaped text.
- A client-declared `web_search` tool is never routed back to the client. M365 runs the search itself through `BingWebSearch` and writes the results into the answer. The declaration stays in the prompt so the model knows the capability exists. When `web_search` is the only declared tool, the request drops out of the simulated tool path entirely and streams as ordinary text.
- When the request declares tools, the turn emits no tool call, and no tool result exists, an answer claiming in the first person to have carried the work out is replaced with a short statement that nothing was verified. The original text is logged at debug level. A third-person statement such as "Go was created at Google", and a long prose answer, are never touched. The replacement also applies to the streaming Chat Completions, Messages and Completions endpoints, which buffer a tool-enabled turn until the parse is done. Only `/v1/responses` streaming publishes content as it decodes it, so there the case is logged instead.

## Built-in coding tools (opt-in)

M365Bridge can run a restricted set of local coding operations on the server. This is **off by default**; `M365_ENABLE_CODE_TOOLS=1` is its main gate. It is available on `/v1/chat/completions`, `/v1/messages` and `/v1/responses`.

Once enabled, tools explicitly included in a request are recognized and executed locally. `M365_AUTO_EXPOSE_TOOLS=1` also adds every built-in tool to requests automatically; leave it at `0` when clients should select tools explicitly. The server sends local results back to the model and continues until the model returns a final answer, emits a caller-defined tool call, or reaches the iteration limit. Because calls and intermediate results must be collected first, a request using built-in tools buffers the complete model response even when `stream: true`, then emits the provider-compatible streaming response.

### Settings

| Variable                        | Default   | Description                                                                                    |
|---------------------------------|-----------|------------------------------------------------------------------------------------------------|
| `M365_ENABLE_CODE_TOOLS`        | `0`       | Main gate. Set to `1` to enable local tool execution.                                          |
| `M365_AUTO_EXPOSE_TOOLS`        | `0`       | Set to `1` to inject all built-in tool schemas when the client does not provide them.          |
| `M365_WORKSPACE_DIR`            | `.`       | Existing directory that confines file and Git operations.                                      |
| `M365_CODE_TOOL_TIMEOUT`        | `30s`     | Timeout for each command or test execution. Accepts Go duration syntax, such as `10s` or `2m`. |
| `M365_CODE_TOOL_MAX_OUTPUT`     | `1048576` | Maximum captured command output in bytes. Longer output is truncated.                          |
| `M365_CODE_TOOL_MAX_READ_BYTES` | `1048576` | Maximum number of bytes returned by a file read.                                               |
| `M365_CODE_TOOL_MAX_ITERATIONS` | `10`      | Maximum model and tool loop iterations per request.                                            |

Set these in `data/.env`. Under Docker, `M365_WORKSPACE_DIR` must name a directory that already exists inside the container. The provided compose file mounts only `./data` at `/app/data`; it exposes no host source workspace.

### The tools

| Tool            | Operation                                                        |
|-----------------|------------------------------------------------------------------|
| `list_files`    | List files and directories under a workspace path.               |
| `read_file`     | Read a file, subject to the configured byte limit.               |
| `write_file`    | Create or replace a file inside the workspace.                   |
| `search_files`  | Search workspace file contents.                                  |
| `git_status`    | Show workspace Git status.                                       |
| `git_diff`      | Show workspace Git changes.                                      |
| `git_log`       | Show recent workspace Git history.                               |
| `shell_command` | Run a shell command with the workspace as its working directory. |
| `apply_patch`   | Apply a unified patch inside the workspace.                      |
| `run_tests`     | Run a test command with the configured timeout and output limit. |

### Before you enable them

Enabling these tools turns the API into a remote code execution and file access surface. **Configure `M365_API_KEYS` or `M365_API_KEY` first. API key authentication is mandatory for every deployment with coding tools enabled.** Do not expose such a deployment directly to the public internet. Use a least-privilege service account, a dedicated workspace, strict filesystem permissions, network isolation and container resource limits.

- **Broken access control**: a missing, leaked or shared API key lets unauthorized callers read, modify or execute within the mounted workspace. Use unique, rotated keys and enforce authorization at a trusted reverse proxy as well.
- **Command injection**: `shell_command` and `run_tests` execute model-selected command strings. Treat prompts, repository content, patches and tool arguments as untrusted input. Isolate the process and never provide production credentials.
- **Path traversal**: the file tools confine resolved paths to `M365_WORKSPACE_DIR`, but an overly broad workspace or an unsafe mount still exposes sensitive files. Mount only the project directory you need, and review symlinks and permissions.
- **Sensitive data exposure**: tool output and file contents are returned to the caller and sent to the M365 backend. Keep secrets, tokens, `.env` files, SSH keys, cloud credentials and customer data outside the workspace.
- **Resource exhaustion**: commands, recursive searches, large files, large output and repeated tool loops consume CPU, memory, disk and process capacity. Keep the timeout, output, read and iteration limits conservative, and enforce container or OS quotas.

## Responses API

`/v1/responses` implements the OpenAI Responses API. It accepts `input` as a string or an array of typed items, plus `instructions`, `max_output_tokens`, `tools`, `reasoning` and `previous_response_id` for conversation continuity.

### Reasoning effort

Codex CLI sends `reasoning: {"effort": ..., "summary": ...}`. The accepted effort values are `none`, `minimal`, `low`, `medium`, `high`, `xhigh` and `max`. Anything else is rejected with HTTP 400 rather than ignored.

M365 exposes no separate effort dial, so effort steers the only lever that exists: `medium` and above routes the request to the model's reasoning variant when the registry has one, for example `gpt5.5` to `gpt5.5-reasoning`. A model without a variant, or a key that already names one, is left unchanged. `summary` is accepted and not acted on.

### Custom tools

A tool declared with `"type": "custom"` takes free-form text rather than JSON arguments. Its calls come back as `custom_tool_call` items with the text under `input`, and the matching `custom_tool_call` and `custom_tool_call_output` history items are read back on the next turn.

### Example

```bash
curl http://127.0.0.1:8000/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "model": "gpt5.5",
    "input": "What is 2+2?",
    "session_id": "my-session"
  }'
```

```json
{
  "id": "resp_...",
  "object": "response",
  "created_at": 1234567890,
  "status": "completed",
  "model": "gpt-5.5",
  "output": [{
    "id": "msg_...",
    "type": "message",
    "status": "completed",
    "role": "assistant",
    "content": [{"type": "output_text", "text": "2+2 equals 4.", "annotations": []}]
  }],
  "output_text": "2+2 equals 4.",
  "usage": {"input_tokens": 5, "output_tokens": 8, "total_tokens": 13}
}
```

With instructions and typed input items:

```bash
curl http://127.0.0.1:8000/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "model": "gpt5.5-reasoning",
    "instructions": "You are a concise assistant.",
    "input": [{"role": "user", "content": [{"type": "input_text", "text": "Explain recursion"}]}],
    "stream": true
  }'
```

### Streaming events

| Event                                    | Description                                                  |
|------------------------------------------|--------------------------------------------------------------|
| `response.created`                       | Response object created, status `in_progress`                |
| `response.in_progress`                   | Response is being generated                                  |
| `response.output_item.added`             | New output item added: message, reasoning or function call   |
| `response.content_part.added`            | Content part added to a message item                         |
| `response.output_text.delta`             | Text delta                                                   |
| `response.output_text.done`              | Text complete                                                |
| `response.content_part.done`             | Content part complete                                        |
| `response.output_item.done`              | Output item complete                                         |
| `response.reasoning_summary_part.added`  | Reasoning part opened                                        |
| `response.reasoning_summary_text.delta`  | Reasoning delta                                              |
| `response.reasoning_summary_text.done`   | Reasoning complete                                           |
| `response.reasoning_summary_part.done`   | Reasoning part closed                                        |
| `response.function_call_arguments.delta` | Tool call arguments delta                                    |
| `response.function_call_arguments.done`  | Tool call arguments complete                                 |
| `response.completed`                     | Full response object, status `completed`                     |
| `response.failed`                        | An error occurred, status `failed`                           |

### Codex compatibility

Codex CLI opens a provider with two probes before it sends any chat request.

- `GET /v1/health` answers `{"status": "ok"}` without an API key and without touching the upstream. A 404 there makes Codex mark the whole provider unreachable.
- A `POST /v1/responses` whose input carries no text, image, tool call or tool result is answered locally with an empty but well-formed Response, streaming or not. Sending that empty turn upstream cost about twelve seconds and one message of the conversation quota. A request carrying `instructions` is a real turn and still reaches M365.

Every streaming endpoint writes a keepalive frame after ten idle seconds, because a tool-enabled turn buffers its text until the tool-call parse completes. The OpenAI-shaped routes send an SSE comment, which no client parses as data; `/v1/messages` and `/v1/complete` send the Anthropic `ping` event.

`/v1/chat/completions` and `/v1/completions` also write that comment the moment the stream opens, before the upstream turn starts. Every other streaming route already emits a frame first (`message_start`, `ping` or `response.created`), so a client never has to tell a slow provider from a dead one.

Two more rules protect a stream whose client went away. Each frame arms a thirty-second write deadline, so a reader that stopped consuming cannot hold the handler and its upstream WebSocket open. A failed keepalive write, or a canceled request context, ends the turn and releases the upstream connection instead of writing into a closed socket.

## Responses Compact API

`/v1/responses/compact` implements the OpenAI Responses Compact API for Codex remote compaction. It accepts the same request body as `/v1/responses` and returns a response containing exactly one `compaction` output item.

1. The conversation history is flattened into a single user message carrying a compaction prompt.
2. That message goes to Copilot, which produces a concise summary.
3. The summary is returned wrapped in a `compaction` output item under `encrypted_content`.

```bash
curl http://127.0.0.1:8000/v1/responses/compact \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-api-key" \
  -d '{
    "model": "gpt5.5-reasoning",
    "input": [
      {"role": "user", "content": "Fix the auth bug in sso.go"},
      {"role": "assistant", "content": "I added the missing sso_reload parameter."},
      {"role": "user", "content": "Now add logging to the refresh path"}
    ]
  }'
```

```json
{
  "id": "resp_...",
  "object": "response",
  "status": "completed",
  "output": [{
    "id": "cmp_...",
    "type": "compaction",
    "encrypted_content": "The conversation focused on fixing an SSO auth bug..."
  }]
}
```

Streaming mode emits the same event sequence as `/v1/responses`, but the output item carries `type: "compaction"` instead of `type: "message"`.

Custom `instructions` in the request body override the default compaction prompt. Use a new session ID rather than reusing an existing conversation, which gives better results.

## Image input

The proxy accepts images in both protocols:

- **OpenAI**: `{"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}`
- **Responses**: `{"type": "input_image", "image_url": "data:image/png;base64,..."}`, where the url is a bare string
- **Anthropic**: `{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "..."}}`

An `image_url` block accepts the bare string form as well, because clients send it under both block names. A `file_id` reference is not supported; this gateway serves no Files API to resolve one against.

Images are uploaded to `POST https://substrate.office.com/m365Copilot/UploadFile` and attached to the WebSocket message as `messageAnnotations`. PNG, JPEG, GIF and WebP are supported.

`input_file`, `file`, `input_audio` and `audio` blocks are dropped with a DEBUG log entry, because the M365 backend accepts image attachments only.

### Remote image URLs

An OpenAI `image_url` block may carry a remote `https://` address instead of a data URL. The proxy downloads it before uploading.

No credential is sent on that download, so any public https host is accepted. The request is still checked, to keep the proxy from being used to reach addresses inside its own network: plain http, loopback, private, link-local, multicast, carrier-grade NAT and cloud metadata targets are rejected, and the host is re-checked after DNS resolution. A response larger than 20 MB, one whose content type is not an image, or one that fails outright drops that single image rather than the whole request. At most 16 remote images are fetched per turn.

Anthropic `image` blocks carry base64 data directly and are unaffected.

## Image generation

An image can also come out of an ordinary chat turn. Asking for one in `/v1/chat/completions`, `/v1/messages`, `/v1/completions` or `/v1/responses` puts a markdown image link in the answer, and the image stays part of the conversation, so the next turn can ask for a change to it.

The address M365 puts in that link cannot be fetched by whoever reads the answer: the download needs the designer access token in `Authorization` plus the `fileToken` as its own header, and an `<img>` element sends neither. So the proxy replaces the address with a route of its own before the answer goes out, and downloads the image itself when that route is called:

```
![image](/v1/images/2f1c8b7e-...)
```

The path is root-relative, so a client resolves it against the base URL it already uses, and it sits behind the API key like every other `/v1` route. The reference is minted by the proxy and lives in memory: a restart drops it, and it expires on its own after twelve hours, which is also roughly how long the address behind it stays valid. A reference the proxy no longer holds answers `404 image_not_found`, and the browser interface shows a short note in place of the image. An address the [host allowlist](#host-allowlist) refuses is removed from the answer rather than passed on, for the reason a generated URL is never handed to a client directly.

Generating a picture takes about a minute, and M365 sends no answer text while it works. It does announce the start, so a streaming response carries an SSE comment as soon as that announcement arrives:

```
: notice image_generating
```

A comment enters no field contract, so every OpenAI and Anthropic client ignores it exactly as it ignores the keepalive comment. The browser interface reads it and shows a line saying the image is being generated; the first content of the answer replaces that line, and because the notice was never answer text, nothing has to be taken back and nothing reaches the transcript. The read timeout is raised to three minutes for the rest of a turn that started generating an image.

The separate Images API endpoints stay one-shot and return the image data itself:

- `POST /v1/images/generations`, JSON body, generates from a text prompt
- `POST /v1/images/edits`, multipart form, edits existing images; up to 16 through repeated `image` fields

| Parameter         | Type   | Default     | Description                                                                       |
|-------------------|--------|-------------|-----------------------------------------------------------------------------------|
| `prompt`          | string | required    | The text prompt                                                                   |
| `n`               | int    | 1           | Number of images; M365 generates one per request                                  |
| `size`            | string | `1024x1024` | Size hint appended to the prompt as natural language; `1024x1024` is skipped      |
| `quality`         | string | `standard`  | Quality hint appended to the prompt; `standard` is skipped                        |
| `style`           | string | `natural`   | Style hint appended to the prompt; `natural` is skipped                           |
| `response_format` | string | `url`       | `url` returns a data URL, `b64_json` returns base64 in a separate field           |
| `session_id`      | string | optional    | Session ID for conversation continuity                                            |
| `user`            | string | optional    | Read as the session ID when `session_id` is absent                                |

With `response_format=url`, the proxy downloads the image server-side and returns a `data:image/png;base64,...` URL, falling back to the raw `designerapp.officeapps.live.com` URL if the download fails. With `b64_json`, it downloads using a broker token and returns base64-encoded PNG data.

```python
from openai import OpenAI
import base64

client = OpenAI(base_url="http://localhost:8230/v1", api_key="your-api-key")

resp = client.images.generate(
    model="gpt5.5-reasoning",
    prompt="a serene mountain landscape at sunset",
    n=1,
    response_format="b64_json",
)

with open("output.png", "wb") as f:
    f.write(base64.b64decode(resp.data[0].b64_json))
```

### Host allowlist

Generated-image URLs are read out of the model's own markdown output, which is untrusted, and the download sends the designerapp access token. The proxy therefore contacts only allowlisted hosts, requires `https`, and rejects hosts resolving to loopback, private, link-local, carrier-grade NAT or cloud metadata addresses. A URL failing these checks is dropped rather than returned.

| Variable                    | Default                | Description                                                                                                   |
|-----------------------------|------------------------|---------------------------------------------------------------------------------------------------------------|
| `M365_IMAGE_HOST_ALLOWLIST` | `.officeapps.live.com` | Comma-separated hosts that may serve generated images. An entry starting with a dot matches that domain and its subdomains. |

### Download token flow

To download a generated image the proxy acquires a JWE access token for `designerappservice.officeapps.live.com` through the MSAL.js broker token flow:

1. The broker app (`c0ab8ce9`) acquires a token on behalf of the M365 web app (`4765445b`) with the `designerappservice.officeapps.live.com/.default` scope.
2. A broker-compatible refresh token is stored encrypted at `data/tokens/rt_broker.txt` and rotated by the background token refresher.
3. If no broker refresh token exists, one is acquired through the SSO cookie broker authorize flow, using PKCE and the `brk-multihub://outlook.office.com` redirect URI.
4. The JWE token and a `fileToken` header download the image from `designerapp.officeapps.live.com`.

## Security

- Refresh tokens are encrypted with AES-256-GCM before storage.
- Login cookies and M365 web cookies are encrypted the same way, at `data/tokens/sso_cookies.json` and `data/tokens/m365_cookies.json`.
- A legacy plaintext M365 cookie store is encrypted automatically on first use.
- The encryption key lives at `data/tokens/encryption.key`. Losing it makes every stored credential unreadable and requires a fresh setup.
- Access tokens are cached at `data/tokens/token_cache.json`, valid about an hour with a 60-second buffer.
- In `serve` mode a background refresher renews the access token every 30 minutes.
- Login cookies re-authenticate silently when the 24-hour refresh token expires.
- Every state file is replaced in one step rather than truncated and rewritten, so an interrupted write cannot leave a half-written credential store.
- No credential is stored in code or in the repository, and `data/` is gitignored.
- API key authentication protects every `/v1/*` route and `/mcp` when configured. The key is read from `Authorization: Bearer <key>` or `x-api-key: <key>`; when a client offers both, either one being valid is enough.
- Every secret is compared in constant time, so a wrong guess cannot be measured byte by byte.

## Project structure

```
cmd/cli/main.go            # Single entry point, subcommand router
pkg/
  atomicfile/              # Write-and-rename, so a crash cannot leave a half-written credential
  auth/auth.go             # TokenManager, token refresh, encrypted refresh token storage
  auth/sso.go              # Cookie re-authentication and the designer broker token flow
  client/client.go         # M365Client, one SignalR WebSocket per request
  client/conversations.go  # ConversationClient: list, rename and delete web conversations
  client/history.go        # Reads the turns of a conversation from its rendered page
  client/citations.go      # Citation resolution in streamed answer text
  client/errors.go         # UpstreamError, carrying the status of a failed dial or upload
  codingtools/             # Built-in local tools, gated by M365_ENABLE_CODE_TOOLS
  crypto/crypto.go         # AES-256-GCM encryption
  logging/                 # Application logging
  models/models.go         # Version, ModelRegistry, Config, LoadConfig, FindModel
  payload/payload.go       # Request payload builders, URL builder, locale and timezone helpers
  servers/
    api.go                 # HTTP adaptation: every endpoint, token counting, session isolation
    auth.go                # The browser interface gate and its two public routes
    cli.go                 # CLI server, interactive mode
    errors.go              # The one error shape every route reports
    mcp.go                 # JSON-RPC 2.0 Model Context Protocol server
    sessions.go            # The session-to-conversation mapping routes
    stopsequence.go        # Stop sequence cutting, including the streaming writer
    transcripts.go         # The only place message content reaches disk
    webui.go               # Serves the embedded browser interface
  setup/wizard.go          # Setup wizard: browser snippet, token verification, data/.env
  textcut/                 # Rune-boundary-safe cutting
  toolcalling/             # Simulated tool calling, its parsers and detectors
  webui/embed.go           # The built interface, compiled into the binary
web/                       # Vite project for the interface; make ui builds it into pkg/webui/dist
docs/                      # Screenshots used by the READMEs
data/                      # Runtime data, gitignored: tokens/, setup.json, cache/, transcripts/
```

## Dependencies

Three direct dependencies, and one they pull in.

| Dependency                      | Purpose                                                                    |
|---------------------------------|----------------------------------------------------------------------------|
| `github.com/google/uuid`        | UUID generation for SIDs and request IDs                                   |
| `github.com/gorilla/websocket`  | WebSocket client for SignalR                                               |
| `github.com/pkoukk/tiktoken-go` | BPE token counting for usage reporting and `max_tokens` enforcement        |
| `github.com/dlclark/regexp2`    | Indirect; the regex engine tiktoken-go splits text with                    |

## Not implemented

- File upload
- Code interpreter

## Disclaimer

This project is for learning and research purposes only. It explores publicly observable network communication protocols.

By using it, you confirm that:

- You hold legitimate Microsoft 365 Copilot authorization
- You use it for personal learning and research rather than commercially
- You understand the risks of using an unofficial interface
- You accept all consequences

This project does not crack encryption, bypass authentication, access or leak anyone else's data, or interfere with Microsoft services. It has no association with Microsoft Corporation.

## License

Research Only
