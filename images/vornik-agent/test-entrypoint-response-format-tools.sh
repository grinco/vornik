#!/usr/bin/env bash
# Regression guard: response_format must NEVER be sent alongside tools.
#
# Sending both makes tool calling impossible on any server implementing
# response_format with guided decoding — the model is constrained to emit
# schema-shaped JSON, so it can never emit a tool call. Measured against
# a self-hosted vLLM server on 2026-08-15 with an otherwise identical request:
#
#   tools only                 -> finish_reason=tool_calls   (calls a tool)
#   tools + json_object        -> finish_reason=stop, no call
#   tools + json_schema strict -> finish_reason=stop, no call
#
# Hosted APIs tolerate the combination, so this was invisible on cloud models
# and fatal on a self-hosted one: every agent answered in ~18 tokens of prose
# on iteration 1, the loop logged "completed successfully", and the step then
# failed its output contract because no file had been written. 8 of 10
# benchmark tasks died this way.
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

printf '%s\n' '{"config":{"permissions":{"allowedTools":["file_read"]}}}' > "$INPUT_FILE"
# shellcheck source=images/vornik-agent/entrypoint.sh
source "$ep"

msgs="$tmp/msgs.json"; printf '%s\n' '[{"role":"user","content":"go"}]' > "$msgs"
schema='{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}'
req="$tmp/req.json"

# --- 1. Tools present: the directive must be ABSENT ------------------------
printf '%s\n' '[{"type":"function","function":{"name":"file_read"}}]' > "$tmp/tools.json"
build_llm_request_file "$req" "$msgs" "$tmp/tools.json" "r" "json_schema" "$schema"
if jq -e 'has("response_format")' "$req" >/dev/null; then
  echo "FAIL: response_format was sent alongside tools — tool calling is impossible under guided decoding" >&2
  jq -c . "$req" >&2
  exit 1
fi
[ "$(jq '.tools | length' "$req")" = "1" ] || { echo "FAIL: tools were dropped" >&2; exit 1; }

# A loose directive is just as fatal as a strict one.
build_llm_request_file "$req" "$msgs" "$tmp/tools.json" "r" "json_object" "null"
if jq -e 'has("response_format")' "$req" >/dev/null; then
  echo "FAIL: loose response_format sent alongside tools" >&2
  exit 1
fi

# --- 2. Tool-free: the directive MUST be present ---------------------------
# Suppressing it during the tool phase only works because the step re-asks
# tool-free afterwards; if the directive never applied, every schema-bearing
# role would silently degrade to free-form prose.
printf '%s\n' '[]' > "$tmp/notools.json"
build_llm_request_file "$req" "$msgs" "$tmp/notools.json" "r" "json_schema" "$schema"
[ "$(jq -r '.response_format.type' "$req")" = "json_schema" ] || {
  echo "FAIL: tool-free request lost its json_schema directive" >&2
  jq -c . "$req" >&2
  exit 1
}
[ "$(jq -r '.response_format.json_schema.strict' "$req")" = "true" ] || {
  echo "FAIL: strict flag lost" >&2; exit 1
}

build_llm_request_file "$req" "$msgs" "$tmp/notools.json" "r" "json_schema" "null"
[ "$(jq -r '.response_format.type' "$req")" = "json_object" ] || {
  echo "FAIL: schema-less json_schema should degrade to json_object" >&2; exit 1
}

build_llm_request_file "$req" "$msgs" "$tmp/notools.json" "r" "" "null"
if jq -e 'has("response_format")' "$req" >/dev/null; then
  echo "FAIL: free-form role gained a directive" >&2; exit 1
fi

# --- 3. max_tokens must survive both paths ---------------------------------
[ "$(jq -r '.max_tokens' "$req")" = "4096" ] || { echo "FAIL: max_tokens dropped" >&2; exit 1; }

echo "PASS: response_format is withheld while tools are offered, and applied tool-free"

# --- 4. Finalization is a POST-tool-phase move -----------------------------
# Regression: the schema-finalization turn fired on a first-turn prose reply,
# stripped the tools, and made the step's declared output file unwritable. The
# assistant research step failed exactly that way three times over — initial
# attempt, shape retry and model fallback — while never issuing a single tool
# call. The guard is that finalization requires a tool phase to have happened.
if ! grep -q 'TOOL_PHASE_HAPPENED:-0}" = "1"' "$ep"; then
  echo "FAIL: schema finalization does not require a tool phase; a prose-only turn " \
       "would strip the tools and make a declared output file unwritable" >&2
  exit 1
fi
if ! grep -q 'NO_TOOL_NUDGE_SENT' "$ep"; then
  echo "FAIL: no nudge for a step that ended without calling any tool" >&2
  exit 1
fi
# The nudge must be bounded, or a model that keeps answering in prose spins.
if ! grep -q 'NO_TOOL_NUDGE_SENT:-0}" = "0"' "$ep"; then
  echo "FAIL: the no-tool nudge is not bounded to one attempt" >&2
  exit 1
fi
echo "PASS: finalization requires a tool phase, and a tool-free turn is nudged once"

# --- 5. Build/test/lint subprocess caps must be scalable --------------------
# These were hardcoded (go/npm 600s, cargo 900s, python 300s, typecheck 120s)
# with no knob at all, unlike the LLM and shell timeouts. On hardware slower
# than the image was tuned for, a healthy build is killed with no way to grant
# it more. Scaled by a FACTOR rather than set absolutely, so the deliberate
# ratios between them survive — a Rust build gets longer than a typecheck.
if grep -qE 'timeout=[0-9]+\)' "$ep"; then
  echo "FAIL: a build/test subprocess timeout is still a bare literal; it cannot be" \
       "raised on slow hardware" >&2
  grep -nE 'timeout=[0-9]+\)' "$ep" >&2
  exit 1
fi
if ! grep -q 'VORNIK_TOOL_TIMEOUT_FACTOR' "$ep"; then
  echo "FAIL: no factor knob for build/test subprocess timeouts" >&2
  exit 1
fi
# The factor must NOT be the decode-speed factor: these are compute-bound, and a
# build does not get faster because the model does.
if ! grep -q 'compute-bound' "$ep"; then
  echo "FAIL: the tool-timeout knob does not record that it is compute-bound, not" \
       "decode-bound — the next reader will wire it to the wrong signal" >&2
  exit 1
fi
echo "PASS: build/test subprocess timeouts are scalable, and scoped as compute-bound"
