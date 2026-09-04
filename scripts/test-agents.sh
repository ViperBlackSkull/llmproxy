#!/bin/bash

# test-agents.sh — Phase 04 credential-free test harness.
#
# Runs each supported coding agent (claude, codex, opencode, pi) against the
# credential-free proxy (`llmproxy serve`) with ZERO real credentials in the
# agent, and prints a one-line verdict per agent:
#
#     PASS|FAIL|SKIPPED <agent> <reason>
#
# Per agent it will: check the binary exists (skip if absent), snapshot every
# config file it must edit (cp to <file>.llmproxy-backup alongside, restored
# via an EXIT trap), apply the Phase 03 wiring snippet from docs/agents/,
# export dummy env keys, start `llmproxy serve` on a free port with a clean
# LOG_DIR (logs-test/<agent>/ so real logs stay clean), run the agent's
# one-shot prompt under `timeout 90`, capture exit code + output tail + new
# capture files, restore all configs, and print the verdict.
#
# Usage:
#   ./scripts/test-agents.sh            # test all four agents
#   ./scripts/test-agents.sh claude     # test a single agent
#   ./scripts/test-agents.sh opencode|codex|pi
#
# Evidence left behind (for docs/testing/agent-matrix.md):
#   logs-test/<agent>/daemon.log         proxy daemon console output
#   logs-test/<agent>/agent-output.log   full agent stdout+stderr
#   logs-test/<agent>/proxy/*.md|.req.json   captured traffic (redacted)
#   logs-test/<agent>.verdict            one-line verdict
#
# Golden rules (from the playbook):
#   1. Never write a real API key into any agent config — dummies only; the
#      proxy injects the real one from .env when forwarding upstream.
#   2. Every config file touched is backed up before editing and restored
#      after the test — no exceptions. If the script is SIGKILLed mid-test,
#      restore manually: mv <file>.llmproxy-backup <file>
#
# The script never prints secrets: all captured output is piped through
# `redact` (same key-shape patterns as the daemon's redactSecrets) first.

set -u

SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
LLMPROXY="$REPO_ROOT/llmproxy"
LOGS_TEST="$REPO_ROOT/logs-test"

TIMEOUT_SECS=90
DUMMY_KEY="llmproxy-dummy"
PROMPT="Reply with exactly: OK"

ALL_AGENTS=(claude opencode codex pi)

# Per-test state (each test runs in its own subshell, so these never leak
# between agents; cleanup() drains them, making it idempotent).
BACKUPS=()        # config files snapshotted to <file>.llmproxy-backup
CREATED_FILES=()  # files the harness created from scratch (deleted on cleanup)
CREATED_DIRS=()   # dirs the harness created (rmdir'd on cleanup if empty)
PROXY_PID=""

# --- Secret redaction (mirrors redactSecrets in main.go) ---------------------

redact() {
    sed -E \
        -e 's/sk-ant-[A-Za-z0-9_-]{20,}/<REDACTED>/g' \
        -e 's/sk-[A-Za-z0-9]{20,}/<REDACTED>/g' \
        -e 's/(Bearer[[:space:]]+)[A-Za-z0-9._-]{16,}/\1<REDACTED>/g' \
        -e 's/(x-api-key:[[:space:]]*)[A-Za-z0-9._-]{16,}/\1<REDACTED>/Ig' \
        -e 's/AKIA[0-9A-Z]{16}/<REDACTED>/g' \
        -e 's/gh[pousrp]_[A-Za-z0-9]{30,}/<REDACTED>/g' \
        -e 's/[A-Fa-f0-9]{32,}/<REDACTED>/g' \
        -e 's/[A-Za-z0-9+\/]{40,}={0,2}/<REDACTED>/g'
}

# --- Backup / restore machinery (same pattern as the wrapper's patch_json) ---

