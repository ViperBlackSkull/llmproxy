---
type: report
title: Agent Test Matrix
created: 2026-09-03
tags:
  - testing
  - agents
  - matrix
related:
  - '[[Agent-Setup-Overview]]'
---

# Agent Test Matrix

Live pass/fail verification that each supported coding agent works against
llmproxy **credential-free**: the agent's config holds only a dummy key, the
request demonstrably reaches the proxy, the proxy's injected call upstream
succeeds, and the agent prints a real completion. The wiring recipes under
test live in the per-agent guides; the shared story is
[[Agent-Setup-Overview]].

Tests are run by `scripts/test-agents.sh` (built in Phase 04, task 1), which
snapshots + restores every config it touches, starts a fresh
`llmproxy serve` on a scratch port with `LOG_DIR=logs-test/<agent>/`, runs a
one-shot prompt under `timeout 90`, and prints one verdict line per agent:
`PASS|FAIL|SKIPPED <agent> <reason>`.

> [!IMPORTANT]
> Rows are updated **immediately after each individual test**, not in a batch
> at the end. Until an agent's test has run, its row stays PENDING.

## Matrix

| Agent | Installed? | Request reached proxy? | Upstream call succeeded? | End-to-end verdict | Issues found | Evidence |
|---|---|---|---|---|---|---|
| Claude Code | PENDING | PENDING | PENDING | PENDING | see [Claude Code](#claude-code) | — |
| OpenCode | PENDING | PENDING | PENDING | PENDING | see [OpenCode](#opencode) | — |
| Codex | PENDING | PENDING | PENDING | PENDING | see [Codex](#codex) | — |
| Pi | PENDING | PENDING | PENDING | PENDING | see [Pi](#pi) | — |

### Column semantics

- **Installed?** — did `command -v` find the binary? Missing binary → the
  harness skips the agent (verdict `SKIPPED`), which is a *not tested* row,
  not a failure of the proxy.
- **Request reached proxy?** — did capture files (`*.md` + matching
  `*.req.json`) appear under `logs-test/<agent>/`? No captures means the
  agent never talked to the proxy (wrong base URL, gateway bypass, etc.).
- **Upstream call succeeded?** — did the forwarded call get a 2xx from the
  real backend? Checked in the capture: response status + a non-empty
  completion, with the `Authorization`/`x-api-key` header in `.req.json`
  showing only the **dummy** value (proving injection happened at the proxy,
  not locally).
- **End-to-end verdict** — the harness verdict line; PASS requires exit 0,
  captured request(s), and agent output.
- **Evidence** — filenames under `logs-test/<agent>/` backing the row.

### Evidence layout

Every test leaves, under `logs-test/<agent>/`:

- `daemon.log` — proxy console output for the run,
- `agent-output.log` — full agent stdout+stderr,
- `../<agent>.verdict` — the one-line verdict,
- captured traffic (`*.md` + `*.req.json`, secrets redacted) in the
  serve-mode agent-type subdirectory (`unknown/` unless `AGENT_TYPE` is set).

`logs-test/` is scratch evidence, not a shipped artifact.

## Claude Code

Guide: [[Claude-Code-Setup]] · Wiring: `~/.claude/settings.json` → `env`
(`ANTHROPIC_BASE_URL` → proxy, `ANTHROPIC_AUTH_TOKEN` → dummy).

```bash
./scripts/test-agents.sh claude
```

**Observed symptoms:** _to be filled by the Claude Code test task._

- **Pre-test note (harness bring-up, 2026-09-03):** the smoke run of the
  harness itself failed with `no request reached the proxy` — nested claude
  answered via this machine's standing **claude-code-router gateway**
  (`127.0.0.1:3456`). The machine's `settings.json` carries gateway vars
  beyond the Phase 03 snippet (`ANTHROPIC_API_BASE_URL`,
  `CLAUDE_AGENT_API_BASE_URL`) plus an `ANTHROPIC_MODEL=zai/glm-5.3` pin
  (logged upstream as `[claude-code:unrecognized_model]`). The Claude test
  task must cover the extra vars and/or add a `zai/*` `MODEL_MAP` catch-all
  in `.env`.

## OpenCode

Guide: [[OpenCode-Setup]] · Wiring: `~/.config/opencode/opencode.json` →
`provider.llmproxy` (dummy `options.apiKey`) + `--model` on the CLI.

```bash
./scripts/test-agents.sh opencode
```

**Observed symptoms:** _to be filled by the OpenCode test task._

- **Pre-test note (harness bring-up, 2026-09-03):** `.env` currently has only
  the Anthropic side (`ANTHROPIC_API_KEY` + `ANTHROPIC_BACKEND`). OpenCode
  talks OpenAI-style (`/v1/chat/completions`), which routes to
  `OPENAI_BACKEND` — unset, so the first run is expected to 401 upstream
  until `OPENAI_API_KEY`/`OPENAI_BACKEND` are added (see the IMPORTANT block
  in [[Agent-Setup-Overview]]).

## Codex

Guide: [[Codex-Setup]] · Wiring: `~/.codex/config.toml` → top-level
`model_provider`/`model` + `[model_providers.llmproxy]` block (dummy key via
`env_key` from the shell).

```bash
./scripts/test-agents.sh codex
```

**Observed symptoms:** _to be filled by the Codex test task._

- **Pre-test note:** same `.env` gap as OpenCode — Codex emits
  `/responses` (or `/chat/completions` with `wire_api = "chat"`), both routed
  to the unset `OPENAI_BACKEND`. Also watch the known MUSL build issue
  (local Codex ignoring `config.toml` base URLs): if it bites, fall back to
  the wrapper's MITM mode (`llmproxy codex "Reply with exactly: OK"`) and
  label the matrix row with **which mode passed**.

## Pi

Guide: [[Pi-Setup]] · Wiring: `~/.pi/agent/providers.json` (array entry) +
profile env in `~/.pi/agent/llmproxy/settings.json`; harness uses the
`--provider` flag instead of patching `defaultProvider`.

```bash
./scripts/test-agents.sh pi
```

**Observed symptoms:** _to be filled by the Pi test task._

- **Pre-test note:** if Pi cannot be redirected by config (Docker-owned
  settings), fall back to the wrapper's MITM mode
  (`HTTPS_PROXY` + `NODE_EXTRA_CA_CERTS`) and record which mode passed. The
  credential-free promise is only *fully* proven in URL mode (config contains
  dummy key only); a MITM-only pass gets an explicit limitation note in
  [[Pi-Setup]].
