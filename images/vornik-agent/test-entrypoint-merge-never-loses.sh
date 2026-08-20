#!/usr/bin/env bash
# Guard: the structured merge must NEVER lose the model's answer.
#
# Measured 2026-08-20 across the bench DB — all 198 "missing required keys" rungs,
# classified by what the step's OWN response artifact held:
#
#   48  valid JSON, key at top level             -> compliant answer, lost
#   22  fenced JSON, merge Pass 2 recoverable    -> ditto
#   18  embedded {...}, merge Pass 3 recoverable -> ditto
#   29  key only as prose                        -> model non-compliance
#   81  key genuinely absent                     -> model non-compliance
#
# So ~88 steps were failed for a key the model HAD supplied in a form this
# harness is written to parse. The artifact is written earlier from the same
# string, which is why the evidence sat on disk next to a step blaming the model.
#
# The hole was not extraction. It was the merge: `jq -s` runs with 2>/dev/null
# behind an `if [ -n "$merged" ]` test, and the filter's type guards
# (`.[0] // {} | if type=="object"`) cannot save a PARSE error — a malformed
# base_result kills jq before the filter runs, and the merge is discarded in
# silence.
#
# The invariant this file defends: if `structured` parses as an object, its keys
# are in the result. Whatever is wrong with the envelope, the model's answer
# survives — losing our own metadata is recoverable, losing the answer fails the
# step and blames the model for it.
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

# --- 1. Happy path: both halves present, both survive ----------------------
out=$(merge_structured_result '{"status":"COMPLETED","metrics":{"iterations":3}}' '{"analysis":{"changelog":"x"}}')
[ "$(printf '%s' "$out" | jq -r '.analysis.changelog')" = "x" ] \
  || fail "structured key lost on the happy path" "$out"
[ "$(printf '%s' "$out" | jq -r '.metrics.iterations')" = "3" ] \
  || fail "base metadata lost on the happy path" "$out"

# --- 2. THE REGRESSION: malformed base must not take the answer with it ----
# This is the 88. base_result is built by an earlier jq; when that produced
# truncated or invalid JSON, the whole merge was dropped.
out=$(merge_structured_result '{"status":"COMPLETED","metrics":{"iterations":' '{"analysis":{"changelog":"x"}}')
[ "$(printf '%s' "$out" | jq -r '.analysis.changelog')" = "x" ] \
  || fail "MALFORMED BASE LOST THE MODEL'S ANSWER — this is the defect" "$out"
printf '%s' "$out" | jq -e 'type == "object"' >/dev/null \
  || fail "output is not a JSON object; the daemon cannot read it" "$out"

# --- 3. Empty base ---------------------------------------------------------
out=$(merge_structured_result '' '{"analysis":{"ok":true}}')
[ "$(printf '%s' "$out" | jq -r '.analysis.ok')" = "true" ] \
  || fail "empty base lost the answer" "$out"

# --- 4. Base that parses but is not an object -----------------------------
# A bare string or array must not defeat the merge either.
for bad in '"just a string"' '[1,2,3]' 'null'; do
  out=$(merge_structured_result "$bad" '{"analysis":{"ok":true}}')
  [ "$(printf '%s' "$out" | jq -r '.analysis.ok')" = "true" ] \
    || fail "non-object base ($bad) lost the answer" "$out"
done

# --- 5. Structured wins on conflict ---------------------------------------
# The model's answer is the point of the step; our envelope is bookkeeping.
out=$(merge_structured_result '{"analysis":{"changelog":"stale"},"status":"X"}' '{"analysis":{"changelog":"fresh"}}')
[ "$(printf '%s' "$out" | jq -r '.analysis.changelog')" = "fresh" ] \
  || fail "base overwrote the model's value" "$out"

# --- 6. No structured half: base passes through untouched -----------------
out=$(merge_structured_result '{"status":"COMPLETED"}' '')
[ "$(printf '%s' "$out" | jq -r '.status')" = "COMPLETED" ] \
  || fail "base mangled when there was nothing to merge" "$out"

# --- 7. Malformed STRUCTURED must not destroy the base -------------------
# Extraction validates before calling this, but a caller that skips it must not
# corrupt the envelope.
out=$(merge_structured_result '{"status":"COMPLETED"}' '{"analysis":')
[ "$(printf '%s' "$out" | jq -r '.status')" = "COMPLETED" ] \
  || fail "malformed structured half destroyed the base" "$out"

echo "PASS: the merge cannot lose a parseable structured answer"
