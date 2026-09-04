---
type: note
title: Agent Config Discovery (Phase 03)
created: 2026-09-03
tags:
  - discovery
  - agents
  - credential-free
related:
  - '[[Agent-Setup-Overview]]'
---

# Agent config discovery — this machine, 2026-09-03

Findings from inspecting the real config surface of each target agent before
writing the Phase 03 guides. All four binaries are installed. Secret values were
redacted at read time and are **not** recorded here.

## Summary

| Agent | Binary | Version | Config file | Present? |
|---|---|---|---|---|
| Claude Code | `~/.local/bin/claude` | 2.1.257 | `~/.claude/settings.json` | yes (CCR-managed) |
| Codex | `/usr/bin/codex` | 0.149.0 | `~/.codex/config.toml` | yes (CCR-managed) |
| OpenCode | `/usr/bin/opencode` | 1.18.21 | `~/.config/opencode/opencode.json` | yes |
| Pi | `~/.local/bin/pi` | 0.80.6 | `~/.pi/agent/{settings,providers}.json` + profile dirs | yes |

## Claude Code

`~/.claude/settings.json` → `env` block (the part that matters):

- `ANTHROPIC_BASE_URL` = `http://127.0.0.1:3456` (**claude-code-router**, not
  Anthropic direct). Also `ANTHROPIC_API_BASE_URL` and `CLAUDE_AGENT_API_BASE_URL`
  set to the same CCR address.
- **No** `ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_API_KEY` in `env` — auth is
  CCR's business (`ANTHROPIC_IDENTITY_TOKEN_FILE` → ccr-local token file).
- Model overrides all set to `zai/glm-5.3`: `ANTHROPIC_MODEL`,
  `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL` (+ CCR/CODEXL variants).

Consequences for the guide:

1. `settings.json` `env` **overrides shell env** (confirmed by the wrapper's
   `claude` case comment) — a shell-only `ANTHROPIC_BASE_URL` one-off will NOT
   take effect while settings.json pins it. Guide must say so.
2. `zai/glm-5.3` matches **no** rule in the default `MODEL_MAP`
   (`claude-opus-*=glm-5.3,...`) and passes through verbatim; z.ai expects bare
   `glm-5.3`. Guide must offer either a `zai/*=glm-5.3` MODEL_MAP rule or
   bare/unset model keys.

## Codex

`~/.codex/config.toml` — top is a CCR-managed profile:
`model_provider = "claude-code-router"`, `model = "zai/glm-5.3"`. At the bottom,
a CCR-managed provider table:

```toml
[model_providers.claude-code-router]
name = "Claude Code Router"
base_url = "http://127.0.0.1:3456/v1"
experimental_bearer_token = "<redacted>"
wire_api = "responses"
```

- **`wire_api = "responses"` is derived from this machine's own working config**
  (codex-cli 0.149.0) — the guide recommends `"responses"` and documents
  `"chat"` as the fallback variant.
- Existing provider uses `base_url` **with** `/v1` suffix → recipe uses
  `http://localhost:8765/v1`; Codex then emits `/v1/responses` (or
  `/v1/chat/completions` under `wire_api = "chat"`).
- `codex exec` subcommand confirmed (`codex exec --help` works) — the verify
  one-liner is real.
- Many `.ccr-backup-*` siblings exist; guide should tell users to leave the
  CCR BEGIN/END blocks alone and just flip `model_provider`.

## OpenCode

`~/.config/opencode/opencode.json` — three providers, all
`@ai-sdk/openai-compatible`: `zai` (`baseURL https://api.z.ai/api/coding/paas/v4`,
real key), `ollama` (`http://10.0.40.107:11434/v1`, no key), `vllm`
(`http://10.0.40.214:8000/v1`, key). Default model is `vllm/hermes3:8b-ctx96k`
via top-level `model` and per-agent `agent.<id>.model` — **no `defaultProvider`
key is used on this install**.

