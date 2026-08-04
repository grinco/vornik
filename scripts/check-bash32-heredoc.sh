#!/usr/bin/env bash
#
# check-bash32-heredoc.sh — refuse a heredoc placed DIRECTLY inside a $( … )
# command substitution when its body contains an apostrophe.
#
# WHY THIS EXISTS. bash 3.2 is still /bin/bash on macOS, and its parser scans for
# the closing paren of $( ) WITHOUT honoring heredoc bodies. So this:
#
#     TEXT=$(cat <<EOF
#     … prose that says it's fine …
#     EOF
#     )
#
# parses the apostrophe in "it's" as an opening single quote, never finds a
# closing one, and the whole script dies at the LAST line with
# "unexpected EOF while looking for matching `''" — a message that points nowhere
# near the real cause. bash 4+ parses it correctly, which is why `bash -n` on a
# Linux dev box and shellcheck both pass it: the bug is unreachable locally and
# only appears on a user's Mac.
#
# It cost a broken SessionStart hook on a fresh macOS install (2026-08-04,
# vornik-companion 0.15.1, reported as "line 333: unexpected EOF").
#
# THE FIX the guard steers you to: wrap the heredoc in a function and call it, so
# the heredoc is no longer inside a command substitution while being parsed.
#
#     emit_text() {
#       cat <<EOF
#       … prose that says it's fine …
#     EOF
#     }
#     TEXT=$(emit_text)
#
# Usage: scripts/check-bash32-heredoc.sh <file> [<file> …]
set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: $0 <shell-script> [...]" >&2
  exit 2
fi

status=0

for f in "$@"; do
  [ -f "$f" ] || { echo "check-bash32-heredoc: no such file: $f" >&2; status=1; continue; }
  # awk state machine: when a line opens a command substitution AND a heredoc on
  # the same line, capture the delimiter and scan the body until the delimiter for
  # an apostrophe. Quoted ('EOF') and unquoted (EOF) delimiters are both affected —
  # the quoting controls expansion inside the body, not how bash 3.2 scans for the
  # closing paren.
  awk -v file="$f" '
    # Opening: something like  X=$(cat <<EOF   or   X=$(cat <<'"'"'EOF'"'"'
    # Comments are not code: a comment DESCRIBING the hazard (this repo has one)
    # must not be reported as the hazard.
    !inbody && /^[[:space:]]*#/ { next }

    !inbody && /\$\(/ && /<</ {
      line = $0
      sub(/.*<<[[:space:]]*-?/, "", line)
      gsub(/^['"'"'"]|['"'"'"].*$/, "", line)
      gsub(/[^A-Za-z0-9_].*$/, "", line)
      if (line != "") { inbody = 1; delim = line; start = NR; found = 0 }
      next
    }
    inbody {
      stripped = $0
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", stripped)
      if (stripped == delim) {
        if (found > 0) {
          printf "%s:%d: heredoc <<%s inside $( ) has an apostrophe at line %d — bash 3.2 (macOS /bin/bash) will fail to parse this file\n", file, start, delim, found
          printf "  fix: move the heredoc into a function and assign from a call to it (see scripts/check-bash32-heredoc.sh)\n"
          bad = 1
        }
        inbody = 0
        next
      }
      if (found == 0 && index($0, "'"'"'") > 0) { found = NR }
      next
    }
    END { exit bad ? 1 : 0 }
  ' "$f" || status=1
done

if [ "$status" -eq 0 ]; then
  echo "check-bash32-heredoc: OK — no apostrophe-bearing heredoc inside a command substitution"
fi
exit "$status"
