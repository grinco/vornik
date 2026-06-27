#!/usr/bin/env bash
#
# verify-workflow.sh — submit a task to a running vornik and verify that
# the workflow-reliability fixes (mailbox patches, commit-derived output
# summary, per-role git verification) actually trigger end-to-end.
#
# Usage:
#   scripts/verify-workflow.sh [project] [prompt]
#
# Defaults:
#   project = snake
#   prompt  = "Add a short VERIFY.md note describing what's in the repo"
#
# Env overrides:
#   VORNIK_URL       base URL of the running daemon (default http://localhost:8080)
#   VORNIK_TOKEN     if auth is enabled, an API key to send as Bearer
#   ARTIFACTS_ROOT   host path where artifacts are persisted
#                    (default /opt/vornik/artifacts)
#   TIMEOUT_SECONDS  how long to wait for the task to reach a terminal
#                    state before giving up (default 900)
#
# Exit codes:
#   0  task COMPLETED with the new-format summary and >=1 patch artifact
#   1  task FAILED or timed out
#   2  task COMPLETED but output still looks like raw role JSON (fixes
#      did not kick in) or zero patch artifacts on disk
#   3  prerequisite missing (curl, jq, daemon unreachable)

set -euo pipefail

PROJECT="${1:-snake}"
PROMPT="${2:-Add a short VERIFY.md note in the project root describing the repo layout. Commit the file.}"

VORNIK_URL="${VORNIK_URL:-http://localhost:8080}"
VORNIK_TOKEN="${VORNIK_TOKEN:-}"
ARTIFACTS_ROOT="${ARTIFACTS_ROOT:-/opt/vornik/artifacts}"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-900}"
POLL_SECONDS=5

# --- colour helpers (no-op on non-TTYs) -------------------------------------

if [ -t 1 ]; then
    BOLD=$'\e[1m'; DIM=$'\e[2m'; RED=$'\e[31m'; GREEN=$'\e[32m'
    YELLOW=$'\e[33m'; BLUE=$'\e[34m'; RESET=$'\e[0m'
else
    BOLD=''; DIM=''; RED=''; GREEN=''; YELLOW=''; BLUE=''; RESET=''
fi

say()  { printf '%s\n' "$*"; }
step() { printf '\n%s==>%s %s\n' "$BLUE$BOLD" "$RESET$BOLD" "$*$RESET"; }
ok()   { printf '%s✓%s %s\n' "$GREEN" "$RESET" "$*"; }
warn() { printf '%s⚠%s %s\n' "$YELLOW" "$RESET" "$*"; }
fail() { printf '%s✗%s %s\n' "$RED" "$RESET" "$*"; }

# --- prerequisites ----------------------------------------------------------

for cmd in curl jq; do
    command -v "$cmd" >/dev/null || { fail "missing required tool: $cmd"; exit 3; }
done

# curl wrapper so every request picks up the optional bearer token.
_curl() {
    if [ -n "$VORNIK_TOKEN" ]; then
        curl -sS -H "Authorization: Bearer $VORNIK_TOKEN" "$@"
    else
        curl -sS "$@"
    fi
}

step "Health check"
if ! _curl --fail "$VORNIK_URL/healthz" -o /dev/null; then
    fail "daemon not reachable at $VORNIK_URL"
    exit 3
fi
ok "$VORNIK_URL is healthy"

# --- submit the task --------------------------------------------------------

step "Submitting task to project '$PROJECT'"
say "${DIM}prompt:${RESET} $PROMPT"

# The data-plane API stores the human prompt as `taskType`; buildAgentInput
# hydrates it into the container as context.prompt at dispatch time. See
# internal/api/handlers.go#CreateTaskRequest and internal/executor/plan.go.
REQ_BODY=$(jq -n --arg p "$PROMPT" '{taskType: $p, workflowId: "adaptive"}')
CREATE_RESP=$(
    _curl --fail \
        -X POST "$VORNIK_URL/api/v1/projects/$PROJECT/tasks" \
        -H "Content-Type: application/json" \
        -d "$REQ_BODY"
)
TASK_ID=$(jq -r '.id // .taskId // empty' <<<"$CREATE_RESP")
if [ -z "$TASK_ID" ]; then
    fail "could not extract task id from response"
    say "$CREATE_RESP"
    exit 1
fi
ok "task created: $TASK_ID"

# --- poll until terminal ----------------------------------------------------

step "Polling for completion (timeout ${TIMEOUT_SECONDS}s)"
START=$(date +%s)
last_status=""
EXECUTION_ID=""
TASK_STATE=""

while :; do
    NOW=$(date +%s)
    ELAPSED=$((NOW - START))
    if [ "$ELAPSED" -ge "$TIMEOUT_SECONDS" ]; then
        fail "timed out after ${TIMEOUT_SECONDS}s (last status: $last_status)"
        exit 1
    fi

    TASK_JSON=$(
        _curl --fail "$VORNIK_URL/api/v1/projects/$PROJECT/tasks/$TASK_ID" || true
    )
    if [ -z "$TASK_JSON" ]; then
        sleep "$POLL_SECONDS"
        continue
    fi

    STATUS=$(jq -r '.status // empty' <<<"$TASK_JSON")
    if [ "$STATUS" != "$last_status" ]; then
        say "  ${DIM}[${ELAPSED}s]${RESET} status → $STATUS"
        last_status="$STATUS"
    fi

    case "$STATUS" in
        COMPLETED|FAILED|CANCELLED)
            TASK_STATE="$STATUS"
            EXECUTION_ID=$(jq -r '.links.execution // empty | sub("^/api/v1/executions/"; "")' <<<"$TASK_JSON")
            break
            ;;
    esac

    sleep "$POLL_SECONDS"