backup_file() { # backup_file <path> — snapshot to <path>.llmproxy-backup
    local f="$1"
    [[ -f "$f" ]] || return 1
    cp -p "$f" "$f.llmproxy-backup" || return 1
    BACKUPS+=("$f")
}

jq_into() { # jq_into <dest> <jq-filter-args...> — read dest's backup, write dest
    local dest="$1"
    shift
    local tmp="$dest.llmproxy-new"
    if ! jq "$@" "$dest.llmproxy-backup" > "$tmp"; then
        rm -f "$tmp"
        return 1
    fi
    mv "$tmp" "$dest"
}

ensure_dir() { # ensure_dir <path> — mkdir -p, remembering dirs we created
    local d="$1"
    if [[ ! -d "$d" ]]; then
        mkdir -p "$d" || return 1
        CREATED_DIRS+=("$d")
    fi
}

create_file() { # create_file <path> — mark a from-scratch config for cleanup
    CREATED_FILES+=("$1")
}

restore_configs() {
    local f i
    for f in ${BACKUPS[@]+"${BACKUPS[@]}"}; do
        if [[ -f "$f.llmproxy-backup" ]]; then
            mv -f "$f.llmproxy-backup" "$f"
        fi
    done
    BACKUPS=()
    for f in ${CREATED_FILES[@]+"${CREATED_FILES[@]}"}; do
        rm -f "$f"
    done
    CREATED_FILES=()
    for ((i = ${#CREATED_DIRS[@]} - 1; i >= 0; i--)); do
        rmdir "${CREATED_DIRS[$i]}" 2>/dev/null || true
    done
    CREATED_DIRS=()
}

stop_proxy() {
    if [[ -n "$PROXY_PID" ]]; then
        kill "$PROXY_PID" 2>/dev/null || true
        wait "$PROXY_PID" 2>/dev/null || true
        PROXY_PID=""
    fi
}

cleanup() {
    restore_configs   # golden rule #2: configs restored first, no exceptions
    stop_proxy
}

# --- Proxy lifecycle -----------------------------------------------------------

pick_port() { # first free port from the test range (above the wrapper's 8765-8770)
    local p
    for p in 8785 8786 8787 8788 8789 8790 8791 8792; do
        if ! ss -tlnH 2>/dev/null | grep -q ":$p "; then
            echo "$p"
            return 0
        fi
    done
    return 1
}

start_proxy() { # start_proxy <port> <agent> — background `llmproxy serve`, wait ready
    local port="$1" agent="$2"
    local log_dir="$LOGS_TEST/$agent"

    # `serve` execs the daemon, so this subshell's PID *becomes* llmproxyd —
    # killing PROXY_PID kills the daemon. LOG_DIR must be absolute: serve
    # cd's to the repo root (for .env) before exec'ing.
    (
        cd "$REPO_ROOT" || exit 1
        exec env LOG_DIR="$log_dir" "$LLMPROXY" serve "$port"
    ) > "$log_dir/daemon.log" 2>&1 &
    PROXY_PID=$!

    # Readiness probe: /__inspect__ answers 404 (token-gated) once the daemon
    # listens — and unlike GET / it is never forwarded upstream.
    local i code
    for i in $(seq 1 50); do
        code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 1 \
            "http://127.0.0.1:$port/__inspect__" 2>/dev/null || true)
        if [[ -n "$code" && "$code" != "000" ]]; then
            return 0
        fi
        kill -0 "$PROXY_PID" 2>/dev/null || break
        sleep 0.1
    done
    return 1
}

# --- Per-agent config patches (the Phase 03 snippets from docs/agents/) -------

patch_claude() {
    # docs/agents/claude-code.md: settings.json env beats shell exports, so the
    # file itself must carry the redirect + dummy token.
    local cfg="$HOME/.claude/settings.json"
    ensure_dir "$HOME/.claude" || return 1
    if [[ -f "$cfg" ]]; then
        backup_file "$cfg" || return 1
        jq_into "$cfg" --arg url "$PROXY_URL" --arg tok "$DUMMY_KEY" \
            '.env = ((.env // {}) + {ANTHROPIC_BASE_URL: $url, ANTHROPIC_AUTH_TOKEN: $tok})' \
            || return 1
    else
        jq -n --arg url "$PROXY_URL" --arg tok "$DUMMY_KEY" \
            '{env: {ANTHROPIC_BASE_URL: $url, ANTHROPIC_AUTH_TOKEN: $tok}}' > "$cfg" \
            || return 1
        create_file "$cfg"
    fi
}

patch_opencode() {
    # docs/agents/opencode.md: provider entry with baseURL (keep /v1) + dummy
    # apiKey; the model is passed on the command line, not patched into the
    # user's default.
    local cfg="$HOME/.config/opencode/opencode.json"
    local entry
    entry=$(jq -n --arg url "$PROXY_URL/v1" --arg key "$DUMMY_KEY" '{
        npm: "@ai-sdk/openai-compatible",
        name: "llmproxy (local)",
        options: {baseURL: $url, apiKey: $key},
        models: {"glm-5.3": {name: "GLM 5.3 (via llmproxy)", attachment: true, reasoning: true}}
    }') || return 1
    ensure_dir "$HOME/.config/opencode" || return 1
    if [[ -f "$cfg" ]]; then
        backup_file "$cfg" || return 1
        jq_into "$cfg" --argjson e "$entry" '.provider.llmproxy = $e' || return 1
    else
        jq -n --argjson e "$entry" \
            '{"$schema": "https://opencode.ai/config.json", provider: {llmproxy: $e}}' > "$cfg" \
            || return 1
        create_file "$cfg"
    fi
}

patch_codex() {
    # docs/agents/codex.md: top-level model_provider + model, plus a
    # [model_providers.llmproxy] block (wire_api = "responses"). TOML, so
    # first-occurrence sed replaces / prepends rather than jq.
    local cfg="$HOME/.codex/config.toml"
    ensure_dir "$HOME/.codex" || return 1
    if [[ ! -f "$cfg" ]]; then
        {
            echo 'model_provider = "llmproxy"'
            echo 'model = "glm-5.3"'
            echo
            echo '[model_providers.llmproxy]'
            echo 'name = "llmproxy (local)"'
            echo "base_url = \"$PROXY_URL/v1\""
            echo 'env_key = "OPENAI_API_KEY"'
            echo 'wire_api = "responses"'
        } > "$cfg" || return 1
        create_file "$cfg"
    else
        backup_file "$cfg" || return 1
        # Top-level keys: replace the first existing assignment, else prepend
        # (must stay above any [table] header to remain top-level).
        if grep -q '^model_provider[[:space:]]*=' "$cfg"; then
            sed -i '0,/^model_provider[[:space:]]*=/{s//model_provider = "llmproxy"/}' "$cfg" || return 1
        else
            sed -i '1i model_provider = "llmproxy"' "$cfg" || return 1
        fi
        if grep -q '^model[[:space:]]*=' "$cfg"; then
            sed -i '0,/^model[[:space:]]*=/{s//model = "glm-5.3"/}' "$cfg" || return 1
        else
            sed -i '1i model = "glm-5.3"' "$cfg" || return 1
        fi
        # Provider block: refresh base_url if present, else append the block.
        if grep -q '^\[model_providers\.llmproxy\]' "$cfg"; then
            awk -v url="$PROXY_URL/v1" '
                /^\[/ { inblock = ($0 ~ /^\[model_providers\.llmproxy\]/) }
                inblock && /^base_url[[:space:]]*=/ { print "base_url = \"" url "\""; next }
                { print }
            ' "$cfg" > "$cfg.llmproxy-new" && mv "$cfg.llmproxy-new" "$cfg" || return 1
        else
            {
                echo
                echo '[model_providers.llmproxy]'
                echo 'name = "llmproxy (local)"'
                echo "base_url = \"$PROXY_URL/v1\""
                echo 'env_key = "OPENAI_API_KEY"'
                echo 'wire_api = "responses"'
            } >> "$cfg" || return 1
        fi
    fi
}

patch_pi() {
    # docs/agents/pi.md: provider catalog entry (array) + profile env block.
    # defaultProvider is NOT patched — the per-run --provider flag selects
    # llmproxy, leaving the standing default untouched.
    local prov="$HOME/.pi/agent/providers.json"
    local profile_dir="$HOME/.pi/agent/llmproxy"
    local profile="$profile_dir/settings.json"
    local entry
    entry=$(jq -n --arg url "$PROXY_URL" '{
        id: "llmproxy",
        name: "LLM Proxy (local, no secrets)",
        profile: "llmproxy",
        base_url_default: $url,
        test_path: "/__inspect__",
        needs_key: false,
        needs_proxy: false,
        env: {},
        models: ["glm-5.3"]
    }') || return 1

    ensure_dir "$HOME/.pi/agent" || return 1
    if [[ -f "$prov" ]]; then
        backup_file "$prov" || return 1
        jq_into "$prov" --argjson e "$entry" 'map(select(.id != "llmproxy")) + [$e]' \
            || return 1
    else
        printf '[%s]\n' "$entry" > "$prov" || return 1
        create_file "$prov"
    fi

    ensure_dir "$profile_dir" || return 1
    if [[ -f "$profile" ]]; then
        backup_file "$profile" || return 1
    else
        create_file "$profile"
    fi
    jq -n --arg url "$PROXY_URL" --arg tok "$DUMMY_KEY" '{
        env: {
            ANTHROPIC_BASE_URL: $url,
            ANTHROPIC_AUTH_TOKEN: $tok,
            OPENAI_BASE_URL: ($url + "/v1"),
            OPENAI_API_KEY: $tok,
            CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1"
        }
    }' > "$profile" || return 1
}

# --- One-shot agent commands ---------------------------------------------------

# Intended child env per agent (applied AFTER the inherited env is sanitized —
# see test_agent). Dummies only; the proxy injects real keys upstream.
agent_env_claude()   { export ANTHROPIC_BASE_URL="$PROXY_URL" ANTHROPIC_AUTH_TOKEN="$DUMMY_KEY"; }
agent_env_codex()    { export OPENAI_API_KEY="$DUMMY_KEY"; } # config.toml env_key must resolve
agent_env_opencode() { :; } # dummy lives in the config's options.apiKey
agent_env_pi()       { :; } # dummies live in the profile settings.json

run_one_shot() { # cwd = scratch dir; combined output goes to $OUT_FILE
    case "$AGENT" in
        claude)
            timeout "$TIMEOUT_SECS" claude -p "$PROMPT" ;;
        codex)
            timeout "$TIMEOUT_SECS" codex exec --skip-git-repo-check "$PROMPT" ;;
        opencode)
            timeout "$TIMEOUT_SECS" opencode run --model "llmproxy/glm-5.3" "$PROMPT" ;;
        pi)
            timeout "$TIMEOUT_SECS" pi --provider llmproxy --model glm-5.3 -p "$PROMPT" ;;
    esac > "$OUT_FILE" 2>&1
}

