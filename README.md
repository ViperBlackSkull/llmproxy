# llmproxy

A transparent reverse proxy that intercepts and logs LLM API requests from any coding agent. Inspired by [proxychains](https://github.com/haad/proxychains) — just wrap your command and it captures all prompt traffic, including hidden system prompts.

Supports **Anthropic**, **OpenAI**, and **Gemini** APIs, including streaming responses and WebSocket-based Realtime API.

## System Prompt Extraction

llmproxy can extract and save the **full system prompts** that coding agents send to LLMs — prompts that are normally hidden from the user. Every request is inspected and the system prompt is saved to a per-agent snapshot file:

```
logs/
├── claude/
│   ├── prompt.md          ← current Claude Code system prompt
│   └── prompt-20260505-*.md
├── codex/
│   ├── prompt.md          ← current Codex system prompt
│   └── ...
```

Works across all supported agents and API formats:
- **Anthropic**: extracts the `system` field (string and content block arrays)
- **OpenAI**: extracts `system` role messages from the `messages` array
- **Codex (WebSocket)**: extracts the `instructions` field from Realtime API events

<p align="center">
  <img src="demo/demo.svg" alt="llmproxy demo" width="854">
</p>

## Supported Agents

| Agent | Command | Auto-detected |
|-------|---------|:---:|
| Claude Code | `llmproxy claude -p "hello"` | Yes |
| OpenAI Codex | `llmproxy codex "fix the bug"` | Yes |
| Aider | `llmproxy aider --model gpt-4o "refactor"` | Yes |
| OpenCode | `llmproxy opencode` | Yes |
| Gemini CLI | `llmproxy gemini -p "explain this"` | Yes |
| Any other | `llmproxy <command> [args...]` | Best-effort |

## Install

Requirements: [Go](https://go.dev/) 1.22+, `jq` (for Claude Code only)

### Option 1 — Prebuilt binary (Linux x86_64)

Download the latest release from the [Releases](https://github.com/ViperBlackSkull/llmproxy/releases) page:

```bash
wget https://github.com/ViperBlackSkull/llmproxy/releases/latest/download/llmproxy_*_linux_amd64.tar.gz
tar -xzf llmproxy_*_linux_amd64.tar.gz
./llmproxy claude -p "hello"
```

Or add the extracted directory to your `PATH` to run `llmproxy` from anywhere.

### Option 2 — Build from source

```bash
git clone https://github.com/ViperBlackSkull/llmproxy.git
cd llmproxy
make build
```

Requires Go 1.22+. The optional MITM libs in `lib/` also require gcc.

## Usage

```bash
# Claude Code
./llmproxy claude -p "hello"

# OpenAI Codex
./llmproxy codex "fix the bug"

# Aider (auto-detects OpenAI or Anthropic based on --model)
./llmproxy aider --model gpt-4o "refactor auth"
./llmproxy aider --model claude-sonnet-4-20250514 "fix tests"

# OpenCode
./llmproxy opencode

# Gemini CLI
./llmproxy gemini -p "explain this code"

# Credential-free proxy (see below)
./llmproxy serve

# Any command — sets all known env vars
./llmproxy curl -X POST http://localhost:8765/v1/messages ...

# List captured logs
./llmproxy logs

# Open inspect dashboard
./llmproxy inspect
```

The proxy starts automatically, intercepts all API traffic, and shuts down when your command finishes.

### Inspect Dashboard

```bash
./llmproxy inspect          # Start on port 8777
./llmproxy inspect 9090     # Custom port
```

Opens a web UI at `http://localhost:8777/__inspect__` where you can browse captured requests, view system prompts, and inspect raw JSON.

### Credential-Free Mode

```bash
cp .env.example .env   # fill in your real key + backend
./llmproxy serve       # foreground proxy on http://localhost:8765
```

`serve` runs a standalone proxy any agent can point at with **no real credentials**: point the agent's base URL at `http://localhost:8765`, send dummy or no API keys, and it still gets real completions. The daemon loads `.env` at startup, injects your real key upstream (both `x-api-key` and `Authorization: Bearer` for Anthropic-style upstreams — works with z.ai and native Anthropic alike), and remaps model names via `MODEL_MAP`:

```
Agent (base URL = http://localhost:8765, dummy credentials)
  │
  ▼
llmproxyd (:8765)
  ├── Detects API type, logs request (original model in .req.json)
  ├── MODEL_MAP remap (e.g. claude-sonnet-5 -> glm-5.3, mapped_model in .meta.json)
  ├── Strips the dummy auth, injects the real key from .env
  └── Forwards to ANTHROPIC_BACKEND (e.g. https://api.z.ai/api/anthropic)
```

Plain `curl` with zero auth headers gets a real completion:

```bash
curl http://localhost:8765/v1/messages \
  -H 'content-type: application/json' \
  -d '{"model":"claude-sonnet-5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}'
```

Transparent wrapping (the default `llmproxy <agent>` flow) is unchanged: traffic to the agent's real original destination still passes through with the agent's own credentials — injection only applies to requests that target the proxy itself.

### Standalone Mode

Run the proxy daemon directly (no wrapper):

```bash
go run . # listens on :8765, forwards to api.anthropic.com
```

Then point any tool at `http://localhost:8765`.

## Security

llmproxy is a **local development tool** for inspecting your own LLM API traffic.

### Critical Security Considerations

- **Inspect Dashboard**: The `/__inspect__` endpoint exposes every proxied prompt verbatim, including full system prompts and user messages. By default, this endpoint is **disabled** and returns 404. To enable it, you must set `LLMPROXY_INSPECT_TOKEN` — then all inspect requests require bearer token authentication (or `?token=` query param).
- **Network Binding**: The proxy binds to `127.0.0.1:8765` (loopback only) by default. Do not run on shared networks or expose the port externally. Use `LISTEN_ADDR` to override only if you understand the risks.
- **Log Files**: Request logs are written to `./logs/` with restrictive permissions (`0o700` directories, `0o600` files). Secrets in request bodies (API keys, bearer tokens) are redacted as `<REDACTED>`, but prompts may still contain sensitive information.
- **API Keys**: Keys are read from environment variables, never hardcoded. Keep your `.env` files out of version control (they're in `.gitignore`).

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LLMPROXY_INSPECT_TOKEN` | — | Bearer token required for `/__inspect__*` endpoints. If unset, dashboard is disabled (404) |
| `LISTEN_ADDR` | `127.0.0.1:8765` | Proxy listen address (loopback only by default) |

### Authorization

The MITM/TLS interception features (`lib/intercept.c`, `lib/redirect.c`, `examples/grab.py`) are intended for **authorized testing and research** on your own machines. Do not use this tool to intercept traffic you are not authorized to intercept.

## Configuration

All config via environment variables. Copy `.env.example` to `.env` and fill in your keys:

```bash
cp .env.example .env
# Edit .env with your actual API keys
```

| Variable | Default | Description |
|----------|---------|-------------|
| `ANTHROPIC_API_KEY` | — | Your Anthropic API key (injected upstream for self-referential/credential-free traffic) |
| `OPENAI_API_KEY` | — | Your OpenAI API key |
| `GEMINI_API_KEY` | — | Your Google Gemini API key |
| `ANTHROPIC_BACKEND` | `https://api.anthropic.com` | Upstream for Anthropic-style requests (e.g. `https://api.z.ai/api/anthropic`) |
| `OPENAI_BACKEND` | `https://api.openai.com` | Upstream for OpenAI-style requests |
| `GEMINI_BACKEND` | `https://generativelanguage.googleapis.com` | Upstream for Gemini-style requests |
| `MODEL_MAP` | — | Comma-separated `pattern=replacement` model remap rules; trailing `*` = prefix match; first match wins (e.g. `claude-sonnet-*=glm-5.3`) |
| `LLMPROXY_INSPECT_TOKEN` | — | Bearer token for inspect dashboard (required to access `/__inspect__`) |
| `BACKEND_URL` | Auto-detected per agent | Upstream API to forward to |
| `PROXY_PORT` | First available from 8765-8770 | Proxy listen port |
| `LOG_DIR` | `./logs` | Directory for captured logs |
| `LISTEN_ADDR` | `127.0.0.1:8765` | Proxy listen address (standalone mode, loopback only) |

The daemon loads `./.env` at startup (real environment variables win over `.env` values); see `.env.example` for a documented template.

## How It Works

Inspired by [proxychains](https://github.com/haad/proxychains): instead of chaining through SOCKS/HTTP proxies, llmproxy chains your coding agent through a local intercepting proxy that logs all LLM traffic — no agent configuration or plugins required.

```
Agent (claude, codex, aider, etc.)
  │
  ├── Env vars set to point at local proxy
  │   (Claude Code: also patches settings.json)
  │
  ▼
llmproxyd (:8765)
  ├── Detects API type (Anthropic / OpenAI / Gemini)
  ├── Extracts system prompt (saves to prompt.md)
  ├── Logs request (system prompt, messages)
  ├── Forwards to real API
  ├── Captures response (including SSE streams)
  └── Logs response
  │
  ▼
api.anthropic.com / api.openai.com / generativelanguage.googleapis.com
```

1. `llmproxy` wrapper detects which agent you're running
2. Sets the appropriate env vars so the agent routes through the proxy
3. `llmproxyd` intercepts requests, logs them as markdown, forwards to the real API
4. On exit, settings are restored and a summary is printed

## Log Format

Logs are stored as markdown files in `./logs/`:

```markdown
# LLM Request

**Time**: 2026-04-30T14:52:00Z
**API**: anthropic
**Model**: claude-sonnet-4-20250514

## System Prompt

You are a helpful assistant...

## Messages

**user**: Hello!

## Response

Hi there! How can I help?
```

Raw request JSON is also saved as `.req.json` alongside each log.

## License

[MIT](LICENSE)
