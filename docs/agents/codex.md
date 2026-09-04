---
type: reference
title: Codex CLI — Credential-Free Setup
created: 2026-09-03
tags:
  - codex
  - setup
  - credential-free
related:
  - '[[Agent-Setup-Overview]]'
  - '[[Claude-Code-Setup]]'
  - '[[OpenCode-Setup]]'
  - '[[Pi-Setup]]'
---

# Codex CLI — Credential-Free Setup

Wire OpenAI's Codex CLI to llmproxy so its config holds **zero real
credentials**: Codex reads a dummy key from the environment, and the proxy
swaps in the real key (from `.env`) when forwarding upstream.

Codex supports custom providers via `~/.codex/config.toml`, so unlike the old
MITM approach no HTTPS_PROXY/CA-cert tricks are needed. The `wire_api` value
below is derived from this machine's own working Codex config (codex-cli
0.149.0): it uses the **Responses API** (`wire_api = "responses"`).

## Prerequisites — the OpenAI backend must be configured

Codex emits OpenAI-style requests (`POST /v1/responses`, or
`/v1/chat/completions` under `wire_api = "chat"`). The proxy routes those to
the **openai** backend — which, if you only followed the Anthropic/z.ai setup,
defaults to `https://api.openai.com` **with no key** → 401. Add to `.env`:

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

**2. Register the provider** in `~/.codex/config.toml`:

```toml
model_provider = "llmproxy"
model = "glm-5.3"

[model_providers.llmproxy]
name = "llmproxy (local)"
base_url = "http://localhost:8765/v1"
env_key = "OPENAI_API_KEY"
wire_api = "responses"
```

- `base_url` keeps the `/v1` suffix — Codex appends `/responses` (or
  `/chat/completions`) to it.
- `env_key = "OPENAI_API_KEY"` tells Codex to read its credential from that
  environment variable instead of storing one in `auth.json`.
- `wire_api`:
  - `"responses"` — the Responses API; what current builds (incl. 0.149.0
    here) use. **Prefer this.**
  - `"chat"` — the classic Chat Completions API; older builds / some proxies.
    Both variants route through llmproxy identically.

If your `config.toml` contains CCR-managed blocks (`# BEGIN CCR …` /
`# END CCR …`), leave them untouched — just change the top-level
`model_provider` and `model` values. Flipping back to the old setup later is a
two-line revert.

**3. Supply the dummy key in your shell** (Codex refuses to run without a
value for `env_key`):

```bash
export OPENAI_API_KEY=llmproxy-dummy
```

**Why a dummy key works:** when a request targets the proxy itself, llmproxy
**strips whatever auth the agent sent** and injects the real `OPENAI_API_KEY`
from `.env` as `Authorization: Bearer`, then forwards to `OPENAI_BACKEND`.
The dummy never leaves your machine.

**4. Verify:**

```bash
codex exec "reply OK"
```

You should get a real completion, and the request appears in the live
dashboard (`http://localhost:8765/__inspect__`).

## Model mapping (MODEL_MAP)

`MODEL_MAP` in `.env` remaps the request's model before forwarding — first
matching rule wins, unmatched names pass through unchanged. The default rules
map Claude names; for Codex either set `model` in `config.toml` directly to a
name your upstream accepts (e.g. `glm-5.3`), or add a remap rule like:

```
MODEL_MAP=…,gpt-*=glm-5.3
```

The model-mapping mechanics are explained in [[Claude-Code-Setup]].

## Alternative — transparent capture, no Codex config edits

```bash
llmproxy codex "fix the bug"
```

The wrapper starts a local daemon and intercepts Codex with `HTTPS_PROXY` +
MITM CA cert (Codex is a statically-linked MUSL binary and ignores
`*_BASE_URL` env vars — MITM is the only transparent path). Codex keeps using
its **own real credentials** against its **real upstream**; llmproxy just logs
the traffic. Use this when you want capture without touching `config.toml`;
use the credential-free recipe above when you want Codex to hold dummies only.

## Troubleshooting

- **401 from upstream** → `.env` missing `OPENAI_API_KEY` (see Prerequisites)
  or `OPENAI_BACKEND` not set, so requests fall through to api.openai.com.
- **Codex says the key is missing** → `env_key` names a variable that isn't
  exported in the shell Codex runs from (`export OPENAI_API_KEY=llmproxy-dummy`).
- **Model errors upstream** → unmapped model name; see MODEL_MAP above.
- **Dashboard 404** → set `LLMPROXY_INSPECT_TOKEN` in `.env`, then reload with
  `Authorization: Bearer <token>` or `?token=<token>`.
