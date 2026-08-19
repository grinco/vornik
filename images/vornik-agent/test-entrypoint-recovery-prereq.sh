#!/usr/bin/env bash
# Regression guard: the missing-prerequisite bail must NOT fire on a lead
# recovery hop.
#
# 2026-08-19. The guard is right for an ordinary step — a file_read that misses
# the same path twice will never materialise it, and the real fix is at the
# producer, so bailing with a named cause beats a degenerate_loop three
# iterations later.
#
# In a RECOVERY hop it is circular. The lead is there BECAUSE a step failed, and
# the two dominant real triggers (plausibility_violation, verify_claims_failed —
# 57 and 29 of the ledger's recover hops) are both "the file is not there"
# shaped. So the lead investigates the missing artifact, trips this guard, and
# the recovery hop dies — requiring the missing thing to exist in order to
# propose what to do about it missing. Measured on the recovery-probe fixture:
# 5 of 15 recover hops failed exactly this way.
#
# The absence is the PREMISE on a recovery hop, so the guard is suppressed there
# and the lead is left to emit its decision checkpoint.
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

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1" >&2; fails=$((fails+1)); }

# shellcheck source=images/vornik-agent/entrypoint.sh
source "$ep"

# --- Ordinary step: no recovery context. The guard must stay armed.
cat > "$INPUT_FILE" <<'JSON'
{"config":{"permissions":{"allowedTools":["file_read"]}},
 "context":{"prompt":"do the thing"}}
JSON
if in_recovery_hop; then
  fail "a step with no context.recovery must not be treated as a recovery hop"
else
  pass "an ordinary step is not a recovery hop"
fi

# --- Recovery hop: context.recovery populated as the executor sends it.
cat > "$INPUT_FILE" <<'JSON'
{"config":{"permissions":{"allowedTools":["file_read"]}},
 "context":{"prompt":"a prior step failed",
            "recovery":{"failedStep":"doomed","failureClass":"missing_prerequisite"}}}
JSON
if in_recovery_hop; then
  pass "a populated context.recovery is recognised"
else
  fail "context.recovery present but in_recovery_hop said no — the guard will keep " \
       "killing recovery hops over the very artifact whose absence caused them"
fi

# --- Malformed / absent input must not crash the predicate, and must fail
# CLOSED (treated as an ordinary step) so the guard stays armed by default.
rm -f "$INPUT_FILE"
if in_recovery_hop; then
  fail "a missing input file must not be read as a recovery hop"
else
  pass "missing input file fails closed (guard stays armed)"
fi
printf 'not json at all' > "$INPUT_FILE"
if in_recovery_hop; then
  fail "unparseable input must not be read as a recovery hop"
else
  pass "unparseable input fails closed (guard stays armed)"
fi

# --- An explicit null must not count either: the executor omits the key when
# there is no recovery, but a null would be the same statement.
cat > "$INPUT_FILE" <<'JSON'
{"context":{"recovery":null}}
JSON
if in_recovery_hop; then
  fail "context.recovery=null must not count as a recovery hop"
else
  pass "an explicit null recovery is not a recovery hop"
fi

echo
[ "$fails" -eq 0 ] || { echo "$fails case(s) failed" >&2; exit 1; }
echo "all cases passed"
