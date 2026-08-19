#!/bin/bash
# vornik-agent — LLM-backed agent
# Reads task.json, calls an OpenAI-compatible LLM endpoint, writes result.json.
#
# Bash is required, not sh: the tool-call loop uses associative arrays
# (FILE_READ_CACHE, FILE_READ_MISSES) for per-turn file_read caching and
# repeat-miss detection. dash doesn't support `declare -gA`.
#
# Required env vars (injected by vornik executor):
#   VORNIK_LLM_ENDPOINT  — base URL, e.g. http://host:11434/v1
#   VORNIK_LLM_MODEL     — model name, e.g. gemma4:26b
# Optional:
#   VORNIK_LLM_API_KEY   — bearer token (default: "none")

set -eu

# These can be overridden via env for testability. The production container
# always uses the /app/* defaults; test harnesses source this script with
# INPUT_FILE / WORKSPACE / etc. pointed at a temp dir.
INPUT_FILE="${INPUT_FILE:-/app/input/task.json}"

# output_contract_satisfied reports whether a file matching the step's declared
# REQUIRE_OUTPUT_GLOB exists in the workspace.
#
# Mirrors the daemon's post-exit check (executor/container.go outputGlobSatisfied)
# closely enough to catch the common miss BEFORE the container exits. It is
# deliberately the weaker test: existence only, no mtime window. A false
# "satisfied" here costs nothing — the daemon still runs the authoritative
# check — while a false "unsatisfied" would nudge an agent that had already
# done the work.
output_contract_satisfied() {
    [ -z "${REQUIRE_OUTPUT_GLOB:-}" ] && return 0
    local matches
    # shellcheck disable=SC2086 # the glob must expand
    matches=$(cd "$WORKSPACE" 2>/dev/null && ls -1 $REQUIRE_OUTPUT_GLOB 2>/dev/null | head -1)
    [ -n "$matches" ]
}

# plausibility_violations prints the gate-mode plausibility rules a candidate
# result.json breaks, as "rule: field" entries joined by "; ". Empty output
# means clean.
#
# WHY THIS RUNS IN THE CONTAINER. The daemon evaluates these rules AFTER the
# container exits (executor/container.go EvaluatePlausibility), and until
# 2026-08-16 nothing told the agent they existed. That was the single largest
# failure cause in the long-horizon arm: 32 of 73 steps set testing.passed=true
# without the fields passed_requires_pinned_validation demands and were failed
# for a contract they were never shown.
#
# The rules are deliberately absent from the provider JSON Schema
# (registry/output_schema.go: conditional draft-2019-09 support is uneven
# across providers), so the prompt and this check are the only channels.
#
# Mirrors EvaluatePlausibility's semantics exactly: a rule fires when EVERY
# entry in `when` matches — an empty `when` means always — and reports each
# `require` field that is missing or empty. warnOnly rules never gate.
plausibility_violations() {
    local candidate="$1"
    [ -z "${PLAUSIBILITY_RULES:-}" ] && return 0
    [ "$(printf '%s' "$PLAUSIBILITY_RULES" | jq 'length' 2>/dev/null || echo 0)" -gt 0 ] || return 0
    printf '%s' "$candidate" | jq -e . >/dev/null 2>&1 || return 0
    printf '%s' "$PLAUSIBILITY_RULES" | jq -r --argjson res "$candidate" '
        [ .[]
          | select(.warnOnly != true)
          | . as $rule
          | select(
              ($rule.when // {}) | to_entries
              | all(. as $c | ($res | getpath($c.key | split("."))) == $c.value)
            )
          | ($rule.require // [])[] as $f
          | ($res | getpath($f | split("."))) as $v
          | select($v == null or $v == "" or $v == [] or $v == {})
          | "\($rule.name): \($f)"
        ] | join("; ")' 2>/dev/null
}


OUTPUT_FILE="${OUTPUT_FILE:-/app/output/result.json}"
CANCEL_FILE="${CANCEL_FILE:-/app/input/CANCEL}"
WORKSPACE="${WORKSPACE:-/app/workspace}"
AGENT_HELPER="${VORNIK_AGENT_HELPER:-vornik-agent-helper}"
START_TIME="${START_TIME:-$(if command -v "$AGENT_HELPER" >/dev/null 2>&1; then "$AGENT_HELPER" now-seconds; else date +%s; fi)}"

# Defaults
LLM_ENDPOINT="${VORNIK_LLM_ENDPOINT:-}"
LLM_MODEL="${VORNIK_LLM_MODEL:-}"
LLM_API_KEY="${VORNIK_LLM_API_KEY:-none}"
LLM_CONTEXT_SIZE="${VORNIK_LLM_CONTEXT_SIZE:-0}"
# Max output tokens per LLM call. Gateways like bedrock-access-gateway default
# to 2048, which is too small for agents that write medium-to-large files.
# Set VORNIK_LLM_MAX_TOKENS to override; 0 means omit and rely on gateway default.
LLM_MAX_TOKENS="${VORNIK_LLM_MAX_TOKENS:-8192}"
MAX_TOOL_ITERATIONS="${VORNIK_MAX_TOOL_ITERATIONS:-30}"
SHELL_TIMEOUT="${VORNIK_SHELL_TIMEOUT:-300}"
# Max bytes of a single tool result fed back to the model. Historically a hard
# 50 KB; raised to 256 KiB so large web_fetch / read_many_files results survive
# for big-context models, and operator-tunable per role via
# agent_llm.tool_result_max_bytes (→ VORNIK_TOOL_RESULT_MAX_BYTES) — set it
# higher (e.g. 1-4 MiB) for 1M-context models.
TOOL_RESULT_MAX_BYTES="${VORNIK_TOOL_RESULT_MAX_BYTES:-262144}"
# Max wall-clock seconds for a single LLM HTTP call. The daemon derives this
# from chat.timeout (or runtime.agent_llm.timeout) and passes it in as
# VORNIK_LLM_TIMEOUT. Fallback default is 300s (5 minutes) — long enough for
# most large-model completions, short enough that a stalled TCP connection
# fails the task instead of wedging it for hours.
LLM_TIMEOUT="${VORNIK_LLM_TIMEOUT:-300}"
AGENT_DEFER_MCP_TOOLS="${VORNIK_AGENT_DEFER_MCP_TOOLS:-1}"
AGENT_DEFER_MCP_THRESHOLD="${VORNIK_AGENT_DEFER_MCP_THRESHOLD:-20}"
AGENT_TOOL_SEARCH_LIMIT="${VORNIK_AGENT_TOOL_SEARCH_LIMIT:-8}"
# 2026-07-21: default flipped ON (rolled out). Lossless — old large tool
# results are compacted to head/tail with full bodies preserved under
# .tool_results/ and rehydratable via tool_result_read. Set
# VORNIK_TOOL_RESULT_HYGIENE=0 to disable per-agent if needed.
TOOL_RESULT_HYGIENE="${VORNIK_TOOL_RESULT_HYGIENE:-1}"
TOOL_RAW_COMPACT_CHARS="${VORNIK_TOOL_RAW_COMPACT_CHARS:-12000}"
TOOL_RAW_KEEP_RECENT_BATCHES="${VORNIK_TOOL_RAW_KEEP_RECENT_BATCHES:-3}"
TOOL_RAW_HEAD_CHARS="${VORNIK_TOOL_RAW_HEAD_CHARS:-1200}"
TOOL_RAW_TAIL_CHARS="${VORNIK_TOOL_RAW_TAIL_CHARS:-800}"
STEP_PROMPT_TOKEN_BUDGET="${VORNIK_STEP_PROMPT_TOKEN_BUDGET:-0}"

# Per-million-token prices for this container's model. Injected by the
# executor from the daemon's pricing.yaml so the agent can log per-iteration
# cost estimates without reaching back across the daemon. Missing/empty
# values keep cost logging at 0.00 — tokens still log, just without the $.
LLM_COST_INPUT_PER_M="${VORNIK_LLM_COST_INPUT_PER_M:-0}"
LLM_COST_OUTPUT_PER_M="${VORNIK_LLM_COST_OUTPUT_PER_M:-0}"

log() { echo "[vornik-agent] $1"; }
debug() { [ "${VORNIK_LOG_LEVEL:-info}" = "debug" ] && echo "[vornik-agent] $1" || true; }

is_truthy() {
    case "${1:-}" in
        ""|0|false|FALSE|False|no|NO|No|off|OFF|Off) return 1 ;;
        *) return 0 ;;
    esac
}

tool_result_hygiene_enabled() {
    is_truthy "$TOOL_RESULT_HYGIENE" || return 1
    local threshold="${TOOL_RAW_COMPACT_CHARS:-12000}"
    case "$threshold" in
        -*)
            ;;
        ""|*[!0-9]*)
            threshold=12000
            ;;
    esac
    [ "$threshold" -gt 0 ] 2>/dev/null
}

step_prompt_token_budget() {
    local budget="${STEP_PROMPT_TOKEN_BUDGET:-0}"
    case "$budget" in
        ""|*[!0-9]*)
            budget=0
            ;;
    esac
    printf '%s' "$budget"
}

# ms_now returns milliseconds-since-epoch as a portable 13-digit value.
# The agent image's `date` is from coreutils-rust, which silently
# treats `%3N` as `%N` and emits the FULL nanosecond count (9 digits)
# instead of the requested millisecond count (3 digits). The result is
# a 19-digit nanosecond-since-epoch string. Tool-call duration deltas
# computed against that value overflow the daemon's `duration_ms`
# integer column (max ~2.1B); the daemon then fails the audit ingest
# with `pq: value "..." is out of range for type integer` and the
# tool_is_cacheable_read — true when (tool_name) is a read-only
# read-idempotent tool whose response can be safely served from
# the per-turn TOOL_READ_CACHE on a duplicate call. Excludes:
#   - place_order / cancel_order: actions, not reads
#   - get_quote: caller wants the freshest mid-spread
#   - current_time: cheap; agents occasionally re-check before
#     time-sensitive decisions
#   - file_read: handled by FILE_READ_CACHE separately
# Default-deny: an unrecognised tool is NOT cached so a future
# write-action tool added to the allow-list doesn't accidentally
# get its mutating call elided.
tool_is_cacheable_read() {
    case "$1" in
        mcp__broker__get_historical_bars|\
        mcp__broker__get_account_summary|\
        mcp__broker__get_positions|\
        mcp__broker__get_orders|\
        mcp__ta__sma|mcp__ta__ema|mcp__ta__rsi|\
        mcp__ta__macd|mcp__ta__bbands|mcp__ta__atr|\
        mcp__news__news_recent|mcp__news__fundamentals_snapshot|\
        memory_search|get_conversation_window|skill_fetch)
            return 0
            ;;
    esac
    return 1
}

# row only lands via the post-step batch (degraded audit fidelity).
# Detect the over-precise output and divide back to ms.
ms_now() {
    local raw
    raw=$(date +%s%3N)
    # GNU date -> 13 digits (e.g. 1762345678123). coreutils-rust ->
    # 19 digits (e.g. 1762345678123456789). Anything 14+ chars is
    # nanoseconds; convert to ms by integer-dividing the trailing
    # 6 digits off.
    if [ ${#raw} -ge 14 ]; then
        raw=$(( raw / 1000000 ))
    fi
    printf '%s' "$raw"
}

get_duration() {
    if command -v "$AGENT_HELPER" >/dev/null 2>&1; then
        "$AGENT_HELPER" duration-seconds "$START_TIME"
    else
        echo $(( $(date +%s) - START_TIME ))
    fi
}

allowed_builtin_tools_json() {
    jq -c '((.config.permissions.allowedTools // ["file_read","file_write","run_shell"]) + ["current_time"] | unique)' "$INPUT_FILE" 2>/dev/null \
        || printf '%s\n' '["current_time","file_read","file_write","run_shell"]'
}

# SECURITY (2026-08-06): this list IS the execution-time allowlist gate. The
# caller in exec_tool refuses a tool only when
#   is_builtin_tool "$name" && ! builtin_tool_allowed "$name"
# so a name MISSING here makes the first conjunct false and the per-role
# allowlist check is skipped entirely — the tool runs for every role regardless
# of its allowedTools. The gate fails OPEN on omission.
#
# memory_search, skill_fetch, get_conversation_window and summarize_thread were
# each implemented with an exec_tool dispatch case and absent from this list,
# so every role could call them irrespective of allowedTools — defeating the
# role-library capability boundary that
# https://docs.vornik.io §5.3 documents as the
# outer bound of every composed automation.
#
# Do not add a dispatch case without adding the name here AND to
# BUILTIN_TOOL_NAMES_JSON AND to internal/agenttools.builtinTools.
# internal/contractreg's registry-disagreement check enforces that on every
# `make lint`; deliberate exemptions live in contractreg.UngatedByDesign with a
# stated reason, not as silent omissions.
is_builtin_tool() {
    case "$1" in
        file_read|file_write|run_shell|current_time) return 0 ;;
        file_edit|read_many_files|grep|glob) return 0 ;;
        git_status|git_diff|git_log|git_show) return 0 ;;
        test_run|lint_run|typecheck_run) return 0 ;;
        backlog_deposit|tool_result_read) return 0 ;;
        query_api|list_apis) return 0 ;;
        memory_search|skill_fetch) return 0 ;;
        get_conversation_window|summarize_thread) return 0 ;;
        *) return 1 ;;
    esac
}

# Canonical list of built-in tool names — single source of truth used by the
# allowlist gate in tool_definitions() and by builtin_tool_allowed(). Keep
# this aligned with is_builtin_tool() above.
BUILTIN_TOOL_NAMES_JSON='["file_read","file_write","run_shell","current_time","file_edit","read_many_files","grep","glob","git_status","git_diff","git_log","git_show","test_run","lint_run","typecheck_run","backlog_deposit","tool_result_read","query_api","list_apis","memory_search","skill_fetch","get_conversation_window","summarize_thread"]'

builtin_tool_allowed() {
    local tool="$1"
    allowed_builtin_tools_json | jq -e --arg tool "$tool" 'index($tool) != null' >/dev/null 2>&1
}

CANCELLED=0
STEP_ID="unknown"
check_cancel() {
    if [ -f "$CANCEL_FILE" ]; then
        log "cancellation requested"
        write_result "CANCELLED" "Agent was cancelled" "" "$(get_duration)"
        CANCELLED=1
    fi
}

# Remove <think>/<thinking>/<reasoning> blocks from LLM output. gpt-oss,
# DeepSeek-R1, and Qwen reasoning variants emit chain-of-thought inline
# alongside the final answer; left in, it leaks into artifact files and
# the result.json message field forwarded to downstream plan roles.
# Uses python3 (already installed) for reliable multiline non-greedy
# regex with \s handling.
strip_reasoning() {
    if command -v "$AGENT_HELPER" >/dev/null 2>&1; then
        "$AGENT_HELPER" strip-reasoning
        return
    fi
    python3 -c '
import re, sys
sys.stdout.write(
    re.sub(r"<(think|thinking|reasoning)>.*?</(think|thinking|reasoning)>\s*",
           "", sys.stdin.read(), flags=re.DOTALL).strip()
)
' 2>/dev/null
}

