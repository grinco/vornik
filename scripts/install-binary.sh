#!/usr/bin/env bash
# install-binary.sh <source> <target> — install an executable over one that may
# be RUNNING.
#
# WHY THIS IS NOT `cp` OR `install`. Both open the target and truncate it, and
# Linux refuses that for a file currently being executed:
#
#     cp: cannot create regular file '.../vornik-enterprise': Text file busy
#
# That is ETXTBSY, and on 2026-09-06 it stopped `make install-enterprise` from
# doing the one thing it exists for — upgrading a running deployment — after it
# had already rebuilt all seven container images, leaving new images beside an
# old daemon.
#
# rename(2) has neither problem. Writing beside the target and moving it into
# place replaces the DIRECTORY ENTRY: the running process keeps executing its
# open inode and exits normally, the new binary is visible to the next exec, and
# there is no window in which the path holds a partially written file. `mv -f`
# within one directory is that rename.
set -euo pipefail

src="${1:?usage: install-binary.sh <source> <target>}"
dst="${2:?usage: install-binary.sh <source> <target>}"

[ -f "$src" ] || { echo "install-binary: source not found: $src" >&2; exit 1; }

dstdir="$(dirname "$dst")"
mkdir -p "$dstdir"

# The temp MUST live in the target's directory: rename(2) cannot cross a
# filesystem, and $TMPDIR frequently is one. Named with a leading dot and the
# pid so a concurrent install cannot collide, and so a crash leaves something
# obviously not-a-binary rather than a plausible-looking one.
tmp="$dstdir/.$(basename "$dst").new.$$"
trap 'rm -f "$tmp"' EXIT

cp "$src" "$tmp"
chmod 755 "$tmp"
mv -f "$tmp" "$dst"
trap - EXIT
