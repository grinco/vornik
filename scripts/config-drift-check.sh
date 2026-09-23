#!/usr/bin/env bash
#
# config-drift-check.sh — report divergence between the repo's tracked config
# source (configs/) and the deployed tree the daemon actually reads
# ($VORNIK_CONFIGS_DIR, default ~/.config/vornik/configs).
#
# vornik reads ONLY the deployed copy (configs live in two trees). A deploy that
# ships a new binary but forgets to sync configs leaves the daemon running stale
# or MISSING definitions — how the `network: daemon-only` lines ended up in HEAD
# but not the deployed trading-swarm.md (2026-05), and how role-library shipped
# empty and left the composer [blocked] (2026-07). This is a read-only
# DIAGNOSTIC: it never edits either tree. Some drift may be intentional
# host-specific tuning — the tool surfaces it; the operator decides.
#
# The set of subtrees/files compared is NOT hard-coded here: it is sourced from
# scripts/config-deployable.sh (the single source of truth), so a new config
# subtree is covered the moment it is added to the manifest.
#
# Symlink policy: deployed config entries are treated as drift, even when a
# symlink target has matching bytes. The deployed tree is the daemon's authority;
# following symlinks would let the diagnostic bless config state whose real
# storage lives outside that tree. Operators who want shared config should copy
# the file into the deployed tree or make the whole configs directory a real
# mount point before this script runs.
#
# Usage:  scripts/config-drift-check.sh [deployed-configs-dir]
# Env:    VORNIK_REPO_CONFIGS_DIR   override the repo-side configs/ root (tests)
#         VORNIK_CONFIGS_DIR        deployed tree (if arg omitted)
# Exit:   0 = in sync, 1 = drift found, 2 = deployed tree missing.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/config-deployable.sh
. "$SCRIPT_DIR/config-deployable.sh"

REPO_CONFIGS="${VORNIK_REPO_CONFIGS_DIR:-$(cd "$SCRIPT_DIR/.." && pwd)/configs}"

