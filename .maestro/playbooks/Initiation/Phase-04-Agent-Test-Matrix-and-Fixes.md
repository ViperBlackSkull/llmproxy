# Phase 04: Run the Four-Agent Test Matrix and Fix What Breaks

The user's explicit stance: "I don't know, we will need to test them." This phase does exactly that — it runs Claude Code, Codex, OpenCode, and Pi against the credential-free proxy with **zero real credentials** configured in the agent, records a pass/fail matrix, and fixes every issue that surfaces until the matrix is green (or the failure is root-caused and documented). Each agent gets a small, reversible config patch (backup + restore, same pattern as the wrapper's `patch_json`), a trivial one-shot prompt, and a verdict backed by captured evidence in the logs.

**Context you need:** the proxy is credential-free since Phase 01 and live since Phase 02; the per-agent wiring snippets come from the Phase 03 guides in `docs/agents/`. The daemon logs land in `logs/<agent>/`. Two golden rules: (1) never write a real API key into any agent config — dummies only, the proxy injects the real one; (2) every config file touched must be backed up before editing and restored after the test, no exceptions (the wrapper's `patch_json`/`cleanup` trap in the `llmproxy` script is the reference implementation of this pattern).

## Tasks

- [ ] Build the test harness `scripts/test-agents.sh` (new script, `chmod +x`):
  - Per-agent test functions that: check the binary exists (`command -v`) → skip with reason if absent; snapshot any config file they must edit (`cp` to a `.llmproxy-backup` alongside, register a `trap` to restore); apply the Phase 03 snippet; export dummy env keys; start `llmproxy serve` fresh with a clean `LOG_DIR` (use a temp dir like `logs-test/<agent>` so real logs stay clean); run the agent's one-shot prompt (e.g. `claude -p "Reply with exactly: OK"` / `codex exec …` / `opencode run …` / `pi -p …`) under `timeout 90`; capture exit code, stdout tail, and whether new files appeared in the log dir; restore configs; print a one-line verdict `PASS|FAIL|SKIPPED <agent> <reason>`
  - Accept an agent name argument to run a single test (`./scripts/test-agents.sh claude`), defaulting to all four
  - The script must never print secrets (redact anything resembling a key from captured output before echoing)

- [ ] Create the structured results doc `docs/testing/agent-matrix.md` (with YAML front matter: `type: report`, `tags: [testing, agents, matrix]`, `related: '[[Agent-Setup-Overview]]'`, `created:` today):
  - A table: agent | installed? | request reached proxy? | upstream call succeeded? | end-to-end verdict | issues found | evidence (log filenames)
  - A per-agent section for reproduction commands and observed symptoms; wiki-link each agent section to its `docs/agents/*-setup` guide
  - Initialize all rows as PENDING — subsequent tasks fill them in as tests run; update the file after every single test, not at the end

- [ ] Test Claude Code and fix whatever breaks:
  - Run `./scripts/test-agents.sh claude` with the real upstream (z.ai via `.env`) live
  - Confirm in `logs-test/claude/` that the request arrived (`.md` + `.req.json`), the auth header in `.req.json` is redacted/dummy (proving injection happened upstream, not locally), the model was remapped per `MODEL_MAP`, and the agent printed a real completion
  - Typical failure modes to check and fix in `main.go` if hit: `x-api-key` vs `Authorization` header form rejected by z.ai (adjust injection), `anthropic-beta`/`anthropic-version` headers stripped or mangled, model-name patterns not matching what Claude Code actually sends (widen the `MODEL_MAP` patterns), SSE streaming not flushed chunk-by-chunk through `proxyAndCapture`
  - Update the matrix row with the verdict and evidence

- [ ] Test OpenCode and fix whatever breaks:
  - Run `./scripts/test-agents.sh opencode`; determine from `.req.json` which endpoint shape it used (`/v1/chat/completions` vs `/responses`) and which backend that routes to — if it routes to `openai` and no `OPENAI_BACKEND` is set, either set `OPENAI_BACKEND` to the z.ai OpenAI-compatible endpoint in `.env` or add a `MODEL_MAP` rule for the GLM chat models, whichever the observed request shape demands
  - Fix common issues at the proxy: `detectAPIType` missing the path variant OpenCode emits; streaming (`"stream": true`) responses buffering instead of streaming; `max_tokens` vs `max_completion_tokens` field passthrough (forward as-is first — only normalize if the upstream 4xx's on it)
  - Update the matrix row

- [ ] Test Codex and fix whatever breaks:
  - Run `./scripts/test-agents.sh codex`; if the local build ignores `config.toml` base URLs (the MUSL problem the wrapper notes), record that the URL mode needs the documented fallback and verify instead via the wrapper's MITM mode (`llmproxy codex "Reply with exactly: OK"`) so the agent still gets a verdict — label the matrix row with which mode passed
  - Watch for `/responses` API traffic (Responses API is OpenAI-style with `instructions` + `input`): confirm `detectAPIType` classifies it as `openai`, the system-prompt extraction still works, and streaming `response.output_text.delta` frames pass through live
  - Update the matrix row

- [ ] Test Pi and fix whatever breaks:
  - Run `./scripts/test-agents.sh pi`; if Pi cannot be redirected by config (Docker-owned settings, per the wrapper comment), fall back to verifying via the wrapper's MITM mode (`HTTPS_PROXY` + `NODE_EXTRA_CA_CERTS`) and record which mode passed in the matrix
  - Confirm the credential-free promise where possible: if URL mode works, the agent config must contain only a dummy key; if only MITM mode works, note the limitation explicitly in `docs/agents/pi.md`
  - Update the matrix row

- [ ] Re-run the full matrix, reconcile the docs, and commit:
  - `./scripts/test-agents.sh` (all four) — every installed agent must PASS end-to-end or carry a root-caused, documented failure; flaky results get one re-run to confirm
  - Update `docs/agents/README.md`'s comparison table status column from the Phase 03 placeholder to actual verdicts, and fix any guide snippet that testing proved wrong
  - Verify `git status` shows no leftover `.llmproxy-backup` files or `logs-test/` content staged (add `logs-test/` to `.gitignore` if it should persist as a scratch dir)
  - Commit on `dev`: `Test matrix: credential-free verification for claude/codex/opencode/pi + fixes` (include the `main.go`/wrapper fixes from this phase)