- Existing entries keep the version path in `baseURL` (`…/v1`, z.ai's `…/v4`)
  and the SDK appends `/chat/completions` → proxy entry should be
  `http://localhost:8765/v1` (keep `/v1`).
- `apiKey` is optional for openai-compatible providers (ollama entry has none) —
  guide still sets `"apiKey": "llmproxy-dummy"` per the dummy-key convention.
- `opencode run` subcommand confirmed.

## Pi

Pi is a **Node CLI** here (`~/.local/bin/pi` → `@earendil-works/pi-coding-agent`),
not Docker-wrapped, but the config retains Docker heritage.

- `~/.pi/agent/settings.json`: `defaultProvider: "opencode"`,
  `defaultModel: "hy3-free"`, `enabledModels: ["glm-*","ollama/*","opencode/*"]`.
  Quirk: `"opencode"` is **not** an id in `providers.json`, so the wrapper's
  `pi_active_profile()` falls back to `zai-claude` on this machine.
- `~/.pi/agent/providers.json`: provider ids `blank`, `zai` (profile
  `zai-claude`), `anthropic`, `ollama`, and **`llmproxy` already exists** —
  profile `llmproxy`, `base_url_default http://host.docker.internal:8765`,
  `test_path /__inspect__`, `needs_key: false`.
- Profile dir `~/.pi/agent/llmproxy/` already exists (May 15) with
  `settings.json` `env`: `ANTHROPIC_BASE_URL http://host.docker.internal:8765`,
  `ANTHROPIC_API_KEY <set, not the literal llmproxy-dummy>`,
  `OPENAI_BASE_URL http://host.docker.internal:8765/v1`, `OPENAI_API_KEY <set>`.
  Provenance unknown — Phase 04 live test should refresh these to
  `http://localhost:8765` + `llmproxy-dummy` (Pi runs on host now).
- Reference profile `~/.pi/agent/zai-claude/settings.json` shows the canonical
  profile shape: `env.ANTHROPIC_AUTH_TOKEN`, `env.ANTHROPIC_BASE_URL`,
  model overrides, plus sibling `pi-settings.json` (`defaultProvider`,
  `defaultModel`).
- CLI flags verified via `pi --help`: `--provider <name>`, `--model provider/id`
  → `pi --provider llmproxy --model glm-5.3` works without touching defaults.
- Caveat for containerized Pi: `host.docker.internal` arrives on the host's
  gateway interface, but `llmproxy serve` binds `127.0.0.1` — a Docker-hosted
  Pi cannot reach it without widening `LISTEN_ADDR`. Flag as Phase 04 item.

## Proxy side (already in place from Phase 01/02)

- `.env` (live, gitignored): `ANTHROPIC_API_KEY` (real z.ai key),
  `ANTHROPIC_BACKEND` (30 chars = `https://api.z.ai/api/anthropic`),
  `MODEL_MAP` (full claude-* → glm-5.3 default set), `LLMPROXY_INSPECT_TOKEN`.
  **No `OPENAI_API_KEY` / `OPENAI_BACKEND` set** → OpenAI-style agents
  (Codex, OpenCode) currently route to `https://api.openai.com` with no key
  (401). Guides must call out the two `.env` lines to add.
- `detectAPIType` (main.go:1305): `chat/completions` or `/responses` in path →
  `openai`; `/messages` → `anthropic`; `generateContent` → `gemini`.
- `injectAPIKey` (main.go:201): for self-referential requests (agent pointed
  BASE_URL at the proxy), strips `Authorization`/`x-api-key`/`x-goog-api-key`
  and injects the real `.env` key — anthropic gets both `x-api-key` and Bearer.
- `resolveTarget` (main.go:143): self-referential host → `cfg.Backends[apiType]`
  (i.e. `*_BACKEND` from `.env`, else provider default).
- `MODEL_MAP` remap runs on the body `model` field before forwarding
  (first match wins; unmatched passes through).
