---
type: reference
title: OpenCode — Credential-Free Setup
created: 2026-09-03
tags:
  - opencode
  - setup
  - credential-free
related:
  - '[[Agent-Setup-Overview]]'
  - '[[Claude-Code-Setup]]'
  - '[[Codex-Setup]]'
  - '[[Pi-Setup]]'
---

# OpenCode — Credential-Free Setup

Wire OpenCode to llmproxy so its config holds **zero real credentials**: the
provider entry carries a dummy `apiKey`, and the proxy swaps in the real key
(from `.env`) when forwarding upstream.

OpenCode configures providers in `~/.config/opencode/opencode.json`. Each
provider's `options.baseURL` carries the version path and the SDK appends the
endpoint path itself — existing entries on this machine (`…/v1` for
ollama/vLLM, `…/v4` for z.ai) all keep it, so the proxy entry does too:
`http://localhost:8765/v1` → OpenCode emits `POST /v1/chat/completions`.

## Prerequisites — the OpenAI backend must be configured

`/v1/chat/completions` routes to the proxy's **openai** backend — which, if
you only followed the Anthropic/z.ai setup, defaults to
`https://api.openai.com` **with no key** → 401. Add to `.env`:

```bash
OPENAI_API_KEY=<your real key>
OPENAI_BACKEND=https://api.z.ai/api/coding/paas/v4   # z.ai OpenAI-compatible endpoint
```

Both lines are required; see [[Agent-Setup-Overview]] for the full routing
table.

## Recipe

**1. Start the proxy** (repo root, where `.env` lives):

```bash
./llmproxy serve          # http://localhost:8765
```

**2. Add the provider** in `~/.config/opencode/opencode.json` (alongside your
existing entries — no need to delete anything):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "llmproxy": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "llmproxy (local)",
      "options": {
        "baseURL": "http://localhost:8765/v1",
        "apiKey": "llmproxy-dummy"
      },
      "models": {
        "glm-5.3": {
          "name": "GLM 5.3 (via llmproxy)",
          "attachment": true,
          "reasoning": true
        }
      }
    }
  },
  "model": "llmproxy/glm-5.3"
}
```

- Keep `/v1` in `baseURL` (mirrors the existing entries' shape).
- `"apiKey": "llmproxy-dummy"` — satisfies the SDK's key requirement without
  a real credential. Strictly, `@ai-sdk/openai-compatible` entries can omit
  `apiKey` entirely (the ollama entry here does), but the dummy keeps the
  "never empty-handed" contract explicit.
- Set the default via top-level `"model": "llmproxy/glm-5.3"` and/or
  per-agent `"agent": { "lead": { "model": "llmproxy/glm-5.3" } }` — this
  install selects models that way (no `defaultProvider` key involved).

**Why a dummy key works:** when a request targets the proxy itself, llmproxy
**strips whatever auth the agent sent** and injects the real `OPENAI_API_KEY`
from `.env` as `Authorization: Bearer`, then forwards to `OPENAI_BACKEND`.
The dummy never leaves your machine.

**3. Verify:**

```bash
opencode run "reply OK"
```

You should get a real completion, and the request appears in the live
dashboard (`http://localhost:8765/__inspect__`).

## Model mapping (MODEL_MAP)

`MODEL_MAP` in `.env` remaps the request's model before forwarding — first
matching rule wins, unmatched names pass through unchanged. With the provider
above, model ids are already upstream-native (`glm-5.3`), so no remap is
needed; if you prefer OpenCode-facing names, add rules to `MODEL_MAP`
accordingly. Mechanics explained in [[Claude-Code-Setup]].

## Alternative — transparent capture, no OpenCode config edits

```bash
llmproxy opencode
```

The wrapper temporarily patches every provider's `baseURL` in
`opencode.json` to the local proxy (backed up and restored on exit) and tells
the daemon the real upstream, so OpenCode keeps using its **own credentials**
and llmproxy just logs the traffic. Use this for per-run capture; use the
credential-free recipe above for a standing setup with dummies only.

## Troubleshooting

- **401 from upstream** → `.env` missing `OPENAI_API_KEY` (see Prerequisites)
  or `OPENAI_BACKEND` not set, so requests fall through to api.openai.com.
- **Provider not listed in OpenCode** → JSON syntax error in `opencode.json`;
  check with `jq . ~/.config/opencode/opencode.json`.
- **Model errors upstream** → unmapped model name; see MODEL_MAP above.
- **Dashboard 404** → set `LLMPROXY_INSPECT_TOKEN` in `.env`, then reload with
  `Authorization: Bearer <token>` or `?token=<token>`.
