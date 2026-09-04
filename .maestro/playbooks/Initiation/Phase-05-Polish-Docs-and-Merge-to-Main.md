# Phase 05: Polish, Final Validation, and Merge Back to Main

The features work; this phase makes them presentable and durable. It refreshes the README to lead with the new credential-free + live-dashboard story, tightens the security posture of everything added on `dev` (injection mode, SSE endpoint, `.env` handling), runs one full validation sweep, and merges `dev` back to `main` as the discovery conversation planned ("merge back once everything's stable"). After this phase, `main` is the shippable state and `dev` remains for future experiments.

**Context you need:** everything landed on `dev` across Phases 01–04: `.env` loading + credential injection + `MODEL_MAP` + `serve` subcommand, the SSE live dashboard, agent guides in `docs/agents/`, the test matrix in `docs/testing/`, and the harness in `scripts/test-agents.sh`. The README currently documents none of it and still describes config tables without `MODEL_MAP` or `.env` semantics. Read the current README end-to-end before editing so the new sections match its voice and structure.

## Tasks

- [ ] Update `README.md` to feature-first the new capabilities:
  - New "Credential-Free Mode" section near the top: the three-line quickstart (`cp .env.example .env` → fill keys → `llmproxy serve`), the point-at-a-URL promise with a dummy key, and a link to `docs/agents/README.md` for per-agent wiring
  - Extend the Inspect Dashboard section: live streaming via SSE, the `● Live` indicator, and the token requirement (`?token=` for EventSource)
  - Extend the environment-variable tables with `MODEL_MAP` (format + example), the `ANTHROPIC_BACKEND`/`OPENAI_BACKEND`/`GEMINI_BACKEND` overrides, and `.env` loading precedence (real env vars beat `.env`)
  - Add the four agents' credential-free status to the Supported Agents table (mirror the final matrix verdicts, noting URL mode vs MITM mode where relevant)
  - Keep the security section accurate: injection means agents hold dummy keys and `.env` holds the only real ones — state that plainly

- [ ] Do a security pass over everything added on `dev`:
  - `git diff main..dev` and review every new code path: SSE endpoint must go through `checkInspectAuth` (verify the route can't be reached without it); event payloads and `writeMetadata` must never contain key material; injection must only rewrite auth on self-referential requests, never on transparent pass-through traffic
  - Confirm the daemon still binds loopback by default and that `llmproxy serve` doesn't weaken that
  - Check `.env` handling: file never logged, never committed (`git log --all -- .env` must be empty), `.gitignore` covers it; consider having the loader warn (not fail) when `.env` permissions are group/world-readable
  - Note findings in `docs/testing/agent-matrix.md`'s related scratch space or fix them directly if small — this pass is verify-and-fix, not a new report

- [ ] Run the full validation sweep on `dev`:
  - `go fmt ./...` (no diff), `go vet ./...`, `go build ./...`, `make build`, `bash -n llmproxy` and `bash -n scripts/test-agents.sh`
  - Fresh-clone sanity: `git stash list` empty, `make build` succeeds from a clean checkout of `dev` in a temp dir (`git worktree add` or `git clone -b dev . /tmp/llmproxy-check`), confirming no untracked-file dependency (`.env.example` must be tracked; `.env` must not be required to build)
  - Smoke the product once more end-to-end: `llmproxy serve` → curl `/v1/messages` with no auth → 200 + completion → dashboard shows the request live → stop
  - Re-run `./scripts/test-agents.sh` once and confirm the matrix verdicts still hold

- [ ] Merge `dev` back to `main`:
  - Commit or stash any stragglers so `dev` is clean, then `git checkout main && git merge dev` (a merge commit, not squash, so the phase history survives; use `--no-ff` if it would fast-forward)
  - Confirm `main` builds (`make build`) and the README renders sensibly (quick read of the diff)
  - Do NOT push — that's the user's call; leave both `main` and `dev` local and report the merge result
  - Switch back to `dev` afterwards so future feature work starts in the right place

- [ ] Leave the workspace in a legible state for the user:
  - Update the `docs/agents/README.md` overview table and `docs/testing/agent-matrix.md` with final statuses if the validation sweep changed anything
  - Clean scratch: remove `.maestro/playbooks/Initiation/Working/` leftovers and any `logs-test/` dirs (they're throwaway), keep `logs/` untouched
  - Print a final summary: what shipped on `main` (credential-free proxy, live dashboard, guides, matrix), the current branch (`dev`), and any open items (e.g. agents that were not installed and still need a manual test run)
