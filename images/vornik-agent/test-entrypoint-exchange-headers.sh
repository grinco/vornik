#!/usr/bin/env bash
# test-entrypoint-exchange-headers.sh — llm_call names the step and the
# iteration to the proxy (llm-exchange record/replay design §2.1), so an
# opted-in project's recorder can key each exchange. Fully deterministic:
# curl is stubbed, no network.
set -u
here="$(cd "$(dirname "$0")" && pwd)"
ep="$here/entrypoint.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/ws" "$tmp/in" "$tmp/out"
printf '{"taskId":"task_x","projectId":"proj_x","workflow":{"executionId":"exec_x","stepId":"plan"},"config":{"permissions":{}}}' > "$tmp/in/task.json"
export WORKSPACE="$tmp/ws" INPUT_FILE="$tmp/in/task.json" OUTPUT_FILE="$tmp/out/result.json"
export VORNIK_LLM_ENDPOINT="http://llm.local" VORNIK_LLM_MODEL="m" VORNIK_LLM_API_KEY="k"
set +u
# shellcheck disable=SC1090
source "$ep"
trap - EXIT
set +e

pass=0; fail=0
ok() { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1"; }

CURL_ARGS_FILE="$tmp/curl_args"
curl() {
    printf '%s\n' "$@" > "$CURL_ARGS_FILE"
    cat > /dev/null
    printf '{"choices":[{"message":{"role":"assistant","content":"ok"}}]}'
}
arg_present() { grep -Fxq -- "$1" "$CURL_ARGS_FILE"; }

# main() reads the step id from task.json; llm_call reads the global.
# shellcheck disable=SC2034 # read by the sourced llm_call, not by this file
STEP_ID="plan"

# --- 1. With an iteration, both headers are sent. ---
llm_call '{"messages":[]}' 7 > /dev/null
if arg_present "X-Vornik-Step-ID: plan"; then ok "step id header carries task.json's stepId"; else bad "step id header missing: $(cat "$CURL_ARGS_FILE" | tr '\n' ' ')"; fi
if arg_present "X-Vornik-Iteration: 7"; then ok "iteration header carries the loop counter"; else bad "iteration header missing"; fi

# --- 2. Without an iteration (a caller outside the loop), the iteration header is absent. ---
llm_call '{"messages":[]}' > /dev/null
if arg_present "X-Vornik-Step-ID: plan"; then ok "step id header is always sent"; else bad "step id header missing without iteration"; fi
if grep -q "X-Vornik-Iteration" "$CURL_ARGS_FILE"; then bad "iteration header sent with no iteration"; else ok "no iteration, no iteration header"; fi

# --- 3. Structural: the loop passes its counter. ---
# shellcheck disable=SC2016
if grep -q '^        response=$(llm_call "$request" "$iteration")' "$ep"; then ok "the loop passes \$iteration to llm_call"; else bad "the loop does not pass its iteration to llm_call"; fi

echo "exchange headers: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
