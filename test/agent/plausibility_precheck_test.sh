#!/usr/bin/env bash
# Regression tests for the 2026-08-16 long-horizon arm's largest failure cause:
# 32 of 73 failed steps (43.8%) were the tester setting testing.passed=true
# without the fields passed_requires_pinned_validation demands.
#
# The daemon evaluates plausibility rules AFTER the container exits
# (executor/container.go), and nothing told the agent they existed. The rules
# are deliberately excluded from the provider JSON Schema
# (registry/output_schema.go — conditional draft-2019-09 support is uneven
# across providers), so the prompt block and this in-container pre-check are
# the only channels the model has.
#
# Covers:
#   1. plausibility_violations mirrors EvaluatePlausibility's semantics —
#      every `when` entry must match, empty/missing `require` fields are
#      reported, warnOnly rules never gate.
#   2. it is inert when there are no rules or the content is not JSON, so a
#      role without rules behaves exactly as before.
#   3. the nudge fires BEFORE finalisation (grep-pinned): once the tool-free
#      turn has run there are no tools left to fix anything with.
set -u

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENTRYPOINT="$REPO_ROOT/images/vornik-agent/entrypoint.sh"
if [ ! -f "$ENTRYPOINT" ]; then
    echo "FAIL: entrypoint.sh not found at $ENTRYPOINT" >&2
    exit 1
fi

PASS=0
FAIL=0
FAILURES=()

ok()  { PASS=$((PASS+1)); echo "PASS: $1"; }
bad() { FAIL=$((FAIL+1)); FAILURES+=("$1"); echo "FAIL: $1"; }

# shellcheck disable=SC1090
source "$ENTRYPOINT" >/dev/null 2>&1 || true

if ! declare -f plausibility_violations >/dev/null; then
    bad "plausibility_violations function must exist in entrypoint.sh"
else
    # The dev-swarm tester's real rules, verbatim from configs/swarms/dev-swarm.md,
    # plus a warnOnly rule to prove advisory rules never gate.
    PLAUSIBILITY_RULES='[
      {"name":"passed_requires_pinned_validation","when":{"testing.passed":true},
       "require":["testing.pinned_cases_validated","testing.cases"],"warnOnly":false},
      {"name":"failure_explained","when":{"testing.passed":false},
       "require":["testing.failures"],"warnOnly":false},
      {"name":"advisory_summary","when":{},"require":["testing.summary"],"warnOnly":true}
    ]'
    export PLAUSIBILITY_RULES

    check() {
        local label="$1" candidate="$2" want="$3" got
        got="$(plausibility_violations "$candidate")"
        if [ "$got" = "$want" ]; then ok "$label"; else bad "$label — got [$got] want [$want]"; fi
    }

    # The incident itself: passed=true with neither required field.
    check "passed=true without pinned validation is caught" \
        '{"testing":{"passed":true}}' \
        "passed_requires_pinned_validation: testing.pinned_cases_validated; passed_requires_pinned_validation: testing.cases"

    check "a complete result is clean" \
        '{"testing":{"passed":true,"pinned_cases_validated":true,"cases":[{"id":"c1","status":"passed"}]}}' ""

    # An empty collection is the half-honest shape plausibility exists to catch:
    # schema-valid, and useless downstream.
    check "an empty cases array still violates" \
        '{"testing":{"passed":true,"pinned_cases_validated":true,"cases":[]}}' \
        "passed_requires_pinned_validation: testing.cases"

    check "the passed=false rule fires on its own condition" \
        '{"testing":{"passed":false}}' "failure_explained: testing.failures"

    check "passed=false with an explanation is clean" \
        '{"testing":{"passed":false,"failures":"3 tests failed"}}' ""

    # warnOnly rules are advisory — testing.summary is absent throughout and
    # must never appear in the gating output.
    check "warnOnly rules never gate" \
        '{"testing":{"passed":true,"pinned_cases_validated":true,"cases":[{"id":"c"}]}}' ""

    # Prose answers must not be treated as violating results, or every
    # free-text role would nudge itself forever.
    check "non-JSON content is inert" 'I could not complete the task.' ""

    PLAUSIBILITY_RULES='[]'
    check "a role with no rules is inert" '{"testing":{"passed":true}}' ""
fi

# The ordering constraint is the whole point: a violation found after the
# tool-free finalisation is unrecoverable, because no tools are offered there.
# Pin that the nudge resets SCHEMA_FINALIZE_PENDING, which is what sends the
# agent back through a tool-bearing turn.
if grep -q 'PLAUSIBILITY_NUDGED' "$ENTRYPOINT" &&
   grep -A 25 'plausibility_violations "\$content"' "$ENTRYPOINT" | grep -q 'SCHEMA_FINALIZE_PENDING=0'; then
    ok "the plausibility nudge re-opens the tool phase rather than finalising"
else
    bad "the plausibility nudge must clear SCHEMA_FINALIZE_PENDING — a violation found during the tool-free turn cannot be fixed"
fi

# The rules must reach the agent at all. Pin the payload key so a rename on the
# daemon side fails here rather than silently restoring the original defect.
if grep -q 'swarm.plausibilityRules' "$ENTRYPOINT"; then
    ok "the agent reads plausibility rules from the payload"
else
    bad "entrypoint.sh must read .swarm.plausibilityRules from task.json"
fi

echo
echo "passed: $PASS, failed: $FAIL"
if [ "$FAIL" -gt 0 ]; then
    printf '  - %s\n' "${FAILURES[@]}"
    exit 1
fi
