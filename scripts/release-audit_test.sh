#!/usr/bin/env bash
#
# release-audit_test.sh — the audit must report failure AND exit non-zero, and
# report success AND exit zero.
#
# The regression of record: the first version printed "All audited releases
# produced what they promised" and exited 1, because `note`'s trailing
# `[ -n "$GITHUB_STEP_SUMMARY" ] && printf ...` is FALSE outside Actions and
# became the function's — and so the script's — status. A control that reports
# success while exiting failure is the tenet-4 failure in its purest form, and
# it was in the script written to catch exactly that class. Caught by reading
# the exit code directly rather than trusting the message.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
AUDIT="$HERE/release-audit.sh"
fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails+1)); }

command -v gh >/dev/null 2>&1 || { echo "release-audit_test: SKIPPED — no gh"; exit 0; }
gh auth status >/dev/null 2>&1 || { echo "release-audit_test: SKIPPED — gh not authenticated"; exit 0; }

# The happy path must exit 0. Guard against the exact inversion above.
out="$("$AUDIT" 1 2>&1)"; rc=$?
if printf '%s' "$out" | grep -q 'produced what they promised'; then
	if [ "$rc" -eq 0 ]; then
		pass "a clean audit reports success AND exits 0"
	else
		fail "a clean audit printed success but exited $rc — the 2026-09-22 inversion is back"
	fi
else
	# A genuinely failing audit is a legitimate outcome here (something really
	# is missing); it must then be non-zero.
	if [ "$rc" -ne 0 ]; then
		pass "a failing audit exits non-zero (exit $rc)"
	else
		fail "audit reported problems but exited 0"
	fi
fi

# A missing artifact must be BOTH reported and fatal.
out="$(RELEASE_AUDIT_IMAGE=ghcr.io/grinco/definitely-not-an-image "$AUDIT" 1 2>&1)"; rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'NO agent image'; then
	pass "a missing agent image is reported and fatal"
else
	fail "missing agent image should report and exit non-zero (rc=$rc); got: $out"
fi

# A release commit absent from the mirror must be BOTH reported and fatal —
# this is the state every release was in before 2026-09-22.
out="$(RELEASE_AUDIT_MIRROR_REPO=grinco/vornik "$AUDIT" 1 2>&1)"; rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'is NOT on'; then
	pass "a release whose source never reached the mirror is reported and fatal"
else
	fail "unmirrored release should report and exit non-zero (rc=$rc); got: $out"
fi

echo ""
if [ "$fails" -eq 0 ]; then echo "release-audit_test: ALL PASS"; exit 0; fi
echo "release-audit_test: $fails case(s) failed"; exit 1
