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
if [ -L "$DEST" ]; then
	echo "config-deploy: refusing symlinked deployed configs dir: $DEST" >&2
	exit 1
fi
mkdir -p "$DEST"

# Strict policy: deployed config paths must be real directories/files, not
# symlinks. The daemon reads the deployed tree as configuration authority; a
# symlink can redirect a copy or validation check outside that tree. This
# portable shell guard narrows accidental/misconfigured trees, but it is not an
# openat/O_NOFOLLOW implementation. The deployment target is expected to be an
# operator-owned config directory, not a concurrently attacker-writable tree.
path_has_symlink_component() {
	local path="$1" rel cur part
	rel="${path#"$DEST"}"
	rel="${rel#/}"
	cur="$DEST"
	[ -L "$cur" ] && return 0
	[ -z "$rel" ] && return 1
	local IFS='/'
	for part in $rel; do
		cur="$cur/$part"
		[ -L "$cur" ] && return 0
	done
	return 1
}

refuse_unsafe_dest_path() {
	local path="$1"
	if [ -L "$path" ]; then
		echo "config-deploy: refusing symlinked deployed path: $path" >&2
		exit 1
	fi
	if path_has_symlink_component "$(dirname "$path")"; then
		echo "config-deploy: refusing deployed path with symlinked parent: $path" >&2
		exit 1
	fi
}

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
	refuse_unsafe_dest_path "$DEST/$dir"
	if [ -e "$DEST/$dir" ] && [ ! -d "$DEST/$dir" ]; then
		echo "config-deploy: refusing non-directory deployed path: $DEST/$dir" >&2
		exit 1
	fi
	mkdir -p "$DEST/$dir"
	while IFS= read -r rel; do
		rel="${rel#./}"
		dest="$DEST/$dir/$rel"
		refuse_unsafe_dest_path "$dest"
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
	refuse_unsafe_dest_path "$DEST/$file"
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
			refuse_unsafe_dest_path "$DEST/$dir/$rel"
			[ -f "$DEST/$dir/$rel" ] || missing="$missing $dir/$rel"
		done < <(cd "$REPO_CONFIGS/$dir" && find . -type f)
	fi
done
for file in "${CONFIG_DEPLOYABLE_FILES[@]}"; do
	[ -f "$REPO_CONFIGS/$file" ] || continue
	refuse_unsafe_dest_path "$DEST/$file"
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
