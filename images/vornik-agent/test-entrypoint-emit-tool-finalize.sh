#!/usr/bin/env bash
# Guard: the forced result-emission tool is the one structured-output mechanism
# that COMPOSES with tools.
#
# response_format cannot be sent while tools are offered — see
# test-entrypoint-response-format-tools.sh for the measurement. That leaves a
# tool-using role's schema resting entirely on one tool-free re-ask at step end,
# fired once and never retried. When that turn produces prose, the daemon fails
# the step for a key the decoder was never constrained to emit:
#
#   schema violation: role "analyst" result.json is missing required keys: [analysis:object]
#
# Measured 2026-08-20 on the dev-pipeline report step across arm 4: every failing
# rung was this, with the declared output file written and the role's own
# `analysis` object absent.
#
# A forced tool call has no such conflict: it IS a tool call, so guided decoding
# and tool use are not competing for the same turn, and the provider validates
# the arguments against the declared JSON Schema before returning them. That is
# how the Bedrock and Anthropic paths already enforce schemas
# (internal/chat: synthetic emit tool + ToolChoice forcing); the
# OpenAI-compatible path used by self-hosted vLLM had no equivalent, while the
# daemon was already publishing the spec at config.resultEmissionTool and the
# agent never read it.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$here/entrypoint.sh"
tmp="$(mktemp -d)"
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT

export WORKSPACE="$tmp"
export INPUT_FILE="$tmp/task.json"
export OUTPUT_FILE="$tmp/result.json"
export VORNIK_LLM_MODEL="test-model"
export VORNIK_LLM_MAX_TOKENS=4096

cat > "$INPUT_FILE" <<'JSON'
{"config":{"permissions":{"allowedTools":["file_read"]},
 "resultEmissionTool":{"name":"emit_analyst_result",
   "description":"Emit the role's structured result.",
   "parameters":{"type":"object","required":["analysis"],
                 "properties":{"analysis":{"type":"object"}}}}}}
JSON

# shellcheck source=images/vornik-agent/entrypoint.sh
source "$ep"

msgs="$tmp/msgs.json"; printf '%s\n' '[{"role":"user","content":"go"}]' > "$msgs"
req="$tmp/req.json"
emit_tools="$tmp/emit_tools.json"
printf '%s\n' '[{"type":"function","function":{"name":"emit_analyst_result","parameters":{"type":"object","required":["analysis"]}}}]' > "$emit_tools"

# --- 1. No force requested: nothing changes -------------------------------
printf '%s\n' '[{"type":"function","function":{"name":"file_read"}}]' > "$tmp/tools.json"
build_llm_request_file "$req" "$msgs" "$tmp/tools.json" "r" "json_schema" "null"
if jq -e 'has("tool_choice")' "$req" >/dev/null; then
  echo "FAIL: tool_choice appeared without being asked for — every ordinary tool" \
       "turn would be forced into one call" >&2
  jq -c . "$req" >&2
  exit 1
fi

# --- 2. Forcing the emit tool ----------------------------------------------
build_llm_request_file "$req" "$msgs" "$emit_tools" "r" "json_schema" "null" "emit_analyst_result"
name=$(jq -r '.tool_choice.function.name // ""' "$req")
[ "$name" = "emit_analyst_result" ] || {
  echo "FAIL: tool_choice did not name the emit tool (got '$name')" >&2
  jq -c . "$req" >&2
  exit 1
}
[ "$(jq -r '.tool_choice.type // ""' "$req")" = "function" ] || {
  echo "FAIL: tool_choice.type must be \"function\" for the OpenAI-compatible shape" >&2
  exit 1
}
[ "$(jq '.tools | length' "$req")" = "1" ] || {
  echo "FAIL: the forced tool must still be OFFERED — tool_choice alone names a tool" \
       "the model cannot see" >&2
  exit 1
}

# --- 3. The forced turn must not also carry response_format ---------------
# This is the whole point: the emit tool replaces the directive rather than
# joining it. Sending both reintroduces the exact conflict that makes tool
# calling impossible under guided decoding.
if jq -e 'has("response_format")' "$req" >/dev/null; then
  echo "FAIL: response_format sent alongside the forced emit tool — that is the" \
       "combination measured to produce finish_reason=stop and no call" >&2
  jq -c . "$req" >&2
  exit 1
fi

# --- 4. The spec is read from task.json -----------------------------------
# The daemon has published config.resultEmissionTool since the deterministic
# output-schema work; the agent ignoring it is what this change fixes, so assert
# the read rather than trusting it.
#
# Called explicitly: the read is deliberately NOT a source-time side effect,
# because entrypoint.sh is sourced by these tests before task.json exists and a
# top-level read would make sourcing order-dependent.
read_emission_tool_config
[ "${EMIT_TOOL_NAME:-}" = "emit_analyst_result" ] || {
  echo "FAIL: config.resultEmissionTool was not read from task.json (EMIT_TOOL_NAME='${EMIT_TOOL_NAME:-}')" >&2
  exit 1
}
[ -n "${RESULT_EMISSION_TOOL:-}" ] || {
  echo "FAIL: the emit tool spec itself was not captured" >&2
  exit 1
}

# --- 4b. emit_tool_definitions builds the array the loop actually sends ----
# Section 2 used a hand-written tools file; this is the function the finalization
# path calls, so it needs its own coverage or the two can drift.
emit_tool_definitions "$tmp/built_tools.json"
[ "$(jq -r '.[0].type' "$tmp/built_tools.json")" = "function" ] || {
  echo "FAIL: emit_tool_definitions did not wrap the spec in the OpenAI function shape" >&2
  jq -c . "$tmp/built_tools.json" >&2
  exit 1
}
[ "$(jq -r '.[0].function.name' "$tmp/built_tools.json")" = "emit_analyst_result" ] || {
  echo "FAIL: wrong tool name in the built array" >&2
  exit 1
}
# The parameters must survive intact — they ARE the schema the provider validates
# against, so dropping them turns a forced call into an unconstrained one.
[ "$(jq -r '.[0].function.parameters.required[0]' "$tmp/built_tools.json")" = "analysis" ] || {
  echo "FAIL: the parameters schema was lost; a forced call with no schema" \
       "constrains nothing" >&2
  jq -c . "$tmp/built_tools.json" >&2
  exit 1
}

# --- 5. Absent spec must degrade to today's behaviour ---------------------
# Roles without an outputSchema, and providers whose enforcement lives in the
# chat adapters, must be untouched.
printf '%s\n' '{"config":{"permissions":{"allowedTools":["file_read"]}}}' > "$INPUT_FILE"
read_emission_tool_config
[ -z "${EMIT_TOOL_NAME:-}" ] || {
  echo "FAIL: EMIT_TOOL_NAME set for a task.json with no resultEmissionTool" >&2
  exit 1
}

echo "PASS: forced emit-tool finalization composes with tools and degrades cleanly"
