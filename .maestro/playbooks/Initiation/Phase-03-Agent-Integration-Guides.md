# Phase 03: Credential-Free Integration Guides for the Four Agents

This phase produces the onboarding layer for the four target agents — **Claude Code, Codex, OpenCode, and Pi** — so that wiring each one to the proxy is a copy-paste operation: point the agent at `http://localhost:8765`, supply a dummy key where the agent refuses to run without one, and never touch real credentials inside the agent. Each guide is a structured markdown doc (front matter + wiki-links) derived from the agent's actual config surface on this machine, so the snippets are verified against reality rather than guessed from docs. Important: this phase only WRITES documentation — it must not permanently modify the user's real agent configs (temporary, backed-up edits for live testing happen in Phase 04).

**Context you need:** the `llmproxy` bash wrapper (restored in Phase 01) already knows each agent's interception quirks — read its per-agent `case` blocks before writing anything (Claude Code: `~/.claude/settings.json` `env.ANTHROPIC_BASE_URL`; Codex: MUSL binary, normally needs `HTTPS_PROXY`+MITM but supports `~/.codex/config.toml` providers; OpenCode: per-provider `baseURL` in `~/.config/opencode/opencode.json`; Pi: profile settings in `~/.pi/agent/`). Inspect the real files where they exist to get exact key names and shapes. The proxy side (`/v1/messages` Anthropic-style, `/v1/chat/completions` + `/v1/responses` OpenAI-style) is already credential-free thanks to Phase 01.

## Tasks

- [ ] Discover each agent's real base-URL and auth config surface on this machine, and record findings in a scratch note (`.maestro/playbooks/Initiation/Working/agent-config-discovery.md`):
  - Claude Code: `~/.claude/settings.json` (`env.ANTHROPIC_BASE_URL`, `env.ANTHROPIC_AUTH_TOKEN`, `env.ANTHROPIC_API_KEY`, model override keys)
  - Codex: `~/.codex/config.toml` (`model_providers` table: `name`, `base_url`, `env_key`, `wire_api`) and whether `codex` is on PATH (`codex --version`)
  - OpenCode: `~/.config/opencode/opencode.json` (`provider.<id>.options.baseURL`, `provider.<id>.options.apiKey`, `defaultProvider`)
  - Pi: `~/.pi/agent/settings.json` + `~/.pi/agent/providers.json` (provider ids, profiles, base URLs)
  - For each: note what is present vs absent, and whether the binary is installed — absent config or a missing binary means the guide gets a "not installed here — generic instructions" marker, not a blocker

- [ ] Write `docs/agents/claude-code.md` — structured doc with YAML front matter (`type: reference`, `tags: [claude-code, setup, credential-free]`, `related: '[[Agent-Setup-Overview]]'`, `created:` today):
  - The credential-free recipe: start `llmproxy serve`, then set in `~/.claude/settings.json` (or shell env for one-off runs) `ANTHROPIC_BASE_URL=http://localhost:8765` and `ANTHROPIC_AUTH_TOKEN=llmproxy-dummy` — explain that the dummy token is replaced by the real z.ai key inside the proxy
  - Note the interaction with the existing transparent-capture mode (`llmproxy claude …`): both work, this is the standing/interactive variant with zero real credentials in agent config
  - Include the model mapping note (Claude names → GLM via `MODEL_MAP`) and a "verify it works" one-liner (`claude -p "reply OK"`)
  - Mention removing/renaming real keys from agent config is safe but optional

- [ ] Write `docs/agents/codex.md` (same front-matter pattern, `tags: [codex, setup, credential-free]`):
  - The credential-free recipe via `~/.codex/config.toml`: a `model_providers.llmproxy` entry with `base_url = "http://localhost:8765/v1"`, `env_key = "OPENAI_API_KEY"` (set `OPENAI_API_KEY=llmproxy-dummy` in the shell), `wire_api` set to whichever the local Codex build uses — derive it from the existing config if present, otherwise document both `"chat"` and `"responses"` variants with a note to prefer `"responses"` on current builds
  - Include selecting the provider (`model_provider = "llmproxy"`) and the verify one-liner (`codex exec "reply OK"` or the local equivalent)
  - Keep the legacy MITM capture mode (`llmproxy codex …`) documented as the alternative that needs no Codex config edits

- [ ] Write `docs/agents/opencode.md` and `docs/agents/pi.md` (same front-matter pattern):
  - OpenCode: `~/.config/opencode/opencode.json` snippet with the provider entry pointing `baseURL` at `http://localhost:8765` (keep `/v1` off unless discovery shows OpenCode appends paths itself — mirror what the existing config does) plus `"apiKey": "llmproxy-dummy"`; verify one-liner (`opencode run "reply OK"`)
  - Pi: snippet for the active provider profile in `~/.pi/agent/` pointing its base URL at the proxy with a dummy key, based on the actual shape found during discovery (mirror `pi_active_profile` logic in the wrapper); verify one-liner (`pi -p "reply OK"`)
  - Each doc links back to `[[Agent-Setup-Overview]]` and to `[[Claude-Code-Setup]]` where the shared MODEL_MAP explanation lives

- [ ] Write `docs/agents/README.md` titled "Agent Setup Overview" (file name `README.md` inside `docs/agents/`, wiki-link target `[[Agent-Setup-Overview]]`):
  - Front matter (`type: reference`, `tags: [setup, agents]`), the shared story in four steps: (1) `cp .env.example .env` + fill real keys, (2) `llmproxy serve`, (3) wire the agent per its guide, (4) open the live dashboard
  - A comparison table: agent | config file | env vars | dummy-key needed? | status (pending live test — Phase 04 fills this in)
  - Wiki-links to all four agent docs and to `[[Agent-Test-Matrix]]` (created in Phase 04 — the link is intentionally forward)
  - Security note: `.env` holds the only real credentials; agents hold dummies; the dashboard needs `LLMPROXY_INSPECT_TOKEN`

- [ ] Cross-check every snippet against the daemon's routing reality and commit:
  - For each guide, sanity-check the URL path the agent will emit against `detectAPIType` in `main.go` (`/v1/messages` → anthropic backend; `/v1/chat/completions` and `/responses`/`/v1/responses` → openai backend) — if an agent's default endpoint would route to a provider with no `*_BACKEND` configured, say explicitly in the guide which `.env` var to set
  - Confirm no guide contains real key values (dummies only)
  - Commit on `dev`: `Docs: credential-free setup guides for Claude Code, Codex, OpenCode, Pi + overview`