# --- Test driver (runs in a subshell for env/trap isolation) -------------------

verdict() { # verdict <PASS|FAIL|SKIPPED> <agent> <reason>
    local line="$1 $2 $3"
    echo "$line"
    mkdir -p "$LOGS_TEST"
    echo "$line" > "$LOGS_TEST/$2.verdict"
}

test_agent() {
    local agent="$1"
    AGENT="$agent"

    trap cleanup EXIT
    trap 'cleanup; exit 130' INT TERM

    local bin
    case "$agent" in
        claude) bin="claude" ;;
        codex) bin="codex" ;;
        opencode) bin="opencode" ;;
        pi) bin="pi" ;;
    esac

    echo
    echo "=================================================================="
    echo "=== $agent"
    echo "=================================================================="

    if ! command -v "$bin" &>/dev/null; then
        verdict "SKIPPED" "$agent" "binary '$bin' not found in PATH"
        return 0
    fi
    case "$agent" in
        claude | opencode | pi)
            if ! command -v jq &>/dev/null; then
                verdict "SKIPPED" "$agent" "jq not found (required to patch config)"
                return 0
            fi
            ;;
    esac

    # Clean LOG_DIR so real logs stay clean and evidence is unambiguous.
    local log_test="$LOGS_TEST/$agent"
    rm -rf "$log_test"
    mkdir -p "$log_test"

    # Start a fresh credential-free proxy.
    local port
    if ! port=$(pick_port); then
        verdict "FAIL" "$agent" "no free port in test range 8785-8792"
        return 0
    fi
    PROXY_URL="http://localhost:$port"
    if ! start_proxy "$port" "$agent"; then
        echo "  proxy did not become ready on :$port; daemon.log tail (redacted):"
        tail -n 10 "$log_test/daemon.log" 2>/dev/null | redact | sed 's/^/    /'
        verdict "FAIL" "$agent" "llmproxy serve failed to start (see logs-test/$agent/daemon.log)"
        return 0
    fi
    echo "  proxy: $PROXY_URL (pid $PROXY_PID, logs: logs-test/$agent/)"

    # Apply the Phase 03 wiring snippet + dummy env keys.
    if ! "patch_$agent"; then
        verdict "FAIL" "$agent" "config patch failed (see above); configs restored"
        return 0
    fi

    # Run the one-shot prompt from a scratch cwd with a sanitized env. The
    # harness often runs from inside an agent session (Maestro/Claude Code),
    # whose inherited vars — model pins, foreign base URLs, proxy settings,
    # nested-session markers — would contaminate the test. Strip them, then
    # apply this agent's intended dummy env. Note Claude Code's settings.json
    # env block still wins over the child's shell env by design — that file is
    # the machine's real standing config and is patched above.
    OUT_FILE="$log_test/agent-output.log"
    local work_dir="$log_test/agent-cwd"
    mkdir -p "$work_dir"
    local rc
    (
        cd "$work_dir" || exit 1
        unset ANTHROPIC_API_KEY ANTHROPIC_BASE_URL ANTHROPIC_API_BASE_URL \
            ANTHROPIC_AUTH_TOKEN ANTHROPIC_MODEL \
            ANTHROPIC_DEFAULT_OPUS_MODEL ANTHROPIC_DEFAULT_SONNET_MODEL \
            ANTHROPIC_DEFAULT_HAIKU_MODEL ANTHROPIC_ORGANIZATION_ID \
            ANTHROPIC_FEDERATION_RULE_ID ANTHROPIC_IDENTITY_TOKEN_FILE \
            OPENAI_API_KEY OPENAI_BASE_URL OPENAI_API_BASE_URL \
            CLAUDE_CONFIG_DIR CLAUDECODE CLAUDE_PID CLAUDE_CODE_ENTRYPOINT \
            CLAUDE_CODE_SESSION_ID CLAUDE_CODE_CHILD_SESSION \
            CLAUDE_CODE_MESSAGING_SOCKET CLAUDE_CODE_MESSAGING_TOKEN \
            CLAUDE_AGENT_API_BASE_URL CCR_CLAUDE_CODE_BIN \
            CCR_CLAUDE_CODE_MODEL CODEXL_CLAUDE_CODE_MODEL \
            HTTPS_PROXY HTTP_PROXY NO_PROXY https_proxy http_proxy no_proxy \
            NODE_EXTRA_CA_CERTS SSL_CERT_FILE
        "agent_env_$AGENT"
        run_one_shot
    )
    rc=$?

    # Evidence: did new capture files appear?
    local md_count req_count evidence
    md_count=$(find "$log_test" -type f -name '*.md' 2>/dev/null | wc -l)
    req_count=$(find "$log_test" -type f -name '*.req.json' 2>/dev/null | wc -l)
    evidence=$(find "$log_test" -type f \( -name '*.md' -o -name '*.req.json' \) 2>/dev/null \
        | sed "s|^$log_test/||" | sort | tr '\n' ' ')
    local out_size
    out_size=$(wc -c < "$OUT_FILE" 2>/dev/null || echo 0)

    echo "  agent rc: $rc"
    echo "  captured: $md_count .md + $req_count .req.json file(s)"
    [[ -n "$evidence" ]] && echo "  evidence: $evidence"
    echo "  output tail (redacted, last 15 lines):"
    tail -n 15 "$OUT_FILE" 2>/dev/null | redact | sed 's/^/    /'

    # Verdict: exit 0 + request reached the proxy + agent produced output.
    local reason=""
    [[ $rc -eq 124 ]] && reason="timed out after ${TIMEOUT_SECS}s"
    [[ $rc -ne 0 && $rc -ne 124 ]] && reason="agent rc=$rc"
    [[ $md_count -eq 0 ]] && reason="${reason:+$reason; }no request reached the proxy (0 capture files)"
    [[ $req_count -eq 0 && $md_count -gt 0 ]] && reason="${reason:+$reason; }.req.json missing for captured request"
    [[ $out_size -eq 0 ]] && reason="${reason:+$reason; }agent produced no output"
    if [[ -z "$reason" ]]; then
        verdict "PASS" "$agent" \
            "exit=0, $md_count request(s) captured in logs-test/$agent, agent produced output"
    else
        verdict "FAIL" "$agent" "$reason (evidence: logs-test/$agent/)"
    fi
    return 0
}

