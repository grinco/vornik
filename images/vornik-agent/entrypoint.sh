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
# Max wall-clock seconds for a single LLM HTTP call. The daemon derives this
# from chat.timeout (or runtime.agent_llm.timeout) and passes it in as
# VORNIK_LLM_TIMEOUT. Fallback default is 300s (5 minutes) — long enough for
# most large-model completions, short enough that a stalled TCP connection
# fails the task instead of wedging it for hours.
LLM_TIMEOUT="${VORNIK_LLM_TIMEOUT:-300}"

# Per-million-token prices for this container's model. Injected by the
# executor from the daemon's pricing.yaml so the agent can log per-iteration
# cost estimates without reaching back across the daemon. Missing/empty
# values keep cost logging at 0.00 — tokens still log, just without the $.
LLM_COST_INPUT_PER_M="${VORNIK_LLM_COST_INPUT_PER_M:-0}"
LLM_COST_OUTPUT_PER_M="${VORNIK_LLM_COST_OUTPUT_PER_M:-0}"

log() { echo "[vornik-agent] $1"; }
debug() { [ "${VORNIK_LOG_LEVEL:-info}" = "debug" ] && echo "[vornik-agent] $1" || true; }

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
        memory_search|get_conversation_window)
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

is_builtin_tool() {
    case "$1" in
        file_read|file_write|run_shell|current_time) return 0 ;;
        file_edit|read_many_files|grep|glob) return 0 ;;
        git_status|git_diff|git_log|git_show) return 0 ;;
        test_run|lint_run|typecheck_run) return 0 ;;
        *) return 1 ;;
    esac
}

# Canonical list of built-in tool names — single source of truth used by the
# allowlist gate in tool_definitions() and by builtin_tool_allowed(). Keep
# this aligned with is_builtin_tool() above.
BUILTIN_TOOL_NAMES_JSON='["file_read","file_write","run_shell","current_time","file_edit","read_many_files","grep","glob","git_status","git_diff","git_log","git_show","test_run","lint_run","typecheck_run"]'

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
                iterations: $iterations
            },
            diagnostics: ({exitCode: $exit_code, durationSeconds: $duration} + if $error_arr[0] != null then {error: $error_arr[0]} else {} end)
        }')
    rm -f "$tool_audit_f" "$artifacts_f" "$error_f"

    # Merge structured LLM response into result.json so workflow gates can
    # match fields like "review.approved == true". Handles pure JSON, JSON
    # wrapped in markdown code fences, and mixed text with embedded JSON.
    if [ -n "$response" ]; then
        local structured=""
        # Pass 1: pure JSON object
        if printf '%s' "$response" | jq -e 'type == "object"' >/dev/null 2>&1; then
            structured="$response"
        else
            # Pass 2: markdown code fences (```json ... ``` or ``` ... ```)
            local stripped
            stripped=$(printf '%s' "$response" | sed -n '/^```/,/^```/{/^```/d;p;}')
            if [ -n "$stripped" ] && printf '%s' "$stripped" | jq -e 'type == "object"' >/dev/null 2>&1; then
                structured="$stripped"
            fi
        fi
        # Pass 3: extract first {...} substring from mixed text, handling
        # multi-line JSON by collapsing newlines before greedy matching.
        if [ -z "$structured" ]; then
            local extracted
            # Collapse newlines so { ... } spans work across lines.
            extracted=$(printf '%s' "$response" | tr '\n' ' ' | grep -o '{.*}' | tail -1)
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

    # Optional outcome override: the budget tripwire bails with status=
    # COMPLETED so the workflow doesn't treat the bail as a failure
    # transition, but the daemon needs the per-step quality signal to
    # be budget_tripwire (not pending_validation, not ok). The
    # BUDGET_TRIPWIRE_DETAIL global is set by the tripwire branch in the
    # tool loop right before it calls write_result — this is the only
    # place where status=COMPLETED carries an alternative outcome label.
    if [ -n "${BUDGET_TRIPWIRE_DETAIL:-}" ]; then
        base_result=$(printf '%s' "$base_result" | jq \
            --arg outcome "budget_tripwire" \
            --arg detail "$BUDGET_TRIPWIRE_DETAIL" \
            '. + {outcome: $outcome, outcomeDetail: $detail}')
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
    local task_id project_id execution_id
    task_id=$(jq -r '.taskId // ""' "$INPUT_FILE" 2>/dev/null || true)
    project_id=$(jq -r '.projectId // ""' "$INPUT_FILE" 2>/dev/null || true)
    execution_id=$(jq -r '.workflow.executionId // ""' "$INPUT_FILE" 2>/dev/null || true)
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
        extras_ungated=$(printf '%s' "$extras_ungated" | jq --argjson tool "$memory_tool" '. + [$tool]')
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

    printf '%s' "$base_tools" | jq \
        --argjson ungated "$extras_ungated" \
        --argjson gated "$extras_gated" \
        --argjson allowed "$(allowed_builtin_tools_json)" \
        --argjson builtin "$BUILTIN_TOOL_NAMES_JSON" \
        '([.[] | select(.function.name as $name | (($builtin | index($name) | not) or ($allowed | index($name) != null)))]) + $ungated + ($gated | map(select(.function.name as $name | $allowed | index($name) != null)))'
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
    if is_builtin_tool "$name" && ! builtin_tool_allowed "$name"; then
        echo "ERROR: tool '$name' is not allowed for this role"
        return
    fi
    case "$name" in
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
lang = detect()
out = {"language": lang, "mode": mode}
def run_go_test():
    if not have("go"):
        return {"error": "go toolchain not available in agent image", "runner": "go test"}
    args = ["go", "test", "-json", "-count=1"]
    args += paths if paths else ["./..."]
    r = subprocess.run(args, cwd=repo, capture_output=True, text=True, timeout=600)
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
    r = subprocess.run(args, cwd=repo, capture_output=True, text=True, timeout=300)
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
    r = subprocess.run(args, cwd=repo, capture_output=True, text=True, timeout=600)
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
    r = subprocess.run(args, cwd=repo, capture_output=True, text=True, timeout=600)
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
    r = subprocess.run(args, cwd=repo, capture_output=True, text=True, timeout=120)
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
    r = subprocess.run(args, cwd=repo, capture_output=True, text=True, timeout=300)
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
    r = subprocess.run(["npm", "run", script, "--silent"], cwd=repo, capture_output=True, text=True, timeout=600)
    return {"runner": f"npm run {script}", "ok": r.returncode == 0,
            "output": cap(r.stdout + r.stderr)}
