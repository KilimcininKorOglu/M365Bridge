# M365Bridge

[![CI](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/ci.yml)
[![Release](https://github.com/KilimcininKorOglu/M365Bridge/actions/workflows/release.yml/badge.svg)](https://github.com/KilimcininKorOglu/M365Bridge/releases)
[![Version](https://img.shields.io/github/v/release/KilimcininKorOglu/M365Bridge)](https://github.com/KilimcininKorOglu/M365Bridge/releases)
[![Docker](https://img.shields.io/badge/docker-ghcr.io-blue)](https://github.com/KilimcininKorOglu/M365Bridge/pkgs/container/m365bridge)
[![Go](https://img.shields.io/badge/Go-1.26.6%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![OpenAI Compatible](https://img.shields.io/badge/API-OpenAI%20Compatible-412991)](#quick-usage)
[![Anthropic Compatible](https://img.shields.io/badge/API-Anthropic%20Compatible-D97757?logo=anthropic&logoColor=white)](#quick-usage)

**English** | **[Türkçe](README.tr.md)**

M365Bridge turns your Microsoft 365 Copilot subscription into an **OpenAI-compatible** and **Anthropic-compatible** HTTP API. Connect any AI client—**Claude Code**, **Cursor**, **Codex**, **Cline**, standard Python/Node SDKs, or custom tools—directly to Copilot.

![M365Bridge Web UI](docs/webui-en.png)

```text
Your AI Client (Claude Code, Cursor, SDK)
  -> M365Bridge (:8230)
  -> substrate.office.com (SignalR WebSocket)
  -> M365 Copilot (GPT-5, GPT-6, Claude)
```

---

## Key Features

- **Zero-Browser OAuth (Device Code Flow)**: Instant login from any device via `m365-bridge login`. No headless browser, no sidecar containers, and no manual cookie extraction required.
- **90-Day Rolling Refresh Token**: Authenticates as a native public client (RFC 8628). Does not expire after 24 hours; tokens rotate automatically.
- **OpenAI & Anthropic Wire Compatibility**: Native streaming (SSE), Responses API, Responses Compact API, and message count endpoints.
- **Simulated Tool Calling**: Function calling for user-defined tools, with strict schema validation and duplicate call pruning.
- **Multimodal & Image Generation**: Image input across both protocols, plus image creation with Microsoft Designer.
- **Reasoning Content**: Exposes internal chain-of-thought via `reasoning_content` (OpenAI) and `thinking` blocks (Anthropic).
- **Embedded Web UI**: Self-contained web interface compiled into the binary with multi-turn chat, markdown rendering, and Turkish/English support.
- **Model Context Protocol (MCP)**: JSON-RPC 2.0 server at `/mcp` exposing Copilot as leaf tools.

---

## Quick Start & Installation

### Option A: Docker (Recommended)

1. Create a `docker-compose.yml` file:
   ```yaml
   services:
     m365bridge:
       image: ghcr.io/kilimcininkoroglu/m365bridge:latest
       container_name: m365bridge
       environment:
         - TZ=Europe/Istanbul
       ports:
         - "8230:8000"
       volumes:
         - ./data:/app/data
       restart: unless-stopped
   ```
2. Start the container:
   ```bash
   docker compose up -d
   ```
   *(Or using `docker run`: `docker run -d --name m365bridge -p 8230:8000 -v "$(pwd)/data:/app/data" --restart unless-stopped ghcr.io/kilimcininkoroglu/m365bridge:latest`)*

3. Sign in to your Microsoft account:
   ```bash
   docker exec -it m365bridge ./bin/m365-bridge login
   ```

### Option B: Pre-built Binary

Download the compiled binary for your operating system from [Releases](https://github.com/KilimcininKorOglu/M365Bridge/releases):

| OS / Architecture   | Binary Name                     |
|---------------------|---------------------------------|
| Linux x86_64        | `m365-bridge-linux-amd64`       |
| Linux ARM64         | `m365-bridge-linux-arm64`       |
| macOS Apple Silicon | `m365-bridge-darwin-arm64`      |
| macOS Intel         | `m365-bridge-darwin-amd64`      |
| Windows x86_64      | `m365-bridge-windows-amd64.exe` |

```bash
mkdir m365bridge && cd m365bridge
curl -L -o m365-bridge https://github.com/KilimcininKorOglu/M365Bridge/releases/latest/download/m365-bridge-linux-amd64
chmod +x m365-bridge
mkdir data

# Sign in
./m365-bridge login

# Start server
./m365-bridge serve --port 8000
```

### Option C: Build from Source

```bash
git clone https://github.com/KilimcininKorOglu/M365Bridge.git
cd M365Bridge
go build -o bin/m365-bridge ./cmd/cli
mkdir -p data
./bin/m365-bridge login
./bin/m365-bridge serve --port 8000
```

---

## Connecting Your Account (Device Login)

Connecting your Microsoft 365 Copilot account takes less than 30 seconds:

```bash
# Docker:
docker exec -it m365bridge ./bin/m365-bridge login

# Standalone binary:
./m365-bridge login
```

1. The terminal displays a verification URL and an 8-character code:
   ```text
   To sign in on any device (phone, laptop, tablet):
     1. Open URL  : https://login.microsoft.com/device
     2. Enter Code: ABCD-1234
   ```
2. Open the URL in any browser on any device, enter the code, and authenticate with your M365 account.
3. The CLI detects authorization, acquires a 90-day rolling refresh token, encrypts it with AES-256-GCM into `data/tokens/`, and writes `data/.env`.

> [!TIP]
> If your tenant enforces strict conditional access policies that block Device Code Flow, use `./m365-bridge login -pkce` to authenticate via interactive browser redirect.

---

## Supported Models

M365Bridge routes model identifiers to specific Copilot backend tones:

| Model Key          | Tone                | OpenAI Equivalent ID | Thinking | Backend |
|--------------------|---------------------|----------------------|----------|---------|
| `auto`             | Magic               | `gpt-4-auto`         | No       | GPT-5   |
| `quick`            | Chat                | `gpt-4-quick`        | No       | GPT-5   |
| `gpt5.5`           | Gpt_5_5_Chat        | `gpt5.5`             | No       | GPT-5   |
| `gpt5.5-reasoning` | Gpt_5_5_Reasoning   | `gpt5.5-reasoning`   | Yes      | GPT-5   |
| `gpt5.6`           | Gpt_5_6_Chat        | `gpt5.6`             | No       | GPT-5   |
| `gpt5.6-reasoning` | Gpt_5_6_Reasoning   | `gpt5.6-reasoning`   | Yes      | GPT-5   |
| `gpt6sol`          | Gpt_6_Sol_Reasoning | `gpt-6-sol`          | No       | GPT-6   |
| `gpt6-astra`       | GPT_6_Astra         | `gpt-6-astra`        | No       | GPT-6   |
| `claude-sonnet`    | Claude_Sonnet       | `claude-sonnet-4.6`  | No       | Claude  |
| `claude-opus`      | Claude_Opus         | `claude-opus-4.6`    | Yes      | Claude  |

### Model Recommendations
| Task                                              | Recommended Model                |
|---------------------------------------------------|----------------------------------|
| General questions and conversational chat         | `auto` or `gpt5.5`               |
| Coding assistants, tool calling, multi-step logic | `gpt5.5-reasoning`               |
| Claude workflows (Claude Code, Anthropic SDK)     | `claude-sonnet` or `claude-opus` |
| Latest reasoning backend                          | `gpt6sol`                        |

---

## Quick Usage

### 1. Command Line Interface (CLI)
```bash
# Ask a quick question
./m365-bridge "Explain the difference between mutex and semaphore"

# Interactive multi-turn chat session
./m365-bridge -i --model gpt5.5-reasoning
```

### 2. cURL (HTTP API)
```bash
curl http://localhost:8230/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt5.5-reasoning",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

### 3. Python (OpenAI SDK)
```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8230/v1", api_key="not-needed")

response = client.chat.completions.create(
    model="gpt5.5-reasoning",
    messages=[{"role": "user", "content": "How do goroutines work?"}],
)
print(response.choices[0].message.content)
```

### 4. Python (Anthropic SDK)
```python
from anthropic import Anthropic

client = Anthropic(base_url="http://localhost:8230", api_key="not-needed")

message = client.messages.create(
    model="claude-opus",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Write a quicksort in Go"}],
)
print(message.content[0].text)
```

---

## Detailed Documentation

Explore in-depth technical documentation in the `docs/` folder:

- **[Configuration Guide](docs/configuration.md)**: Environment variables, Web UI password protection, and API key management.
- **[API Reference](docs/api.md)**: Complete endpoint catalog, OpenAI/Anthropic wire specifications, quota tracking, error contracts, and context compaction.
- **[Tool Calling & Coding Tools](docs/tool-calling.md)**: Function calling emulation, agent loop management (Claude Code/Codex), and server-side local coding tools.
- **[Multimodal & Image Generation](docs/multimodal.md)**: Vision image input, SSRF safeguards, and AI image generation via Microsoft Designer.
- **[Sessions & Conversations](docs/sessions.md)**: Context persistence, model suffix routing (`model:session`), and cloud conversation management.
- **[Architecture & Security](docs/architecture.md)**: SignalR transport internals, AES-256-GCM encryption, atomic file writes, and project layout.

---

## Disclaimer & License

This project is for educational and research purposes only. It interacts with publicly observable network interfaces and does not bypass security mechanisms or compromise private accounts. It has no affiliation with Microsoft Corporation.

Licensed under **Research Only**. See repository license terms.
