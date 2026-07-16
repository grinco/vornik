#!/usr/bin/env bash
#
# test-config-deploy.sh — tests for config-deploy.sh (LLD 2026-07-16 §4.2/§6).
# The installer copies every manifest-listed deployable subtree into the
# deployed tree, preserves operator-tuned files, and (strict) aborts if a
# canonical subtree is missing after copy.
#
# Self-contained; drives config-deploy.sh against a fixture repo configs/ and a
# temp target dir. No bats.
#
# Usage: scripts/test-config-deploy.sh
# Exit:  0 = all pass, 1 = a case failed
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
DEPLOY="$HERE/config-deploy.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/cfgdeploy-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails+1)); }

build_repo() {
	REPO="$TMP/repo/configs"; rm -rf "$TMP/repo"; mkdir -p "$REPO"
	for d in swarms workflows role-library; do mkdir -p "$REPO/$d"; done
	echo "coder"   > "$REPO/role-library/coder.md"
	echo "lead"    > "$REPO/role-library/lead.md"
	echo "dev"     > "$REPO/swarms/dev-swarm.md"
	echo "adaptive"> "$REPO/workflows/adaptive.md"
	mkdir -p "$REPO/project-templates/blog"
	echo "tmpl"    > "$REPO/project-templates/blog/project.md"
	echo "pricing" > "$REPO/pricing.yaml"
}

run_deploy() { VORNIK_REPO_CONFIGS_DIR="$REPO" "$DEPLOY" "$TGT" 2>&1; }

# --- Case 1 (regression: role-library was never copied 2026-07-16): a fresh
# install copies every deployable subtree, including role-library. ---
build_repo
TGT="$TMP/t1"; rm -rf "$TGT"
out="$(run_deploy)"; rc=$?
if [ "$rc" -eq 0 ] \
	&& [ -f "$TGT/configs/role-library/coder.md" ] \
	&& [ -f "$TGT/configs/role-library/lead.md" ] \
	&& [ -f "$TGT/configs/swarms/dev-swarm.md" ] \
	&& [ -f "$TGT/configs/workflows/adaptive.md" ] \
	&& [ -f "$TGT/configs/project-templates/blog/project.md" ] \
	&& [ -f "$TGT/configs/pricing.yaml" ]; then
	pass "fresh install copies all deployable subtrees incl role-library + templates + pricing"
else
	fail "fresh install missing a deployable (rc=$rc); got: $out; tree: $(find "$TGT" -type f 2>/dev/null | sort | tr '\n' ' ')"
fi

# --- Case 2: preserve-AND-add — an operator-tuned deployed file survives, AND a
# new repo file lands next to it (older-GNU `cp -n` skip-don't-descend would
# preserve coder.md but silently drop the new lead.md; review finding). ---
build_repo
TGT="$TMP/t2"; rm -rf "$TGT"; mkdir -p "$TGT/configs/role-library"
echo "OPERATOR TUNED" > "$TGT/configs/role-library/coder.md"   # pre-existing, tuned
run_deploy >/dev/null 2>&1
got="$(cat "$TGT/configs/role-library/coder.md" 2>/dev/null)"
if [ "$got" = "OPERATOR TUNED" ] && [ -f "$TGT/configs/role-library/lead.md" ]; then
	pass "preserve-existing keeps coder.md AND adds the new lead.md"
else
	fail "preserve+add failed: coder.md='$got', lead.md present=$([ -f "$TGT/configs/role-library/lead.md" ] && echo yes || echo NO)"
fi

# --- Case 3 (strict): a real copy-I/O failure — repo HAS role-library content
# but it can't land on disk (unwritable target subdir) — aborts under
# STRICT_CONFIG_DEPLOY=1. This is the copy-I/O guard the self-check exists for.
# Skipped as root (chmod 500 doesn't block root writes -> would false-red). ---
if [ "$(id -u)" -eq 0 ]; then
	echo "skip - strict copy-I/O case (running as root; chmod 500 doesn't block root)"
else
	build_repo                                # repo role-library has coder.md, lead.md
	TGT="$TMP/t3"; rm -rf "$TGT"; mkdir -p "$TGT/configs/role-library"
	chmod 500 "$TGT/configs/role-library"     # r-x, no write -> cp cannot populate it
	out="$(STRICT_CONFIG_DEPLOY=1 VORNIK_REPO_CONFIGS_DIR="$REPO" "$DEPLOY" "$TGT" 2>&1)"; rc=$?
	chmod 700 "$TGT/configs/role-library" 2>/dev/null || true   # restore for cleanup
	if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q "role-library"; then
		pass "strict mode aborts on a real copy-I/O failure (role-library couldn't land)"
	else
		fail "strict mode should abort naming role-library (rc=$rc); got: $out"
	fi
fi

# --- Case 4 (review finding 4): partial copy of a canonical dir fails the
# per-file self-check under strict (one file lands, another doesn't). ---
if [ "$(id -u)" -eq 0 ]; then
	echo "skip - partial-copy self-check case (running as root)"
else
	build_repo
	TGT="$TMP/t4"; rm -rf "$TGT"; mkdir -p "$TGT/configs/role-library"
	# Pre-place coder.md so it's preserved, then block new writes: lead.md can't land.
	echo "kept" > "$TGT/configs/role-library/coder.md"
	chmod 500 "$TGT/configs/role-library"
	out="$(STRICT_CONFIG_DEPLOY=1 VORNIK_REPO_CONFIGS_DIR="$REPO" "$DEPLOY" "$TGT" 2>&1)"; rc=$?
	chmod 700 "$TGT/configs/role-library" 2>/dev/null || true
	if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q "role-library"; then
		pass "strict per-file self-check catches a PARTIAL canonical copy (lead.md missing)"
	else
		fail "partial canonical copy should abort under strict (rc=$rc); got: $out"
	fi
fi

echo ""
if [ "$fails" -eq 0 ]; then echo "test-config-deploy: ALL PASS"; exit 0; fi
echo "test-config-deploy: $fails case(s) failed"; exit 1
