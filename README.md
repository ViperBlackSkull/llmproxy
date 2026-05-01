# llmproxy

A transparent reverse proxy that intercepts and logs LLM API requests. Works like `proxychains` — wrap any command and it captures all prompt traffic in markdown format.

Supports both **Anthropic** and **OpenAI** compatible APIs, including streaming responses.

## Features

- Transparent proxying — your tool doesn't know it's there
- Captures system prompts, messages, and responses
- Streaming support (SSE) with real-time capture
- Built-in web dashboard for inspecting logs
- Markdown-formatted logs, raw JSON preserved
- Works with Claude Code, Codex, or any tool that calls LLM APIs

## Install

Requirements: [Go](https://go.dev/) 1.22+, `jq`, `lsof`

```bash
git clone https://github.com/ViperBlackSkull/llmproxy.git
cd llmproxy
make build
```

## Usage

```bash
# Run any command through the proxy
./llmproxy claude -p "hello"
./llmproxy codex "fix the bug"

# List captured logs
./llmproxy logs

# Open the inspect dashboard
./llmproxy inspect
```

The proxy starts automatically, intercepts all API traffic, and shuts down when your command finishes.

### Inspect Dashboard

```bash
./llmproxy inspect          # Start on port 8777
./llmproxy inspect 9090     # Custom port
```

Opens a web UI at `http://localhost:8777/__inspect__` where you can browse captured requests, view system prompts, and inspect raw JSON.

### Standalone Mode

Run the proxy daemon directly (no wrapper):

```bash
go run . # listens on :8765, forwards to api.anthropic.com
```

Then point any tool at `http://localhost:8765`.

## Configuration

All config via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `BACKEND_URL` | `ANTHROPIC_BASE_URL` or `https://api.anthropic.com` | Upstream API to forward to |
| `LISTEN_ADDR` | `:8765` | Proxy listen address |
| `LOG_DIR` | `./logs` | Directory for captured logs |

## How It Works

```
Tool (claude, codex, etc.)
  │
  ├── settings.json patched to point at local proxy
  │
  ▼
llmproxyd (:8765)
  ├── Logs request (system prompt, messages)
  ├── Forwards to real API
  ├── Captures response (including SSE streams)
  └── Logs response
  │
  ▼
api.anthropic.com / api.openai.com
```

1. `llmproxy` wrapper starts the daemon, patches your tool's config to route through it
2. `llmproxyd` intercepts requests, logs them as markdown, forwards to the real API
3. On exit, settings are restored and a summary is printed

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