def run_cargo(sub):
    if not have("cargo"):
        return {"error": "rust toolchain not available in agent image", "runner": f"cargo {sub}"}
    r = subprocess.run(["cargo", sub], cwd=repo, capture_output=True, text=True, timeout=900)
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

main() {
    log "starting (model=$LLM_MODEL)"
    debug "env: LLM_ENDPOINT=$LLM_ENDPOINT LLM_MODEL=$LLM_MODEL API_KEY=${LLM_API_KEY:+set(${#LLM_API_KEY}chars)}"
    CANCELLED=0
    # Per-task LLM usage accumulators. Read in write_result and surfaced
    # to the executor via result.json → prom metric. Reset here so warm
    # containers don't carry usage from a prior task.
    TOTAL_PROMPT_TOKENS=0
    TOTAL_COMPLETION_TOKENS=0
    TOTAL_CACHE_CREATION_TOKENS=0
    TOTAL_CACHE_READ_TOKENS=0
    TOTAL_ITERATIONS=0
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

    # Build tools file: merge built-in tools with MCP tools.
    local builtin_tools_tmp="$WORKSPACE/.builtin_tools.json"
    tool_definitions > "$builtin_tools_tmp"
    jq -s '.[0] + .[1]' "$builtin_tools_tmp" "$mcp_tools_file" > "$tools_file"

    # Degenerate loop detection: if the same tool call (name+args) repeats
    # consecutively, the LLM is stuck. Break out after 3 identical repeats
    # instead of burning the entire iteration budget.
    local last_tool_sig=""
    local repeat_count=0
    MAX_REPEATS=3

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

    # Size-based compaction threshold: 80% of the model's context window,
    # converted from tokens to approximate bytes (4 bytes/token for
    # JSON-encoded chat messages). Falls back to 28000 bytes when
    # VORNIK_LLM_CONTEXT_SIZE is not configured.
    SIZE_KEEP_RECENT=6
    if [ "$LLM_CONTEXT_SIZE" -gt 0 ] 2>/dev/null; then
        SIZE_COMPACT_THRESHOLD=$(( LLM_CONTEXT_SIZE * 4 * 80 / 100 ))
    else
        SIZE_COMPACT_THRESHOLD=28000
    fi
    debug "size compaction threshold: $SIZE_COMPACT_THRESHOLD bytes (ctx=$LLM_CONTEXT_SIZE tokens)"

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
        fi

        debug "LLM call iteration $iteration/$MAX_TOOL_ITERATIONS"

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
        jq -n --arg model "$LLM_MODEL" \
            --slurpfile msgs "$msgs_file" \
            --slurpfile tools "$tools_file" \
            --argjson ctx_size "${LLM_CONTEXT_SIZE:-0}" \
            --argjson max_tokens "${LLM_MAX_TOKENS:-0}" \
            --arg response_format "$response_format" \
            --arg schema_name "$schema_name" \
            --argjson response_schema "${response_schema:-null}" \
            '{"model":$model,"messages":$msgs[0],"tools":$tools[0]}
             | if $max_tokens > 0 then . + {"max_tokens":$max_tokens} else . end
             | if $ctx_size > 0 then . + {"options":{"num_ctx":$ctx_size}} else . end
             | if $response_format == "json_schema" and ($response_schema != null) then
                   . + {"response_format":{"type":"json_schema","json_schema":{"name":$schema_name,"schema":$response_schema,"strict":true}}}
               elif $response_format == "json_schema" then
                   # Degraded: operator requested json_schema but no
                   # schema body landed in task.json (race during
                   # role migration, e.g. effectiveResponseFormat
                   # decided json_schema but applyRoleSchemaOpts saw
                   # an empty render). Fall back to json_object so
                   # the model still gets a structured-output nudge
                   # rather than a malformed empty json_schema
                   # request that the gateway would reject outright.
                   . + {"response_format":{"type":"json_object"}}
               elif $response_format != "" then
                   . + {"response_format":{"type":$response_format}}
               else . end' > "$req_file"
        local request
        request=$(cat "$req_file")

        local req_size=${#request}
        debug "sending request ($req_size bytes)"

        local response
        response=$(llm_call "$request")
        local resp_size=${#response}
        debug "received response ($resp_size bytes)"

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
                    jq -n --arg content "$content" \
                        '[{"role":"assistant","content":$content},{"role":"user","content":"You tried to call a tool by writing XML/text, but you must use the tool_calls API. Do NOT write <function=...> or similar markup. Use the provided tools (file_read, file_write, run_shell, current_time) through the proper function calling interface. Now complete the original task."}]' > "$nudge_msg_file"
                    jq --slurpfile msgs "$nudge_msg_file" '. + $msgs[0]' "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"
                    continue
                    ;;
            esac

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
                    if [ "$repeat_count" -ge "$MAX_REPEATS" ]; then
                        log "ERROR: degenerate loop detected — $tc_name called $MAX_REPEATS times with identical args"
                        local last_content
                        last_content=$(jq -r 'map(select(.role=="assistant" and .content != null)) | last.content // "Agent entered degenerate loop"' "$msgs_file")
                        write_result "FAILED" "Agent entered a degenerate loop (repeated $tc_name $MAX_REPEATS times with the same arguments). This usually means the context window is exhausted." "$last_content" "$(get_duration)" "degenerate tool loop"
                        return 1
                    fi
                else
                    last_tool_sig="$tool_sig"
                    repeat_count=1
                fi
            fi

            # GNU date's %s%3N concatenates seconds with zero-padded
            # milliseconds → a straight millisecond epoch we can
            # subtract to get real ms resolution.
            local tc_start_ms
            tc_start_ms=$(ms_now)

            if [ "$tc_cache_hit" = "0" ]; then
                debug "tool: $tc_name (id=$tc_id)"
                tool_result=$(exec_tool "$tc_name" "$tc_args" 2>&1 | head -c 50000)

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

            local tc_duration_ms=$(( $(ms_now) - tc_start_ms ))

            # Terminal: file_read hit the same missing path twice in
            # this turn. Retrying never materialises an upstream file —
            # the real fix is at the producer. Bail out with a
            # missing_prerequisite-style failure so the downstream
            # consumer (or operator watching the task) sees the real
            # cause instead of a generic "degenerate loop" tripping
            # three iterations later.
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

            # Append tool result message via file to avoid ARG_MAX
            local tool_msg_file="$WORKSPACE/.tool_msg.json"
            jq -n --arg id "$tc_id" --arg content "$tool_result" \
                '{"role":"tool","tool_call_id":$id,"content":$content}' > "$tool_msg_file"
            jq --slurpfile msg "$tool_msg_file" '. + $msg' "$msgs_file" > "$msgs_file.tmp" && mv "$msgs_file.tmp" "$msgs_file"

            tc_idx=$((tc_idx + 1))
        done
    done

    # Iteration cap reached — fail deterministically so the executor does not
    # advance to the next workflow step with incomplete output.
    log "ERROR: tool iteration cap reached ($MAX_TOOL_ITERATIONS)"
    local last_content
    last_content=$(jq -r 'map(select(.role=="assistant" and .content != null)) | last.content // "Agent reached iteration limit without final response"' "$msgs_file")
    write_result "FAILED" "Tool iteration limit ($MAX_TOOL_ITERATIONS) reached. The task was too complex for the configured limit. Increase VORNIK_MAX_TOOL_ITERATIONS or simplify the task." "$last_content" "$(get_duration)" "tool iteration cap reached ($MAX_TOOL_ITERATIONS iterations)"
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
