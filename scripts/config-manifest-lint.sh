#!/usr/bin/env bash
#
# config-manifest-lint.sh — CI-side, repo-only classification guard for the
# config-deployable manifest (LLD 2026-07-16 §4.4).
#
# Two invariants, both derived from the ONE manifest (config-deployable.sh):
#
#   1. COMPLETENESS. Every directory under repo configs/ must be classified in
#      exactly one manifest set (deployable / host-local / ignored), and every
#      top-level file must be a deployable file or match an ignored glob. A new,
#      unclassified subtree fails CI the day it is added — which is what would
#      have caught role-library on 2026-07-10.
#
#   2. NO IGNORE-LIST BYPASS (review finding 2). CONFIG_IGNORED is a CLOSED set:
#      every entry must appear in KNOWN_NON_RUNTIME_IGNORES below, a short
#      allowlist hard-coded IN THIS LINT of directories proven to hold no
#      daemon-read content. So the path of least resistance under a completeness
#      failure — "add it to the ignore list to make the lint shut up" — does NOT
#      work: silencing a new subtree requires a deliberate, reviewed edit to
#      this file, not a one-line manifest tweak. That is what stops a real,
#      daemon-read subtree (role-library) from being hidden in the ignore list
#      and left undeployed (the 2026-07-16 outcome). A code-reference grep was
#      tried first and rejected: config subtrees are referenced as bare quoted
#      literals ("role-library"), so any grep loose enough to catch them also
#      matches generic words ("evals"/"examples") and false-positives. The
#      two-place edit (manifest + this allowlist) is the intended safety cost.
#
# Needs only the repo (no deployed tree) so it runs in CI. Read-only.
#
# Usage: scripts/config-manifest-lint.sh
# Env:   VORNIK_MANIFEST           manifest to source (tests)
#        VORNIK_REPO_CONFIGS_DIR   repo configs/ root to scan (tests)
# Exit:  0 = manifest classifies everything and ignore-set is closed, 1 = violation(s).
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
MANIFEST="${VORNIK_MANIFEST:-$SCRIPT_DIR/config-deployable.sh}"
REPO_CONFIGS="${VORNIK_REPO_CONFIGS_DIR:-$REPO_ROOT/configs}"

# CLOSED allowlist of directories permitted in CONFIG_IGNORED — each proven to
# hold no daemon/feature-doctor-read content. Adding a new ignore requires
# editing THIS list (a reviewed act), which is the anti-bypass (see header §2).
KNOWN_NON_RUNTIME_IGNORES=(evals examples)

# shellcheck source=scripts/config-deployable.sh
. "$MANIFEST"

if [ ! -d "$REPO_CONFIGS" ]; then
	echo "config-manifest-lint: repo configs dir not found: $REPO_CONFIGS" >&2
	exit 1
fi

violations=0
viol() { echo "VIOLATION: $1" >&2; violations=$((violations+1)); }

in_list() { # in_list <needle> <list-elem...>
	local needle="$1"; shift
	local e
	for e in "$@"; do [ "$e" = "$needle" ] && return 0; done
	return 1
}

# --- Invariant 1a: every configs/ directory is classified ---
for d in "$REPO_CONFIGS"/*/; do
	[ -d "$d" ] || continue
	name="$(basename "$d")"
	if in_list "$name" "${CONFIG_DEPLOYABLE_DIRS[@]}"; then continue; fi
	if in_list "$name" "${CONFIG_HOSTLOCAL_DIRS[@]}"; then continue; fi
	if in_list "$name" "${CONFIG_IGNORED[@]}"; then continue; fi
	viol "configs/$name/ is not classified in the manifest (add to CONFIG_DEPLOYABLE_DIRS, CONFIG_HOSTLOCAL_DIRS, or CONFIG_IGNORED in config-deployable.sh)"
done

# --- Invariant 1b: every top-level configs/ file is deployable or ignored-glob ---
for f in "$REPO_CONFIGS"/*; do
	[ -f "$f" ] || continue
	base="$(basename "$f")"
	if in_list "$base" "${CONFIG_DEPLOYABLE_FILES[@]}"; then continue; fi
	matched=0
	for g in "${CONFIG_IGNORED_GLOBS[@]}"; do
		# shellcheck disable=SC2053
		[[ "$base" == $g ]] && { matched=1; break; }
	done
	[ "$matched" -eq 1 ] && continue
	viol "configs/$base is not classified (add to CONFIG_DEPLOYABLE_FILES or CONFIG_IGNORED_GLOBS)"
done

# --- Invariant 2: CONFIG_IGNORED is a closed set (anti-bypass) ---
for name in "${CONFIG_IGNORED[@]}"; do
	if ! in_list "$name" "${KNOWN_NON_RUNTIME_IGNORES[@]}"; then
		viol "configs/$name/ is in CONFIG_IGNORED but not in this lint's closed KNOWN_NON_RUNTIME_IGNORES allowlist — a subtree cannot be silenced by ignoring it; if it ships to the daemon add it to CONFIG_DEPLOYABLE_DIRS (or CONFIG_HOSTLOCAL_DIRS if per-operator), or if it is genuinely non-runtime add it to KNOWN_NON_RUNTIME_IGNORES in config-manifest-lint.sh with justification (a reviewed act)"
	fi
done

if [ "$violations" -eq 0 ]; then
	echo "config-manifest-lint: OK — every configs/ subtree is classified and CONFIG_IGNORED is a closed non-runtime set"
	exit 0
fi
echo "" >&2
echo "config-manifest-lint: $violations violation(s). The manifest (scripts/config-deployable.sh) is the single source of truth for what deploys — fix it there." >&2
exit 1
