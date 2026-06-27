#!/bin/sh
#
# config-lint.sh — repo-internal invariant guarding the config-drift class.
#
# The source<->deployed config drift incident (see config-drift-check.sh)
# was driven by CRLF line endings: operator UI edits saved deployed
# configs with Windows-style \r\n, and when those were folded back into
# the repo the \r bytes made every byte-level diff "drift" forever. The
# deployed tree does not exist in CI, so the diff check (config-diff) is a
# host/post-deploy step. What IS checkable with only the repo is the
# artifact that caused the drift: a tracked config file containing CR.
#
# This linter fails if ANY tracked file under configs/ contains a carriage
# return (\r). All swarm/workflow/config sources must be pure LF. It needs
# nothing but the repo, so it is the CI-enforceable half of the gate.
#
# Usage: scripts/config-lint.sh
# Exit:  0 = all tracked configs/ files are LF-only
#        1 = one or more files contain CR (offenders printed to stderr)
set -eu

cd "$(dirname "$0")/.."

# Tracked files only — generated/untracked scratch under configs/ is not
# our concern, and git ls-files needs no deployed tree.
if ! files="$(git ls-files configs)"; then
	echo "config-lint: 'git ls-files configs' failed (not a git repo?)" >&2
	exit 1
fi

if [ -z "$files" ]; then
	echo "config-lint: no tracked files under configs/ — nothing to check"
	exit 0
fi

found=0
# IFS=newline so paths with spaces survive; configs/ is plain today but
# this keeps the linter correct if that ever changes.
OLDIFS="$IFS"
IFS='
'
for f in $files; do
	[ -f "$f" ] || continue
	# grep -lU: treat as binary so \r is matched literally, list the file
	# once if any line contains CR. Portable across GNU/BusyBox/macOS grep.
	if grep -lU "$(printf '\r')" "$f" >/dev/null 2>&1; then
		echo "CRLF (carriage return found): $f" >&2
		found=1
	fi
done
IFS="$OLDIFS"

if [ "$found" -ne 0 ]; then
	echo "" >&2
	echo "config-lint: tracked configs/ files must use LF line endings only." >&2
	echo "Fix with:  sed -i 's/\r\$//' <file>   (or: dos2unix <file>)" >&2
	echo "Background: CRLF in deployed configs is what drove the source<->deployed" >&2
	echo "drift incident; keeping the repo LF-only prevents it recurring." >&2
	exit 1
fi

echo "config-lint: all tracked configs/ files are LF-only"
exit 0