# --missed-template-changes: report ONLY the drift that means "the shipped
# template has something this deployment never got", and stay silent about the
# operator's own tuning.
#
# WHY A FLAT DRIFT LIST IS THE WRONG SHAPE. `configs/` is a TEMPLATE — the
# starting point a fresh install or a CE user receives. The deployed tree is
# PRODUCTION, tuned against this host's observed behaviour. So most drift is
# EXPECTED and permanent: a step timeout of 1350s against a template's 15m is
# not a defect, and telling someone about it every run is how a check stops
# being read. On this host the flat list was 14 entries of which 2 mattered.
#
# THE SIGNAL IS THE DIFF'S ASYMMETRY, not its size. Reading
# `diff <template> <deployed>`:
#
#   d hunk  lines the TEMPLATE has and the deployment lacks  -> MISSED CHANGE
#   a hunk  lines only the deployment has                    -> tuning
#   c hunk  same line, different value                       -> tuning
#
# and a d hunk of only comments or blanks is documentation, not behaviour —
# `research.md`'s template carries a comment explaining why the TEMPLATE omits
# retry values, which is not something a deployment is missing.
#
# It found two real gaps the size-based list had buried, both from 2026-09-18
# work that never deployed: dev-swarm without `onViolation: warn` (absent
# defaults to FAIL, so an unrecognised tier failed the step instead of
# degrading to 1.0x) and dev-pipeline without the pinned-case id contract.
# assistant-swarm has 1,466 differing lines and ZERO missed changes.
if [ "${1:-}" = "--missed-template-changes" ]; then
	shift
	DEPLOYED="${1:-${VORNIK_CONFIGS_DIR:-$HOME/.config/vornik/configs}}"
	if [ ! -d "$DEPLOYED" ]; then
		echo "config-drift-check: deployed configs dir not found: $DEPLOYED" >&2
		exit 2
	fi
	ack_file="$REPO_CONFIGS/config-divergence-acknowledged.txt"
	[ -f "$ack_file" ] || ack_file="$SCRIPT_DIR/config-divergence-acknowledged.txt"
	acked="$(sed -e 's/#.*//' -e 's/[[:space:]].*//' "$ack_file" 2>/dev/null | grep -E '^[^[:space:]]+$' || true)"
	examined=0
	missed=0
	unacked=0
	while IFS= read -r rel; do
		[ -n "$rel" ] || continue
		src="$REPO_CONFIGS/$rel"; dep="$DEPLOYED/$rel"
		[ -f "$src" ] && [ -f "$dep" ] || continue
		diff -q "$src" "$dep" >/dev/null 2>&1 && continue
		examined=$((examined + 1))
		# Template-only lines, from `d` hunks only. `-e` output marks a pure
		# deletion as `NdM`; awk keeps the `<` lines under such a hunk and
		# stops at the next hunk header.
		tmpl_only="$(diff "$src" "$dep" | awk '
			/^[0-9,]+d[0-9,]+$/ { keep = 1; next }
			/^[0-9,]+[ac][0-9,]+$/ { keep = 0; next }
			keep && /^< / { print substr($0, 3) }
		')"
		# Comments and blanks are documentation, not a missed behaviour change.
		behaviour="$(printf '%s\n' "$tmpl_only" | sed -e 's/^[[:space:]]*//' \
			| grep -vE '^(#|$)' || true)"
		[ -n "$behaviour" ] || continue
		missed=$((missed + 1))
		if printf '%s\n' "$acked" | grep -qxF "$rel"; then
			echo "ACKNOWLEDGED: $rel carries template changes deliberately not deployed"
			continue
		fi
		unacked=$((unacked + 1))
		echo "MISSED TEMPLATE CHANGE: $rel"
		printf '%s\n' "$behaviour" | sed 's/^/    + /'
	done < <(cd "$REPO_CONFIGS" && find . -type f 2>/dev/null | sed 's|^\./||')
	# THE DENOMINATOR (tenet 7.4): "none found" and "nothing was examined"
	# must not render the same.
	echo "config-drift-check: ${unacked} unacknowledged missed template change(s), ${missed} total, of ${examined} drifted file(s) examined in ${DEPLOYED}"
	[ "$unacked" -eq 0 ] || exit 1
	exit 0
fi

DEPLOYED="${1:-${VORNIK_CONFIGS_DIR:-$HOME/.config/vornik/configs}}"

if [ ! -d "$DEPLOYED" ]; then
	echo "config-drift-check: deployed configs dir not found: $DEPLOYED" >&2
	exit 2
fi

drift=0
canonical_drift=0   # MISSING/DRIFT of a non-tunable canonical asset (role-library, pricing)

