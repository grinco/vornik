#!/usr/bin/env bash
#
# config-deploy.sh — copy the repo's deployable config assets into the deployed
# tree the daemon reads (LLD 2026-07-16 §4.2). Driven by the ONE manifest
# (config-deployable.sh) so a new subtree ships the moment it is listed — the
# fix for role-library never being copied (2026-07-16) and workflows before it.
#
# Preserve-existing: an operator-tuned deployed file is never clobbered
# (recursive, per-file). After copying, a self-check verifies every deployable
# landed on disk (a COPY-I/O guard — not a content-drift check; content drift is
# config-drift-check.sh's job). Under STRICT_CONFIG_DEPLOY=1 a missing/empty
# canonical subtree aborts the install instead of warning.
#
# Usage: scripts/config-deploy.sh <target-config-dir>
#        (writes into <target-config-dir>/configs/)
# Env:   VORNIK_REPO_CONFIGS_DIR   repo configs/ source (default <repo>/configs)
#        STRICT_CONFIG_DEPLOY=1     abort on a missing deployable after copy
# Exit:  0 = all deployables present after copy, 1 = usage error or (strict) a
#        deployable missing/empty on disk.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/config-deployable.sh
. "$SCRIPT_DIR/config-deployable.sh"

REPO_CONFIGS="${VORNIK_REPO_CONFIGS_DIR:-$(cd "$SCRIPT_DIR/.." && pwd)/configs}"
TARGET="${1:-}"
if [ -z "$TARGET" ]; then
	echo "config-deploy: usage: config-deploy.sh <target-config-dir>" >&2
	exit 1
fi
DEST="$TARGET/configs"
mkdir -p "$DEST"

# --- copy deployable directories (portable per-file recursive, preserve-existing) ---
# Per-file rather than `cp -rn`: BusyBox cp has no -n (would error and, with
# stderr suppressed + `|| true`, silently copy NOTHING), and older GNU `cp -n`
# skips existing dirs without descending (a NEW file in an existing deployed
# subdir would never land). This loop copies each repo file only if absent —
# portable, descends, preserves operator-tuned files, adds new ones. cp errors
# surface (not suppressed) so a genuine failure is visible.
for dir in "${CONFIG_DEPLOYABLE_DIRS[@]}"; do
	src="$REPO_CONFIGS/$dir"
	[ -d "$src" ] || continue
	mkdir -p "$DEST/$dir"
	while IFS= read -r rel; do
		rel="${rel#./}"
		dest="$DEST/$dir/$rel"
		if [ ! -e "$dest" ]; then
			mkdir -p "$(dirname "$dest")"
			cp "$src/$rel" "$dest" || echo "WARN: cp failed: $dir/$rel" >&2
		fi
	done < <(cd "$src" && find . -type f)
done

# --- copy deployable top-level files (preserve-existing) ---
for file in "${CONFIG_DEPLOYABLE_FILES[@]}"; do
	src="$REPO_CONFIGS/$file"
	[ -f "$src" ] || continue
	[ -f "$DEST/$file" ] || cp "$src" "$DEST/$file" || echo "WARN: cp failed: $file" >&2
done

# --- self-check: deployables landed on disk (copy-I/O guard, not content) ---
# Canonical (non-tunable) dirs get PER-FILE completeness — every repo file must
# exist in the deployed tree (catches a partial copy: some files land, others
# don't). Tunable dirs get the looser any-file check: it tolerates a partial
# I/O failure on a non-daemon-critical dir (swarms/workflows/templates) rather
# than aborting the whole install. (Note: the copy loop restores any absent
# file, so operator-pruning of a tunable file does not survive install anyway —
# the looser check is about I/O tolerance, not preserving deletions.)
missing=""
for dir in "${CONFIG_DEPLOYABLE_DIRS[@]}"; do
	[ -d "$REPO_CONFIGS/$dir" ] || continue
	# skip dirs with no repo content — an empty repo subtree ships nothing
	[ -n "$(find "$REPO_CONFIGS/$dir" -type f 2>/dev/null | head -1)" ] || continue
	if config_is_tunable "$dir"; then
		[ -n "$(find "$DEST/$dir" -type f 2>/dev/null | head -1)" ] || missing="$missing $dir/"
	else
		while IFS= read -r rel; do
			rel="${rel#./}"
			[ -f "$DEST/$dir/$rel" ] || missing="$missing $dir/$rel"
		done < <(cd "$REPO_CONFIGS/$dir" && find . -type f)
	fi
done
for file in "${CONFIG_DEPLOYABLE_FILES[@]}"; do
	[ -f "$REPO_CONFIGS/$file" ] || continue
	[ -f "$DEST/$file" ] || missing="$missing $file"
done

if [ -n "$missing" ]; then
	msg="config-deploy: deployable(s) missing on disk after copy:$missing"
	if [ "${STRICT_CONFIG_DEPLOY:-0}" = "1" ]; then
		echo "$msg" >&2
		echo "config-deploy: aborting (STRICT_CONFIG_DEPLOY=1). A canonical config subtree did not deploy." >&2
		exit 1
	fi
	echo "WARN: $msg" >&2
	echo "config-deploy: continuing (set STRICT_CONFIG_DEPLOY=1 to make this fatal)." >&2
fi

echo "config-deploy: deployed config assets into $DEST"
exit 0
