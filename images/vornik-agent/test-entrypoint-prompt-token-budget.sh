#!/usr/bin/env bash
# Regression guard for the per-step prompt-token budget. The full LLM loop is
# integration-owned; this pins the deterministic helpers and static call path.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$here/entrypoint.sh"
tmp="$(mktemp -d)"
cleanup() {
  rm -rf "$tmp"
}
trap cleanup EXIT

export WORKSPACE="$tmp"
export INPUT_FILE="$tmp/task.json"
export OUTPUT_FILE="$tmp/result.json"
export VORNIK_STEP_PROMPT_TOKEN_BUDGET=12345
export VORNIK_LLM_MODEL="test-model"
export VORNIK_LLM_CONTEXT_SIZE=64000
export VORNIK_LLM_MAX_TOKENS=1024

printf '%s\n' '{"config":{"permissions":{"allowedTools":["file_read"]}}}' > "$INPUT_FILE"

# shellcheck source=images/vornik-agent/entrypoint.sh
source "$ep"

[ "$(step_prompt_token_budget)" = "12345" ] || {
  echo "FAIL: valid prompt-token budget was not parsed" >&2
  exit 1
}

STEP_PROMPT_TOKEN_BUDGET=not-a-number
[ "$(step_prompt_token_budget)" = "0" ] || {
  echo "FAIL: invalid prompt-token budget should disable the guard" >&2
  exit 1
}

STEP_PROMPT_TOKEN_BUDGET=-1
[ "$(step_prompt_token_budget)" = "0" ] || {
  echo "FAIL: negative prompt-token budget should disable the guard" >&2
  exit 1
}
STEP_PROMPT_TOKEN_BUDGET=12345

msgs="$tmp/messages.json"
tools="$tmp/tools.json"
req="$tmp/request.json"
printf '%s\n' '[{"role":"system","content":"sys"},{"role":"user","content":"work"}]' > "$msgs"
printf '%s\n' '[{"type":"function","function":{"name":"file_read","parameters":{"type":"object","properties":{}}}}]' > "$tools"

build_llm_request_file "$req" "$msgs" "$tools" "role_result" "" "null"
jq -e '.model == "test-model"' "$req" >/dev/null
jq -e '.messages | length == 2' "$req" >/dev/null
jq -e '.tools | length == 1' "$req" >/dev/null
jq -e '.max_tokens == 1024' "$req" >/dev/null
jq -e '.options.num_ctx == 64000' "$req" >/dev/null

empty_tools="$tmp/empty-tools.json"
printf '[]\n' > "$empty_tools"
build_llm_request_file "$req" "$msgs" "$empty_tools" "role_result" "" "null"
jq -e '.tools == []' "$req" >/dev/null

entrypoint_text="$(cat "$ep")"
case "$entrypoint_text" in
  *"TOTAL_PROMPT_TOKENS_ESTIMATED"* ) ;;
  *) echo "FAIL: prompt-token guard does not track estimated cumulative usage" >&2; exit 1 ;;
esac
case "$entrypoint_text" in
  *"PROMPT_TOKEN_BUDGET_FINAL_CALL=1"* ) ;;
  *) echo "FAIL: prompt-token guard does not arm a final-call sentinel" >&2; exit 1 ;;
esac
case "$entrypoint_text" in
  *"printf '[]\\n' > \"\$WORKSPACE/.empty_tools.json\""* ) ;;
  *) echo "FAIL: prompt-token guard does not force an empty tool list" >&2; exit 1 ;;
esac
case "$entrypoint_text" in
  *"outcome \"prompt_token_budget\""* | *"prompt_token_budget"* ) ;;
  *) echo "FAIL: prompt-token guard outcome is not surfaced" >&2; exit 1 ;;
esac

STEP_ID=prompt-budget-test
TOTAL_PROMPT_TOKENS=100
TOTAL_PROMPT_TOKENS_ESTIMATED=250
TOTAL_COMPLETION_TOKENS=20
TOTAL_CACHE_CREATION_TOKENS=0
TOTAL_CACHE_READ_TOKENS=0
TOTAL_ITERATIONS=2
MAX_REQUEST_BYTES=900
MAX_PROMPT_TOKENS_ESTIMATE=300
MAX_PROMPT_TOKENS_ACTUAL=120
PROMPT_TOKEN_BUDGET_DETAIL="test prompt budget detail"
BUDGET_TRIPWIRE_DETAIL=""
write_result "COMPLETED" "done" "" 0
jq -e '.outcome == "prompt_token_budget"' "$OUTPUT_FILE" >/dev/null
jq -e '.outcomeDetail == "test prompt budget detail"' "$OUTPUT_FILE" >/dev/null
jq -e '.usage.prompt_tokens == 100' "$OUTPUT_FILE" >/dev/null
jq -e '.usage.prompt_tokens_estimated_total == 250' "$OUTPUT_FILE" >/dev/null

echo "OK: prompt-token budget helper and finalization path are pinned"