# --- Entry point ----------------------------------------------------------------

usage() {
    cat <<EOF
Usage: ./scripts/test-agents.sh [agent]

Run the Phase 04 credential-free test matrix: each agent is patched to point
at a fresh \`llmproxy serve\` with dummy keys only, gets a one-shot prompt,
and is restored afterwards. Verdicts: PASS|FAIL|SKIPPED per agent.

  agent    claude | codex | opencode | pi | all   (default: all)

Evidence lands in logs-test/<agent>/ ; configs are restored automatically.
EOF
}

main() {
    local selected=("${ALL_AGENTS[@]}")

    if [[ $# -gt 0 ]]; then
        case "$1" in
            help | --help | -h)
                usage
                exit 0
                ;;
            all) ;;
            claude | codex | opencode | pi)
                selected=("$1")
                ;;
            *)
                echo "Unknown agent: $1" >&2
                usage >&2
                exit 2
                ;;
        esac
    fi

    if [[ ! -x "$LLMPROXY" ]]; then
        echo "Error: $LLMPROXY not found or not executable." >&2
        exit 2
    fi
    if [[ ! -x "$REPO_ROOT/llmproxyd" ]]; then
        echo "Error: llmproxyd binary missing — run 'make build' first." >&2
        exit 2
    fi
    if [[ ! -f "$REPO_ROOT/.env" ]]; then
        echo "WARNING: no .env in repo root — upstream keys/backends missing, tests will likely 401." >&2
    fi

    mkdir -p "$LOGS_TEST"

    local agent any_fail=0
    for agent in "${selected[@]}"; do
        ( test_agent "$agent" )
    done

    echo
    echo "=================================================================="
    echo "=== Summary"
    echo "=================================================================="
    for agent in "${selected[@]}"; do
        local line
        line=$(head -n 1 "$LOGS_TEST/$agent.verdict" 2>/dev/null || echo "FAIL $agent no verdict recorded")
        echo "$line"
        [[ "$line" == FAIL* ]] && any_fail=1
    done

    exit "$any_fail"
}

main "$@"