done

# --- analyse the result -----------------------------------------------------

if [ "$TASK_STATE" != "COMPLETED" ]; then
    fail "task finished in state $TASK_STATE"
    LAST_ERR=$(jq -r '.lastError // empty' <<<"$TASK_JSON")
    [ -n "$LAST_ERR" ] && printf '%s%s%s\n' "$RED" "$LAST_ERR" "$RESET"
    exit 1
fi

if [ -z "$EXECUTION_ID" ]; then
    # GET /tasks/:id only populates links.execution while a task is
    # actively running. By the time we observe COMPLETED the link is
    # gone, so fall back to the project's execution list filtered by
    # task id client-side.
    EXECUTION_ID=$(
        _curl --fail "$VORNIK_URL/api/v1/projects/$PROJECT/executions" \
            | jq -r ".executions[] | select(.taskId==\"$TASK_ID\") | .executionId" \
            | head -1
    )
fi

if [ -z "$EXECUTION_ID" ]; then
    warn "task COMPLETED but no execution id surfaced — can't introspect result"
    exit 2
fi

step "Fetching execution $EXECUTION_ID"
EXEC_JSON=$(
    _curl --fail "$VORNIK_URL/api/v1/executions/$EXECUTION_ID"
)

# The API serves .result as an already-decoded JSON object (json.RawMessage
# on the wire). Pull each field directly from the execution envelope; no
# second jq-parse needed.
if [ "$(jq -r '.result == null' <<<"$EXEC_JSON")" = "true" ]; then
    fail "execution has no result payload"
    exit 2
fi

# --- verification: new-format output ----------------------------------------

step "Checking task output format"

MESSAGE=$(jq -r '.result.message // ""' <<<"$EXEC_JSON")
CHANGES_FROM=$(jq -r '.result.changes.from // ""' <<<"$EXEC_JSON")
CHANGES_TO=$(jq -r '.result.changes.to // ""' <<<"$EXEC_JSON")
PATCH_COUNT=$(jq -r '.result.changes.patchCount // 0' <<<"$EXEC_JSON")
COMMIT_COUNT=$(jq -r '.result.changes.commitCount // 0' <<<"$EXEC_JSON")

exit_code=0

if [ -n "$CHANGES_FROM" ] && [ -n "$CHANGES_TO" ]; then
    ok "result contains commit-derived 'changes' block (${CHANGES_FROM}..${CHANGES_TO})"
    ok "patchCount=$PATCH_COUNT  commitCount=$COMMIT_COUNT"
else
    warn "result does NOT contain a 'changes' block — fix may have fallen back to last-role JSON"
    say "  This is expected when the plan made no commits."
    exit_code=2
fi

say ""
say "${BOLD}--- execution message (what UI + Telegram show) ---${RESET}"
if [ -n "$MESSAGE" ]; then
    # Heuristic: if message starts with "{" it's raw JSON — the new
    # format was not applied.
    if [[ "$MESSAGE" == "{"* ]]; then
        fail "message starts with '{' — looks like raw role JSON, not a commit summary"
        exit_code=2
    fi
    printf '%s\n' "$MESSAGE"
else
    warn "message field is empty"
    exit_code=2
fi
say ""

# --- verification: artifacts on disk ---------------------------------------

ARTIFACT_DIR="$ARTIFACTS_ROOT/$PROJECT/$EXECUTION_ID"
step "Checking artifacts at $ARTIFACT_DIR"

if [ ! -d "$ARTIFACT_DIR" ]; then
    warn "artifact directory does not exist — daemon may write elsewhere (check artifacts.storage path)"
    exit_code=2
else
    PATCH_FILES=$(find "$ARTIFACT_DIR" -maxdepth 1 -name '*.patch' -type f 2>/dev/null | sort)
    PATCH_ON_DISK=$(echo -n "$PATCH_FILES" | grep -c . || true)
    if [ "$PATCH_ON_DISK" -gt 0 ]; then
        ok "$PATCH_ON_DISK patch file(s) persisted as artifacts:"
        while IFS= read -r p; do
            [ -z "$p" ] && continue
            SIZE=$(stat -c '%s' "$p" 2>/dev/null || echo "?")
            printf '  %s%s%s  (%s bytes)\n' "$DIM" "$(basename "$p")" "$RESET" "$SIZE"
        done <<<"$PATCH_FILES"
    else
        warn "no .patch files in artifact directory"
        exit_code=2
    fi

    if [ -f "$ARTIFACT_DIR/CHANGES.md" ]; then
        ok "CHANGES.md present"
    else
        warn "CHANGES.md is missing"
        exit_code=2
    fi
fi

# --- summary ---------------------------------------------------------------

say ""
if [ "$exit_code" -eq 0 ]; then
    ok "${GREEN}${BOLD}Verification passed.${RESET} Task output is the commit summary; patches are attached as artifacts."
else
    warn "${YELLOW}${BOLD}Verification completed with findings above.${RESET}"
    say "The plan succeeded, but one or more expected post-conditions of the"
    say "workflow-reliability fixes did not hold. Most common causes:"
    say "  • the plan made zero commits (valid for non-code tasks)"
    say "  • the worktree was not a git repo, so patch generation was skipped"
    say "  • the daemon is running an older binary; rebuild + restart"
fi

exit "$exit_code"
