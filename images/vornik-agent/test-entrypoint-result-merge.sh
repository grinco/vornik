#!/usr/bin/env bash
# Regression guard: the structured-JSON merge must see the model's answer even
# when it arrives as write_result's MESSAGE rather than its RESPONSE.
#
# 2026-08-19. Two symptoms, one cause.
#   - writer steps failed with "result.json is missing required keys:
#     [message:string produced_files:array writing.written:bool writing:object]"
#     while <step>-response.md held all four, correctly formed.
#   - the lead's recovery hop emitted a textbook decision checkpoint into
#     <step>-response.md and the daemon still recorded outcome="missing",
#     failing every recovery attempt.
#
# write_result writes the artifact from "${response:-$message}" — it already
# falls back. The structured-JSON merge next to it was guarded by
# `[ -n "$response" ]` alone, so on every early-exit path that passes the
# model's text as MESSAGE with an empty RESPONSE (prompt-token budget stop,
# tool-loop bail) the artifact captured the answer and result.json never did.
# The happy path passes both, which is why this only ever showed up on the
# paths that were already unusual.
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
export STEP_ID="recover_lead_lead"
printf '%s\n' '{"config":{"permissions":{"allowedTools":["file_read"]}}}' > "$INPUT_FILE"

# shellcheck source=images/vornik-agent/entrypoint.sh
source "$ep"

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1" >&2; fails=$((fails+1)); }

# --- Case 1: the recovery-lead shape, delivered as MESSAGE with no RESPONSE.
decision='{"outcome":"checkpoint","checkpoint_kind":"decision","options":[{"id":"abort","label":"Abort"}]}'
write_result "COMPLETED" "$decision" "" "1"

if [ ! -f "$OUTPUT_FILE" ]; then
  fail "no result.json written at all"
else
  got_outcome=$(jq -r '.outcome // "MISSING"' "$OUTPUT_FILE")
  if [ "$got_outcome" = "checkpoint" ]; then
    pass "a message-only answer is merged into result.json"
  else
    fail "result.json .outcome = '$got_outcome', want 'checkpoint' — the daemon's lead-outcome parser reads this field and recorded outcome=\"missing\" for every recovery hop because of it"
  fi
  # The base envelope must survive the merge.
  [ "$(jq -r '.status // ""' "$OUTPUT_FILE")" = "COMPLETED" ] \
    && pass "base envelope survives the merge" \
    || fail "merge clobbered the base envelope's status"
fi

# The artifact and result.json must agree about what the model said. They read
# the same text now; before this fix they disagreed, which is what made the
# failure so hard to see.
art="$WORKSPACE/artifacts/out/${STEP_ID}-response.md"
if [ -f "$art" ] && grep -q '"outcome"' "$art"; then
  pass "response artifact carries the answer (it always did)"
else
  fail "response artifact lost the answer"
fi

# --- Case 2: the happy path must be unchanged — response set, and winning.
rm -f "$OUTPUT_FILE"
write_result "COMPLETED" "some prose summary" '{"writing":{"written":true}}' "1"
[ "$(jq -r '.writing.written // "MISSING"' "$OUTPUT_FILE")" = "true" ] \
  && pass "an explicit RESPONSE still merges" \
  || fail "the explicit-response path regressed"

# --- Case 3: a non-JSON message must not corrupt result.json.
rm -f "$OUTPUT_FILE"
write_result "COMPLETED" "just prose, no json here" "" "1"
if jq -e . "$OUTPUT_FILE" >/dev/null 2>&1; then
  pass "a prose-only message leaves valid JSON"
else
  fail "a prose-only message produced invalid result.json"
fi

# --- Case 4: the agent's step-quality label must not destroy the lead's decision.
#
# The two are different things that shared one field until 2026-08-19:
#   * `outcome` = the LEAD's workflow decision (checkpoint / external_wait /
#     closure_request), read by the daemon's ParseLeadOutcome.
#   * the AGENT's quality label (iteration_exhausted / prompt_token_budget /
#     budget_tripwire), read by runContainerStep to stop a clean early exit
#     being swept to OK.
# The quality label was injected AFTER the structured merge, so a recovery hop
# that also hit its iteration cap emitted a textbook decision checkpoint and had
# it overwritten with `iteration_exhausted`. ParseLeadOutcome then saw a value
# outside its vocabulary, every recovery attempt failed its contract, and the
# artifact looked perfect the whole time.
rm -f "$OUTPUT_FILE"
ITERATION_CAP_DETAIL="hit the tool cap"
write_result "COMPLETED" "$decision" "" "1"
ITERATION_CAP_DETAIL=""

if [ "$(jq -r '.outcome // "MISSING"' "$OUTPUT_FILE")" = "checkpoint" ]; then
  pass "the lead's decision survives an agent quality label"
else
  fail "agent quality label overwrote the lead's decision: .outcome = '$(jq -r '.outcome // "MISSING"' "$OUTPUT_FILE")'"
fi
if [ "$(jq -r '.agentOutcome // "MISSING"' "$OUTPUT_FILE")" = "iteration_exhausted" ]; then
  pass "the agent quality label is carried in its own field"
else
  fail "agent quality label lost: .agentOutcome = '$(jq -r '.agentOutcome // "MISSING"' "$OUTPUT_FILE")'"
fi
[ "$(jq -r '.agentOutcomeDetail // ""' "$OUTPUT_FILE")" = "hit the tool cap" ] \
  && pass "the quality label keeps its detail" \
  || fail "quality detail lost"

# And with no model-supplied outcome, the quality label must still be recorded.
rm -f "$OUTPUT_FILE"
ITERATION_CAP_DETAIL="cap again"
write_result "COMPLETED" "prose only" "" "1"
ITERATION_CAP_DETAIL=""
[ "$(jq -r '.agentOutcome // "MISSING"' "$OUTPUT_FILE")" = "iteration_exhausted" ] \
  && pass "quality label recorded when the model supplied no outcome" \
  || fail "quality label missing on the prose-only path"

echo
[ "$fails" -eq 0 ] || { echo "$fails case(s) failed" >&2; exit 1; }
echo "all cases passed"
