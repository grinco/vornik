#!/usr/bin/env bash
#
# test-config-drift-check.sh — tests for config-drift-check.sh's manifest-driven
# coverage (LLD 2026-07-16-config-tree-drift-coverage-design.md §6).
#
# Self-contained: builds fixture repo + deployed config trees in a tempdir and
# drives scripts/config-drift-check.sh against them via VORNIK_REPO_CONFIGS_DIR
# (repo side) + the deployed-dir arg. No bats dependency (matches the repo's
# plain-shell test style, cf. config-lint.sh).
#
# Usage: scripts/test-config-drift-check.sh
# Exit:  0 = all cases pass, 1 = a case failed
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
DRIFT="$HERE/config-drift-check.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/cfgdrift-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails+1)); }

# Build a fresh (repo, deployed) fixture pair that is fully in sync across every
# deployable subtree + pricing.yaml. Individual cases then perturb one thing.
build_fixture() {
	REPO="$TMP/repo/configs"; DEP="$TMP/deployed"
	rm -rf "$TMP/repo" "$DEP"
	for d in swarms workflows role-library project-templates projects; do
		mkdir -p "$REPO/$d" "$DEP/$d"
	done
	echo "coder archetype" > "$REPO/role-library/coder.md";        cp "$REPO/role-library/coder.md" "$DEP/role-library/coder.md"
	echo "dev swarm"       > "$REPO/swarms/dev-swarm.md";          cp "$REPO/swarms/dev-swarm.md" "$DEP/swarms/dev-swarm.md"
	echo "adaptive"        > "$REPO/workflows/adaptive.md";        cp "$REPO/workflows/adaptive.md" "$DEP/workflows/adaptive.md"
	echo "tmpl"            > "$REPO/project-templates/x.md";       cp "$REPO/project-templates/x.md" "$DEP/project-templates/x.md"
	echo "pricing v1"      > "$REPO/pricing.yaml";                 cp "$REPO/pricing.yaml" "$DEP/pricing.yaml"
	# projects/ is host-local: deliberately divergent, must NOT count as drift.
	echo "repo example proj" > "$REPO/projects/example.md"
	echo "live operator proj" > "$DEP/projects/ibkr-trader.md"
}

run_drift() { VORNIK_REPO_CONFIGS_DIR="$REPO" "$DRIFT" "$DEP" 2>&1; }

# --- Case 1 (test of record for the 2026-07-16 role-library incident): a repo
# role-library archetype absent from the deployed tree is MISSING drift. ---
build_fixture
rm -f "$DEP/role-library/coder.md"
out="$(run_drift)"; rc=$?
if [ "$rc" -eq 1 ] && printf '%s' "$out" | grep -q "MISSING" && printf '%s' "$out" | grep -q "role-library/coder.md"; then
	pass "role-library MISSING is drift (regression: 2026-07-16 composer-blocked incident)"
else
	fail "role-library MISSING should exit 1 with a MISSING role-library line (rc=$rc); got: $out"
fi

# --- Case 2: fully-synced fixture reports in sync (exit 0). ---
build_fixture
out="$(run_drift)"; rc=$?
if [ "$rc" -eq 0 ]; then pass "in-sync fixture exits 0"; else fail "in-sync should exit 0 (rc=$rc); got: $out"; fi

# --- Case 3: pricing.yaml (a deployable FILE, not a dir) content drift. ---
build_fixture
echo "pricing v2 TUNED" > "$DEP/pricing.yaml"
out="$(run_drift)"; rc=$?
if [ "$rc" -eq 1 ] && printf '%s' "$out" | grep -q "pricing.yaml"; then
	pass "pricing.yaml content drift is detected"
else
	fail "pricing.yaml drift should exit 1 naming pricing.yaml (rc=$rc); got: $out"
fi

# --- Case 4: projects/ (host-local) divergence must NOT set the drift code. ---
build_fixture
# fixture already has divergent projects/; nothing else perturbed
out="$(run_drift)"; rc=$?
if [ "$rc" -eq 0 ]; then
	pass "host-local projects/ divergence does not count as drift"
else
	fail "host-local projects/ must not set drift exit (rc=$rc); got: $out"
fi

# --- Case 5: a deployable-DIR content drift (swarms) is detected. ---
build_fixture
echo "dev swarm EDITED" > "$DEP/swarms/dev-swarm.md"
out="$(run_drift)"; rc=$?
if [ "$rc" -eq 1 ] && printf '%s' "$out" | grep -q "swarms/dev-swarm.md"; then
	pass "swarms/ content drift is detected"
else
	fail "swarms drift should exit 1 naming the file (rc=$rc); got: $out"
fi

