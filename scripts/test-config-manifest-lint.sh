#!/usr/bin/env bash
#
# test-config-manifest-lint.sh — tests for config-manifest-lint.sh (LLD
# 2026-07-16 §4.4/§6). The lint is the CI-side, repo-only classification guard:
# every repo configs/ subtree must be classified in the manifest, and the
# ignore list cannot be used to silence a runtime-referenced subtree (the
# role-library bypass the review flagged).
#
# Self-contained; drives the real lint against fixture configs/, a fixture
# manifest, and a fixture code root via env overrides. No bats.
#
# Usage: scripts/test-config-manifest-lint.sh
# Exit:  0 = all pass, 1 = a case failed
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
LINT="$HERE/config-manifest-lint.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/cfgmanifest-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails+1)); }

# A fixture manifest we can perturb (the real one has fixed arrays).
write_manifest() {
	cat > "$TMP/manifest.sh" <<EOF
CONFIG_DEPLOYABLE_DIRS=($1)
CONFIG_DEPLOYABLE_FILES=(pricing.yaml)
CONFIG_HOSTLOCAL_DIRS=(projects)
CONFIG_IGNORED=($2)
CONFIG_IGNORED_GLOBS=('*.example')
CONFIG_TUNABLE_DIRS=(swarms)
config_is_tunable() { local n="\$1" t; for t in "\${CONFIG_TUNABLE_DIRS[@]}"; do [ "\$t" = "\$n" ] && return 0; done; return 1; }
EOF
}

build_configs() {
	CFG="$TMP/configs"; rm -rf "$CFG"; mkdir -p "$CFG"
	for d in "$@"; do mkdir -p "$CFG/$d"; echo "x" > "$CFG/$d/a.md"; done
	echo "pricing" > "$CFG/pricing.yaml"
}

run_lint() {
	VORNIK_MANIFEST="$TMP/manifest.sh" VORNIK_REPO_CONFIGS_DIR="$CFG" "$LINT" 2>&1
}

# --- Case A: an unclassified repo configs/ dir fails. ---
write_manifest "swarms workflows" "evals"
build_configs swarms workflows newthing        # newthing not in any manifest set
out="$(run_lint)"; rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q "newthing"; then
	pass "unclassified subtree 'newthing' fails the lint"
else
	fail "unclassified subtree should fail naming it (rc=$rc); got: $out"
fi

# --- Case B (the bypass, review finding 2): an arbitrary subtree dumped into
# CONFIG_IGNORED must FAIL — it is not in the lint's closed allowlist, so you
# cannot silence a real subtree (role-library) by ignoring it. ---
write_manifest "swarms workflows" "evals widgets"   # widgets wrongly ignored
build_configs swarms workflows widgets
out="$(run_lint)"; rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q "widgets"; then
	pass "arbitrary dir in CONFIG_IGNORED fails the closed-set check (role-library bypass closed)"
else
	fail "ignored non-allowlisted subtree should fail naming it (rc=$rc); got: $out"
fi

# --- Case C (control): an allowlisted non-runtime ignored dir passes. ---
write_manifest "swarms workflows" "evals"
build_configs swarms workflows evals
out="$(run_lint)"; rc=$?
if [ "$rc" -eq 0 ]; then
	pass "allowlisted ignored dir (evals) passes"
else
	fail "allowlisted ignored dir should pass (rc=$rc); got: $out"
fi

# --- Case D (real repo): the shipped manifest classifies the real configs/
# completely and role-library is not hidden. Proves consistency on real data. ---
out="$("$LINT" 2>&1)"; rc=$?
if [ "$rc" -eq 0 ]; then
	pass "real repo configs/ is fully classified (role-library deployable, not ignored)"
else
	fail "real repo should lint clean (rc=$rc); got: $out"
fi

echo ""
if [ "$fails" -eq 0 ]; then echo "test-config-manifest-lint: ALL PASS"; exit 0; fi
echo "test-config-manifest-lint: $fails case(s) failed"; exit 1
