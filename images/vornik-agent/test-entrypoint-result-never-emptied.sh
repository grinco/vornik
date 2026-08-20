#!/usr/bin/env bash
# Guard: no jq failure anywhere in write_result may EMPTY result.json.
#
# The asymmetry that made 2026-08-20 expensive: the per-step artifact is written
# BEFORE base_result is built, so anything that empties base_result afterwards
# leaves <step>-response.md holding the model's answer while result.json holds
# nothing. The daemon then reports "role %q result.json is missing required keys"
# and every reading of that message points at the model.
#
# Command substitution of a FAILED jq yields an empty string, and the tail of
# write_result had three unguarded ones:
#
#   base_result=$(printf '%s' "$base_result" | jq --arg outcome ... '. + {...}')
#
# followed immediately by `printf '%s\n' "$base_result" > "$OUTPUT_FILE"`. One jq
# error and the whole result is gone.
#
# Confirming evidence I initially waved away: on EVERY failing rung in bench arms
# 4-6, tool_calls_used and effective_tool_budget were NULL while every ok rung had
# values. Those come from result.json's metrics block. An empty result.json loses
# the metrics AND the role's keys — one symptom, not two, and I read it as "the
# failure path just doesn't populate them".
#
# This is the third instance of the same defect class (unguarded jq destroys the
# result), after the structured merge and the base construction, so it is guarded
# by a shared primitive rather than a third point fix.
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
printf '%s\n' '{"config":{"permissions":{"allowedTools":["file_read"]}}}' > "$INPUT_FILE"

# shellcheck source=images/vornik-agent/entrypoint.sh
source "$ep"

fail() { echo "FAIL: $1" >&2; shift; [ $# -gt 0 ] && printf '%s\n' "$@" >&2; exit 1; }

# --- 1. A successful update is taken -------------------------------------
out=$(guard_result_update '{"status":"COMPLETED","agentOutcome":"iteration_exhausted"}' \
                          '{"status":"COMPLETED"}' 'agentOutcome injection')
[ "$(printf '%s' "$out" | jq -r '.agentOutcome')" = "iteration_exhausted" ] \
  || fail "a valid update was discarded" "$out"

# --- 2. THE REGRESSION: an empty update must not empty the result -------
out=$(guard_result_update '' '{"status":"COMPLETED","analysis":{"changelog":"x"}}' 'agentOutcome injection' 2>/dev/null)
[ "$(printf '%s' "$out" | jq -r '.analysis.changelog')" = "x" ] \
  || fail "AN EMPTY jq RESULT EMPTIED THE RESULT — this is the defect" "$out"

# --- 3. Malformed update is refused, previous kept ----------------------
out=$(guard_result_update '{"status":' '{"status":"COMPLETED","analysis":{"a":1}}' 'stage' 2>/dev/null)
[ "$(printf '%s' "$out" | jq -r '.analysis.a')" = "1" ] \
  || fail "a malformed update destroyed the previous result" "$out"

# --- 4. Non-object update is refused ------------------------------------
for bad in '"str"' '[1]' 'null'; do
  out=$(guard_result_update "$bad" '{"status":"COMPLETED"}' 'stage' 2>/dev/null)
  [ "$(printf '%s' "$out" | jq -r '.status')" = "COMPLETED" ] \
    || fail "non-object update ($bad) replaced the result" "$out"
done

# --- 5. It must never print nothing ------------------------------------
# Even with both halves broken, returning empty would write an empty
# result.json, which is the failure mode being closed.
out=$(guard_result_update '' '' 'stage' 2>/dev/null)
printf '%s' "$out" | jq -e 'type == "object"' >/dev/null 2>&1 \
  || fail "with both halves unusable it must still emit a JSON object, not nothing" "$out"

# --- 6. Warnings go to stderr, never stdout ---------------------------
# log() echoes to STDOUT and this function's stdout IS its return value; a
# warning on stdout would corrupt the result it is protecting.
out=$(guard_result_update '' '{"status":"COMPLETED"}' 'stage' 2>/dev/null)
printf '%s' "$out" | jq -e 'type == "object"' >/dev/null 2>&1 \
  || fail "stdout was contaminated by a log line" "$out"

echo "PASS: result.json cannot be emptied by a failed jq"