# --- Case 6 (review finding 1): a NON-.md file in a deployable dir that is
# missing from the deployed tree must be detected — the installer copies all
# files, so the drift-check must too (not just *.md). ---
build_fixture
echo "regime: {}" > "$REPO/role-library/params.yaml"   # non-md repo file, not deployed
out="$(run_drift)"; rc=$?
if [ "$rc" -eq 1 ] && printf '%s' "$out" | grep -q "role-library/params.yaml"; then
	pass "non-.md file in a deployable dir is covered (extension-agnostic)"
else
	fail "non-.md MISSING should exit 1 naming params.yaml (rc=$rc); got: $out"
fi

# --- Case 7 (review finding 1): nested subdir content (project-templates/<slug>/)
# drift is detected — templates live in subdirs, not top-level *.md. ---
build_fixture
mkdir -p "$REPO/project-templates/blog"
echo "tmpl v1" > "$REPO/project-templates/blog/project.md"   # in repo, not deployed
out="$(run_drift)"; rc=$?
if [ "$rc" -eq 1 ] && printf '%s' "$out" | grep -q "project-templates/blog/project.md"; then
	pass "nested subdir content (project-templates) is covered"
else
	fail "nested template MISSING should exit 1 naming the nested file (rc=$rc); got: $out"
fi

run_drift_strict() { VORNIK_REPO_CONFIGS_DIR="$REPO" STRICT_CONFIG_DEPLOY=1 "$DRIFT" "$DEP" 2>&1; }

# --- Case 8 (strict tier): a CANONICAL (non-tunable) asset drift is FATAL (exit 3)
# under STRICT_CONFIG_DEPLOY=1 — role-library/pricing must never diverge. ---
build_fixture
rm -f "$DEP/role-library/coder.md"   # canonical MISSING
out="$(run_drift_strict)"; rc=$?
if [ "$rc" -eq 3 ] && printf '%s' "$out" | grep -q "role-library/coder.md" \
	&& printf '%s' "$out" | grep -q "STRICT_CONFIG_DEPLOY=1 and a CANONICAL asset drifted"; then
	pass "strict: canonical (role-library) drift is fatal (exit 3) + fatal banner emitted"
else
	fail "strict canonical drift should exit 3 with the fatal banner (rc=$rc); got: $out"
fi

# --- Case 9 (strict tier): TUNABLE-only drift stays exit 1 (warn), NOT fatal —
# operator sovereignty over swarms/workflows tuning. ---
build_fixture
echo "dev swarm TUNED" > "$DEP/swarms/dev-swarm.md"   # tunable DRIFT only
out="$(run_drift_strict)"; rc=$?
if [ "$rc" -eq 1 ]; then
	pass "strict: tunable-only drift stays warn (exit 1, not fatal)"
else
	fail "strict tunable-only drift should exit 1 (rc=$rc); got: $out"
fi

# --- Case 10: non-strict canonical drift is unchanged (exit 1 backstop). ---
build_fixture
rm -f "$DEP/role-library/coder.md"
out="$(run_drift)"; rc=$?
if [ "$rc" -eq 1 ]; then
	pass "non-strict canonical drift stays exit 1 (unchanged)"
else
	fail "non-strict canonical drift should exit 1 (rc=$rc); got: $out"
fi

# --- Case 11 (security): a deployed symlink in a canonical config slot is
# drift, even if its target has matching content. The checker must not diff
# through symlinks and accidentally bless a config tree that points outside
# itself. ---
build_fixture
outside="$TMP/outside-role.md"; cp "$REPO/role-library/coder.md" "$outside"
rm -f "$DEP/role-library/coder.md"
ln -s "$outside" "$DEP/role-library/coder.md"
out="$(run_drift)"; rc=$?
if [ "$rc" -eq 1 ] && printf '%s' "$out" | grep -q "SYMLINK" && printf '%s' "$out" | grep -q "role-library/coder.md"; then
	pass "deployed canonical symlink is reported as drift"
else
	fail "canonical symlink should be drift (rc=$rc); got: $out"
fi

# --- Case 12 (strict): the same canonical symlink is fatal under strict. ---
build_fixture
outside="$TMP/outside-pricing.yaml"; cp "$REPO/pricing.yaml" "$outside"
rm -f "$DEP/pricing.yaml"
ln -s "$outside" "$DEP/pricing.yaml"
out="$(run_drift_strict)"; rc=$?
if [ "$rc" -eq 3 ] && printf '%s' "$out" | grep -q "SYMLINK" && printf '%s' "$out" | grep -q "pricing.yaml"; then
	pass "strict: deployed canonical symlink is fatal"
else
	fail "strict canonical symlink should exit 3 (rc=$rc); got: $out"
fi

echo ""
if [ "$fails" -eq 0 ]; then echo "test-config-drift-check: ALL PASS"; exit 0; fi
echo "test-config-drift-check: $fails case(s) failed"; exit 1
