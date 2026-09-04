---
type: reference
title: Pi Coding Agent — Credential-Free Setup
created: 2026-09-03
tags:
  - pi
  - setup
  - credential-free
related:
  - '[[Agent-Setup-Overview]]'
  - '[[Claude-Code-Setup]]'
  - '[[Codex-Setup]]'
  - '[[OpenCode-Setup]]'
---

# Pi Coding Agent — Credential-Free Setup

Wire the Pi coding agent (earendil-works) to llmproxy so its config holds
**zero real credentials**: the profile carries dummy keys, and the proxy swaps
in the real key (from `.env`) when forwarding upstream.

Pi's configuration lives in `~/.pi/agent/`:

- `providers.json` — the provider catalog: each entry has an `id`, a
  `profile` (the per-provider config dir under `~/.pi/agent/<profile>/`),
  `base_url_default`, and whether it needs a key.
- `settings.json` — `defaultProvider` selects the active catalog entry.
- `~/.pi/agent/<profile>/settings.json` — the profile's env (base URLs, keys).

On this machine a `llmproxy` provider entry **already exists** in
`providers.json` (with `needs_key: false`), and so does the profile dir —
both date from an earlier Docker-based Pi setup and point at
`host.docker.internal:8765`. Pi now runs as a host Node CLI, so the recipe
below refreshes them to loopback addresses and dummy keys.

## Recipe

**1. Start the proxy** (repo root, where `.env` lives):

```bash
./llmproxy serve          # http://localhost:8765
```

**2. Register the provider** in `~/.pi/agent/providers.json` (keep it as an
array entry):

```json
{
  "id": "llmproxy",
  "name": "LLM Proxy (local, no secrets)",
  "profile": "llmproxy",
  "base_url_default": "http://localhost:8765",
  "test_path": "/__inspect__",
  "needs_key": false,
  "needs_proxy": false,
  "env": {},
  "models": ["glm-5.3"]
}
```

**3. Configure the profile** — `~/.pi/agent/llmproxy/settings.json`, env
block (shape mirrors the existing profiles):

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:8765",
    "ANTHROPIC_AUTH_TOKEN": "llmproxy-dummy",
    "OPENAI_BASE_URL": "http://localhost:8765/v1",
    "OPENAI_API_KEY": "llmproxy-dummy",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"
  }
}
```

Pi speaks Anthropic-style by default (`/v1/messages` → proxy routes to
`ANTHROPIC_BACKEND` + `ANTHROPIC_API_KEY`). The OpenAI env vars are harmless
to set and cover any openai-style extension; if you actually use one, the
openai backend needs `OPENAI_API_KEY` + `OPENAI_BACKEND` in `.env` — see
[[Agent-Setup-Overview]].

**4. Select the provider.** Either per-run:

```bash
pi --provider llmproxy --model glm-5.3 -p "reply OK"
```

or standing, in `~/.pi/agent/settings.json`:

```json
{
  "defaultProvider": "llmproxy",
  "defaultModel": "glm-5.3"
}
```

> [!NOTE]
> `defaultProvider` must match an `id` in `providers.json`. A stale value
> that matches no entry silently falls back to the `zai-claude` profile
> (that's what the `llmproxy` wrapper's profile detection does today).

**Why dummies work:** when a request targets the proxy itself, llmproxy
**strips whatever auth the agent sent** and injects the real key from `.env`
(as both `x-api-key` and `Authorization: Bearer` for Anthropic-style
upstreams), then forwards to `ANTHROPIC_BACKEND`. The dummies never leave
your machine.

**5. Verify:**

```bash
pi -p "reply OK"
```

(with `defaultProvider` set; otherwise add `--provider llmproxy
--model glm-5.3`). You should get a real completion, and the request appears
in the live dashboard (`http://localhost:8765/__inspect__`).

## Model mapping (MODEL_MAP)

`MODEL_MAP` in `.env` remaps the request's model before forwarding — first
matching rule wins, unmatched names pass through unchanged. Default rules map
Claude names (`claude-sonnet-*` → `glm-5.3`); with the provider above you use
upstream-native ids (`glm-5.3`) directly. Mechanics explained in
[[Claude-Code-Setup]].

## Containerized Pi caveat

If Pi runs **inside Docker**, `localhost` won't reach the proxy — use
`http://host.docker.internal:8765` (as the pre-existing entry does). But note:
`llmproxy serve` binds `127.0.0.1` only, and a container's
`host.docker.internal` traffic arrives on the host's gateway interface, so
the daemon won't accept it without widening `LISTEN_ADDR` in `.env`
(e.g. `LISTEN_ADDR=0.0.0.0:8765` — only on a trusted single-user network).
This combination is **pending live verification** (see [[Agent-Test-Matrix]]).

## Alternative — transparent capture, no Pi config edits

```bash
llmproxy pi -p "hello"
```

The wrapper intercepts Pi with `HTTPS_PROXY` + MITM CA cert
(`NODE_EXTRA_CA_CERTS`, since Pi is Node.js) — Pi keeps its **own
credentials** against its real upstream and llmproxy just logs the traffic.
Use this for per-run capture; use the credential-free recipe above for a
standing setup with dummies only.

## Troubleshooting

- **401 from upstream** → `.env` missing `ANTHROPIC_API_KEY` or
    `ANTHROPIC_BACKEND` pointing at an endpoint that rejects it.
- **Pi starts on the wrong provider/model** → `defaultProvider`/
    `defaultModel` stale or not matching a `providers.json` id (see NOTE
    above); pass `--provider llmproxy --model glm-5.3` explicitly.
- **Connection refused** → proxy not running, or a Docker-hosted Pi pointing
    at `localhost` (see Containerized Pi caveat).
- **Dashboard 404** → set `LLMPROXY_INSPECT_TOKEN` in `.env`, then reload
    with `Authorization: Bearer <token>` or `?token=<token>`.