# write_result STATUS MESSAGE RESPONSE DURATION [ERROR]
# clamp_tool_contents FILE CAP — truncate every tool-role message whose
# content exceeds CAP bytes in the JSON messages FILE, appending a visible
# truncation marker. Guarantees the conversation can be brought under the
# model's context budget no matter how fat individual tool results are —
# keep-tail compaction alone cannot (2026-07-12 incident: six ~256KB scraper
# results in the kept tail exceeded glm-5's whole window and every LLM call
# 400'd deterministically; task_20260712145902_18667395d2826b72).
clamp_tool_contents() {
    local file="$1" cap="$2"
    jq --argjson cap "$cap" '
        map(if .role == "tool" and ((.content // "") | length) > $cap
            then .content = (.content[:$cap] + "\n…[tool result truncated to fit the model context window]")
            else . end)
    ' "$file" > "$file.tmp" && mv "$file.tmp" "$file"
}

compact_old_tool_results() {
    local file="$1"
    if ! tool_result_hygiene_enabled; then
        return 0
    fi
    TOOL_RAW_COMPACT_CHARS="$TOOL_RAW_COMPACT_CHARS" \
    TOOL_RAW_KEEP_RECENT_BATCHES="$TOOL_RAW_KEEP_RECENT_BATCHES" \
    TOOL_RAW_HEAD_CHARS="$TOOL_RAW_HEAD_CHARS" \
    TOOL_RAW_TAIL_CHARS="$TOOL_RAW_TAIL_CHARS" \
    python3 - "$file" <<'PY'
import json
import os
import re
import sys

path = sys.argv[1]

def int_env(name, default):
    try:
        return int(os.environ.get(name, "") or default)
    except ValueError:
        return default

threshold = int_env("TOOL_RAW_COMPACT_CHARS", 12000)
if threshold <= 0:
    raise SystemExit(0)
keep_batches = max(1, int_env("TOOL_RAW_KEEP_RECENT_BATCHES", 3))
head_chars = max(200, int_env("TOOL_RAW_HEAD_CHARS", 1200))
tail_chars = max(200, int_env("TOOL_RAW_TAIL_CHARS", 800))

with open(path, "r", encoding="utf-8") as f:
    messages = json.load(f)

batch_for_tool_id = {}
batch_idx = 0
for msg in messages:
    if msg.get("role") == "assistant" and isinstance(msg.get("tool_calls"), list):
        batch_idx += 1
        for call in msg.get("tool_calls") or []:
            tool_id = call.get("id")
            if tool_id:
                batch_for_tool_id[tool_id] = batch_idx

keep_from = max(1, batch_idx - keep_batches + 1)
changed = False

for msg in messages:
    if msg.get("role") != "tool":
        continue
    tool_id = msg.get("tool_call_id") or ""
    if not re.fullmatch(r"[A-Za-z0-9_.:-]+", tool_id):
        continue
    msg_batch = batch_for_tool_id.get(tool_id, 0)
    if msg_batch >= keep_from:
        continue
    content = msg.get("content")
    if not isinstance(content, str):
        continue
    original_len = len(content)
    if original_len <= threshold:
        continue
    if head_chars + tail_chars >= original_len:
        continue
    head = content[:head_chars]
    tail = content[-tail_chars:]
    ref = f".tool_results/{tool_id}.txt" if tool_id else ".tool_results/<unknown>.txt"
    msg["content"] = (
        f"[tool result compacted: tool_call_id={tool_id} "
        f"original_chars={original_len} retained_head_chars={head_chars} "
        f"retained_tail_chars={tail_chars} full_result_ref={ref}]\n\n"
        "--- retained head ---\n"
        f"{head}\n\n"
        "--- retained tail ---\n"
        f"{tail}"
    )
    changed = True

if changed:
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(messages, f, ensure_ascii=False, separators=(",", ":"))
        f.write("\n")
    os.replace(tmp, path)
PY
}

# in_recovery_hop reports whether this step is a lead RECOVERY hop — the
# executor populates context.recovery when a prior step failed and the workflow
# routed here to propose alternatives (agent_input_context.go).
#
# Used to suppress the missing-prerequisite bail: see its call site. Fails
# CLOSED — a missing or unparseable input file reads as "ordinary step", so the
# guard stays armed by default rather than being silently disabled.
in_recovery_hop() {
    [ -n "${INPUT_FILE:-}" ] && [ -f "$INPUT_FILE" ] || return 1
    local rec
    rec=$(jq -r '.context.recovery // empty' "$INPUT_FILE" 2>/dev/null) || return 1
    [ -n "$rec" ]
}

write_result() {
    local status="$1" message="$2" response="$3" duration="$4" error="${5:-}"

    # Strip chain-of-thought tags before anything downstream (artifact
    # file, result.json message, structured-JSON merge) sees the text.
    if [ -n "$message" ]; then
        message=$(printf '%s' "$message" | strip_reasoning)
    fi
    if [ -n "$response" ]; then
        response=$(printf '%s' "$response" | strip_reasoning)
    fi

    # Always write a per-step artifact so every workflow step's output
    # is preserved (e.g. plan-response.md, implement-response.md).
    # Use response if available, otherwise fall back to the message.
    local response_name="${STEP_ID}-response.md"
    local artifact_content="${response:-$message}"
    mkdir -p "$WORKSPACE/artifacts/out"
    if [ -n "$artifact_content" ]; then
        printf '%s' "$artifact_content" > "$WORKSPACE/artifacts/out/$response_name"
    else
        printf 'status: %s\n' "$status" > "$WORKSPACE/artifacts/out/$response_name"
    fi

    local artifacts
    artifacts=$(printf '[{"name":"%s","path":"/app/workspace/artifacts/out/%s"}]' "$response_name" "$response_name")

    local exit_code=1
    if [ "$status" = "COMPLETED" ]; then exit_code=0; fi

    local error_json="null"
    if [ -n "$error" ]; then
        error_json=$(printf '%s' "$error" | jq -Rs .)
    fi

    # Collect tool audit from per-call JSON files.
    local tool_audit="[]"
    if [ -d "$WORKSPACE/.tool_audit" ]; then
        # Merge all files in lexicographical order (timestamp-prefixed).
        # jq -s '.' concatenates objects into an array.
        # We use a subshell and find to avoid ARG_MAX issues with many files.
        tool_audit=$(find "$WORKSPACE/.tool_audit" -name "*.json" | sort | xargs jq -s '.' 2>/dev/null | jq -s 'add' 2>/dev/null || echo "[]")
    fi

    # Build the base result object.
    #
    # IMPORTANT: pass large JSON values ($tool_audit, $artifacts, $error_json)
    # via --slurpfile rather than --argjson. Roles that make many TA calls
    # (the trading strategist hits 16 watchlist symbols × {bars,sma,rsi,macd,atr})
    # accumulate $tool_audit into hundreds of KB of bars data. A single
    # `--argjson tool_audit "$huge_json"` call then exceeds ARG_MAX
    # (~128K on this kernel), jq exits with "Argument list too long",
    # base_result is empty, and the downstream merge sees `null * {...}`
    # which jq rejects with "object × null cannot be multiplied". The
    # net effect is result.json never gets the structured fields the
    # role required (e.g. `proposals`) — observed as the cascading
    # failure in the strategist's task_20260505201512 / 202342 runs.
    # --slurpfile reads from a file descriptor and is unbounded.
    local base_result tool_audit_f artifacts_f error_f
    tool_audit_f=$(mktemp)
    artifacts_f=$(mktemp)
    error_f=$(mktemp)
    printf '%s' "$tool_audit" > "$tool_audit_f"
    printf '%s' "$artifacts"  > "$artifacts_f"
    printf '%s' "$error_json" > "$error_f"
    base_result=$(jq -n \
        --arg status "$status" \
        --arg message "$message" \
        --slurpfile artifacts_arr "$artifacts_f" \
        --argjson exit_code "$exit_code" \
        --argjson duration "$duration" \
        --slurpfile error_arr "$error_f" \
        --slurpfile tool_audit_arr "$tool_audit_f" \
        --argjson prompt_tokens "${TOTAL_PROMPT_TOKENS:-0}" \
        --argjson completion_tokens "${TOTAL_COMPLETION_TOKENS:-0}" \
        --argjson cache_creation_tokens "${TOTAL_CACHE_CREATION_TOKENS:-0}" \
        --argjson cache_read_tokens "${TOTAL_CACHE_READ_TOKENS:-0}" \
        --argjson iterations "${TOTAL_ITERATIONS:-0}" \
        --argjson max_request_bytes "${MAX_REQUEST_BYTES:-0}" \
        --argjson max_prompt_tokens_estimate "${MAX_PROMPT_TOKENS_ESTIMATE:-0}" \
        --argjson prompt_tokens_estimated_total "${TOTAL_PROMPT_TOKENS_ESTIMATED:-0}" \
        --argjson max_prompt_tokens_actual "${MAX_PROMPT_TOKENS_ACTUAL:-0}" \
        --argjson context_size "${LLM_CONTEXT_SIZE:-0}" \
        --argjson max_tokens "${LLM_MAX_TOKENS:-0}" \
        '{
            status: $status,
            message: $message,
            outputArtifacts: $artifacts_arr[0],
            delegatedTasks: [],
            toolAudit: $tool_audit_arr[0],
            usage: {
                prompt_tokens: $prompt_tokens,
                completion_tokens: $completion_tokens,
                cache_creation_tokens: $cache_creation_tokens,
                cache_read_tokens: $cache_read_tokens,
                total_tokens: ($prompt_tokens + $completion_tokens),
                iterations: $iterations,
                max_request_bytes: $max_request_bytes,
                max_prompt_tokens_estimate: $max_prompt_tokens_estimate,
                prompt_tokens_estimated_total: $prompt_tokens_estimated_total,
                max_prompt_tokens_actual: $max_prompt_tokens_actual,
                context_size: $context_size,
                max_tokens: $max_tokens
            },
            diagnostics: ({exitCode: $exit_code, durationSeconds: $duration} + if $error_arr[0] != null then {error: $error_arr[0]} else {} end)
        }')
    rm -f "$tool_audit_f" "$artifacts_f" "$error_f"

    # Merge structured LLM response into result.json so workflow gates can
    # match fields like "review.approved == true". Handles pure JSON, JSON
    # wrapped in markdown code fences, and mixed text with embedded JSON.
    #
    # Reads "${response:-$message}", the SAME fallback the artifact write above
    # uses. It was guarded on "$response" alone until 2026-08-19, and every
    # early-exit path passes the model's text as MESSAGE with an empty RESPONSE
    # (the prompt-token-budget stop and the tool-loop bail both do). On those
    # paths the artifact captured the answer and result.json never did, so the
    # daemon saw a result missing every field the role declared. Two symptoms,
    # one cause: writer steps failing with "result.json is missing required
    # keys" while <step>-response.md held them all, and the lead's recovery hop
    # emitting a textbook decision checkpoint that the daemon still recorded as
    # outcome="missing" — which failed every recovery attempt.
    #
    # The happy path passes both arguments, which is why this only ever bit the
    # paths that were already unusual.
    local merge_src="${response:-$message}"
    if [ -n "$merge_src" ]; then
        local structured=""
        # Pass 1: pure JSON object
        if printf '%s' "$merge_src" | jq -e 'type == "object"' >/dev/null 2>&1; then
            structured="$merge_src"
        else
            # Pass 2: markdown code fences (```json ... ``` or ``` ... ```)
            local stripped
            stripped=$(printf '%s' "$merge_src" | sed -n '/^```/,/^```/{/^```/d;p;}')
            if [ -n "$stripped" ] && printf '%s' "$stripped" | jq -e 'type == "object"' >/dev/null 2>&1; then
                structured="$stripped"
            fi
        fi
        # Pass 3: extract first {...} substring from mixed text, handling
        # multi-line JSON by collapsing newlines before greedy matching.
        if [ -z "$structured" ]; then
            local extracted
            # Collapse newlines so { ... } spans work across lines.
            # `|| true`: grep exits 1 when the text holds no braces at all, and
            # under `set -o pipefail` that aborts the whole agent through the
            # exit trap ("unexpected exit (code 1), writing emergency result").
            # Latent while this block was reachable only for a non-empty
            # RESPONSE that had already failed passes 1 and 2; reading $message
            # as a fallback (2026-08-19) makes prose-only answers reach here,
            # and prose-only is the common case for a bail path.
            extracted=$(printf '%s' "$merge_src" | tr '\n' ' ' | grep -o '{.*}' | tail -1 || true)
            if [ -n "$extracted" ] && printf '%s' "$extracted" | jq -e 'type == "object"' >/dev/null 2>&1; then
                structured="$extracted"
            fi
        fi
        if [ -n "$structured" ]; then
            # Same ARG_MAX defence as the base_result construction
            # above. A large LLM response (the strategist's full
            # proposals + rationale block can exceed the kernel's
            # ARG_MAX) lands fine via stdin pipe but base_result
            # would also be on the command line if we kept the
            # printf %s %s shape — go via temp files for both
            # halves and guard against either being null (which
            # historically crashed jq with "object × null cannot
            # be multiplied" when the prior jq had already failed).
            local base_f structured_f merged
            base_f=$(mktemp)
            structured_f=$(mktemp)
            printf '%s' "$base_result" > "$base_f"
            printf '%s' "$structured"  > "$structured_f"
            merged=$(jq -s '
                ((.[0] // {}) | if type == "object" then . else {} end) as $a
                | ((.[1] // {}) | if type == "object" then . else {} end) as $b
                | $a * $b
            ' "$base_f" "$structured_f" 2>/dev/null)
            if [ -n "$merged" ]; then
                base_result="$merged"
            fi
            rm -f "$base_f" "$structured_f"
        fi
    fi

    # The AGENT's step-quality label, in `agentOutcome` — NOT `outcome`.
    #
    # These bails exit status=COMPLETED so the workflow does not take a failure
    # transition, while the daemon's per-step quality row must still say
    # budget_tripwire / prompt_token_budget / iteration_exhausted rather than
    # being terminal-swept to ok. The detail globals are set by the branches in
    # the tool loop right before they call write_result.
    #
    # WHY ITS OWN FIELD (2026-08-19). This used to write top-level `outcome`,
    # which the LEAD also uses for something entirely different — its workflow
    # decision (checkpoint / external_wait / closure_request), read by the
    # daemon's ParseLeadOutcome. Two consumers, one field, disjoint
    # vocabularies. Worse, this injection runs AFTER the structured merge, so
    # whenever both applied the agent's label silently destroyed the lead's
    # decision: a recovery hop that also hit its iteration cap emitted a
    # textbook decision checkpoint, had it overwritten with
    # `iteration_exhausted`, and the daemon recorded outcome="missing" and
    # failed the recovery contract. The artifact looked perfect throughout,
    # which is what made it so hard to see.
    #
    # `outcomeDetail` is kept in step with the new name for the same reason.
    # The daemon reads agentOutcome first and falls back to outcome, so an older
    # agent image keeps working against a newer daemon.
    if [ -n "${BUDGET_TRIPWIRE_DETAIL:-}" ]; then
        base_result=$(printf '%s' "$base_result" | jq \
            --arg outcome "budget_tripwire" \
            --arg detail "$BUDGET_TRIPWIRE_DETAIL" \
            '. + {agentOutcome: $outcome, agentOutcomeDetail: $detail}')
    elif [ -n "${PROMPT_TOKEN_BUDGET_DETAIL:-}" ]; then
        base_result=$(printf '%s' "$base_result" | jq \
            --arg outcome "prompt_token_budget" \
            --arg detail "$PROMPT_TOKEN_BUDGET_DETAIL" \
            '. + {agentOutcome: $outcome, agentOutcomeDetail: $detail}')
    elif [ -n "${ITERATION_CAP_DETAIL:-}" ]; then
        base_result=$(printf '%s' "$base_result" | jq \
            --arg outcome "iteration_exhausted" \
            --arg detail "$ITERATION_CAP_DETAIL" \
            '. + {agentOutcome: $outcome, agentOutcomeDetail: $detail}')
    fi

    printf '%s\n' "$base_result" > "$OUTPUT_FILE"
}

# Resolve a vornik endpoint URL for curl. Under the daemon-only network
# policy (Step B) the container has NO network device and reaches the
# daemon over a bind-mounted unix socket, so VORNIK_LLM_ENDPOINT /
# VORNIK_API_URL / VORNIK_MEM_URL arrive as "unix://<sock>[/path]". For
# those, this sets VORNIK_CURL_OPT to "--unix-socket <sock>" (split at
# the ".sock" boundary) and VORNIK_URL to an http://localhost<path> URL
# curl can use. For ordinary http(s):// endpoints VORNIK_CURL_OPT is
# cleared and VORNIK_URL is the input unchanged.
#
# IMPORTANT: this sets GLOBALS as a side effect, so it MUST be called as
# a plain command — `vornik_resolve_url "$u"` — NOT via command
# substitution `x=$(vornik_resolve_url "$u")`. Command substitution runs
# it in a subshell, so the VORNIK_CURL_OPT assignment would be lost and
# curl would drop --unix-socket and hit localhost:80 (the daemon-only
# LLM-call regression fixed here). After calling, use "$VORNIK_URL" and
# pass $VORNIK_CURL_OPT UNQUOTED.
VORNIK_CURL_OPT=""
VORNIK_URL=""
vornik_resolve_url() {
    case "$1" in
        unix://*)
            local rest="${1#unix://}"
            VORNIK_CURL_OPT="--unix-socket ${rest%%.sock*}.sock"
            VORNIK_URL="http://localhost${rest#*.sock}"
            ;;
        *)
            VORNIK_CURL_OPT=""
            VORNIK_URL="$1"
            ;;
    esac
}

# Call the LLM with a JSON request body, print the response JSON.
# Uses stdin to avoid shell ARG_MAX limits with large payloads.
llm_call() {
    local request_body="$1"
    vornik_resolve_url "${LLM_ENDPOINT}/chat/completions"
    local url="$VORNIK_URL"
    local task_id project_id execution_id role
    task_id=$(jq -r '.taskId // ""' "$INPUT_FILE" 2>/dev/null || true)
    project_id=$(jq -r '.projectId // ""' "$INPUT_FILE" 2>/dev/null || true)
    execution_id=$(jq -r '.workflow.executionId // ""' "$INPUT_FILE" 2>/dev/null || true)
    # Role steers OpenAI server-side prompt-cache affinity: the daemon
    # derives prompt_cache_key = "<project>:<role>" from this header so
    # requests sharing a role's static prefix hit the same cache bucket.
    # Sent only when a role is known — omitting it (rather than sending an
    # empty header) keeps the header's presence in the daemon access log a
    # legible signal that this agent image carries the feature.
    role=$(jq -r '.swarm.role // .role // ""' "$INPUT_FILE" 2>/dev/null || true)
    [ -z "$role" ] && role="${VORNIK_ROLE:-}"
    local role_hdr=()
    if [ -n "$role" ]; then
        role_hdr=(-H "X-Vornik-Role: ${role}")
    fi
    # Log to stderr — stdout is captured by the caller as the response.
    [ "${VORNIK_LOG_LEVEL:-info}" = "debug" ] && echo "[vornik-agent] calling $url (model=$LLM_MODEL)" >&2 || true
    local curl_err
    curl_err=$(mktemp)
    local result
    result=$(printf '%s' "$request_body" | curl -sS --max-time "$LLM_TIMEOUT" \
        $VORNIK_CURL_OPT \
        -X POST "$url" \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer ${LLM_API_KEY}" \
        -H "X-Vornik-Project-ID: ${project_id:-${VORNIK_PROJECT_ID:-}}" \
        -H "X-Vornik-Task-ID: ${task_id:-${VORNIK_TASK_ID:-}}" \
        -H "X-Vornik-Execution-ID: ${execution_id:-${VORNIK_EXECUTION_ID:-}}" \
        "${role_hdr[@]}" \
        -d @- 2>"$curl_err")
    local rc=$?
    if [ "$rc" -ne 0 ]; then
        local err
        err=$(cat "$curl_err")
        echo "[vornik-agent] ERROR: curl failed (exit $rc): $err" >&2
        jq -n --arg msg "curl failed (exit $rc): $err" \
            '{"error":{"message":$msg}}'
        rm -f "$curl_err"
        return 0
    fi
    rm -f "$curl_err"
    printf '%s' "$result"
}

build_llm_request_file() {
    # A response_format directive sent ALONGSIDE tools makes tool calling
    # IMPOSSIBLE on any server that implements response_format with guided
    # decoding: the model is constrained to emit schema-shaped JSON, so it can
    # never emit a tool call. Measured 2026-08-15 against a self-hosted vLLM
    # server, with an identical request but for this field:
    #     tools only                 -> finish_reason=tool_calls   (calls a tool)
    #     tools + json_object        -> finish_reason=stop, no call
    #     tools + json_schema strict -> finish_reason=stop, no call
    #
    # Hosted APIs tolerate the combination, so this was invisible on cloud
    # models and fatal on a self-hosted one. Every agent answered in ~18 tokens
    # of prose on iteration 1; the loop saw a final response, logged "completed
    # successfully", and the step then failed its output contract because no
    # file had been written. 8 of 10 benchmark tasks died this way.
    #
    # So the directive is omitted whenever tools are offered. The caller re-asks
    # TOOL-FREE for the structured answer once the tool phase is over, which is
    # the same shape as the existing prompt-budget finalization call.
    local out_file="$1" msgs_file="$2" tools_file="$3" schema_name="$4" response_format="$5" response_schema_json="${6:-null}"
    jq -n --arg model "$LLM_MODEL" \
        --slurpfile msgs "$msgs_file" \
        --slurpfile tools "$tools_file" \
        --argjson ctx_size "${LLM_CONTEXT_SIZE:-0}" \
        --argjson max_tokens "${LLM_MAX_TOKENS:-0}" \
        --arg response_format "$response_format" \
        --arg schema_name "$schema_name" \
        --argjson response_schema "$response_schema_json" \
        '{"model":$model,"messages":$msgs[0],"tools":$tools[0]}
         | if $max_tokens > 0 then . + {"max_tokens":$max_tokens} else . end
         | if $ctx_size > 0 then . + {"options":{"num_ctx":$ctx_size}} else . end
         | if ($tools[0] | length) > 0 then . else
             if $response_format == "json_schema" and ($response_schema != null) then
               . + {"response_format":{"type":"json_schema","json_schema":{"name":$schema_name,"schema":$response_schema,"strict":true}}}
           elif $response_format == "json_schema" then
               . + {"response_format":{"type":"json_object"}}
           elif $response_format != "" then
               . + {"response_format":{"type":$response_format}}
           else . end
           end' > "$out_file"
}

# Build a tool definition JSON array for the LLM.
# When VORNIK_MEM_URL is set, memory_search is appended so agents can query
# project memory. It is omitted when the endpoint is not available to avoid
# confusing the model with a non-functional tool.
tool_definitions() {
    local base_tools
    base_tools=$(cat <<'TOOLS_EOF'
[
  {
    "type": "function",
    "function": {
      "name": "file_read",
      "description": "Read the contents of a file. Paths are relative to /app/workspace/ (the working directory); the persistent project folder is at project/ (e.g. 'project/src/main.py'). Output is capped at 30KB per file — for larger files use grep to find specific lines, run_shell with `head -c 200000` for a bigger window (200KB), or read_many_files which has the same per-file cap but lets you pull a directory in one call.",
      "parameters": {
        "type": "object",
        "properties": {
          "path": { "type": "string", "description": "Relative path from /app/workspace/. Use 'project/' prefix for the persistent shared project folder." }
        },
        "required": ["path"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "file_write",
      "description": "Write content to a file. Creates parent directories as needed. Paths are relative to /app/workspace/. Use 'project/' prefix for the persistent shared project folder.",
      "parameters": {
        "type": "object",
        "properties": {
          "path": { "type": "string", "description": "Relative path from /app/workspace/. Use 'project/' prefix for the persistent shared project folder." },
          "content": { "type": "string", "description": "File content to write" }
        },
        "required": ["path", "content"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "run_shell",
      "description": "Run a shell command in the workspace directory. Use for tasks like listing files, running builds, etc. stdout+stderr is capped at 200KB; if you need more, pipe through grep/awk/tail to filter at the source (e.g. `go tool cover -func=cov.out | grep ^github.com/myorg/mypkg/`). gcc + libc6-dev are installed so `go test -race` works.",
      "parameters": {
        "type": "object",
        "properties": {
          "command": { "type": "string", "description": "Shell command to execute" }
        },
        "required": ["command"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "current_time",
      "description": "Return the current date and time in a requested IANA timezone, with UTC included for verification. Use this whenever the task depends on today's date, current time, market hours, deadlines, or timezone conversion. Do not calculate timezone offsets yourself.",
      "parameters": {
        "type": "object",
        "properties": {
          "timezone": { "type": "string", "description": "IANA timezone name such as 'UTC', 'Europe/Prague', 'America/New_York', or 'Asia/Tokyo'. Defaults to UTC." }
        }
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "file_edit",
      "description": "Edit a file by replacing an exact string. Prefer this over file_write for modifying existing files — it only sends the diff region and fails fast if old_string is absent or ambiguous. Fails if old_string does not match exactly once (unless replace_all is true). Paths are relative to /app/workspace/ — use 'project/' prefix for the persistent project folder.",
      "parameters": {
        "type": "object",
        "properties": {
          "path": { "type": "string", "description": "Relative path from /app/workspace/." },
          "old_string": { "type": "string", "description": "Exact string to replace. Must match byte-for-byte including whitespace and indentation." },
          "new_string": { "type": "string", "description": "Replacement string. May be empty to delete the match." },
          "replace_all": { "type": "boolean", "description": "Replace every occurrence instead of requiring exactly one. Default: false." }
        },
        "required": ["path", "old_string", "new_string"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "read_many_files",
      "description": "Read multiple files in one call. Returns a concatenated blob with '===== FILE: <path> =====' headers per file. Each file is capped at 30KB; total output is capped at 120KB (later files are truncated or dropped). Prefer this over N sequential file_read calls when exploring a directory.",
      "parameters": {
        "type": "object",
        "properties": {
          "paths": { "type": "array", "items": { "type": "string" }, "description": "Relative paths from /app/workspace/." }
        },
        "required": ["paths"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "grep",
      "description": "Search file contents for a regex pattern. Faster and more token-efficient than run_shell 'grep -r'. Default output is files_with_matches; switch to content mode only when you need line numbers and the matching lines themselves. Results are capped at head_limit lines (default 200) — if a search returns 'truncated' or you want narrower results, supply a more specific pattern, scope via path/glob, or raise head_limit.",
      "parameters": {
        "type": "object",
        "properties": {
          "pattern": { "type": "string", "description": "Regex pattern (POSIX extended)." },
          "path": { "type": "string", "description": "Directory to search under. Default: workspace root." },
          "glob": { "type": "string", "description": "Filename glob filter (e.g. '*.go', '**/*.py'). Default: all files." },
          "output_mode": { "type": "string", "enum": ["files_with_matches", "content", "count"], "description": "files_with_matches (default): paths only. content: matching lines with line numbers. count: per-file match counts." },
          "ignore_case": { "type": "boolean", "description": "Case-insensitive match." },
          "head_limit": { "type": "integer", "description": "Max result lines. Default: 200." }
        },
        "required": ["pattern"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "glob",
      "description": "List files matching a glob pattern. Supports '**' for recursive matching. Faster than run_shell 'find'. Returns paths sorted by modification time (newest first), capped at 500 entries.",
      "parameters": {
        "type": "object",
        "properties": {
          "pattern": { "type": "string", "description": "Glob pattern (e.g. '**/*.go', 'project/src/*.ts')." },
          "path": { "type": "string", "description": "Root directory. Default: workspace root." }
        },
        "required": ["pattern"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "git_status",
      "description": "Show git working-tree status as typed JSON {branch, ahead, behind, files:[{path,status}]}. Use this before committing or when assessing what a prior role changed.",
      "parameters": {
        "type": "object",
        "properties": {
          "path": { "type": "string", "description": "Repo root. Default: 'project'." }
        }
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "git_diff",
      "description": "Show a unified diff. Default compares working tree to index (unstaged changes); set staged=true to compare index to HEAD, or revision to diff arbitrary refs.",
      "parameters": {
        "type": "object",
        "properties": {
          "path": { "type": "string", "description": "Repo root. Default: 'project'." },
          "staged": { "type": "boolean", "description": "Diff index vs HEAD instead of working tree vs index." },
          "revision": { "type": "string", "description": "Revision spec (e.g. 'HEAD~3..HEAD', 'main'). Overrides staged." },
          "paths": { "type": "array", "items": { "type": "string" }, "description": "Restrict diff to these paths." }
        }
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "git_log",
      "description": "Show commit history as typed JSON [{sha, short_sha, author, date, subject}]. More token-efficient than parsing run_shell 'git log' output.",
      "parameters": {
        "type": "object",
        "properties": {
          "path": { "type": "string", "description": "Repo root. Default: 'project'." },
          "max": { "type": "integer", "description": "Max commits. Default: 20." },
          "revision": { "type": "string", "description": "Revision range (e.g. 'main..HEAD'). Default: HEAD." },
          "paths": { "type": "array", "items": { "type": "string" }, "description": "Limit history to commits touching these paths." }
        }
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "git_show",
      "description": "Show a commit's metadata plus its diff. Use when inspecting what a specific commit changed.",
      "parameters": {
        "type": "object",
        "properties": {
          "path": { "type": "string", "description": "Repo root. Default: 'project'." },
          "revision": { "type": "string", "description": "Revision to show (e.g. 'HEAD', 'abc1234'). Default: HEAD." },
          "paths": { "type": "array", "items": { "type": "string" }, "description": "Restrict diff to these paths." }
        }
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "test_run",
      "description": "Detect project language and run the test suite. Returns {language, runner, passed, failed, skipped, failures:[{test,message}], output}. Gracefully reports when the required toolchain is not installed in the agent image.",
      "parameters": {
        "type": "object",
        "properties": {
          "path": { "type": "string", "description": "Project root. Default: 'project'." },
          "paths": { "type": "array", "items": { "type": "string" }, "description": "Limit to specific test files/packages." }
        }
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "lint_run",
      "description": "Detect project language and run the configured linter (go vet / eslint / ruff). Returns {language, linter, issues:[{file,line,message}], output}.",
      "parameters": {
        "type": "object",
        "properties": {
          "path": { "type": "string", "description": "Project root. Default: 'project'." },
          "paths": { "type": "array", "items": { "type": "string" }, "description": "Limit to specific files or packages." }
        }
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "typecheck_run",
      "description": "Detect project language and run type checking (go build / tsc --noEmit / mypy). Returns {language, checker, errors:[{file,line,message}], output}.",
      "parameters": {
        "type": "object",
        "properties": {
          "path": { "type": "string", "description": "Project root. Default: 'project'." },
          "paths": { "type": "array", "items": { "type": "string" }, "description": "Limit to specific files or packages." }
        }
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "backlog_deposit",
      "description": "Record an OFF-SCOPE finding (bug, optimisation opportunity, code inefficiency, refactor candidate) into the project backlog WITHOUT changing any code. Use when you notice something worth fixing that is outside your current task's scope. Never use it for work you are currently assigned.",
      "parameters": {
        "type": "object",
        "required": ["kind", "title", "detail"],
        "properties": {
          "kind":   {"type": "string", "enum": ["bug", "optimisation", "inefficiency", "refactor"]},
          "title":  {"type": "string", "maxLength": 140, "description": "Specific, imperative, self-contained (a future task prompt)"},
          "detail": {"type": "string", "maxLength": 2000, "description": "What is wrong / suboptimal and what better looks like"},
          "evidence": {"type": "string", "maxLength": 500, "description": "file:line reference(s) plus one-sentence proof"},
          "regression": {"type": "boolean", "description": "Set ONLY when re-reporting a previously-fixed finding that has returned; requires evidence"}
        }
      }
    }
  }
]
TOOLS_EOF
)

    # Two extras buckets: ungated (memory_search — opt-in by env
    # only, matches pre-Phase-32 behaviour) and gated (lifecycle
    # tools — opt-in via allowedTools so only roles that ask for
    # them see them; the lead is typical; researchers/coders
    # don't need the conversation window).
    local extras_ungated='[]'
    local extras_gated='[]'

    if [ -n "${VORNIK_MEM_URL:-}" ]; then
        local memory_tool
        memory_tool=$(cat <<'MEM_EOF'
{
    "type": "function",
    "function": {
      "name": "memory_search",
      "description": "Search project memory for relevant past findings, research notes, and task outputs from previous tasks in this project.",
      "parameters": {
        "type": "object",
        "properties": {
          "query": {"type": "string", "description": "Natural language query to search for"},
          "limit": {"type": "integer", "description": "Max results to return (default 5, max 20)"}
        },
        "required": ["query"]
      }
    }
  }
MEM_EOF
)
        # GATED, not ungated (2026-08-16). memory_search is in
        # is_builtin_tool, so the execution gate applies the per-role
        # allowlist to it — advertising it ungated meant every role SAW a
        # tool most of them could not call. That made it the second most
        # refused tool grant on this deployment (12), and one adaptive route
        # step reached for it twice, the second time as the invented
        # "/memory_search", because it could see the tool and could not use
        # it.
        #
        # Same defect and same fix as grant_step_tools on 2026-08-14: the
        # advertisement must follow the ceiling (RoleMayGrantTools), not
        # bypass it. Ungating it instead would revert the 2026.8.1 security
        # fix (356e74cd) that closed exactly this allowlist bypass for
        # memory_search, skill_fetch, get_conversation_window and
        # summarize_thread.
        #
        # A role that should search memory declares memory_search in its
        # allowedTools; per-chunk `permitted_roles` in the Memory Firewall
        # remains the finer-grained control.
        extras_gated=$(printf '%s' "$extras_gated" | jq --argjson tool "$memory_tool" '. + [$tool]')
    fi

    # Phase 32 — task-lifecycle working-memory tools.
    if [ -n "${VORNIK_API_URL:-}" ] && [ -n "${VORNIK_TASK_ID:-}" ]; then
        local lifecycle_tools
        lifecycle_tools=$(cat <<'LC_EOF'
[
  {
    "type": "function",
    "function": {
      "name": "get_conversation_window",
      "description": "Read messages from THIS task's conversation thread. Returns chronological messages (operator + lead exchanges, checkpoints, answers, directives, phase markers). Use this to recall older content the prompt's recent-window may have summarised away.",
      "parameters": {
        "type": "object",
        "properties": {
          "after": {"type": "string", "description": "Optional cursor: only return messages created after this message ID"},
          "limit": {"type": "integer", "description": "Max messages to return (default 50, max 200)"},
          "kind": {"type": "string", "description": "Optional comma-separated message kinds to filter by (e.g. 'directive,answer')"}
        }
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "summarize_thread",
      "description": "Compress a span of older conversation messages into a single 'note' summary that travels with the task. The originals are filtered out of future prompt windows; this summary is shown in their place. You write the summary text yourself — this tool just persists it. Use when the conversation grows long and older details no longer need to be quoted verbatim.",
      "parameters": {
        "type": "object",
        "properties": {
          "messageIds": {"type": "array", "items": {"type": "string"}, "description": "IDs of messages this summary covers (the originals are hidden from future prompt windows)"},
          "summary": {"type": "string", "description": "The summary text. One paragraph; cap 4 KB."}
        },
        "required": ["messageIds", "summary"]
      }
    }
  }
]
LC_EOF
)
        extras_gated=$(printf '%s' "$extras_gated" | jq --argjson tools "$lifecycle_tools" '. + $tools')
    fi

    if tool_result_hygiene_enabled; then
        local tool_result_read_tool
        tool_result_read_tool=$(cat <<'TR_EOF'
{
    "type": "function",
    "function": {
      "name": "tool_result_read",
      "description": "Read the full saved body for a prior compacted tool result by tool_call_id. Use only when the retained head/tail preview is insufficient.",
      "parameters": {
        "type": "object",
        "properties": {
          "tool_call_id": {"type": "string", "description": "The tool_call_id shown in the compacted tool result placeholder."}
        },
        "required": ["tool_call_id"]
      }
    }
  }
TR_EOF
)
        extras_ungated=$(printf '%s' "$extras_ungated" | jq --argjson tool "$tool_result_read_tool" '. + [$tool]')
    fi

    # skill_fetch — progressive-disclosure knowledge skills (LLD
    # 2026-07-12-skill-progressive-disclosure-design). The system
    # prompt carries a compact LEARNED SKILLS index; this tool pulls a
    # listed skill's full instructions on demand. Ungated (like
    # memory_search): the index only appears when skills exist, and a
    # fetch without an index entry just 404s harmlessly.
    if [ -n "${VORNIK_API_URL:-}" ]; then
        local skill_fetch_tool
        skill_fetch_tool=$(cat <<'SF_EOF'
{
    "type": "function",
    "function": {
      "name": "skill_fetch",
      "description": "Fetch the full instructions of a learned skill listed in the LEARNED SKILLS index of your system prompt. Call this BEFORE doing work a listed skill covers, then follow the returned instructions.",
      "parameters": {
        "type": "object",
        "properties": {
          "name": {"type": "string", "description": "Exact skill name as shown in the LEARNED SKILLS index"}
        },
        "required": ["name"]
      }
    }
  }
SF_EOF
)
        extras_ungated=$(printf '%s' "$extras_ungated" | jq --argjson tool "$skill_fetch_tool" '. + [$tool]')
    fi

    # query_api / list_apis — authenticated third-party API access via the
    # shipped gateway (LLD 2026-07-21-query-api-task-agents-design §3). Exposed
    # only when the daemon API is reachable (VORNIK_API_URL); gated further by
    # allowedTools (they are normal builtins, so the extras_gated allowlist
    # filter below applies). The daemon injects the credential, enforces the
    # per-project provider allowlist, agent read-only policy, per-task budget,
    # redaction and the response byte cap — all server-side, so the agent
    # cannot opt out.
    if [ -n "${VORNIK_API_URL:-}" ]; then
        local api_query_tools
        api_query_tools=$(cat <<'API_EOF'
[
  {
    "type": "function",
    "function": {
      "name": "query_api",
      "description": "Call an authenticated third-party API through the vornik gateway. The daemon injects the provider credential server-side — NEVER put API keys, tokens, or secrets in the arguments. Use list_apis first to discover available providers. Responses are redacted and size-capped by the daemon.",
      "parameters": {
        "type": "object",
        "properties": {
          "provider": {"type": "string", "description": "Provider id as shown by list_apis (e.g. 'maps')"},
          "method": {"type": "string", "description": "HTTP method (default GET). Writes are refused for agents unless the role is explicitly granted write access."},
          "path": {"type": "string", "description": "Request path on the provider (e.g. '/maps/api/place/textsearch/json')"},
          "query": {"type": "object", "description": "Optional query-string parameters as a JSON object"},
          "body": {"type": "object", "description": "Optional request body as a JSON object (for write methods)"}
        },
        "required": ["provider", "path"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "list_apis",
      "description": "List the third-party API providers reachable from this project via the vornik gateway, with the paths/methods each allows. Call before query_api to discover the correct provider id and path.",
      "parameters": {
        "type": "object",
        "properties": {
          "query": {"type": "string", "description": "Optional filter to narrow the provider list"}
        }
      }
    }
  }
]
API_EOF
)
        extras_gated=$(printf '%s' "$extras_gated" | jq --argjson tools "$api_query_tools" '. + $tools')
    fi

    printf '%s' "$base_tools" | jq \
        --argjson ungated "$extras_ungated" \
        --argjson gated "$extras_gated" \
        --argjson allowed "$(allowed_builtin_tools_json)" \
        --argjson builtin "$BUILTIN_TOOL_NAMES_JSON" \
        '([.[] | select(.function.name as $name | (($builtin | index($name) | not) or ($allowed | index($name) != null)))]) + $ungated + ($gated | map(select(.function.name as $name | $allowed | index($name) != null)))'
}

tool_search_definition() {
    cat <<'TOOLS_EOF'
{
  "type": "function",
  "function": {
    "name": "tool_search",
    "description": "Search the deferred MCP tool catalogue and expose matching tools for the next turn. Use this when you need an MCP/integration tool that is not currently visible.",
    "parameters": {
      "type": "object",
      "properties": {
        "query": {"type": "string", "description": "Natural language description or exact MCP tool name to search for."},
        "limit": {"type": "integer", "description": "Maximum matches to expose. Default 8."}
      },
      "required": ["query"]
    }
  }
}
TOOLS_EOF
}

defer_mcp_tools_enabled() {
    local mcp_count="$1"
    is_truthy "$AGENT_DEFER_MCP_TOOLS" && [ "$mcp_count" -gt "${AGENT_DEFER_MCP_THRESHOLD:-20}" ] 2>/dev/null
}

trim_expanded_mcp_tools() {
    local expanded_names_file="$1"
    local cap=$(( ${AGENT_TOOL_SEARCH_LIMIT:-8} * 2 ))
    [ "$cap" -lt 1 ] 2>/dev/null && cap=8
    local trimmed
    trimmed=$(mktemp)
    awk 'NF { line[++n]=$0; seen[$0]=n } END { for (i=1; i<=n; i++) if (seen[line[i]] == i) print line[i] }' "$expanded_names_file" \
        | tail -n "$cap" > "$trimmed"
    mv "$trimmed" "$expanded_names_file"
}

rebuild_tools_file() {
    local builtin_tools_file="$1" mcp_tools_file="$2" expanded_names_file="$3" pinned_names_file="$4" tools_file="$5"
    local mcp_count
    mcp_count=$(jq 'length' "$mcp_tools_file" 2>/dev/null || echo 0)

    if defer_mcp_tools_enabled "$mcp_count"; then
        local search_tool_file visible_mcp_file
        search_tool_file=$(mktemp)
        visible_mcp_file=$(mktemp)
        tool_search_definition > "$search_tool_file"
        touch "$expanded_names_file"
        touch "$pinned_names_file"
        jq --rawfile expanded "$expanded_names_file" --rawfile pinned "$pinned_names_file" '
            (($expanded + "\n" + $pinned) | split("\n") | map(select(length > 0)) | unique) as $allowed
            | [.[] | select(.function.name as $name | $allowed | index($name) != null)]
        ' "$mcp_tools_file" > "$visible_mcp_file"
        jq -s '.[0] + [.[1]] + .[2]' "$builtin_tools_file" "$search_tool_file" "$visible_mcp_file" > "$tools_file"
        rm -f "$search_tool_file" "$visible_mcp_file"
    else
        jq -s '.[0] + .[1]' "$builtin_tools_file" "$mcp_tools_file" > "$tools_file"
    fi
}

handle_tool_search() {
    local params="$1"
    local query limit mcp_tools_file expanded_names_file
    query=$(printf '%s' "$params" | jq -r '.query // ""')
    limit=$(printf '%s' "$params" | jq -r ".limit // ${AGENT_TOOL_SEARCH_LIMIT:-8}")
    mcp_tools_file="${MCP_TOOLS_FILE:-$WORKSPACE/.mcp_tools.json}"
    expanded_names_file="${EXPANDED_MCP_TOOLS_FILE:-$WORKSPACE/.expanded_mcp_tools.txt}"

    if [ -z "$query" ] || [ "$query" = "null" ]; then
        echo '{"matches":[],"message":"query is required"}'
        return
    fi
    if [ ! -s "$mcp_tools_file" ]; then
        echo '{"matches":[],"message":"no MCP tools were discovered for this agent"}'
        return
    fi
    case "$limit" in
        ''|*[!0-9]*) limit="${AGENT_TOOL_SEARCH_LIMIT:-8}" ;;
    esac
    [ "$limit" -lt 1 ] 2>/dev/null && limit=1
    [ "$limit" -gt 20 ] 2>/dev/null && limit=20

    local matches_file
    matches_file=$(mktemp)
    jq --arg q "$query" --argjson limit "$limit" '
        def terms:
            ascii_downcase
            | gsub("[^a-z0-9_/-]+"; " ")
            | split(" ")
            | map(select(length > 1));
        ($q | terms) as $terms
        | map(. as $tool
            | (($tool.function.name // "") + " " + ($tool.function.description // "") | ascii_downcase) as $hay
            | {
                name: ($tool.function.name // ""),
                description: ($tool.function.description // ""),
                score: ([$terms[] as $term | select($hay | contains($term))] | length)
              })
        | map(select(.name != "" and .score > 0))
        | sort_by([(-.score), .name])
        | .[:$limit]
    ' "$mcp_tools_file" > "$matches_file"

    mkdir -p "$(dirname "$expanded_names_file")"
    jq -r '.[].name' "$matches_file" >> "$expanded_names_file"
    trim_expanded_mcp_tools "$expanded_names_file"

    jq -n --slurpfile matches "$matches_file" \
        '{matches: $matches[0], message: (if ($matches[0] | length) > 0 then "matching MCP tools will be available on the next turn" else "no matching MCP tools found" end)}'
    rm -f "$matches_file"
}

handle_tool_result_read() {
    local params="$1"
    local tool_call_id
    tool_call_id=$(printf '%s' "$params" | jq -r '.tool_call_id // ""')
    if ! tool_result_hygiene_enabled; then
        echo "ERROR: tool_result_read is unavailable because tool-result hygiene is disabled"
        return
    fi
    if [ -z "$tool_call_id" ] || [ "$tool_call_id" = "null" ]; then
        echo "ERROR: tool_call_id is required"
        return
    fi
    case "$tool_call_id" in
        *[!A-Za-z0-9_.:-]*)
            echo "ERROR: invalid tool_call_id"
            return
            ;;
    esac
    local path="$WORKSPACE/.tool_results/${tool_call_id}.txt"
    if [ ! -f "$path" ]; then
        echo "ERROR: no saved tool result for tool_call_id=$tool_call_id"
        return
    fi
    local size
    size=$(wc -c < "$path")
    if [ "$size" -gt "$TOOL_RESULT_MAX_BYTES" ]; then
        head -c "$TOOL_RESULT_MAX_BYTES" "$path"
        printf '\n\n[... truncated at %d bytes by tool_result_read, total %d bytes]' "$TOOL_RESULT_MAX_BYTES" "$size"
    else
        cat "$path"
    fi
}

# Phase 32 — handle get_conversation_window tool call.
# Reads task_messages for THIS task via the daemon's
# /api/v1/projects/{p}/tasks/{id}/messages endpoint.
handle_get_conversation_window() {
    local params="$1"
    local after limit kind url project_id task_id
    after=$(printf '%s' "$params" | jq -r '.after // ""')
    limit=$(printf '%s' "$params" | jq -r '.limit // 50')
    kind=$(printf '%s' "$params" | jq -r '.kind // ""')
    project_id=$(jq -r '.projectId // .project_id // ""' "$INPUT_FILE")
    task_id="${VORNIK_TASK_ID:-}"

    if [ -z "${VORNIK_API_URL:-}" ] || [ -z "$project_id" ] || [ -z "$task_id" ]; then
        printf '{"error":"get_conversation_window unavailable (VORNIK_API_URL=%s project_id=%s task_id=%s)"}' \
            "${VORNIK_API_URL:-<unset>}" "${project_id:-<unset>}" "${task_id:-<unset>}"
        return
    fi

    url="${VORNIK_API_URL}/api/v1/projects/${project_id}/tasks/${task_id}/messages?limit=${limit}"
    if [ -n "$after" ]; then
        url="${url}&after=$(printf '%s' "$after" | jq -Rr @uri)"
    fi
    if [ -n "$kind" ]; then
        url="${url}&kind=$(printf '%s' "$kind" | jq -Rr @uri)"
    fi

    vornik_resolve_url "$url"; url="$VORNIK_URL"
    # X-API-Key: required since the 2026-06-06 auth flip (per-task key
    # injected by the executor as VORNIK_API_KEY).
    curl -sS --max-time 10 $VORNIK_CURL_OPT \
        -H "X-API-Key: ${VORNIK_API_KEY:-}" \
        "$url" 2>/dev/null || echo '{"error":"request failed"}'
}

# Phase 32 — handle summarize_thread tool call.
# Persists the lead-generated summary as a 'note' task_message
# whose metadata records the summarized message_ids. The prompt
# builder filters those originals out of subsequent windows.
handle_summarize_thread() {
    local params="$1"
    local body url project_id task_id
    project_id=$(jq -r '.projectId // .project_id // ""' "$INPUT_FILE")
    task_id="${VORNIK_TASK_ID:-}"

    if [ -z "${VORNIK_API_URL:-}" ] || [ -z "$project_id" ] || [ -z "$task_id" ]; then
        printf '{"error":"summarize_thread unavailable (VORNIK_API_URL=%s project_id=%s task_id=%s)"}' \
            "${VORNIK_API_URL:-<unset>}" "${project_id:-<unset>}" "${task_id:-<unset>}"
        return
    fi

    # Pass the params straight through (messageIds + summary).
    # The daemon validates required fields + size cap.
    body="$params"
    vornik_resolve_url "${VORNIK_API_URL}/api/v1/projects/${project_id}/tasks/${task_id}/summarize"; url="$VORNIK_URL"

    curl -sS --max-time 10 $VORNIK_CURL_OPT -X POST \
        -H "Content-Type: application/json" \
        -H "X-API-Key: ${VORNIK_API_KEY:-}" \
        -d "$body" \
        "$url" 2>/dev/null || echo '{"error":"request failed"}'
}

# Handle memory_search tool call.
handle_memory_search() {
    local params="$1"
    local query limit url project_id encoded_q response
    query=$(printf '%s' "$params" | jq -r '.query // ""')
    limit=$(printf '%s' "$params" | jq -r '.limit // 5')
    # Accept both spellings; the daemon writes "projectId" (camelCase, matching
    # the Go struct tags), older task.json fixtures may still use "project_id".
    project_id=$(jq -r '.projectId // .project_id // ""' "$INPUT_FILE")

    if [ -z "${VORNIK_MEM_URL:-}" ] || [ -z "$project_id" ] || [ -z "$query" ]; then
        printf '{"error":"memory search not available (VORNIK_MEM_URL=%s project_id=%s query=%s)"}' \
            "${VORNIK_MEM_URL:-<unset>}" "${project_id:-<unset>}" "${query:-<unset>}"
        return
    fi

    url="${VORNIK_MEM_URL}/api/v1/projects/${project_id}/memory/search?limit=${limit}"
    encoded_q=$(printf '%s' "$query" | jq -Rr @uri)
    url="${url}&q=${encoded_q}"
    vornik_resolve_url "$url"; url="$VORNIK_URL"

    # X-API-Key: required since the 2026-06-06 auth flip. This bare curl
    # was the one straggler the dry-run soak caught (401s on
    # /memory/search from live agents).
    response=$(curl -sS --max-time 10 $VORNIK_CURL_OPT \
        -H "X-API-Key: ${VORNIK_API_KEY:-}" \
        "$url" 2>/dev/null || echo '{"error":"request failed"}')
    printf '%s' "$response"
}

# Handle skill_fetch tool call (progressive-disclosure skills, LLD
# 2026-07-12-skill-progressive-disclosure-design). Pulls a learned
# skill's full body from the daemon; the daemon records the fired
# signal + the execution association there, so no telemetry here.
handle_skill_fetch() {
    local params="$1"
    local name project_id role url encoded_name encoded_role response
    name=$(printf '%s' "$params" | jq -r '.name // ""')
    project_id=$(jq -r '.projectId // .project_id // ""' "$INPUT_FILE")
    role=$(jq -r '.swarm.role // .role // ""' "$INPUT_FILE")

    if [ -z "${VORNIK_API_URL:-}" ] || [ -z "$project_id" ] || [ -z "$role" ] || [ -z "$name" ]; then
        printf '{"error":"skill fetch not available (VORNIK_API_URL=%s project_id=%s role=%s name=%s)"}' \
            "${VORNIK_API_URL:-<unset>}" "${project_id:-<unset>}" "${role:-<unset>}" "${name:-<unset>}"
        return
    fi

    encoded_name=$(printf '%s' "$name" | jq -Rr @uri)
    encoded_role=$(printf '%s' "$role" | jq -Rr @uri)
    url="${VORNIK_API_URL}/api/v1/projects/${project_id}/skills/fetch?name=${encoded_name}&role=${encoded_role}"
    if [ -n "${VORNIK_EXECUTION_ID:-}" ]; then
        url="${url}&execution_id=${VORNIK_EXECUTION_ID}"
    fi
    vornik_resolve_url "$url"; url="$VORNIK_URL"

    response=$(curl -sS --max-time 10 $VORNIK_CURL_OPT \
        -H "X-API-Key: ${VORNIK_API_KEY:-}" \
        "$url" 2>/dev/null || echo '{"error":"request failed"}')
    printf '%s' "$response"
}

# Handle backlog_deposit tool call (autonomous-dev-loop C1, see
# https://docs.vornik.io).
# Mirrors handle_memory_search's shape but POSTs to the daemon's internal
# backlog endpoint (same auth + URL-resolution pattern as the tool-audit
# stream POST). The daemon's accepted/rejected JSON is returned verbatim so
# the model can see why a finding was rejected (dedup, secret-scan, rate
# cap) and proceed either way — never treated as a fatal tool error.
handle_backlog_deposit() {
    local params="$1"
    local project_id task_id execution_id role
    local kind title detail evidence regression
    local body url response
    project_id=$(jq -r '.projectId // .project_id // ""' "$INPUT_FILE")
    task_id="${VORNIK_TASK_ID:-}"
    execution_id="${VORNIK_EXECUTION_ID:-}"
    role=$(jq -r '.swarm.role // .role // ""' "$INPUT_FILE")

    if [ -z "${VORNIK_API_URL:-}" ] || [ -z "$project_id" ] || [ -z "$task_id" ]; then
        printf '{"error":"backlog_deposit not available (VORNIK_API_URL=%s project_id=%s task_id=%s)"}' \
            "${VORNIK_API_URL:-<unset>}" "${project_id:-<unset>}" "${task_id:-<unset>}"
        return
    fi

    kind=$(printf '%s' "$params" | jq -r '.kind // ""')
    title=$(printf '%s' "$params" | jq -r '.title // ""')
    detail=$(printf '%s' "$params" | jq -r '.detail // ""')
    evidence=$(printf '%s' "$params" | jq -r '.evidence // ""')
    regression=$(printf '%s' "$params" | jq -r 'if has("regression") then (.regression == true) else false end')

    body=$(jq -n \
        --arg pid "$project_id" \
        --arg tid "$task_id" \
        --arg eid "$execution_id" \
        --arg sid "$STEP_ID" \
        --arg role "$role" \
        --arg kind "$kind" \
        --arg title "$title" \
        --arg detail "$detail" \
        --arg evidence "$evidence" \
        --argjson regression "$regression" \
        '{project_id:$pid, task_id:$tid, execution_id:$eid, step_id:$sid, role:$role, kind:$kind, title:$title, detail:$detail, evidence:$evidence, regression:$regression}')

    vornik_resolve_url "${VORNIK_API_URL%/}/api/v1/internal/backlog-deposit"; url="$VORNIK_URL"

    response=$(curl -sS --max-time 10 $VORNIK_CURL_OPT -X POST \
        -H "Content-Type: application/json" \
        -H "X-API-Key: ${VORNIK_API_KEY:-}" \
        --data "$body" \
        "$url" 2>/dev/null || echo '{"error":"request failed"}')
    printf '%s' "$response"
}

# Resolve the X-Execution-ID for project-scoped agent API calls. The daemon
# validates it against the task-scoped key's binding. Mirrors llm_call's
# resolution (INPUT_FILE .workflow.executionId, falling back to the
# executor-stamped VORNIK_EXECUTION_ID that skill_fetch/backlog_deposit use).
vornik_execution_id() {
    local eid
    eid=$(jq -r '.workflow.executionId // ""' "$INPUT_FILE" 2>/dev/null || true)
    [ -z "$eid" ] && eid="${VORNIK_EXECUTION_ID:-}"
    # Defensive: strip CR/LF/tabs/spaces so a malformed value can't smuggle
    # extra HTTP headers into the X-Execution-ID curl header (CRLF injection).
    # Execution IDs are opaque tokens with no internal whitespace.
    eid=$(printf '%s' "$eid" | tr -d ' \t\r\n')
    printf '%s' "$eid"
}

# Handle query_api tool call (authenticated third-party API access, LLD
# 2026-07-21-query-api-task-agents-design §3). POSTs the LLM-supplied
# provider/method/path/query/body to the daemon's project-scoped
# /api/query endpoint; the daemon injects the credential, enforces the
# provider allowlist + agent read-only policy + per-task budget, and
# redacts/caps the response. Auth + attribution mirror handle_memory_search
# (X-API-Key = the minted per-task key) plus the X-Execution-ID header the
# Phase-2 endpoint requires (see resolveAgentAPIContext). The agent adds NO
# client-side cap — the server already applied it.
handle_query_api() {
    local params="$1"
    local provider method path query body project_id execution_id
    local req url out http_code response refusal
    provider=$(printf '%s' "$params" | jq -r '.provider // ""')
    method=$(printf '%s' "$params" | jq -r '.method // ""')
    path=$(printf '%s' "$params" | jq -r '.path // ""')
    query=$(printf '%s' "$params" | jq -c '.query // {}')
    body=$(printf '%s' "$params" | jq -c '.body // {}')
    project_id=$(jq -r '.projectId // .project_id // ""' "$INPUT_FILE")
    execution_id=$(vornik_execution_id)

    if [ -z "${VORNIK_API_URL:-}" ] || [ -z "$project_id" ] || [ -z "$provider" ] || [ -z "$path" ]; then
        printf '{"error":"query_api not available (VORNIK_API_URL=%s project_id=%s provider=%s path=%s)"}' \
            "${VORNIK_API_URL:-<unset>}" "${project_id:-<unset>}" "${provider:-<unset>}" "${path:-<unset>}"
        return
    fi

    # Credentials are injected daemon-side — the agent never sends them.
    req=$(jq -n \
        --arg provider "$provider" \
        --arg method "$method" \
        --arg path "$path" \
        --argjson query "$query" \
        --argjson body "$body" \
        '{provider:$provider, method:$method, path:$path, query:$query, body:$body}')

    vornik_resolve_url "${VORNIK_API_URL%/}/api/v1/projects/${project_id}/api/query"; url="$VORNIK_URL"

    out=$(printf '%s' "$req" | curl -sS --max-time 30 $VORNIK_CURL_OPT -X POST \
        -w '\n%{http_code}' \
        -H "Content-Type: application/json" \
        -H "X-API-Key: ${VORNIK_API_KEY:-}" \
        -H "X-Execution-ID: ${execution_id}" \
        --data @- \
        "$url" 2>/dev/null) || { printf '{"error":"request failed"}'; return; }

    http_code="${out##*$'\n'}"
    response="${out%$'\n'*}"

    if [ "$http_code" != "200" ]; then
        printf '{"error":"query_api request failed (HTTP %s)"}' "${http_code:-000}"
        return
    fi

    # A policy refusal arrives as {"refusal":"..."} with HTTP 200 (design §3);
    # surface the text so the LLM can self-correct.
    refusal=$(printf '%s' "$response" | jq -r '.refusal // ""' 2>/dev/null || true)
    if [ -n "$refusal" ]; then
        printf '%s' "$refusal"
        return
    fi
    # Success: hand the LLM the API response body (already redacted + capped
    # server-side), not the JSON envelope. The server's byte cap embeds its own
    # truncation marker INSIDE .body (capToolResultBytes), so we do NOT append a
    # second one here — a duplicate marker just confuses the model.
    local api_body
    api_body=$(printf '%s' "$response" | jq -r '.body // ""' 2>/dev/null)
    if [ -z "$api_body" ]; then
        printf '%s' "$response" # not the expected envelope — return verbatim rather than swallow it
        return
    fi
    printf '%s' "$api_body"
}

# Handle list_apis tool call (provider discovery, LLD §3). GETs the
# daemon's project-scoped /api/providers endpoint with an optional ?query=
# filter. Same auth (X-API-Key) + X-Execution-ID header as query_api;
# discovery is in-budget and audited server-side.
handle_list_apis() {
    local params="$1"
    local query project_id execution_id encoded_q url out http_code response refusal
    query=$(printf '%s' "$params" | jq -r '.query // ""')
    project_id=$(jq -r '.projectId // .project_id // ""' "$INPUT_FILE")
    execution_id=$(vornik_execution_id)

    if [ -z "${VORNIK_API_URL:-}" ] || [ -z "$project_id" ]; then
        printf '{"error":"list_apis not available (VORNIK_API_URL=%s project_id=%s)"}' \
            "${VORNIK_API_URL:-<unset>}" "${project_id:-<unset>}"
        return
    fi

    url="${VORNIK_API_URL%/}/api/v1/projects/${project_id}/api/providers"
    if [ -n "$query" ]; then
        encoded_q=$(printf '%s' "$query" | jq -Rr @uri)
        url="${url}?query=${encoded_q}"
    fi
    vornik_resolve_url "$url"; url="$VORNIK_URL"

    out=$(curl -sS --max-time 30 $VORNIK_CURL_OPT \
        -w '\n%{http_code}' \
        -H "X-API-Key: ${VORNIK_API_KEY:-}" \
        -H "X-Execution-ID: ${execution_id}" \
        "$url" 2>/dev/null) || { printf '{"error":"request failed"}'; return; }

    http_code="${out##*$'\n'}"
    response="${out%$'\n'*}"

    if [ "$http_code" != "200" ]; then
        printf '{"error":"list_apis request failed (HTTP %s)"}' "${http_code:-000}"
        return
    fi

    refusal=$(printf '%s' "$response" | jq -r '.refusal // ""' 2>/dev/null || true)
    if [ -n "$refusal" ]; then
        printf '%s' "$refusal"
        return
    fi
    printf '%s' "$response"
}

# Resolve a path to an absolute path under the workspace.
# Agents must use workspace-relative paths (e.g. "project/file.txt").
# Absolute paths within $WORKSPACE are accepted as-is.
# All other absolute paths are confined to the workspace to prevent
# agents from accessing container-internal files (/app/input/task.json,
# /etc/, etc.) outside the designated workspace.
resolve_path() {
    local relpath="$1"
    python3 - "$WORKSPACE" "$relpath" <<'PY'
import os
import sys

workspace = os.path.realpath(sys.argv[1])
raw = sys.argv[2]

if os.path.isabs(raw):
    if raw == workspace or raw.startswith(workspace + os.sep):
        candidate = raw
    else:
        candidate = os.path.join(workspace, raw.lstrip(os.sep))
else:
    candidate = os.path.join(workspace, raw)

resolved = os.path.realpath(os.path.normpath(candidate))
if resolved != workspace and not resolved.startswith(workspace + os.sep):
    print(f"ERROR: path escapes workspace: {raw}", file=sys.stderr)
    sys.exit(1)

print(resolved)
PY
}

# Execute a single tool call. Prints the result string.
exec_tool() {
    local name="$1" arguments="$2"
    if [ "$name" != "tool_search" ] && [ "$name" != "tool_result_read" ] && is_builtin_tool "$name" && ! builtin_tool_allowed "$name"; then
        echo "ERROR: tool '$name' is not allowed for this role"
        return
    fi
    case "$name" in
        tool_search)
            handle_tool_search "$arguments"
            ;;
        tool_result_read)
            handle_tool_result_read "$arguments"
            ;;
        file_read)
            local path
            path=$(printf '%s' "$arguments" | jq -r '.path // empty')
            if [ -z "$path" ] || [ "$path" = "null" ]; then
                echo "ERROR: path is required"
            else
                if ! path="$(resolve_path "$path" 2>&1)"; then
                    echo "$path"
                    return
                fi
                case "$path" in
                    "$WORKSPACE"/.tool_results/*)
                        echo "ERROR: .tool_results is only readable through tool_result_read"
                        return
                        ;;
                esac
                if [ -f "$path" ]; then
                    # Cap file output to 30KB to avoid blowing up the LLM
                    # context window. Large files cause degenerate tool loops.
                    local size
                    size=$(wc -c < "$path")
                    if [ "$size" -gt 30000 ]; then
                        head -c 30000 "$path"
                        printf '\n\n[... truncated at 30KB, total %d bytes]' "$size"
                    else
                        cat "$path"
                    fi
                else
                    echo "ERROR: file not found: $path"
                fi
            fi
            # Cache state is maintained by the caller loop (see the
            # file_read cache block around `if [ "$tc_name" = "file_read" ]`).
            # exec_tool runs inside a $(...) subshell so any array writes
            # here are lost when it returns — we deliberately keep this
            # function pure and let the parent own the cache.
            ;;
        file_write)
            local path content
            path=$(printf '%s' "$arguments" | jq -r '.path // empty')
            content=$(printf '%s' "$arguments" | jq -r '.content // empty')
            if [ -z "$path" ] || [ "$path" = "null" ]; then
                echo "ERROR: path is required"
            elif [ -z "$content" ] || [ "$content" = "null" ]; then
                echo "ERROR: content is required for file_write. If the content was cut off, your context window may be exhausted — try writing a shorter version of the file, or break it into multiple smaller file_write calls."
            else
                if ! path="$(resolve_path "$path" 2>&1)"; then
                    echo "$path"
                    return
                fi
                mkdir -p "$(dirname "$path")"
                printf '%s' "$content" > "$path"
                echo "OK: wrote $(wc -c < "$path") bytes to $path"
            fi
            ;;
        run_shell)
            local cmd
            cmd=$(printf '%s' "$arguments" | jq -r '.command // empty')
            if [ -z "$cmd" ] || [ "$cmd" = "null" ]; then
                echo "ERROR: command is required"
            else
                debug "tool run_shell: $cmd"
                # Run with timeout, capture both stdout and stderr.
                # Cap output to 200KB to preserve LLM context budget.
                # Pre-2026-05-08 the cap was 30KB, but coverage tooling
                # (`go tool cover -func`, `go test -v`, `npm test`) and
                # bulk grep results routinely exceed that — agents would
                # get truncated output, fail to aggregate, and produce
                # "cannot run coverage" reports. 200KB fits a typical
                # mid-sized project's full coverage output while still
                # bounding malicious / runaway commands. Agents that
                # genuinely need more should pipe through grep / awk /
                # tail to filter at the source.
                local shell_out
                shell_out=$( (cd "$WORKSPACE" && timeout "$SHELL_TIMEOUT" sh -c "$cmd" 2>&1) || echo "(exit code: $?)" )
                # A git write inside the project worktree CANNOT succeed: the
                # runtime bind-mounts the main .git read-only, and a worktree
                # keeps its index/HEAD/logs under that .git. The bare git error
                # ("Read-only file system", an unwritable .lock) reads as a
                # transient glitch, so agents retried it — 32 of this fleet's
                # 108 degenerate-loop kills were a repeated git write, and
                # another 64 were git reads whose output never changed
                # (measured 2026-08-18). Naming the cause in the tool result is
                # the one channel that reaches the model mid-step, and a result
                # that CHANGES is what breaks the loop.
                case "$shell_out" in
                    *"Read-only file system"*|*"could not lock config file"*|*".lock"*"Permission denied"*|*"Unable to create"*".lock"*)
                        shell_out=$(printf '%s\n\n[this failed because the workspace git metadata is READ-ONLY — git writes (add/commit/stash) cannot succeed here and retrying will fail identically. You do not need to commit: the harness commits everything left in the workspace when the step ends. Carry on with the actual task.]' "$shell_out")
                        ;;
                esac
                local shell_len=${#shell_out}
                if [ "$shell_len" -gt 200000 ]; then
                    printf '%.200000s\n\n[... truncated at 200KB, total %d bytes — pipe through grep/awk/tail to filter]' "$shell_out" "$shell_len"
                else
                    printf '%s' "$shell_out"
                fi
            fi
            ;;
        current_time)
            local timezone
            timezone=$(printf '%s' "$arguments" | jq -r '.timezone // "UTC"')
            if [ -z "$timezone" ] || [ "$timezone" = "null" ]; then
                timezone="UTC"
            fi
            TIMEZONE="$timezone" python3 <<'PY'
import datetime as dt
import json
import os
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

tz_name = os.environ.get("TIMEZONE") or "UTC"
try:
    tz = ZoneInfo(tz_name)
except ZoneInfoNotFoundError:
    print(f"ERROR: invalid timezone: {tz_name}")
    raise SystemExit(0)

now_utc = dt.datetime.now(dt.timezone.utc)
local = now_utc.astimezone(tz)
offset = local.utcoffset() or dt.timedelta()
offset_seconds = int(offset.total_seconds())
sign = "+" if offset_seconds >= 0 else "-"
abs_seconds = abs(offset_seconds)
utc_offset = f"{sign}{abs_seconds // 3600:02d}:{(abs_seconds % 3600) // 60:02d}"

print(json.dumps({
    "timezone": tz_name,
    "date": local.date().isoformat(),
    "time": local.strftime("%H:%M:%S"),
    "weekday": local.strftime("%A"),
    "rfc3339": local.isoformat(),
    "utc": now_utc.isoformat().replace("+00:00", "Z"),
    "utc_offset": utc_offset,
    "is_dst": bool(local.dst() and local.dst().total_seconds() != 0),
    "unix": int(now_utc.timestamp()),
}, indent=2))
PY
            ;;
        memory_search)
            handle_memory_search "$arguments"
            ;;
        skill_fetch)
            handle_skill_fetch "$arguments"
            ;;
        backlog_deposit)
            handle_backlog_deposit "$arguments"
            ;;
        query_api)
            handle_query_api "$arguments"
            ;;
        list_apis)
            handle_list_apis "$arguments"
            ;;
        get_conversation_window)
            handle_get_conversation_window "$arguments"
            ;;
        summarize_thread)
            handle_summarize_thread "$arguments"
            ;;
        file_edit)
            local path old_string new_string replace_all
            path=$(printf '%s' "$arguments" | jq -r '.path // empty')
            old_string=$(printf '%s' "$arguments" | jq -r '.old_string // ""')
            new_string=$(printf '%s' "$arguments" | jq -r '.new_string // ""')
            replace_all=$(printf '%s' "$arguments" | jq -r '.replace_all // false')
            if [ -z "$path" ] || [ "$path" = "null" ]; then
                echo "ERROR: path is required"
            elif [ -z "$old_string" ]; then
                echo "ERROR: old_string is required (empty match would match everywhere)"
            else
                if ! path="$(resolve_path "$path" 2>&1)"; then
                    echo "$path"
                    return
                fi
                if [ ! -f "$path" ]; then
                    echo "ERROR: file not found: $path"
                    return
                fi
                # Strings pass through env to avoid any shell interpolation of
                # the user payload. Python handles exact-string matching +
                # atomic replace without sed's escape hell.
                OLD_STR="$old_string" NEW_STR="$new_string" REPLACE_ALL="$replace_all" \
                python3 - "$path" <<'PY'
import os, sys
path = sys.argv[1]
old = os.environ["OLD_STR"]
new = os.environ["NEW_STR"]
replace_all = os.environ.get("REPLACE_ALL", "false").lower() == "true"
with open(path, "r", encoding="utf-8", errors="replace") as f:
    content = f.read()
count = content.count(old)
if count == 0:
    print("ERROR: old_string not found in file")
    sys.exit(0)
if count > 1 and not replace_all:
    print(f"ERROR: old_string matches {count} times — pass replace_all=true to replace every occurrence, or provide a longer old_string that uniquely identifies the location")
    sys.exit(0)
if replace_all:
    new_content = content.replace(old, new)
    replaced = count
else:
    new_content = content.replace(old, new, 1)
    replaced = 1
tmp = path + ".tmp.edit"
with open(tmp, "w", encoding="utf-8") as f:
    f.write(new_content)
os.replace(tmp, path)
print(f"OK: replaced {replaced} occurrence(s) in {path} ({len(new_content)} bytes)")
PY
            fi
            ;;
        read_many_files)
            local paths_json
            paths_json=$(printf '%s' "$arguments" | jq -c '.paths // []')
            if [ "$paths_json" = "[]" ] || [ "$paths_json" = "null" ]; then
                echo "ERROR: paths array is required"
            else
                WORKSPACE="$WORKSPACE" PATHS_JSON="$paths_json" python3 <<'PY'
import json, os
workspace = os.path.realpath(os.environ["WORKSPACE"])
paths = json.loads(os.environ["PATHS_JSON"])
PER_FILE_CAP = 30_000
TOTAL_CAP = 120_000
out_parts = []
total = 0
for raw in paths:
    if total >= TOTAL_CAP:
        out_parts.append(f"===== SKIPPED (total cap reached): {raw} =====")
        continue
    if os.path.isabs(raw):
        if raw == workspace or raw.startswith(workspace + os.sep):
            candidate = raw
        else:
            candidate = os.path.join(workspace, raw.lstrip(os.sep))
    else:
        candidate = os.path.join(workspace, raw)
    resolved = os.path.realpath(os.path.normpath(candidate))
    if resolved != workspace and not resolved.startswith(workspace + os.sep):
        out_parts.append(f"===== ERROR: path escapes workspace: {raw} =====")
        continue
    if not os.path.isfile(resolved):
        out_parts.append(f"===== FILE: {raw} =====")
        out_parts.append("ERROR: file not found")
        continue
    try:
        with open(resolved, "rb") as f:
            data = f.read(PER_FILE_CAP + 1)
    except OSError as e:
        out_parts.append(f"===== FILE: {raw} =====")
        out_parts.append(f"ERROR: {e}")
        continue
    text = data.decode("utf-8", errors="replace")
    truncated = len(data) > PER_FILE_CAP
    if truncated:
        text = text[:PER_FILE_CAP]
        size = os.path.getsize(resolved)
    out_parts.append(f"===== FILE: {raw} =====")
    out_parts.append(text)
    if truncated:
        out_parts.append(f"[... truncated at 30KB, total {size} bytes]")
    total += len(text)
body = "\n".join(out_parts)
if len(body) > TOTAL_CAP:
    body = body[:TOTAL_CAP] + "\n[... output truncated at 120KB]"
print(body)
PY
            fi
            ;;
        grep)
            local pattern search_path glob_pat output_mode ignore_case head_limit
            pattern=$(printf '%s' "$arguments" | jq -r '.pattern // empty')
            search_path=$(printf '%s' "$arguments" | jq -r '.path // empty')
            glob_pat=$(printf '%s' "$arguments" | jq -r '.glob // empty')
            output_mode=$(printf '%s' "$arguments" | jq -r '.output_mode // "files_with_matches"')
            ignore_case=$(printf '%s' "$arguments" | jq -r '.ignore_case // false')
            head_limit=$(printf '%s' "$arguments" | jq -r '.head_limit // 200')
            if [ -z "$pattern" ] || [ "$pattern" = "null" ]; then
                echo "ERROR: pattern is required"
            else
                if [ -z "$search_path" ] || [ "$search_path" = "null" ]; then
                    search_path="$WORKSPACE"
                else
                    if ! search_path="$(resolve_path "$search_path" 2>&1)"; then
                        echo "$search_path"
                        return
                    fi
                fi
                PATTERN="$pattern" SEARCH_PATH="$search_path" GLOB_PAT="$glob_pat" \
                OUTPUT_MODE="$output_mode" IGNORE_CASE="$ignore_case" HEAD_LIMIT="$head_limit" \
                python3 <<'PY'
import os, re, fnmatch
pattern = os.environ["PATTERN"]
root = os.environ["SEARCH_PATH"]
glob_pat = os.environ.get("GLOB_PAT", "") or ""
mode = os.environ.get("OUTPUT_MODE", "files_with_matches") or "files_with_matches"
ignore_case = os.environ.get("IGNORE_CASE", "false").lower() == "true"
try:
    head = int(os.environ.get("HEAD_LIMIT", "200") or "200")
except ValueError:
    head = 200
flags = re.IGNORECASE if ignore_case else 0
try:
    regex = re.compile(pattern, flags)
except re.error as e:
    print(f"ERROR: invalid regex: {e}")
    raise SystemExit(0)
def matches_glob(relpath):
    if not glob_pat:
        return True
    if fnmatch.fnmatch(relpath, glob_pat):
        return True
    if fnmatch.fnmatch(os.path.basename(relpath), glob_pat):
        return True
    # ** handling: fnmatch doesn't do recursive; approximate by allowing
    # any depth when pattern starts with **/
    if glob_pat.startswith("**/"):
        return fnmatch.fnmatch(relpath, glob_pat[3:]) or fnmatch.fnmatch(os.path.basename(relpath), glob_pat[3:])
    return False
results = []
file_counts = {}
SKIP_DIRS = {".git", "node_modules", ".venv", "__pycache__", ".mypy_cache", "dist", "build"}
done = False
for dirpath, dirnames, filenames in os.walk(root):
    dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
    for fname in filenames:
        fpath = os.path.join(dirpath, fname)
        rel = os.path.relpath(fpath, root)
        if not matches_glob(rel):
            continue
        try:
            matched_here = 0
            with open(fpath, "r", encoding="utf-8", errors="replace") as f:
                for lineno, line in enumerate(f, 1):
                    if regex.search(line):
                        matched_here += 1
                        if mode == "content":
                            results.append(f"{rel}:{lineno}:{line.rstrip()}")
                            if len(results) >= head:
                                done = True
                                break
            if matched_here > 0:
                if mode == "files_with_matches":
                    results.append(rel)
                elif mode == "count":
                    file_counts[rel] = matched_here
                if mode == "files_with_matches" and len(results) >= head:
                    done = True
        except OSError:
            continue
        if done:
            break
    if done:
        break
if mode == "count":
    for path, n in sorted(file_counts.items()):
        results.append(f"{path}:{n}")
if not results:
    print("(no matches)")
else:
    shown = results[:head]
    print("\n".join(shown))
    if len(results) > head:
        print(f"[... truncated at {head} of {len(results)} results]")
PY
            fi
            ;;
        glob)
            local pattern glob_root
            pattern=$(printf '%s' "$arguments" | jq -r '.pattern // empty')
            glob_root=$(printf '%s' "$arguments" | jq -r '.path // empty')
            if [ -z "$pattern" ] || [ "$pattern" = "null" ]; then
                echo "ERROR: pattern is required"
            else
                if [ -z "$glob_root" ] || [ "$glob_root" = "null" ]; then
                    glob_root="$WORKSPACE"
                else
                    if ! glob_root="$(resolve_path "$glob_root" 2>&1)"; then
                        echo "$glob_root"
                        return
                    fi
                fi
                PATTERN="$pattern" GLOB_ROOT="$glob_root" python3 <<'PY'
import os, glob
pattern = os.environ["PATTERN"]
root = os.environ["GLOB_ROOT"]
cwd = os.getcwd()
try:
    os.chdir(root)
    matches = glob.glob(pattern, recursive=True)
finally:
    os.chdir(cwd)
entries = []
for p in matches:
    full = os.path.join(root, p)
    if os.path.isfile(full):
        try:
            entries.append((os.path.getmtime(full), p))
        except OSError:
            continue
entries.sort(reverse=True)
paths = [p for _, p in entries][:500]
if not paths:
    print("(no matches)")
else:
    print("\n".join(paths))
    if len(entries) > 500:
        print(f"[... truncated at 500 of {len(entries)} matches]")
PY
            fi
            ;;
        git_status)
            local repo_path
            repo_path=$(printf '%s' "$arguments" | jq -r '.path // "project"')
            if ! repo_path="$(resolve_path "$repo_path" 2>&1)"; then
                echo "$repo_path"
                return
            fi
            if ! (cd "$repo_path" 2>/dev/null && git rev-parse --git-dir >/dev/null 2>&1); then
                echo "ERROR: not a git repository: $repo_path"
                return
            fi
            REPO_PATH="$repo_path" python3 <<'PY'
import os, subprocess, json
repo = os.environ["REPO_PATH"]
def run(args):
    return subprocess.run(args, cwd=repo, capture_output=True, text=True)
branch = run(["git", "rev-parse", "--abbrev-ref", "HEAD"]).stdout.strip()
ahead, behind = 0, 0
ab = run(["git", "rev-list", "--left-right", "--count", "HEAD...@{u}"])
if ab.returncode == 0:
    parts = ab.stdout.strip().split()
    if len(parts) == 2:
        ahead, behind = int(parts[0]), int(parts[1])
porc = run(["git", "status", "--porcelain=v1"]).stdout
files = []
for line in porc.splitlines():
    if len(line) < 3:
        continue
    files.append({"path": line[3:], "status": line[:2]})
print(json.dumps({"branch": branch, "ahead": ahead, "behind": behind, "files": files}, indent=2))
PY
            ;;
        git_diff)
            local repo_path staged revision paths_json
            repo_path=$(printf '%s' "$arguments" | jq -r '.path // "project"')
            staged=$(printf '%s' "$arguments" | jq -r '.staged // false')
            revision=$(printf '%s' "$arguments" | jq -r '.revision // empty')
            paths_json=$(printf '%s' "$arguments" | jq -c '.paths // []')
            if ! repo_path="$(resolve_path "$repo_path" 2>&1)"; then
                echo "$repo_path"
                return
            fi
            local git_args=(diff)
            if [ -n "$revision" ] && [ "$revision" != "null" ]; then
                git_args+=("$revision")
            elif [ "$staged" = "true" ]; then
                git_args+=(--cached)
            fi
            if [ "$paths_json" != "[]" ] && [ "$paths_json" != "null" ]; then
                git_args+=(--)
                while IFS= read -r p; do
                    git_args+=("$p")
                done < <(printf '%s' "$paths_json" | jq -r '.[]')
            fi
            local diff_out
            diff_out=$( (cd "$repo_path" && git "${git_args[@]}" 2>&1) || true )
            local diff_len=${#diff_out}
            if [ "$diff_len" -gt 30000 ]; then
                printf '%.30000s\n\n[... truncated at 30KB, total %d bytes]' "$diff_out" "$diff_len"
            else
                printf '%s' "$diff_out"
            fi
            ;;
        git_log)
            local repo_path log_max revision paths_json
            repo_path=$(printf '%s' "$arguments" | jq -r '.path // "project"')
            log_max=$(printf '%s' "$arguments" | jq -r '.max // 20')
            revision=$(printf '%s' "$arguments" | jq -r '.revision // empty')
            paths_json=$(printf '%s' "$arguments" | jq -c '.paths // []')
            if ! repo_path="$(resolve_path "$repo_path" 2>&1)"; then
                echo "$repo_path"
                return
            fi
            REPO_PATH="$repo_path" MAX="$log_max" REVISION="$revision" PATHS_JSON="$paths_json" \
            python3 <<'PY'
import os, subprocess, json
repo = os.environ["REPO_PATH"]
try:
    n = int(os.environ.get("MAX", "20") or "20")
except ValueError:
    n = 20
if n < 1: n = 1
if n > 200: n = 200
rev = os.environ.get("REVISION", "") or ""
paths = json.loads(os.environ.get("PATHS_JSON", "[]") or "[]")
fmt = "%H%x1f%h%x1f%an%x1f%aI%x1f%s"
args = ["git", "log", f"-{n}", f"--pretty=format:{fmt}"]
if rev:
    args.append(rev)
if paths:
    args.append("--")
    args.extend(paths)
r = subprocess.run(args, cwd=repo, capture_output=True, text=True)
if r.returncode != 0:
    print(f"ERROR: {r.stderr.strip()}")
    raise SystemExit(0)
commits = []
for line in r.stdout.splitlines():
    parts = line.split("\x1f")
    if len(parts) == 5:
        commits.append({
            "sha": parts[0], "short_sha": parts[1],
            "author": parts[2], "date": parts[3], "subject": parts[4],
        })
print(json.dumps(commits, indent=2))
PY
            ;;
        git_show)
            local repo_path revision paths_json
            repo_path=$(printf '%s' "$arguments" | jq -r '.path // "project"')
            revision=$(printf '%s' "$arguments" | jq -r '.revision // "HEAD"')
            paths_json=$(printf '%s' "$arguments" | jq -c '.paths // []')
            if ! repo_path="$(resolve_path "$repo_path" 2>&1)"; then
                echo "$repo_path"
                return
            fi
            local show_args=(show "$revision")
            if [ "$paths_json" != "[]" ] && [ "$paths_json" != "null" ]; then
                show_args+=(--)
                while IFS= read -r p; do
                    show_args+=("$p")
                done < <(printf '%s' "$paths_json" | jq -r '.[]')
            fi
            local show_out
            show_out=$( (cd "$repo_path" && git "${show_args[@]}" 2>&1) || true )
            local show_len=${#show_out}
            if [ "$show_len" -gt 30000 ]; then
                printf '%.30000s\n\n[... truncated at 30KB, total %d bytes]' "$show_out" "$show_len"
            else
                printf '%s' "$show_out"
            fi
            ;;
        test_run|lint_run|typecheck_run)
            local repo_path paths_json mode
            repo_path=$(printf '%s' "$arguments" | jq -r '.path // "project"')
            paths_json=$(printf '%s' "$arguments" | jq -c '.paths // []')
            mode="$name"
            if ! repo_path="$(resolve_path "$repo_path" 2>&1)"; then
                echo "$repo_path"
                return
            fi
            REPO_PATH="$repo_path" PATHS_JSON="$paths_json" MODE="$mode" python3 <<'PY'
import os, subprocess, json, shutil, re
repo = os.environ["REPO_PATH"]
mode = os.environ["MODE"]
paths = json.loads(os.environ.get("PATHS_JSON", "[]") or "[]")
# SECURITY (audit HIGH-1, 2026-07-09): test_run/lint_run/typecheck_run execute
# the REVIEWED repository's own code — a test suite (conftest.py, build.rs, an
# npm "test" script) is arbitrary untrusted code, and for a fork/external PR
# the reviewer agent runs it over the contributor's tree. The daemon injects a
# push-capable GitHub installation token as GH_TOKEN/GITHUB_TOKEN into the
# container env for the agent's own gh/git tools; that token must NEVER be
# visible to the reviewed code's own processes. Build a scrubbed environment
# (secrets stripped) and pass it to every runner below. The agent's first-class
# git/gh operations run elsewhere and keep the real env; pushes are performed
# host-side by the daemon (forge.open_change_request), so stripping the token
# here cannot break the publish path.
_SECRET_ENV_KEYS = ("GH_TOKEN", "GITHUB_TOKEN")
SAFE_ENV = {k: v for k, v in os.environ.items() if k not in _SECRET_ENV_KEYS}
def have(cmd): return shutil.which(cmd) is not None
def detect():
    if os.path.isfile(os.path.join(repo, "go.mod")): return "go"
    if os.path.isfile(os.path.join(repo, "package.json")): return "node"
    if (os.path.isfile(os.path.join(repo, "pyproject.toml"))
        or os.path.isfile(os.path.join(repo, "setup.py"))
        or os.path.isfile(os.path.join(repo, "pytest.ini"))
        or os.path.isdir(os.path.join(repo, "tests"))):
        return "python"
    if os.path.isfile(os.path.join(repo, "Cargo.toml")): return "rust"
    return "unknown"
def cap(s, n=20000):
    return s if len(s) <= n else s[:n] + f"\n[... truncated at {n} of {len(s)} bytes]"
def tool_timeout(default):
    """Seconds a build/test/lint subprocess may run.

    These were hardcoded, and unlike the LLM and shell timeouts they had no
    knob at all — so on hardware slower than the image was tuned for, a
    perfectly healthy `go build` is killed with no way to grant it more.

    Scaled by VORNIK_TOOL_TIMEOUT_FACTOR rather than set absolutely: the
    ratios between these budgets were chosen deliberately (a Rust build gets
    longer than a typecheck), and a single override would flatten them. The
    factor is NOT the decode-speed factor — these are compute-bound, and a
    build does not get faster because the model does.
    """
    try:
        f = float(os.environ.get("VORNIK_TOOL_TIMEOUT_FACTOR", "1") or "1")
    except ValueError:
        f = 1.0
    if f <= 0:
        f = 1.0
    return int(default * f)
lang = detect()
out = {"language": lang, "mode": mode}
def run_go_test():
    if not have("go"):
        return {"error": "go toolchain not available in agent image", "runner": "go test"}
    args = ["go", "test", "-json", "-count=1"]
    args += paths if paths else ["./..."]
    r = subprocess.run(args, cwd=repo, env=SAFE_ENV, capture_output=True, text=True, timeout=tool_timeout(600))
    passed = failed = skipped = 0
    failures = []
    for line in r.stdout.splitlines():
        try: ev = json.loads(line)
        except json.JSONDecodeError: continue
        if not ev.get("Test"): continue
        a = ev.get("Action")
        if a == "pass": passed += 1
        elif a == "fail":
            failed += 1
            failures.append({"test": ev.get("Test"), "package": ev.get("Package","")})
        elif a == "skip": skipped += 1
    return {"runner": "go test", "passed": passed, "failed": failed, "skipped": skipped,
            "failures": failures[:50], "output": cap(r.stdout + r.stderr)}
def run_go_vet():
    if not have("go"):
        return {"error": "go toolchain not available in agent image", "runner": "go vet"}
    args = ["go", "vet"] + (paths if paths else ["./..."])
    r = subprocess.run(args, cwd=repo, env=SAFE_ENV, capture_output=True, text=True, timeout=tool_timeout(300))
    issues = []
    for line in r.stderr.splitlines():
        m = re.match(r"([^:]+):(\d+):(?:\d+:)?\s*(.*)", line)
        if m:
            issues.append({"file": m.group(1), "line": int(m.group(2)), "message": m.group(3)})
    return {"runner": "go vet", "clean": r.returncode == 0, "issues": issues[:100],
            "output": cap(r.stdout + r.stderr)}
def run_go_build():
    if not have("go"):
        return {"error": "go toolchain not available in agent image", "runner": "go build"}
    args = ["go", "build"] + (paths if paths else ["./..."])
    r = subprocess.run(args, cwd=repo, env=SAFE_ENV, capture_output=True, text=True, timeout=tool_timeout(600))
    errors = []
    for line in r.stderr.splitlines():
        m = re.match(r"([^:]+):(\d+):(?:\d+:)?\s*(.*)", line)
        if m:
            errors.append({"file": m.group(1), "line": int(m.group(2)), "message": m.group(3)})
    return {"runner": "go build", "ok": r.returncode == 0, "errors": errors[:100],
            "output": cap(r.stdout + r.stderr)}
def run_pytest():
    if not have("pytest"):
        return {"error": "pytest not available in agent image", "runner": "pytest"}
    args = ["pytest", "-q", "--tb=short"] + paths
    r = subprocess.run(args, cwd=repo, env=SAFE_ENV, capture_output=True, text=True, timeout=tool_timeout(600))
    passed = failed = skipped = 0
    for line in reversed(r.stdout.splitlines()):
        m = re.search(r"(\d+)\s+passed", line);  passed = int(m.group(1)) if m else passed
        m = re.search(r"(\d+)\s+failed", line);  failed = int(m.group(1)) if m else failed
        m = re.search(r"(\d+)\s+skipped", line); skipped = int(m.group(1)) if m else skipped
        if "passed" in line or "failed" in line or "error" in line:
            break
    return {"runner": "pytest", "passed": passed, "failed": failed, "skipped": skipped,
            "output": cap(r.stdout + r.stderr)}
def run_ruff():
    if not have("ruff"):
        return {"error": "ruff not available in agent image", "runner": "ruff check"}
    args = ["ruff", "check", "--output-format=json"] + (paths if paths else ["."])
    r = subprocess.run(args, cwd=repo, env=SAFE_ENV, capture_output=True, text=True, timeout=tool_timeout(120))
    issues = []
    try:
        for d in json.loads(r.stdout or "[]"):
            issues.append({"file": d.get("filename",""), "line": d.get("location",{}).get("row",0),
                           "code": d.get("code",""), "message": d.get("message","")})
    except json.JSONDecodeError:
        pass
    return {"runner": "ruff check", "clean": len(issues) == 0, "issues": issues[:100],
            "output": cap(r.stdout + r.stderr)}
def run_mypy():
    if not have("mypy"):
        return {"error": "mypy not available in agent image", "runner": "mypy"}
    args = ["mypy", "--no-color-output"] + (paths if paths else ["."])
    r = subprocess.run(args, cwd=repo, env=SAFE_ENV, capture_output=True, text=True, timeout=tool_timeout(300))
    errors = []
    for line in r.stdout.splitlines():
        m = re.match(r"([^:]+):(\d+):\s*(error|note):\s*(.*)", line)
        if m and m.group(3) == "error":
            errors.append({"file": m.group(1), "line": int(m.group(2)), "message": m.group(4)})
    return {"runner": "mypy", "ok": r.returncode == 0, "errors": errors[:100],
            "output": cap(r.stdout + r.stderr)}
def run_node_script(script):
    if not have("npm"):
        return {"error": "node/npm toolchain not available in agent image", "runner": f"npm run {script}"}
    r = subprocess.run(["npm", "run", script, "--silent"], cwd=repo, env=SAFE_ENV, capture_output=True, text=True, timeout=tool_timeout(600))
    return {"runner": f"npm run {script}", "ok": r.returncode == 0,
            "output": cap(r.stdout + r.stderr)}
def run_cargo(sub):
    if not have("cargo"):
        return {"error": "rust toolchain not available in agent image", "runner": f"cargo {sub}"}
    r = subprocess.run(["cargo", sub], cwd=repo, env=SAFE_ENV, capture_output=True, text=True, timeout=tool_timeout(900))
    return {"runner": f"cargo {sub}", "ok": r.returncode == 0,
            "output": cap(r.stdout + r.stderr)}
if lang == "go":
    if mode == "test_run":       out.update(run_go_test())
    elif mode == "lint_run":     out.update(run_go_vet())
    elif mode == "typecheck_run": out.update(run_go_build())
elif lang == "python":
    if mode == "test_run":       out.update(run_pytest())
    elif mode == "lint_run":     out.update(run_ruff())
    elif mode == "typecheck_run": out.update(run_mypy())
elif lang == "node":
    script = {"test_run": "test", "lint_run": "lint", "typecheck_run": "typecheck"}[mode]
    out.update(run_node_script(script))
elif lang == "rust":
    sub = {"test_run": "test", "lint_run": "clippy", "typecheck_run": "check"}[mode]
    out.update(run_cargo(sub))
else:
    out["error"] = f"could not detect language at {repo} (looked for go.mod, package.json, pyproject.toml, setup.py, tests/, Cargo.toml)"
print(json.dumps(out, indent=2))
PY
            ;;
        mcp__*)
            if command -v mcp-bridge >/dev/null 2>&1; then
                mcp-bridge call "$name" "$arguments" 2>&1 || echo "ERROR: mcp-bridge call failed"
            else
                echo "ERROR: mcp-bridge not available — MCP tool '$name' cannot be called"
            fi
            ;;
        *)
            echo "ERROR: unknown tool: $name"
            ;;
    esac
}

# GH_TOKEN → git credential wiring for private-repo pushes/clones over
# HTTPS (autonomous-dev-loop, forge.open_change_request path). `gh auth
# setup-git` writes a git credential helper config pointing at GH_TOKEN so
# git operations against github.com work without the agent ever embedding
# the token in a remote URL. Idempotent — safe to call at the top of every
# main() invocation, including repeated calls in warm-container mode.
# Deliberately NO credential-helper shim fallback when `gh` is missing
# (security decision, see https://docs.vornik.io):
# we leave git unauthenticated rather than hand-roll one.
setup_gh_git_credentials() {
    if [ -n "${GH_TOKEN:-}" ]; then
        if command -v gh >/dev/null 2>&1; then
            gh auth setup-git >/dev/null 2>&1 || log "WARN: gh auth setup-git failed; private-repo git will be unauthenticated"
        else
            log "WARN: GH_TOKEN set but gh missing; private-repo git will be unauthenticated"
        fi
    fi
}

main() {
    log "starting (model=$LLM_MODEL)"
    debug "env: LLM_ENDPOINT=$LLM_ENDPOINT LLM_MODEL=$LLM_MODEL API_KEY=${LLM_API_KEY:+set(${#LLM_API_KEY}chars)}"
    CANCELLED=0
    setup_gh_git_credentials
    # Per-task LLM usage accumulators. Read in write_result and surfaced
    # to the executor via result.json → prom metric. Reset here so warm
    # containers don't carry usage from a prior task.
    TOTAL_PROMPT_TOKENS=0
    TOTAL_PROMPT_TOKENS_ESTIMATED=0
    TOTAL_COMPLETION_TOKENS=0
    TOTAL_CACHE_CREATION_TOKENS=0
    TOTAL_CACHE_READ_TOKENS=0
    TOTAL_ITERATIONS=0
    MAX_REQUEST_BYTES=0
    MAX_PROMPT_TOKENS_ESTIMATE=0
    MAX_PROMPT_TOKENS_ACTUAL=0
    PROMPT_TOKEN_BUDGET_FINAL_CALL=0
    PROMPT_TOKEN_BUDGET_DETAIL=""
    BUDGET_TRIPWIRE_DETAIL=""
    # Cumulative cost in USD across all iterations of this step.
    # Streamed to the daemon after every iteration so cancelled
    # tasks still carry the correct cost summary even when the
    # step-finalize path doesn't run.
    TOTAL_COST_USD="0"
    check_cancel
    if [ "$CANCELLED" = "1" ]; then return 0; fi

    # Validate environment
    if [ -z "$LLM_ENDPOINT" ]; then
        log "ERROR: VORNIK_LLM_ENDPOINT not set"
        write_result "FAILED" "LLM endpoint not configured" "" "$(get_duration)" "VORNIK_LLM_ENDPOINT not set"
        return 1
    fi
    if [ -z "$LLM_MODEL" ]; then
        log "ERROR: VORNIK_LLM_MODEL not set"
        write_result "FAILED" "LLM model not configured" "" "$(get_duration)" "VORNIK_LLM_MODEL not set"
        return 1
    fi

    # Read input
    if [ ! -f "$INPUT_FILE" ]; then
        log "ERROR: input file not found: $INPUT_FILE"
        write_result "FAILED" "Input file not found" "" "$(get_duration)" "missing $INPUT_FILE"
        return 1
    fi

    # Verify project directory is accessible (catches SELinux/mount issues early).
    if [ -d "/app/workspace/project" ] && ! ls /app/workspace/project/ >/dev/null 2>&1; then
        log "ERROR: project/ directory exists but is not accessible (likely SELinux context mismatch)"
        write_result "FAILED" "Cannot access project/ directory — permission denied despite correct Unix permissions. Check SELinux labels on the host: ls -Z the project workspace path, and ensure the podman volume uses :z (shared) not :Z (private)." "" "$(get_duration)" "project dir inaccessible"
        return 1
    fi

    local task_id role prompt system_prompt previous_result project_id execution_id
    task_id=$(jq -r '.taskId // "unknown"' "$INPUT_FILE")
    role=$(jq -r '.swarm.role // "agent"' "$INPUT_FILE")
    STEP_ID=$(jq -r '.workflow.stepId // "unknown"' "$INPUT_FILE")
    # The step's declared output-file contract, when it has one. The daemon
    # enforces this AFTER the agent exits (executor/container.go), so before
    # 2026-08-16 the agent could not know it existed: it would finish, log
    # "completed successfully", and only then have the step failed for a file it
    # was never told to write. Knowing the glob lets the agent notice and fix it
    # inside the same step instead of burning a shape-retry.
    REQUIRE_OUTPUT_GLOB=$(jq -r '.workflow.requireOutputGlob // ""' "$INPUT_FILE")
    # The role's gate-mode plausibility rules. Role-level, not step-level —
    # unlike requireOutputGlob above — because that is where they are declared.
    # See plausibility_violations for why the agent needs them at all.
    PLAUSIBILITY_RULES=$(jq -c '.swarm.plausibilityRules // []' "$INPUT_FILE")
    # project_id + execution_id are needed for the realtime
    # tool-audit POST per call. Extracted here so they're in scope
    # inside the tool-call loop. Pre-existing audit-file writes
    # remain unchanged; the streaming POST is best-effort.
    project_id=$(jq -r '.projectId // ""' "$INPUT_FILE")
    execution_id=$(jq -r '.workflow.executionId // ""' "$INPUT_FILE")
    prompt=$(jq -r '.context.prompt // "No instructions provided."' "$INPUT_FILE")
    system_prompt=$(jq -r '.context.systemPrompt // ""' "$INPUT_FILE")
    previous_result=$(jq -r '.context.previousStepResult // ""' "$INPUT_FILE")
    # response_format: when the role declares responseFormat:
    # "json_object" in swarm YAML, the gateway request gains a
    # response_format directive so the model's first attempt is
    # structurally valid by construction. Empty string disables
    # the directive (free-form text). Distinct from the
    # plausibility / required-keys layer which validates AFTER
    # the response — JSON-mode prevents the prose-only failure
    # class upstream of any retry.
    response_format=$(jq -r '.config.responseFormat // ""' "$INPUT_FILE")
    # response_schema (item 7 of https://docs.vornik.io):
    # when the role declares an outputSchema, the executor surfaces
    # the JSON Schema body here so the request can land the typed
    # `{"type":"json_schema","json_schema":{"name":...,"schema":...}}`
    # directive instead of the looser json_object form. The chat-proxy
    # lifts it onto the per-request context and Bedrock / OpenAI /
    # Anthropic providers each translate it into the strongest
    # enforcement their wire shape supports. Empty when the role has
    # no outputSchema — falls back cleanly to the legacy response_format
    # behaviour. The schema name (used as a stable identifier for
    # caching / debugging) defaults to "<role>_result" so different
    # roles produce distinct schemas in the gateway's tooling.
    response_schema=$(jq -c '.config.responseSchema // empty' "$INPUT_FILE")

    debug "task=$task_id role=$role step=$STEP_ID"
    debug "prompt: $prompt"

    # Build system message
    if [ -z "$system_prompt" ]; then
        system_prompt="You are a $role agent in a software development workflow (step: $STEP_ID).

Complete the task described in the user message. Be concise and produce actionable output.

You have four tools: file_read, file_write, run_shell, and current_time.
- All paths are relative to /app/workspace/ (the working directory).
- The project folder is at project/ — it persists across tasks and is shared between agents.
- The rest of the workspace is ephemeral and cleaned between tasks.
- run_shell executes in /app/workspace/. Use 'cd project && ...' for project commands.
- current_time returns the current date and time for an IANA timezone. Use it for today's date, current time, deadlines, market hours, or timezone conversion instead of calculating offsets yourself."
    fi

    # Append tool call budget to system prompt so the LLM can plan its work.
    system_prompt="${system_prompt}

## Tool call budget
You have a budget of ${MAX_TOOL_ITERATIONS} tool calls for this task. Plan accordingly: prioritise the most important reads and writes, and avoid redundant or exploratory calls. When the budget is nearly exhausted, stop starting new work and produce your best output with what you have."

    # Conditional output requirements. These are enforced by the daemon after
    # this container exits and are deliberately NOT in the provider JSON schema
    # (conditional schema support is uneven across providers), so this prompt
    # block is the only place the model can learn they exist. Rendering them in
    # the rule's own terms — "if you set X, you must also provide Y" — is what
    # turns a post-hoc rejection into something the model can comply with.
    if [ "$(printf '%s' "${PLAUSIBILITY_RULES:-[]}" | jq '[.[] | select(.warnOnly != true)] | length' 2>/dev/null || echo 0)" -gt 0 ]; then
        local _plaus_lines
        _plaus_lines=$(printf '%s' "$PLAUSIBILITY_RULES" | jq -r '
            .[] | select(.warnOnly != true)
            | if ((.when // {}) | length) == 0 then
                "- You must always provide: " + ((.require // []) | join(", ")) + "."
              else
                "- If " + (((.when // {}) | to_entries | map("`" + .key + "` is " + (.value | tostring)) | join(" and "))) +
                ", you must also provide: " + ((.require // []) | join(", ")) + "."
              end' 2>/dev/null)
        if [ -n "$_plaus_lines" ]; then
            system_prompt="${system_prompt}

## Conditional output requirements
Your result is checked against these rules after you finish. A rule that fires with a missing or empty field FAILS the step, even if the JSON shape is otherwise valid. An empty string, empty array or empty object counts as missing.
${_plaus_lines}"
        fi
    fi

    system_prompt="${system_prompt}

## Time and timezone
If the task depends on today's date, the current time, deadlines, market hours, or timezone conversion, call current_time with the relevant IANA timezone. Do not calculate timezone offsets yourself."

    # Inject memory search guidance when the memory endpoint is available.
    if [ -n "${VORNIK_MEM_URL:-}" ]; then
        system_prompt="${system_prompt}

## Project memory
You have access to a memory_search tool that retrieves relevant findings from past tasks in this project. Search before starting new research to avoid duplicating work."
    fi

    # Check for input artifacts. Text artifacts get inlined into the
    # prompt as before; image artifacts (jpg/jpeg/png/gif/webp) are
    # routed to the multimodal builder so they reach the LLM as
    # image_url content blocks instead of garbage bytes in the prompt.
    local artifact_context=""
    local artifact_image_args=()
    local artifact_count
    artifact_count=$(jq -r '.context.inputArtifacts | length // 0' "$INPUT_FILE" 2>/dev/null || echo 0)
    if [ "$artifact_count" -gt 0 ]; then
        local i=0
        while [ "$i" -lt "$artifact_count" ]; do
            local aname apath aext
            aname=$(jq -r ".context.inputArtifacts[$i].name" "$INPUT_FILE")
            apath=$(jq -r ".context.inputArtifacts[$i].path" "$INPUT_FILE")
            if [ -f "$apath" ]; then
                aext=$(printf '%s' "$apath" | awk -F. 'NF>1{print tolower($NF)}')
                case "$aext" in
                    jpg|jpeg|png|gif|webp)
                        artifact_image_args+=("--image" "$apath")
                        artifact_context="${artifact_context}

--- Input artifact: $aname (image attached for vision analysis) ---"
                        ;;
                    *)
                        artifact_context="${artifact_context}

--- Input artifact: $aname ---
$(cat "$apath")
--- End: $aname ---"
                        ;;
                esac
            fi
            i=$((i + 1))
        done
    fi

    local user_message="$prompt"
    if [ -n "$previous_result" ]; then
        user_message="${user_message}

--- Previous Step Result ---
${previous_result}
--- End Previous Step Result ---"
    fi
    if [ -n "$artifact_context" ]; then
        user_message="${user_message}${artifact_context}"
    fi

    # Use temp files for messages and request to avoid ARG_MAX limits.
    # Conversation history grows with each tool call iteration and can
    # easily exceed the OS argument size limit for jq --argjson.
    local msgs_file="$WORKSPACE/.messages.json"
    local req_file="$WORKSPACE/.request.json"
    local tools_file="$WORKSPACE/.tools.json"
    local mcp_tools_file="$WORKSPACE/.mcp_tools.json"
    local expanded_mcp_tools_file="$WORKSPACE/.expanded_mcp_tools.txt"
    local pinned_mcp_tools_file="$WORKSPACE/.pinned_mcp_tools.txt"
    MCP_TOOLS_FILE="$mcp_tools_file"
    EXPANDED_MCP_TOOLS_FILE="$expanded_mcp_tools_file"

    # Build the user content via vornik-agent-helper. Without images
    # the helper emits a JSON string (text-only fast path); with one
    # or more --image flags it emits a JSON array of content blocks
    # (text + image_url(s)). The chat layer accepts both shapes.
    local user_text_file="$WORKSPACE/.user_text.txt"
    local user_content_file="$WORKSPACE/.user_content.json"
    printf '%s' "$user_message" > "$user_text_file"
    vornik-agent-helper build-user-content \
        --text-file "$user_text_file" \
        "${artifact_image_args[@]}" > "$user_content_file"

    jq -n --arg sys "$system_prompt" --slurpfile usr "$user_content_file" \
        '[{"role":"system","content":$sys},{"role":"user","content":$usr[0]}]' > "$msgs_file"

    # Clear tool audit log for this invocation.
    rm -rf "$WORKSPACE/.tool_audit"
    mkdir -p "$WORKSPACE/.tool_audit"
    rm -rf "$WORKSPACE/.tool_results"
    mkdir -p "$WORKSPACE/.tool_results"
    rm -f "$expanded_mcp_tools_file"
    rm -f "$pinned_mcp_tools_file"
    touch "$expanded_mcp_tools_file"
    touch "$pinned_mcp_tools_file"

    # Discover MCP tools from the daemon proxy when available, otherwise
    # from project config written by the executor to /app/input/mcp.json.
    printf '[]' > "$mcp_tools_file"
    if command -v mcp-bridge >/dev/null 2>&1 && { [ -n "${VORNIK_API_URL:-}" ] || [ -f "/app/input/mcp.json" ]; }; then
        log "MCP: discovering tools"
        if mcp_out=$(mcp-bridge discover 2>/tmp/mcp_discover_err); then
            printf '%s' "$mcp_out" > "$mcp_tools_file"
            log "MCP: $(jq 'length' "$mcp_tools_file" 2>/dev/null || echo '?') tool(s) loaded"
        else
            log "WARN: mcp-bridge discover failed: $(cat /tmp/mcp_discover_err 2>/dev/null || true)"
        fi
        rm -f /tmp/mcp_discover_err
    fi

    # Build base built-in tool file once. The visible merged tool list is
    # rebuilt before every LLM call because tool_search can expand MCP tools
    # mid-loop without another discovery pass.
    local builtin_tools_tmp="$WORKSPACE/.builtin_tools.json"
    tool_definitions > "$builtin_tools_tmp"
    if defer_mcp_tools_enabled "$(jq 'length' "$mcp_tools_file" 2>/dev/null || echo 0)"; then
        printf '%s\n' "$system_prompt" \
            | grep -oE 'mcp__[A-Za-z0-9_-]+__[A-Za-z0-9_-]+' \
            | sort -u > "$pinned_mcp_tools_file" 2>/dev/null || true
        jq '.[0].content += "\n\nMCP tools may be lazy-loaded to reduce LLM context. If you need an MCP tool that is referenced by name but not visible in your function list, call tool_search with a short query first; matching tools will be available on the next turn."' \
            "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"
    fi
    rebuild_tools_file "$builtin_tools_tmp" "$mcp_tools_file" "$expanded_mcp_tools_file" "$pinned_mcp_tools_file" "$tools_file"
    if defer_mcp_tools_enabled "$(jq 'length' "$mcp_tools_file" 2>/dev/null || echo 0)"; then
        log "MCP: deferred exposure enabled (threshold=${AGENT_DEFER_MCP_THRESHOLD:-20}, search_limit=${AGENT_TOOL_SEARCH_LIMIT:-8})"
    fi

    # Degenerate loop detection: if the same tool call (name+args) repeats
    # CONSECUTIVELY, the LLM is stuck.
    #
    # Consecutive is load-bearing: any different call resets repeat_count
    # below, so two counted repeats are necessarily adjacent, and nothing can
    # have mutated between them. An edit-then-retest cycle — file_edit followed
    # by an identical `go test ./...` — breaks the chain and is never
    # suppressed. That adjacency is why no run_shell result cache is needed
    # here, and why one must not be added: a cache keyed on anything broader
    # would report "unchanged" to an agent whose edit HAD changed things.
    #
    # 3 -> 4 (2026-08-16). Degenerate loops were 23 of the 73 failures in the
    # long-horizon arm, spread across coder (12), analyst (8) and tester (3) —
    # a harness defect, not role tuning. The detector killed the step without
    # ever telling the model it was repeating itself, and its third call was
    # wasted work whose result was already known. The second adjacent repeat is
    # now answered with the previous result plus an explicit nudge (see
    # DEGENERATE_NUDGE_AT); the kill moves to the fourth, so a model that
    # ignores the nudge is still stopped.
    local last_tool_sig=""
    local repeat_count=0
    local last_tool_result=""
    MAX_REPEATS=4
    DEGENERATE_NUDGE_AT=2

    # NEAR-repeat detection. The exact detector above compares (tool, args)
    # byte-for-byte, so a call that varies one number evades it completely.
    # Observed 2026-08-18: a coder walked a file with `sed -n '175,270p'`, then
    # '176,280p', then '177,290p' — one line further per iteration — never
    # tripping the detector and burning the whole 125-iteration budget, which
    # then reported as iteration_exhausted rather than as the loop it was.
    #
    # The near signature collapses every run of digits to '#', so that family
    # of calls compares equal. Thresholds are deliberately looser than the
    # exact ones because legitimate work does repeat a shape — paging through
    # a large file is the honest version of exactly this pattern. So the first
    # response is ADVISORY (the call still runs, its result carries a warning)
    # and the kill sits at 3x the exact threshold.
    local last_near_sig=""
    local near_repeat_count=0
    NEAR_MAX_REPEATS=12
    NEAR_REPEAT_NUDGE_AT=5

    # Per-turn file_read cache. Two maps keyed by resolved absolute path:
    #   FILE_READ_CACHE[path] = "<iter>|<body>"   (successful reads)
    #   FILE_READ_MISSES[path] = "<iter>"          (file-not-found hits)
    # FILE_READ_REPEAT_MISS holds the path that triggered the terminal
    # not-found loop, set by exec_tool and checked by the caller.
    #
    # Purpose: stop the model from re-reading the same file as context
    # grows. Haiku in particular loops on re-reading PROJECT_CONTEXT.md
    # once its message history evicts the earlier read. A cached
    # "[already read on turn N]" still lets the model see the content
    # without paying another round-trip.
    #
    # For MISSES we treat the SECOND identical hit as a missing-
    # prerequisite failure: the first miss may be a legitimate
    # "check whether this exists" probe, but re-asking for a file we
    # already told you doesn't exist means the model is stuck waiting
    # for an upstream role's output that never materialised — retrying
    # never helps, fail fast so the task surfaces the real problem.
    # Declared with -g so exec_tool (a file-scope function) can read
    # and write them — bash's dynamic scoping on -A arrays inside a
    # function is reliable but -g makes the intent unambiguous.
    declare -gA FILE_READ_CACHE=()
    declare -gA FILE_READ_MISSES=()
    declare -g FILE_READ_REPEAT_MISS=""

    # Per-turn cache for OTHER read-only tools (broker bars/positions/
    # account, TA indicators, news, memory_search). Keyed by
    # "<tool>:<sha256(args)>". Same intent as FILE_READ_CACHE: when the
    # model re-asks for data it already fetched in this turn, return
    # the cached payload prefixed with "[already fetched on turn N]"
    # instead of paying another broker round-trip. Pre-cache audit
    # (2026-05-06 exec_20260506182952): strategist re-fetched NVDA
    # bars 3×, JPM 3×, MSFT 2×, TSLA 2×, SPY 2×, AMD 2× in a single
    # turn — six wasted calls that pushed the budget over the edge
    # and triggered the abstain-empty bail-out.
    #
    # NOT cacheable (returns deliberately omitted from the cache):
    #   - place_order / cancel_order: actions, not reads
    #   - get_quote: caller usually wants freshest mid-spread
    #   - current_time: cheap and the agent occasionally wants a
    #     recheck before a time-sensitive decision
    declare -gA TOOL_READ_CACHE=()

    # Conversation compaction interval: every N iterations, trim old tool
    # exchanges to prevent context exhaustion. Keep system + user + last
    # KEEP_RECENT messages, replace everything in between with a summary.
    COMPACT_EVERY=8
    KEEP_RECENT=10

    # Size-based compaction threshold, converted from tokens to approximate
    # bytes. 2026-07-12 incident (glm-5 rejected a 194561-token prompt;
    # task_20260712145902_18667395d2826b72) hardened the math:
    #   - reserve the OUTPUT budget (max_tokens) + a safety margin — the
    #     provider counts input+output against the window, the old formula
    #     counted input only;
    #   - estimate 3 bytes/token, not 4 — dense JSON/scraped-web content
    #     tokenizes short, and the old estimate ran ~20% optimistic;
    #   - keep the 80% factor on top as slack for the system/tools schema.
    # Falls back to 28000 bytes when VORNIK_LLM_CONTEXT_SIZE is not set.
    SIZE_KEEP_RECENT=6
    if [ "$LLM_CONTEXT_SIZE" -gt 0 ] 2>/dev/null; then
        local budget_tokens=$(( LLM_CONTEXT_SIZE - ${LLM_MAX_TOKENS:-8192} - 2048 ))
        [ "$budget_tokens" -lt 4000 ] && budget_tokens=4000
        SIZE_COMPACT_THRESHOLD=$(( budget_tokens * 3 * 80 / 100 ))
    else
        SIZE_COMPACT_THRESHOLD=28000
    fi
    # A single tool result must never dominate the window: compaction keeps
    # the last SIZE_KEEP_RECENT messages verbatim, so cap per-result bytes
    # at a quarter of the budget (the incident's kept tail alone exceeded
    # the whole model window at 256 KiB/result).
    if [ "$TOOL_RESULT_MAX_BYTES" -gt $(( SIZE_COMPACT_THRESHOLD / 4 )) ] 2>/dev/null; then
        TOOL_RESULT_MAX_BYTES=$(( SIZE_COMPACT_THRESHOLD / 4 ))
        debug "tool result cap lowered to $TOOL_RESULT_MAX_BYTES bytes (quarter of context budget)"
    fi
    debug "size compaction threshold: $SIZE_COMPACT_THRESHOLD bytes (ctx=$LLM_CONTEXT_SIZE tokens, max_tokens=${LLM_MAX_TOKENS:-8192})"

    # Tool-calling loop
    local iteration=0
    while [ "$iteration" -lt "$MAX_TOOL_ITERATIONS" ]; do
        iteration=$((iteration + 1))
        check_cancel
        if [ "$CANCELLED" = "1" ]; then return 0; fi

        # Workspace sanity check: a daemon-side cleanup race can wipe
        # $WORKSPACE while we're still running (the host's
        # pruneAllWorktrees on daemon restart removes the bind-mount
        # source). Without this check, the next 30+ shell ops cascade
        # "No such file or directory" errors and we never write
        # result.json — the executor just sees the container exit
        # weirdly. Detect it once, write a clean failure, exit.
        if [ ! -d "$WORKSPACE" ]; then
            log "ERROR: workspace dir vanished mid-execution ($WORKSPACE) — host-side cleanup race; aborting turn"
            exit 2
        fi

        # Compact conversation history periodically to stay within the
        # LLM context window. The first two messages (system + user) are
        # always preserved. Middle messages are replaced with a summary.
        if [ "$iteration" -gt 1 ] && [ "$(( (iteration - 1) % COMPACT_EVERY ))" -eq 0 ]; then
            local msg_count
            msg_count=$(jq 'length' "$msgs_file")
            if [ "$msg_count" -gt "$(( KEEP_RECENT + 2 ))" ]; then
                debug "compacting conversation: $msg_count messages"
                # Safe-start compaction: if the cut point lands on a tool-result
                # message, walk back to the owning assistant+tool_calls message so
                # we never send an orphaned toolResult with no matching toolUse.
                # Bedrock rejects requests where toolResult blocks appear without a
                # preceding toolUse in the same conversation turn.
                jq --argjson keep "$KEEP_RECENT" '
                    . as $all |
                    (length - $keep) as $raw |
                    (if ($raw > 2) and ($all[$raw] | .role == "tool") then
                        [range($raw) | . as $i |
                         if ($all[$i].role == "assistant" and ($all[$i] | has("tool_calls")))
                         then $i else empty end] |
                        if length > 0 then last else $raw end
                    else $raw end) as $safe |
                    ($safe - 2) as $trim |
                    [.[0], .[1]] +
                    (if $trim > 0 then
                        [{"role":"user","content":("(Previous " + ($trim|tostring) + " tool exchanges were compacted to save context. Continue from where you left off.)")}]
                    else [] end) +
                    .[$safe:]
                ' "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"
            fi
        fi

        # Size-based compaction: compact whenever messages exceed the threshold,
        # regardless of iteration count. Large tool outputs (file reads, shell
        # output) can blow up the context in just a few iterations, causing the
        # model to truncate its output and produce degenerate incomplete tool calls.
        local msgs_bytes
        msgs_bytes=$(wc -c < "$msgs_file" 2>/dev/null || echo 0)
        if [ "$msgs_bytes" -gt "$SIZE_COMPACT_THRESHOLD" ]; then
            local msg_count
            msg_count=$(jq 'length' "$msgs_file")
            if [ "$msg_count" -gt "$(( SIZE_KEEP_RECENT + 2 ))" ]; then
                debug "size compaction ($msgs_bytes bytes): $msg_count messages"
                jq --argjson keep "$SIZE_KEEP_RECENT" '
                    . as $all |
                    (length - $keep) as $raw |
                    (if ($raw > 2) and ($all[$raw] | .role == "tool") then
                        [range($raw) | . as $i |
                         if ($all[$i].role == "assistant" and ($all[$i] | has("tool_calls")))
                         then $i else empty end] |
                        if length > 0 then last else $raw end
                    else $raw end) as $safe |
                    ($safe - 2) as $trim |
                    [.[0], .[1]] +
                    (if $trim > 0 then
                        [{"role":"user","content":("(Conversation compacted: " + ($trim|tostring) + " earlier tool exchanges removed to stay within context window. Continue from where you left off.)")}]
                    else [] end) +
                    .[$safe:]
                ' "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"
            fi
            # Emergency clamp (2026-07-12): keep-tail compaction preserves the
            # last $SIZE_KEEP_RECENT messages VERBATIM, so with fat tool
            # results the compacted conversation can still exceed the model
            # window — glm-5 then 400s deterministically on every call.
            # Truncate oversized tool contents until the file is guaranteed
            # under budget; the model loses old tool detail, not the task.
            msgs_bytes=$(wc -c < "$msgs_file" 2>/dev/null || echo 0)
            if [ "$msgs_bytes" -gt "$SIZE_COMPACT_THRESHOLD" ]; then
                local per_msg_cap=$(( SIZE_COMPACT_THRESHOLD / (SIZE_KEEP_RECENT + 4) ))
                debug "emergency clamp: $msgs_bytes bytes still over threshold — capping tool contents at $per_msg_cap bytes"
                clamp_tool_contents "$msgs_file" "$per_msg_cap"
            fi
        fi
        local hygiene_before_bytes hygiene_after_bytes
        hygiene_before_bytes=$(wc -c < "$msgs_file" 2>/dev/null || echo 0)
        compact_old_tool_results "$msgs_file"
        hygiene_after_bytes=$(wc -c < "$msgs_file" 2>/dev/null || echo 0)
        if [ "$hygiene_after_bytes" -lt "$hygiene_before_bytes" ]; then
            debug "tool result hygiene: compacted message history ${hygiene_before_bytes}->${hygiene_after_bytes} bytes"
        fi

        debug "LLM call iteration $iteration/$MAX_TOOL_ITERATIONS"
        rebuild_tools_file "$builtin_tools_tmp" "$mcp_tools_file" "$expanded_mcp_tools_file" "$pinned_mcp_tools_file" "$tools_file"

        # Low-budget warning: at 80% of the budget, inject a user message so
        # the LLM sees the constraint as a recent, salient instruction.
        local warn_at=$(( MAX_TOOL_ITERATIONS * 8 / 10 ))
        local remaining=$(( MAX_TOOL_ITERATIONS - iteration + 1 ))
        if [ "$iteration" -eq "$warn_at" ] && [ "$warn_at" -gt 0 ]; then
            log "budget warning: $remaining tool calls remaining ($iteration/$MAX_TOOL_ITERATIONS used)"
            jq --argjson remaining "$remaining" --argjson total "$MAX_TOOL_ITERATIONS" \
                '. + [{"role":"user","content":("⚠ Tool budget: " + ($remaining|tostring) + " of " + ($total|tostring) + " calls remaining. Finish what you are doing and produce a final result — do not start new subtasks.")}]' \
                "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"
        fi

        # Build request from files — no shell variable size limits.
        # Build request. Include max_tokens to override gateway defaults (e.g.
        # bedrock-access-gateway defaults to 2048 which truncates large file writes).
        # Also include options.num_ctx for Ollama-direct endpoints that respect it;
        # OpenAI-compatible gateways (Bedrock, etc.) silently ignore the options field.
        #
        # response_format precedence (item 7 / item 8 of
        # https://docs.vornik.io):
        #   1. response_format=="json_schema" AND response_schema non-empty
        #      → typed directive `{"type":"json_schema","json_schema":{...}}`.
        #      The chat-proxy stamps it on ctx; per-provider adapters
        #      translate it into Bedrock's synthetic emit_response tool
        #      (with ToolChoice forcing), the Anthropic emit_result tool
        #      path, or OpenAI's native response_format field.
        #   2. response_format non-empty (e.g. "json_object") → loose
        #      type-only directive `{"type":"json_object"}`. The
        #      legacy fallback for roles without a full schema.
        #   3. response_format empty → no directive (free-form). For
        #      writer/dispatcher/vision roles that emit prose.
        # Schema name defaults to <role>_result for stable
        # observability — appears in upstream gateway tooling as the
        # schema identifier.
        local schema_name="${role}_result"
        # SCHEMA_FINALIZE_PENDING is set when the tool phase has ended and the
        # step still owes a schema-shaped answer. Building tool-free is what
        # lets build_llm_request_file emit the response_format directive at all.
        local step_tools_file="$tools_file"
        if [ "${SCHEMA_FINALIZE_PENDING:-0}" = "1" ]; then
            printf '[]\n' > "$WORKSPACE/.empty_tools.json"
            step_tools_file="$WORKSPACE/.empty_tools.json"
        fi
        build_llm_request_file "$req_file" "$msgs_file" "$step_tools_file" "$schema_name" "$response_format" "${response_schema:-null}"
        local request
        request=$(cat "$req_file")

        local req_size=${#request}
        local prompt_tokens_estimate visible_tools_count mcp_tools_count
        prompt_tokens_estimate=$(( (req_size + 2) / 3 ))
        visible_tools_count=$(jq 'length' "$tools_file" 2>/dev/null || echo 0)
        mcp_tools_count=$(jq 'length' "$mcp_tools_file" 2>/dev/null || echo 0)
        if [ "$req_size" -gt "${MAX_REQUEST_BYTES:-0}" ] 2>/dev/null; then
            MAX_REQUEST_BYTES="$req_size"
        fi
        if [ "$prompt_tokens_estimate" -gt "${MAX_PROMPT_TOKENS_ESTIMATE:-0}" ] 2>/dev/null; then
            MAX_PROMPT_TOKENS_ESTIMATE="$prompt_tokens_estimate"
        fi
        # Kept for diagnostics that fire LATER in the step (the degenerate-loop
        # guard below). Without it those messages can only guess at context
        # pressure, and a guess printed as a cause sends operators to the wrong
        # knob — see the message built at the guard.
        LAST_PROMPT_TOKENS_ESTIMATE="$prompt_tokens_estimate"
        log "preflight task_id=${task_id:-} execution_id=${execution_id:-} step_id=${STEP_ID:-} role=${role:-} iteration=$iteration request_bytes=$req_size prompt_tokens_estimate=$prompt_tokens_estimate context_size=${LLM_CONTEXT_SIZE:-0} max_tokens=${LLM_MAX_TOKENS:-0} visible_tools=$visible_tools_count mcp_catalog_tools=$mcp_tools_count"

        local step_prompt_budget prompt_tokens_budget_base projected_prompt_tokens
        step_prompt_budget=$(step_prompt_token_budget)
        prompt_tokens_budget_base="${TOTAL_PROMPT_TOKENS:-0}"
        if [ "${TOTAL_PROMPT_TOKENS_ESTIMATED:-0}" -gt "$prompt_tokens_budget_base" ] 2>/dev/null; then
            prompt_tokens_budget_base="$TOTAL_PROMPT_TOKENS_ESTIMATED"
        fi
        projected_prompt_tokens=$(( prompt_tokens_budget_base + prompt_tokens_estimate ))
        if [ "$step_prompt_budget" -gt 0 ] && [ "$projected_prompt_tokens" -gt "$step_prompt_budget" ]; then
            if [ "${PROMPT_TOKEN_BUDGET_FINAL_CALL:-0}" = "1" ]; then
                PROMPT_TOKEN_BUDGET_DETAIL="step prompt-token budget ${step_prompt_budget} would be exceeded again before a final answer; cumulative_prompt_tokens=${TOTAL_PROMPT_TOKENS}; cumulative_prompt_tokens_estimate=${TOTAL_PROMPT_TOKENS_ESTIMATED}; next_prompt_tokens_estimate=${prompt_tokens_estimate}"
                local last_content
                last_content=$(jq -r 'map(select(.role=="assistant" and .content != null)) | last.content // "Step stopped before another LLM call because the per-step prompt-token budget was exhausted."' "$msgs_file")
                write_result "COMPLETED" "$last_content" "" "$(get_duration)"
                log "prompt-token budget stop: $PROMPT_TOKEN_BUDGET_DETAIL"
                return 0
            fi

            PROMPT_TOKEN_BUDGET_FINAL_CALL=1
            log "prompt-token budget finalization: cumulative=$TOTAL_PROMPT_TOKENS cumulative_estimate=$TOTAL_PROMPT_TOKENS_ESTIMATED next_estimate=$prompt_tokens_estimate budget=$step_prompt_budget - making one tool-free final call"
            jq --argjson budget "$step_prompt_budget" \
               --argjson used "$prompt_tokens_budget_base" \
               --argjson next "$prompt_tokens_estimate" \
               '. + [{"role":"user","content":("Prompt-token budget: this step has used about " + ($used|tostring) + " prompt tokens, and the next request is estimated at " + ($next|tostring) + ", exceeding the per-step budget of " + ($budget|tostring) + ". Do not call tools. Summarize what you have, state any gaps, and produce the best final answer now.")}]' \
               "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"
            printf '[]\n' > "$WORKSPACE/.empty_tools.json"
            build_llm_request_file "$req_file" "$msgs_file" "$WORKSPACE/.empty_tools.json" "$schema_name" "$response_format" "${response_schema:-null}"
            request=$(cat "$req_file")
            req_size=${#request}
            prompt_tokens_estimate=$(( (req_size + 2) / 3 ))
            visible_tools_count=0
            projected_prompt_tokens=$(( prompt_tokens_budget_base + prompt_tokens_estimate ))
            if [ "$req_size" -gt "${MAX_REQUEST_BYTES:-0}" ] 2>/dev/null; then
                MAX_REQUEST_BYTES="$req_size"
            fi
            if [ "$prompt_tokens_estimate" -gt "${MAX_PROMPT_TOKENS_ESTIMATE:-0}" ] 2>/dev/null; then
                MAX_PROMPT_TOKENS_ESTIMATE="$prompt_tokens_estimate"
            fi
            PROMPT_TOKEN_BUDGET_DETAIL="step prompt-token budget ${step_prompt_budget} triggered a tool-free finalization call; cumulative_prompt_tokens_before_final=${TOTAL_PROMPT_TOKENS}; cumulative_prompt_tokens_estimate_before_final=${TOTAL_PROMPT_TOKENS_ESTIMATED}; final_prompt_tokens_estimate=${prompt_tokens_estimate}; projected_total=${projected_prompt_tokens}"
            log "preflight finalization task_id=${task_id:-} execution_id=${execution_id:-} step_id=${STEP_ID:-} role=${role:-} iteration=$iteration request_bytes=$req_size prompt_tokens_estimate=$prompt_tokens_estimate context_size=${LLM_CONTEXT_SIZE:-0} max_tokens=${LLM_MAX_TOKENS:-0} visible_tools=$visible_tools_count mcp_catalog_tools=$mcp_tools_count"
        fi

        local response
        response=$(llm_call "$request")
        local resp_size=${#response}
        debug "received response ($resp_size bytes)"
        TOTAL_PROMPT_TOKENS_ESTIMATED=$((TOTAL_PROMPT_TOKENS_ESTIMATED + prompt_tokens_estimate))

        # Accumulate token usage for cost metrics. BAG echoes Bedrock's
        # usage block verbatim; missing fields default to 0. Do this before
        # the validation branch so even error responses with a usage block
        # (rare, but some gateways return it on partial completions) count.
        if printf '%s' "$response" | jq -e '.usage' >/dev/null 2>&1; then
            _p=$(printf '%s' "$response" | jq -r '.usage.prompt_tokens // 0')
            _c=$(printf '%s' "$response" | jq -r '.usage.completion_tokens // 0')
            _cc=$(printf '%s' "$response" | jq -r '.usage.cache_creation_tokens // .usage.cache_creation_input_tokens // 0')
            _cr=$(printf '%s' "$response" | jq -r '.usage.cache_read_tokens // .usage.cache_read_input_tokens // .usage.prompt_tokens_details.cached_tokens // 0')
            TOTAL_PROMPT_TOKENS=$((TOTAL_PROMPT_TOKENS + _p))
            TOTAL_COMPLETION_TOKENS=$((TOTAL_COMPLETION_TOKENS + _c))
            TOTAL_CACHE_CREATION_TOKENS=$((TOTAL_CACHE_CREATION_TOKENS + _cc))
            TOTAL_CACHE_READ_TOKENS=$((TOTAL_CACHE_READ_TOKENS + _cr))
            if [ "$_p" -gt "${MAX_PROMPT_TOKENS_ACTUAL:-0}" ] 2>/dev/null; then
                MAX_PROMPT_TOKENS_ACTUAL="$_p"
            fi
            # Per-iteration cost hint. Uses injected pricing so a runaway
            # tool loop is visible as a rising trail in the log stream
            # rather than only surfacing after the task completes.
            _est_cost=$(awk -v p="$_p" -v c="$_c" -v ip="$LLM_COST_INPUT_PER_M" -v op="$LLM_COST_OUTPUT_PER_M" \
                'BEGIN { printf "%.4f", (p*ip + c*op) / 1000000.0 }')
            LAST_ITERATION_COST_USD="$_est_cost"
            TOTAL_COST_USD=$(awk -v t="$TOTAL_COST_USD" -v i="$_est_cost" 'BEGIN { printf "%.6f", t + i }')
            log "iteration=$iteration tokens_in=$_p tokens_out=$_c cache_write=$_cc cache_read=$_cr est_cost=\$$_est_cost (cumulative in=$TOTAL_PROMPT_TOKENS out=$TOTAL_COMPLETION_TOKENS cache_write=$TOTAL_CACHE_CREATION_TOKENS cache_read=$TOTAL_CACHE_READ_TOKENS)"
        fi
        TOTAL_ITERATIONS=$iteration

        # LLM usage stream: cumulative numbers for this (task, step,
        # role) row, posted after every iteration with a deterministic
        # ID so each call upserts into the same DB row. Closes the
        # "cancelled-task shows $0" gap because the daemon always has
        # the latest cumulative cost without depending on step
        # finalize. Best-effort: a non-2xx response is logged but
        # never fails the iteration — the post-step batch path still
        # writes the final row at step end.
        if [ -n "${VORNIK_API_URL:-}" ] && [ -n "$project_id" ] && [ -n "$role" ]; then
            local usage_id="tu_${task_id}_${STEP_ID}_${role}"
            local usage_body
            usage_body=$(jq -nc \
                --arg uid "$usage_id" \
                --arg pid "$project_id" \
                --arg tid "$task_id" \
                --arg eid "$execution_id" \
                --arg sid "$STEP_ID" \
                --arg role "$role" \
                --arg model "${LLM_MODEL:-}" \
                --argjson pt "$TOTAL_PROMPT_TOKENS" \
                --argjson ct "$TOTAL_COMPLETION_TOKENS" \
                --argjson cct "$TOTAL_CACHE_CREATION_TOKENS" \
                --argjson crt "$TOTAL_CACHE_READ_TOKENS" \
                --argjson it "$TOTAL_ITERATIONS" \
                --argjson cost "$TOTAL_COST_USD" \
                '{usage_id:$uid, project_id:$pid, task_id:$tid, execution_id:$eid, step_id:$sid, role:$role, model:$model, prompt_tokens:$pt, completion_tokens:$ct, cache_creation_tokens:$cct, cache_read_tokens:$crt, iterations:$it, cost_usd:$cost}')
            local usage_url
            vornik_resolve_url "${VORNIK_API_URL%/}/api/v1/internal/llm-usage"; local usage_url="$VORNIK_URL"
            curl -sS --max-time 5 -o /dev/null -w "%{http_code}" $VORNIK_CURL_OPT \
                -X POST -H "Content-Type: application/json" \
                -H "X-API-Key: ${VORNIK_API_KEY:-}" \
                --data "$usage_body" \
                "$usage_url" \
                > "$WORKSPACE/.llm_usage_stream_status" 2>/dev/null || true
            local usage_http
            usage_http=$(cat "$WORKSPACE/.llm_usage_stream_status" 2>/dev/null || echo "")
            if [ "$usage_http" != "204" ] && [ -n "$usage_http" ]; then
                debug "llm usage stream: HTTP $usage_http (will be persisted from result.json at step end)"
            fi
        fi

        # Budget tripwire: if the daemon injected VORNIK_BUDGET_*_REMAINING_USD
        # at step start, project whether the NEXT LLM call would breach the
        # remaining envelope and bail cleanly if so. Skipped when no envelope
        # was injected (project has no caps, or the snapshot failed) and on
        # iteration 1 (the dispatch-time budget gate already cleared this
        # task before we got here, and we have no per-iteration cost
        # observation yet to project from).
        #
        # Projection: assume next call costs the same as the most recent
        # one. Crude but stable — a runaway loop tends to grow monotonically
        # so the check trips before the truly expensive call. The trade-off
        # is that one cheap iteration after an expensive one might let
        # through one extra call; the daemon's eventual usage-record write
        # catches that on the next dispatch.
        if [ "$iteration" -ge 1 ] && [ -n "${LAST_ITERATION_COST_USD:-}" ] && \
           { [ -n "${VORNIK_BUDGET_DAILY_REMAINING_USD:-}" ] || [ -n "${VORNIK_BUDGET_MONTHLY_REMAINING_USD:-}" ]; }; then
            _envelope=$(awk -v d="${VORNIK_BUDGET_DAILY_REMAINING_USD:-999999999}" \
                            -v m="${VORNIK_BUDGET_MONTHLY_REMAINING_USD:-999999999}" \
                'BEGIN { printf "%.4f", (d < m) ? d : m }')
            _step_spent=$(awk -v p="$TOTAL_PROMPT_TOKENS" -v c="$TOTAL_COMPLETION_TOKENS" \
                              -v ip="$LLM_COST_INPUT_PER_M" -v op="$LLM_COST_OUTPUT_PER_M" \
                'BEGIN { printf "%.4f", (p*ip + c*op) / 1000000.0 }')
            _projected_next="$LAST_ITERATION_COST_USD"
            _bail=$(awk -v sp="$_step_spent" -v np="$_projected_next" -v env="$_envelope" \
                'BEGIN { print ((sp + np) >= env) ? 1 : 0 }')
            if [ "$_bail" = "1" ]; then
                log "BUDGET TRIPWIRE: step_spent=\$${_step_spent} projected_next_call=\$${_projected_next} remaining_envelope=\$${_envelope} — bailing before next LLM call"
                # Take the most recent non-empty assistant text as the
                # bail-out message so the operator sees what the agent had
                # produced just before stopping. Falls back to a synthetic
                # explainer if no assistant text exists yet (e.g. iteration
                # 1 was a tool-call that we didn't get to consume).
                _bail_msg=$(jq -r '[.[] | select(.role=="assistant" and .content != null and .content != "")] | last.content // ""' "$msgs_file")
                if [ -z "$_bail_msg" ]; then
                    _bail_msg="Step bailed mid-loop to stay within remaining budget envelope. No final assistant text was produced before the bail-out."
                fi
                _tripwire_detail="step spent ~\$${_step_spent}; projected next call ~\$${_projected_next}; remaining envelope ~\$${_envelope}"
                # Set the global so write_result merges outcome+detail into
                # the result.json the daemon parses.
                BUDGET_TRIPWIRE_DETAIL="$_tripwire_detail"
                write_result "COMPLETED" "$_bail_msg" "" "$(get_duration)"
                log "tripwire bail-out complete; exiting step cleanly"
                return 0
            fi
        fi

        if [ -z "$response" ] || ! printf '%s' "$response" | jq -e '.choices[0]' >/dev/null 2>&1; then
            local raw_preview
            raw_preview=$(printf '%.500s' "$response")
            log "ERROR: raw response: $raw_preview"
            local err_msg
            err_msg=$(printf '%s' "$response" | jq -r '.error.message // empty' 2>/dev/null)
            if [ -z "$err_msg" ]; then
                err_msg="LLM returned invalid response (no .choices[0]). Raw: $raw_preview"
            fi
            # Context-overflow rescue (2026-07-12): the proxy reports a
            # deterministic prompt-too-large 400 with a distinct code.
            # Re-sending the same conversation can never succeed — but
            # shrinking it can. Tighten the budget, force a compaction +
            # clamp pass, and retry within this container instead of dying
            # to the executor's retry ladder. Two rescues max: if the
            # conversation can't fit after that, something is structurally
            # wrong and failing loud is correct.
            local err_code
            err_code=$(printf '%s' "$response" | jq -r '.error.code // empty' 2>/dev/null)
            if [ "$err_code" = "CONTEXT_OVERFLOW" ] && [ "${OVERFLOW_RESCUES:-0}" -lt 2 ]; then
                OVERFLOW_RESCUES=$(( ${OVERFLOW_RESCUES:-0} + 1 ))
                SIZE_COMPACT_THRESHOLD=$(( SIZE_COMPACT_THRESHOLD / 2 ))
                [ "$SIZE_COMPACT_THRESHOLD" -lt 12000 ] && SIZE_COMPACT_THRESHOLD=12000
                log "WARN: context overflow from proxy — halving budget to $SIZE_COMPACT_THRESHOLD bytes, compacting and retrying (rescue $OVERFLOW_RESCUES/2)"
                local rescue_cap=$(( SIZE_COMPACT_THRESHOLD / (SIZE_KEEP_RECENT + 4) ))
                jq --argjson keep "$SIZE_KEEP_RECENT" '
                    . as $all |
                    (length - $keep) as $raw |
                    (if ($raw > 2) and ($all[$raw] | .role == "tool") then
                        [range($raw) | . as $i |
                         if ($all[$i].role == "assistant" and ($all[$i] | has("tool_calls")))
                         then $i else empty end] |
                        if length > 0 then last else $raw end
                    else $raw end) as $safe |
                    (if $safe > 2 then
                        [.[0], .[1],
                         {"role":"user","content":("(Conversation compacted: " + (($safe - 2)|tostring) + " earlier tool exchanges removed to stay within context window. Continue from where you left off.)")}] +
                        .[$safe:]
                    else . end)
                ' "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"
                clamp_tool_contents "$msgs_file" "$rescue_cap"
                continue
            fi
            log "ERROR: LLM call failed: $err_msg"
            write_result "FAILED" "LLM call failed: $err_msg" "" "$(get_duration)" "$err_msg"
            return 1
        fi

        local finish_reason
        finish_reason=$(printf '%s' "$response" | jq -r '.choices[0].finish_reason // "stop"')

        if [ "$finish_reason" != "tool_calls" ]; then
            # Final text response
            local content
            content=$(printf '%s' "$response" | jq -r '.choices[0].message.content // ""')
            # If the LLM returned empty content (some models do this after
            # tool calls), extract the last non-empty assistant message.
            if [ -z "$content" ]; then
                content=$(jq -r '[.[] | select(.role=="assistant" and .content != null and .content != "")] | last.content // "Task completed (no text response)"' "$msgs_file")
            fi

            # Some models emit tool calls as text (XML/function syntax)
            # instead of using the API tool_calls field. Detect this and
            # nudge the model back on track instead of treating it as
            # a final response.
            case "$content" in
                *'<function='*|*'<tool_call>'*|*'```tool_code'*)
                    log "WARN: LLM emitted tool call as text instead of using tool_calls API — nudging"
                    # Append the broken response + correction to conversation
                    local nudge_msg_file="$WORKSPACE/.nudge_msgs.json"
                    # --rawfile (not --arg): assistant content is max_tokens-bounded
                    # but can exceed MAX_ARG_STRLEN (128 KB) on large output caps.
                    local nudge_content_file="$WORKSPACE/.nudge_content_raw"
                    printf '%s' "$content" > "$nudge_content_file"
                    jq -n --rawfile content "$nudge_content_file" \
                        '[{"role":"assistant","content":$content},{"role":"user","content":"You tried to call a tool by writing XML/text, but you must use the tool_calls API. Do NOT write <function=...> or similar markup. Use the provided tools (file_read, file_write, run_shell, current_time) through the proper function calling interface. Now complete the original task."}]' > "$nudge_msg_file"
                    jq --slurpfile msgs "$nudge_msg_file" '. + $msgs[0]' "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"
                    continue
                    ;;
            esac

            # The tool phase suppressed response_format, because sending it
            # alongside tools makes tool calling impossible under guided
            # decoding. If this step declares a schema, spend ONE more
            # tool-free turn so the structured answer is still produced rather
            # than left to free-form prose. Once only — SCHEMA_FINALIZE_PENDING
            # is not cleared on the way back in, so a model that ignores the
            # schema cannot spin here.
            # A model that answered in prose WITHOUT ever calling a tool has not
            # finished a tool phase — it never started one. Finalizing here
            # removes the tools and makes a declared output file unwritable, which
            # is exactly how the research step failed three times over (initial,
            # shape retry and model fallback) while never issuing a single tool
            # call. Nudge once instead, keeping the tools, then let the normal
            # path run.
            if [ "${TOOL_PHASE_HAPPENED:-0}" = "0" ] && \
               [ "${NO_TOOL_NUDGE_SENT:-0}" = "0" ] && \
               [ "$(jq 'length' "$tools_file" 2>/dev/null || echo 0)" -gt 0 ]; then
                NO_TOOL_NUDGE_SENT=1
                log "no-tool nudge: step ended on iteration $iteration without a single tool call"
                printf '%s' "$response" | jq -c '[.choices[0].message]' > "$WORKSPACE/.notool_msg.json"
                jq --slurpfile msg "$WORKSPACE/.notool_msg.json" \
                   '. + $msg[0] + [{"role":"user","content":"You produced a text answer without calling any tool. If this step must write a file, you have not written it yet — use the provided tools to do the work now, then finish. Do not answer in prose alone."}]' \
                   "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"
                continue
            fi

            if [ -n "$response_format" ] && \
               [ "${SCHEMA_FINALIZE_PENDING:-0}" = "0" ] && \
               [ "${TOOL_PHASE_HAPPENED:-0}" = "1" ] && \
               [ "$(jq 'length' "$tools_file" 2>/dev/null || echo 0)" -gt 0 ]; then
                SCHEMA_FINALIZE_PENDING=1
                log "schema finalization: tool phase ended, re-asking tool-free so response_format applies"
                printf '%s' "$response" | jq -c '[.choices[0].message]' > "$WORKSPACE/.final_msg.json"
                jq --slurpfile msg "$WORKSPACE/.final_msg.json" '. + $msg[0]' "$msgs_file" > "$msgs_file.tmp" \
                    && mv "$msgs_file.tmp" "$msgs_file"
                continue
            fi

            # LAST CHANCE to satisfy a declared output-file contract.
            #
            # The daemon fails the step if no file matches the glob, and by the
            # time it looks the container is gone — so an agent that simply
            # forgot loses the whole step. Worse, the tool-free schema
            # finalization above GUARANTEES the file can never be written once
            # entered, because no tools are offered there.
            #
            # Observed 2026-08-16: dp-02-parser-hardening failed 3/3 runs this
            # way. The agent logged "completed successfully" and the step failed
            # on a missing artifacts/out/CHANGELOG-partial.md.
            #
            # One nudge only. If the agent ignores it, the daemon's check still
            # fails the step — this converts a common miss into a self-correction
            # without inventing a new way to loop.
            if [ -n "${REQUIRE_OUTPUT_GLOB:-}" ] && [ "${OUTPUT_CONTRACT_NUDGED:-0}" = "0" ] \
               && ! output_contract_satisfied; then
                OUTPUT_CONTRACT_NUDGED=1
                log "output contract unmet ($REQUIRE_OUTPUT_GLOB) — nudging before finish"
                printf '%s' "$response" | jq -c '[.choices[0].message]' > "$WORKSPACE/.final_msg.json"
                jq --slurpfile msg "$WORKSPACE/.final_msg.json" '. + $msg[0]' "$msgs_file" > "$msgs_file.tmp" \
                    && mv "$msgs_file.tmp" "$msgs_file"
                jq --arg g "$REQUIRE_OUTPUT_GLOB" \
                   '. + [{"role":"user","content":("This step declares a required output file and no file matching " + $g + " has been written yet. Write it now with the file_write tool, then finish. The step will FAIL without it.")}]' \
                   "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"
                SCHEMA_FINALIZE_PENDING=0
                continue
            fi

            # Plausibility pre-check, symmetric with the output-contract nudge
            # above and for the same reason: the daemon applies these rules
            # AFTER this container exits, so a violation discovered there costs
            # the whole step. One nudge, naming the exact rule and field — if
            # the agent ignores it the daemon still fails the step, so this
            # converts the commonest miss into a self-correction without
            # inventing a new way to loop.
            #
            # Must run BEFORE finalisation completes: once the tool-free turn
            # has happened there are no tools left to fix anything with, which
            # is the trap the require_output_glob fix documented.
            if [ "${PLAUSIBILITY_NUDGED:-0}" = "0" ]; then
                local _plaus_bad
                _plaus_bad=$(plausibility_violations "$content")
                if [ -n "$_plaus_bad" ]; then
                    PLAUSIBILITY_NUDGED=1
                    log "plausibility violation before finish ($_plaus_bad) — nudging"
                    printf '%s' "$response" | jq -c '[.choices[0].message]' > "$WORKSPACE/.final_msg.json"
                    jq --slurpfile msg "$WORKSPACE/.final_msg.json" '. + $msg[0]' "$msgs_file" > "$msgs_file.tmp" \
                        && mv "$msgs_file.tmp" "$msgs_file"
                    jq --arg v "$_plaus_bad" \
                       '. + [{"role":"user","content":("Your result breaks a required output rule and the step will FAIL as written: " + $v + ". Either supply those fields with real values, or change the condition that triggered the rule if it is not actually true. Re-emit the complete result.")}]' \
                       "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"
                    SCHEMA_FINALIZE_PENDING=0
                    continue
                fi
            fi

            debug "LLM returned final response (${#content} chars)"
            write_result "COMPLETED" "$content" "$content" "$(get_duration)"
            log "completed successfully"
            return 0
        fi

        # Process tool calls — append assistant message to conversation via file
        local assistant_msg_file="$WORKSPACE/.assistant_msg.json"
        printf '%s' "$response" | jq -c '.choices[0].message' > "$assistant_msg_file"
        jq --slurpfile msg "$assistant_msg_file" '. + $msg' "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"

        local tool_calls
        tool_calls=$(printf '%s' "$response" | jq -c '.choices[0].message.tool_calls // []')
        local tc_count
        tc_count=$(printf '%s' "$tool_calls" | jq 'length')
        # Whether a tool phase happened AT ALL this step. Schema finalization is
        # a post-tool-phase move; without this it fires on a first-turn prose
        # reply, strips the tools, and guarantees a declared output file can
        # never be written.
        TOOL_PHASE_HAPPENED=1

        debug "processing $tc_count tool call(s)"

        local tc_idx=0
        while [ "$tc_idx" -lt "$tc_count" ]; do
            local tc_id tc_name tc_args
            tc_id=$(printf '%s' "$tool_calls" | jq -r ".[$tc_idx].id")
            tc_name=$(printf '%s' "$tool_calls" | jq -r ".[$tc_idx].function.name")
            tc_args=$(printf '%s' "$tool_calls" | jq -r ".[$tc_idx].function.arguments")

            # file_read cache lookup BEFORE the degenerate-loop detector.
            # When the model re-reads the same file, we short-circuit to
            # the cached body and don't count this call toward the
            # detector's repeat streak — cache hits are free, the only
            # thing to protect against is the model doing real work on
            # the same input. Lives in the parent shell so writes below
            # actually persist (exec_tool runs in a $(...) subshell
            # which discards array mutations).
            local tool_result=""
            local tc_cache_hit=0
            # Per-call, not per-step: without the reset the first near-repeat
            # would brand every later call's result with the advisory.
            local near_repeat_warn=0
            if [ "$tc_name" = "file_read" ]; then
                local rp_raw rp_abs
                rp_raw=$(printf '%s' "$tc_args" | jq -r '.path // empty')
                if [ -n "$rp_raw" ] && [ "$rp_raw" != "null" ]; then
                    if rp_abs="$(resolve_path "$rp_raw" 2>&1)"; then
                        if [ -n "${FILE_READ_CACHE[$rp_abs]+x}" ]; then
                            local _cached="${FILE_READ_CACHE[$rp_abs]}"
                            local _prev_iter="${_cached%%|*}"
                            local _prev_body="${_cached#*|}"
                            tool_result=$(printf '[already read on turn %s — content unchanged]\n\n%s' "$_prev_iter" "$_prev_body")
                            tc_cache_hit=1
                            debug "tool: file_read (id=$tc_id) [cache hit from turn $_prev_iter]"
                        fi
                    fi
                fi
            elif tool_is_cacheable_read "$tc_name"; then
                # Other read-only tools: dedup on (tool, args). The
                # canonical key uses jq -cS to normalise whitespace +
                # key order so semantically-identical args (e.g. {a,b}
                # vs {b,a}) collapse to one cache entry.
                local _norm_args _read_key
                _norm_args=$(printf '%s' "$tc_args" | jq -cS . 2>/dev/null) || _norm_args="$tc_args"
                _read_key="${tc_name}:${_norm_args}"
                if [ -n "${TOOL_READ_CACHE[$_read_key]+x}" ]; then
                    local _rcached="${TOOL_READ_CACHE[$_read_key]}"
                    local _rprev_iter="${_rcached%%|*}"
                    local _rprev_body="${_rcached#*|}"
                    tool_result=$(printf '[already fetched on turn %s — same args, cached result]\n\n%s' "$_rprev_iter" "$_rprev_body")
                    tc_cache_hit=1
                    debug "tool: $tc_name (id=$tc_id) [read-cache hit from turn $_rprev_iter]"
                fi
            fi

            # Degenerate loop detection: same tool+args repeated
            # consecutively. Cache hits bypass this check — they're
            # free and harmless; only real exec_tool work should count.
            if [ "$tc_cache_hit" = "0" ]; then
                local tool_sig="${tc_name}:${tc_args}"
                if [ "$tool_sig" = "$last_tool_sig" ]; then
                    repeat_count=$((repeat_count + 1))
                    # Second adjacent identical call: do not execute it. The
                    # arguments are byte-identical and nothing has run in
                    # between — adjacency guarantees that — so the result
                    # cannot differ, and re-running it is pure waste. Return
                    # what it produced last time with an explicit statement
                    # that repeating will not help. This is the only channel
                    # that tells the model WHY the step is about to die.
                    if [ "$repeat_count" -ge "$DEGENERATE_NUDGE_AT" ] && [ -n "$last_tool_result" ]; then
                        log "degenerate repeat $repeat_count/$MAX_REPEATS on $tc_name — returning the previous result with a nudge instead of re-running"
                        tool_result=$(printf '[identical to your previous %s call, which ran with the same arguments. Nothing has changed since, so calling it again will return this same result. Change your approach or produce your final answer now.]\n\n%s' "$tc_name" "$last_tool_result")
                        tc_cache_hit=1
                    fi
                    if [ "$repeat_count" -ge "$MAX_REPEATS" ]; then
                        log "ERROR: degenerate loop detected — $tc_name called $MAX_REPEATS times with identical args"
                        local last_content
                        last_content=$(jq -r 'map(select(.role=="assistant" and .content != null)) | last.content // "Agent entered degenerate loop"' "$msgs_file")
                        # Report MEASURED context pressure, not a guess.
                        #
                        # This message used to assert "This usually means the
                        # context window is exhausted." The 2026-08-16 benchmark
                        # arm falsified that: sw-10-no-clean-answer looped 3/3
                        # runs at prompt_tokens_estimate=16603 against
                        # context_size=100000 — 17% used. An operator reading
                        # the old text would raise the context size, which was
                        # never the constraint.
                        local _ctx_used="${LAST_PROMPT_TOKENS_ESTIMATE:-0}"
                        local _ctx_size="${LLM_CONTEXT_SIZE:-0}"
                        local _ctx_note
                        if [ "${_ctx_size:-0}" -gt 0 ] 2>/dev/null; then
                            local _ctx_pct=$(( _ctx_used * 100 / _ctx_size ))
                            if [ "$_ctx_pct" -ge 80 ]; then
                                _ctx_note="Context was ${_ctx_pct}% full (~${_ctx_used}/${_ctx_size} tokens), so context exhaustion is the likely cause."
                            else
                                _ctx_note="Context was only ${_ctx_pct}% full (~${_ctx_used}/${_ctx_size} tokens), so this is NOT context exhaustion — the model is repeating itself for another reason (commonly an unsatisfiable instruction, or a tool whose result does not change)."
                            fi
                        else
                            _ctx_note="Context utilisation unknown."
                        fi
                        write_result "FAILED" "Agent entered a degenerate loop (repeated $tc_name $MAX_REPEATS times with the same arguments). ${_ctx_note}" "$last_content" "$(get_duration)" "degenerate tool loop"
                        return 1
                    fi
                else
                    last_tool_sig="$tool_sig"
                    repeat_count=1
                fi

                # Near-repeat: same tool, arguments identical once digits are
                # collapsed. Tracked independently of the exact counter — a
                # sliding window never repeats exactly, so the exact counter
                # stays at 1 throughout while this one climbs.
                local near_sig
                near_sig="${tc_name}:$(printf '%s' "$tc_args" | tr -s '0-9' '#')"
                if [ "$near_sig" = "$last_near_sig" ]; then
                    near_repeat_count=$((near_repeat_count + 1))
                    if [ "$near_repeat_count" -ge "$NEAR_MAX_REPEATS" ]; then
                        log "ERROR: degenerate loop detected — $tc_name called $NEAR_MAX_REPEATS times with near-identical args"
                        local last_near_content
                        last_near_content=$(jq -r 'map(select(.role=="assistant" and .content != null)) | last.content // "Agent entered degenerate loop"' "$msgs_file")
                        write_result "FAILED" "Agent entered a degenerate loop (repeated $tc_name $NEAR_MAX_REPEATS times with near-identical arguments — the same command varying only in numbers, e.g. a line range advancing one step at a time). Vary your approach rather than your arguments: read the whole file with file_read, or accept what you have and answer." "$last_near_content" "$(get_duration)" "degenerate tool loop"
                        return 1
                    fi
                    if [ "$near_repeat_count" -ge "$NEAR_REPEAT_NUDGE_AT" ]; then
                        near_repeat_warn=1
                    fi
                else
                    last_near_sig="$near_sig"
                    near_repeat_count=1
                fi
            fi

            # GNU date's %s%3N concatenates seconds with zero-padded
            # milliseconds → a straight millisecond epoch we can
            # subtract to get real ms resolution.
            local tc_start_ms
            tc_start_ms=$(ms_now)

            if [ "$tc_cache_hit" = "0" ]; then
                debug "tool: $tc_name (id=$tc_id)"
                tool_result=$(exec_tool "$tc_name" "$tc_args" 2>&1 | head -c "$TOOL_RESULT_MAX_BYTES")
                # Advisory near-repeat warning: the call RAN and its real
                # result is preserved — only a note is appended. Unlike the
                # exact-repeat nudge we cannot substitute the previous result,
                # because the arguments genuinely differ and so may the answer.
                if [ "${near_repeat_warn:-0}" = "1" ]; then
                    log "near-repeat $near_repeat_count/$NEAR_MAX_REPEATS on $tc_name — appending advisory"
                    tool_result=$(printf '%s\n\n[you have now called %s %s times with the same command varying only in numbers. That pattern usually means you are inching through something a single call could fetch — read the whole file with file_read, widen the range once, or answer with what you already have. The step will be stopped at %s such calls.]' "$tool_result" "$tc_name" "$near_repeat_count" "$NEAR_MAX_REPEATS")
                fi
                # Kept so an adjacent identical repeat can be answered with
                # what this call produced instead of re-running it. Only the
                # most recent executed result is retained — the degenerate
                # detector is consecutive-only, so nothing older can ever be
                # the answer to a repeat.
                last_tool_result="$tool_result"

                # Populate the cache / miss tracker AFTER exec_tool for
                # file_read. Must happen here (parent shell) — the
                # subshell inside $(exec_tool ...) can't write to the
                # associative arrays declared in this function's scope.
                if [ "$tc_name" = "file_read" ]; then
                    local post_raw post_abs
                    post_raw=$(printf '%s' "$tc_args" | jq -r '.path // empty')
                    if [ -n "$post_raw" ] && [ "$post_raw" != "null" ]; then
                        if post_abs="$(resolve_path "$post_raw" 2>&1)"; then
                            case "$tool_result" in
                                "ERROR: file not found:"*)
                                    # Advisory paths: agent role systemPrompts
                                    # often probe optional artifacts
                                    # (CURRENT_TASK.md, PROJECT_CONTEXT.md,
                                    # COVERAGE_REPORT.md, BACKLOG.md). Treat
                                    # repeated misses on these as exploration,
                                    # not a missing prerequisite — the role's
                                    # own spec or previousStepResult covers
                                    # the case where they don't exist. Match
                                    # by basename so probes with or without a
                                    # `.autonomy/` prefix are both treated as
                                    # advisory (LLMs invent both shapes). The
                                    # strict guard still fires on every other
                                    # path so a confused agent looping on a
                                    # real prerequisite still aborts.
                                    case "${post_abs##*/}" in
                                        CURRENT_TASK.md|PROJECT_CONTEXT.md|COVERAGE_REPORT.md|BACKLOG.md)
                                            tool_result="ERROR: file not found (advisory — proceed using the step prompt or previousStepResult as spec): $post_abs"
                                            ;;
                                        *)
                                            if [ -n "${FILE_READ_MISSES[$post_abs]+x}" ]; then
                                                FILE_READ_REPEAT_MISS="$post_abs"
                                                tool_result="ERROR: file not found (already confirmed missing on turn ${FILE_READ_MISSES[$post_abs]}): $post_abs"
                                            else
                                                FILE_READ_MISSES[$post_abs]="$iteration"
                                            fi
                                            ;;
                                    esac
                                    ;;
                                "ERROR:"*)
                                    : # other errors (bad path, etc) — don't cache
                                    ;;
                                *)
                                    FILE_READ_CACHE[$post_abs]="${iteration}|${tool_result}"
                                    ;;
                            esac
                        fi
                    fi
                elif tool_is_cacheable_read "$tc_name"; then
                    # Cache successful reads only. ERROR / mcp-bridge
                    # 502 / null-return responses don't get cached so
                    # the model can legitimately retry once if the
                    # upstream service was transiently flaky.
                    case "$tool_result" in
                        "ERROR:"*|"mcp-bridge:"*)
                            : # transient; allow retry
                            ;;
                        *)
                            local _post_norm _post_key
                            _post_norm=$(printf '%s' "$tc_args" | jq -cS . 2>/dev/null) || _post_norm="$tc_args"
                            _post_key="${tc_name}:${_post_norm}"
                            TOOL_READ_CACHE[$_post_key]="${iteration}|${tool_result}"
                            ;;
                    esac
                fi
            fi

            if tool_result_hygiene_enabled; then
                case "$tc_id" in
                    *[!A-Za-z0-9_.:-]*)
                        debug "tool result hygiene: not saving result for unsafe tool_call_id=$tc_id"
                        ;;
                    *)
                        mkdir -p "$WORKSPACE/.tool_results"
                        printf '%s' "$tool_result" > "$WORKSPACE/.tool_results/${tc_id}.txt"
                        ;;
                esac
            fi

            local tc_duration_ms=$(( $(ms_now) - tc_start_ms ))

            # Terminal: file_read hit the same missing path twice in
            # this turn. Retrying never materialises an upstream file —
            # the real fix is at the producer. Bail out with a
            # missing_prerequisite-style failure so the downstream
            # consumer (or operator watching the task) sees the real
            # cause instead of a generic "degenerate loop" tripping
            # three iterations later.
            if [ -n "$FILE_READ_REPEAT_MISS" ] && in_recovery_hop; then
                # A RECOVERY hop is here BECAUSE a step failed, and the two
                # dominant real triggers — plausibility_violation and
                # verify_claims_failed, 57 and 29 of the ledger's recover hops —
                # are both "the file is not there" shaped. Bailing would require
                # the missing artifact to exist before the lead may propose what
                # to do about it missing, which is circular. Measured on the
                # recovery-probe fixture 2026-08-19: 5 of 15 recover hops died
                # exactly this way.
                #
                # Cleared, not just skipped, so it cannot re-fire on the next
                # iteration; logged so the suppression is visible rather than
                # looking like the guard never triggered.
                log "missing_prerequisite SUPPRESSED (recovery hop): file_read of $FILE_READ_REPEAT_MISS missed twice, but a missing artifact is the premise here — continuing so the lead can emit its decision"
                FILE_READ_REPEAT_MISS=""
            fi
            if [ -n "$FILE_READ_REPEAT_MISS" ]; then
                log "ERROR: missing_prerequisite — file_read of $FILE_READ_REPEAT_MISS failed twice, aborting turn"
                local last_content
                last_content=$(jq -r 'map(select(.role=="assistant" and .content != null)) | last.content // "Agent hit missing prerequisite"' "$msgs_file")
                write_result "FAILED" "Missing prerequisite: file_read of \"$FILE_READ_REPEAT_MISS\" returned not-found twice. This usually means an upstream role (researcher, planner) did not produce the expected artifact — check that step's outcome." "$last_content" "$(get_duration)" "missing_prerequisite"
                return 1
            fi

            # Record tool invocation for audit. Write to a unique JSON file
            # instead of a shared JSONL — avoids race conditions during
            # concurrent or interrupted tool calls.
            local tc_audit_id="ta_${tc_start_ms}_${tc_id}"
            local tc_audit_file="$WORKSPACE/.tool_audit/${tc_audit_id}.json"
            local tc_output_truncated
            tc_output_truncated=$(printf '%.4096s' "$tool_result")
            jq -nc \
                --arg id "$tc_audit_id" \
                --arg name "$tc_name" \
                --arg input "$tc_args" \
                --arg output "$tc_output_truncated" \
                --argjson ms "$tc_duration_ms" \
                '{"audit_id":$id,"tool":$name,"input":$input,"output":$output,"duration_ms":$ms}' \
                > "$tc_audit_file"

            # Realtime audit stream — flush this row to the daemon
            # NOW so a crashed agent doesn't lose its trail. The
            # daemon's INSERT is idempotent on audit_id so the
            # post-step batch (built from $WORKSPACE/.tool_audit/
            # files) won't double-count. Best-effort: a non-2xx
            # response is logged but doesn't fail the tool call —
            # the post-step batch is still the safety net.
            if [ -n "${VORNIK_API_URL:-}" ] && [ -n "$project_id" ]; then
                local audit_body
                audit_body=$(jq -nc \
                    --arg id "$tc_audit_id" \
                    --arg pid "$project_id" \
                    --arg tid "$task_id" \
                    --arg eid "$execution_id" \
                    --arg sid "$STEP_ID" \
                    --arg name "$tc_name" \
                    --arg input "$tc_args" \
                    --arg output "$tc_output_truncated" \
                    --argjson ms "$tc_duration_ms" \
                    '{audit_id:$id, project_id:$pid, task_id:$tid, execution_id:$eid, step_id:$sid, tool_name:$name, tool_input:$input, tool_output:$output, duration_ms:$ms}')
                local audit_url
                vornik_resolve_url "${VORNIK_API_URL%/}/api/v1/internal/tool-audit"; local audit_url="$VORNIK_URL"
                curl -sS --max-time 5 -o /dev/null -w "%{http_code}" $VORNIK_CURL_OPT \
                    -X POST -H "Content-Type: application/json" \
                    -H "X-API-Key: ${VORNIK_API_KEY:-}" \
                    --data "$audit_body" \
                    "$audit_url" \
                    > "$WORKSPACE/.tool_audit_stream_status" 2>/dev/null || true
                local audit_http
                audit_http=$(cat "$WORKSPACE/.tool_audit_stream_status" 2>/dev/null || echo "")
                if [ "$audit_http" != "204" ] && [ -n "$audit_http" ]; then
                    debug "tool audit stream: HTTP $audit_http for $tc_name (will be persisted from result.json at step end)"
                fi
            fi

            # Append tool result message. Pass the (possibly large) content via a
            # FILE with --rawfile, NOT --arg: a single command-line argument is
            # capped at MAX_ARG_STRLEN (128 KB) regardless of total ARG_MAX, so a
            # large tool result fails with "jq: Argument list too long". printf is
            # a shell builtin, so writing the value to the file isn't arg-limited.
            local tool_msg_file="$WORKSPACE/.tool_msg.json"
            local tool_result_file="$WORKSPACE/.tool_result_raw"
            printf '%s' "$tool_result" > "$tool_result_file"
            jq -n --arg id "$tc_id" --rawfile content "$tool_result_file" \
                '{"role":"tool","tool_call_id":$id,"content":$content}' > "$tool_msg_file"
            jq --slurpfile msg "$tool_msg_file" '. + $msg' "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"

            tc_idx=$((tc_idx + 1))
        done
    done

    # Iteration cap reached. ONE forced tool-free turn before giving up.
    #
    # WHY. The agent is TOLD about its budget — the system prompt says "when
    # the budget is nearly exhausted, stop starting new work and produce your
    # best output with what you have", and a warning fires at 80% — but at the
    # cap it was never given a turn in which to comply. The step produced
    # nothing at all, so it was not even judgeable: it left the schema
    # denominator entirely and landed in NoOutput.
    #
    # In the 2026-08-16 ctx32k arm this was 14 of 47 failed steps (30%), and
    # every one sat EXACTLY at its budget — analyst 50 used 50/51/52/53, coder
    # 250 used 258. A wall, not a tail.
    #
    # This is the same shape as the prompt-token-budget finalisation above,
    # which already does exactly this for its own ceiling. Applying an
    # established pattern to a path that lacked it, not inventing one.
    #
    # NOT A SILENT PASS. The result carries outcome=iteration_exhausted, so the
    # ledger still records the budget exhaustion and the quality row never
    # reads ok. Raising the caps is deliberately NOT the fix here: the analyst
    # is already at 50 and the tester at 100, and the degenerate-loop nudge
    # reduces wasted iterations at the source.
    log "tool iteration cap reached ($MAX_TOOL_ITERATIONS) — one tool-free finalization turn before giving up"
    local last_content
    last_content=$(jq -r 'map(select(.role=="assistant" and .content != null)) | last.content // ""' "$msgs_file")

    jq --argjson cap "$MAX_TOOL_ITERATIONS" \
       '. + [{"role":"user","content":("You have used all " + ($cap|tostring) + " of your tool calls and no more are available. Do not call tools. Using only what you already have, produce your best complete final answer now, in the required output format. If some part is unfinished, say so explicitly rather than omitting it.")}]' \
       "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"

    printf '[]\n' > "$WORKSPACE/.empty_tools.json"
    local cap_req="$WORKSPACE/.cap_final_request.json" cap_resp cap_content=""
    build_llm_request_file "$cap_req" "$msgs_file" "$WORKSPACE/.empty_tools.json" \
        "${role}_result" "$response_format" "${response_schema:-null}"
    if cap_resp=$(llm_call "$(cat "$cap_req")" 2>/dev/null); then
        cap_content=$(printf '%s' "$cap_resp" | jq -r '.choices[0].message.content // ""' 2>/dev/null)
    fi

    if [ -n "$cap_content" ]; then
        ITERATION_CAP_DETAIL="tool iteration cap reached ($MAX_TOOL_ITERATIONS iterations); answered on a forced tool-free finalization turn"
        log "iteration cap: produced a final answer without tools (${#cap_content} chars)"
        write_result "COMPLETED" "$cap_content" "$cap_content" "$(get_duration)"
        return 0
    fi

    log "ERROR: tool iteration cap reached ($MAX_TOOL_ITERATIONS) and the tool-free finalization produced nothing"
    write_result "FAILED" "Tool iteration limit ($MAX_TOOL_ITERATIONS) reached and a final tool-free turn produced no answer. The task was too complex for the configured limit. Increase VORNIK_MAX_TOOL_ITERATIONS or simplify the task." "$last_content" "$(get_duration)" "tool iteration cap reached ($MAX_TOOL_ITERATIONS iterations)"
    log "failed (iteration cap)"
    return 1
}

# Warm mode: loop waiting for new tasks via .ready sentinel file.
# Set VORNIK_WARM_MODE=1 to enable.
warm_loop() {
    log "warm mode enabled — waiting for tasks"
    READY_FILE="/app/input/.ready"
    SHUTDOWN_FILE="/app/input/.shutdown"
    DONE_FILE="/app/output/.done"

    while true; do
        # Wait for ready signal or shutdown
        while [ ! -f "$READY_FILE" ]; do
            if [ -f "$SHUTDOWN_FILE" ]; then
                log "shutdown requested"
                exit 0
            fi
            sleep 0.5
        done

        # Clear previous output
        rm -f "$OUTPUT_FILE" "$DONE_FILE"
        rm -rf "$WORKSPACE/artifacts/out"
        START_TIME=$(date +%s)

        log "warm: task ready signal received"
        rm -f "$READY_FILE"

        # Run the task. Temporarily disable set -e: in Busybox ash the
        # "func || true" idiom does NOT prevent set -e from calling exit()
        # inside the function body — only set +e/set -e properly isolates it.
        set +e
        main "$@"
        _task_rc=$?
        set -e
        if [ "$_task_rc" -ne 0 ]; then
            log "warn: task main() exited with code $_task_rc"
        fi

        # Signal completion to host
        touch "$DONE_FILE"
        log "warm: task done, waiting for next"
    done
}

# Trap unexpected exits to always produce a result.json.
# Without this, set -e kills the script silently and the executor
# sees no output — just "container exited with code 1".
trap_handler() {
    _exit_code=$?
    if [ "$_exit_code" -ne 0 ] && [ ! -f "$OUTPUT_FILE" ]; then
        log "ERROR: unexpected exit (code $_exit_code), writing emergency result"
        mkdir -p "$(dirname "$OUTPUT_FILE")"
        printf '{"status":"FAILED","message":"Agent crashed unexpectedly (exit code %d). Check container logs for details.","outputArtifacts":[],"delegatedTasks":[],"diagnostics":{"exitCode":%d,"durationSeconds":%d}}\n' \
            "$_exit_code" "$_exit_code" "$(( $(date +%s) - START_TIME ))" > "$OUTPUT_FILE"
    fi
}
trap trap_handler EXIT

# Skip main() when the script is being sourced — tests source this file to
# invoke exec_tool() directly against a temp workspace.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
    if [ "${VORNIK_WARM_MODE:-}" = "1" ]; then
        warm_loop "$@"
    else
        # Disable set -e for main() so a failing jq/curl doesn't kill the
        # script without writing result.json. main() handles its own errors.
        set +e
        main "$@"
        _rc=$?
        set -e
        exit $_rc
    fi
fi
