---
type: reference
title: Claude Code — Credential-Free Setup
created: 2026-09-03
tags:
  - claude-code
  - setup
  - credential-free
related:
  - '[[Agent-Setup-Overview]]'
  - '[[Codex-Setup]]'
  - '[[OpenCode-Setup]]'
  - '[[Pi-Setup]]'
---

# Claude Code — Credential-Free Setup

Wire Claude Code to llmproxy so its config holds **zero real credentials**: the
agent sends a dummy token, and the proxy swaps in the real key (from `.env`)
when forwarding upstream.

Claude Code talks Anthropic-style, so it emits `POST /v1/messages` — the proxy
routes that to `ANTHROPIC_BACKEND` and injects `ANTHROPIC_API_KEY`. See
[[Agent-Setup-Overview]] for the shared four-step story and the routing table.

## Recipe

**1. Start the proxy** (repo root, where `.env` lives):

```bash
./llmproxy serve          # http://localhost:8765
```

**2. Point Claude Code at it.** Standing setup — in `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:8765",
    "ANTHROPIC_AUTH_TOKEN": "llmproxy-dummy"
  }
}
```

One-off shell variant (no settings edit):

```bash
ANTHROPIC_BASE_URL=http://localhost:8765 \
ANTHROPIC_AUTH_TOKEN=llmproxy-dummy \
claude -p "reply OK"
```

> [!IMPORTANT]
> `settings.json`'s `env` block **overrides shell environment variables**. If
> `env.ANTHROPIC_BASE_URL` is already set there, the shell one-off above will
> silently not take effect — either edit the settings file or remove that key
> first. On this machine `settings.json` pins it to claude-code-router
> (`http://127.0.0.1:3456`), so the settings-file route is the one that counts.

**Why a dummy token works:** Claude Code refuses to start without *some*
credential, so we hand it `llmproxy-dummy`. When a request targets the proxy
itself, llmproxy **strips whatever auth the agent sent** and injects the real
key from `.env` (as both `x-api-key` and `Authorization: Bearer`), then
forwards to `ANTHROPIC_BACKEND`. The dummy never leaves your machine.

**3. Verify:**

```bash
claude -p "reply OK"
```

You should get a real completion, and the request appears in the live
dashboard (`http://localhost:8765/__inspect__`).

## Model mapping (MODEL_MAP)

Claude Code sends model names like `claude-opus-4-6` / `claude-sonnet-4-6`.
`.env`'s `MODEL_MAP` remaps them before they reach the upstream:

```
MODEL_MAP=claude-opus-*=glm-5.3,claude-sonnet-*=glm-5.3,claude-haiku-*=glm-5.3,claude-3-5-haiku-*=glm-5.3
```

First matching rule wins; models that match no rule pass through unchanged.
This is where the shared model-mapping story lives — the other agent guides
link back here.

> [!WARNING]
> **If your `settings.json` pins model overrides** (e.g.
> `ANTHROPIC_MODEL`, `ANTHROPIC_DEFAULT_OPUS_MODEL`, …) to a router-prefixed
> name like `zai/glm-5.3`, that name matches **no** default `MODEL_MAP` rule
> and is forwarded verbatim — and z.ai's endpoint expects the bare name
> (`glm-5.3`), not the prefixed one. Fix it one of two ways:
>
> 1. Add a catch-all rule to `MODEL_MAP` in `.env`: `…,zai/*=glm-5.3`
> 2. Or point the model override keys directly at the upstream's model name
>    (`glm-5.3`) / remove them so the real Claude names flow and `MODEL_MAP`
>    does the remap.

## Coexistence with the other modes

| Mode | Command | What it does with credentials |
|---|---|---|
| Transparent capture | `llmproxy claude …` | Agent config untouched (temporarily patched + restored). Agent's own creds pass through to its **real** upstream; proxy just logs. |
| Credential-free (this guide) | `llmproxy serve` + settings above | Agent holds **dummies only**; proxy injects the real key toward `ANTHROPIC_BACKEND`. |
| Direct (no llmproxy) | `claude …` | Whatever you had before. |

Both llmproxy modes work independently — you can keep using
`llmproxy claude -p "…"` for per-run capture while the standing
credential-free setup stays in `settings.json` (they fight over
`ANTHROPIC_BASE_URL`, so don't run both against the same file simultaneously;
the wrapper restores whatever it patched on exit).

## Real keys already in agent config?

Removing or renaming them is **safe but optional** while pointing at the
proxy: any auth the agent sends to the proxy is stripped and replaced on
forward, so a stale real key in `settings.json` is inert (it never reaches the
upstream). Obvious hygiene still applies — if the config no longer needs a
real key, take it out.

## Troubleshooting

- **401 from upstream** → `.env` missing `ANTHROPIC_API_KEY` (the proxy falls
  back to the agent's dummy auth, which the upstream rejects) or
  `ANTHROPIC_BACKEND` pointing somewhere that doesn't accept that key.
- **Model not found upstream** → see the `MODEL_MAP` warning above; likely a
  prefixed (`zai/…`) or unmapped model name passing through.
- **Requests bypass the proxy** → `settings.json` `env` beats your shell
  exports; check both places (see the IMPORTANT note above).
- **Dashboard 404** → set `LLMPROXY_INSPECT_TOKEN` in `.env`, then reload with
  `Authorization: Bearer <token>` or `?token=<token>`.
