#!/usr/bin/env bash
#
# coverage-gate.sh — enforce a minimum total coverage on a merged Go
# coverage profile, excluding non-product (test-scaffolding) packages.
#
# The honest coverage number is the MERGED unit + integration + e2e
# profile (see `make test-coverage-merged` / the CI coverage-gate job):
# integration-only code (the Postgres repository layer) and the daemon
# composition root (initHTTPServer and the rest of internal/service's
# container wiring, exercised only by the booted e2e binary) read as
# ~0% in the unit lane but are genuinely covered by the other lanes.
#
# Test-scaffolding packages are excluded from the denominator: they are
# test code, not product code, and have no own-package tests so they
# would drag the number down for no signal.
#
# Usage: scripts/coverage-gate.sh <merged-profile> [threshold]
#   threshold defaults to the COVERAGE_THRESHOLD env var, then 80.
set -euo pipefail

PROFILE="${1:?usage: coverage-gate.sh <merged-profile> [threshold]}"
THRESHOLD="${2:-${COVERAGE_THRESHOLD:-80}}"

# Packages excluded from the product-code denominator. Pure test
# scaffolding: shared contract-suite runners, generated mocks, backend
# test harnesses, and test utilities.
EXCLUDE='/(repotest|mocks|backendtest|testutil)/'

if [ ! -f "$PROFILE" ]; then
	echo "coverage-gate: profile not found: $PROFILE" >&2
	exit 2
fi

# Rebuild a filtered profile (keep the mode: header, drop excluded
# packages) and let `go tool cover -func` recompute the statement-
# weighted total over what remains.
FILTERED="$(mktemp)"
trap 'rm -f "$FILTERED"' EXIT
head -1 "$PROFILE" >"$FILTERED"
grep -vE '^mode:' "$PROFILE" | grep -vE "$EXCLUDE" >>"$FILTERED" || true

TOTAL="$(go tool cover -func="$FILTERED" | awk '/^total:/{gsub(/%/,"",$3); print $3}')"

echo "coverage-gate: product-code coverage = ${TOTAL}% (threshold ${THRESHOLD}%, excluding test scaffolding)"

# bc-free float compare via awk.
if awk -v c="$TOTAL" -v t="$THRESHOLD" 'BEGIN{exit !(c+0 < t+0)}'; then
	echo "coverage-gate: FAIL — ${TOTAL}% is below the ${THRESHOLD}% floor" >&2
	exit 1
fi
echo "coverage-gate: PASS"