# --- deployable directories (from the manifest) ---
# Extension-agnostic + recursive: the installer copies EVERY file (any
# extension, nested subdirs like project-templates/<slug>/), so the drift-check
# compares every file too — not just top-level *.md (review 2026-07-16: a
# *.md-only compare silently re-opened the drift gap for .yaml/.json files and
# for nested template content).
for sub in "${CONFIG_DEPLOYABLE_DIRS[@]}"; do
	src="$REPO_CONFIGS/$sub"
	[ -d "$src" ] || continue
	tag="canonical"; config_is_tunable "$sub" && tag="tunable"
	# forward: every repo file present + identical in the deployed tree
	while IFS= read -r rel; do
		rel="${rel#./}"
		dep="$DEPLOYED/$sub/$rel"
		if [ -L "$dep" ]; then
			echo "SYMLINK (refusing deployed symlink): $sub/$rel [$tag]"
			drift=1; [ "$tag" = canonical ] && canonical_drift=1
		elif [ ! -f "$dep" ]; then
			echo "MISSING (in repo, not deployed): $sub/$rel [$tag]"
			drift=1; [ "$tag" = canonical ] && canonical_drift=1
		elif ! diff -q "$src/$rel" "$dep" >/dev/null 2>&1; then
			echo "DRIFT: $sub/$rel [$tag]"
			drift=1; [ "$tag" = canonical ] && canonical_drift=1
		fi
	done < <(cd "$src" && find . -type f)
	# reverse: deployed files with no repo counterpart (host-only or orphaned)
	#
	# OPERATOR BACKUPS ARE SKIPPED. The documented edit procedure for a deployed
	# config is "copy it to <name>.bak-<stamp>-<why>, then edit", so every
	# careful change leaves a file here that is BY DEFINITION not in the repo.
	# Reporting those is reporting this script's own advice back at the reader:
	# on the live host, 2026-09-22, the reverse scan emitted 72 backup lines
	# against 23 real drift lines. At that ratio nobody reads the output, and
	# the companion reviewer's swarm sat drifted underneath it for weeks until
	# an unrelated incident surfaced it — a control whose signal is buried is
	# a control that is not running.
	#
	# The filter is on the SUFFIX only. A host-only file with an ordinary name
	# is still reported, because that is the case this scan exists for: an
	# orphaned config the daemon loads and the repo has never heard of.
	if [ -d "$DEPLOYED/$sub" ]; then
		while IFS= read -r rel; do
			rel="${rel#./}"
			case "$rel" in
				*.bak|*.bak-*|*.bak.*|*.pre-*|*~|*.orig|*.rej) continue ;;
			esac
			[ -f "$src/$rel" ] || echo "DEPLOYED-ONLY (not in repo): $sub/$rel"
		done < <(cd "$DEPLOYED/$sub" && find . -type f)
	fi
done

# --- deployable top-level files (from the manifest) ---
for file in "${CONFIG_DEPLOYABLE_FILES[@]}"; do
	src="$REPO_CONFIGS/$file"
	[ -f "$src" ] || continue
	dep="$DEPLOYED/$file"
	if [ -L "$dep" ]; then
		echo "SYMLINK (refusing deployed symlink): $file [canonical]"
		drift=1; canonical_drift=1
	elif [ ! -f "$dep" ]; then
		echo "MISSING (in repo, not deployed): $file [canonical]"
		drift=1; canonical_drift=1
	elif ! diff -q "$src" "$dep" >/dev/null 2>&1; then
		echo "DRIFT: $file [canonical]"
		drift=1; canonical_drift=1
	fi
done

# --- host-local directories: report divergence at INFO, never set drift ---
for sub in "${CONFIG_HOSTLOCAL_DIRS[@]}"; do
	[ -d "$REPO_CONFIGS/$sub" ] || [ -d "$DEPLOYED/$sub" ] || continue
	echo "INFO: $sub/ is host-local — not compared (per-operator; drift expected)"
done

if [ "$drift" -eq 0 ]; then
	echo "config-drift-check: repo configs in sync with $DEPLOYED"
	exit 0
fi
echo ""
echo "config-drift-check: drift found. Review with:  diff configs/<path> $DEPLOYED/<path>"
echo "If the repo copy is canonical, sync it into the deployed tree and reload; if the"
echo "deployed copy is intentional host tuning, fold the change back into the repo."

# Strict tier (design §4.4): DRIFT/MISSING of a non-tunable CANONICAL asset
# (role-library, pricing.yaml) is FATAL (exit 3) under STRICT_CONFIG_DEPLOY=1 —
# those must never diverge. Tunable-dir drift (swarms/workflows/templates) stays
# exit 1 (warn) so an operator's legitimate tuning doesn't fail a strict deploy.
if [ "${STRICT_CONFIG_DEPLOY:-0}" = "1" ] && [ "$canonical_drift" -eq 1 ]; then
	echo "config-drift-check: STRICT_CONFIG_DEPLOY=1 and a CANONICAL asset drifted — failing." >&2
	exit 3
fi
exit "$drift"
