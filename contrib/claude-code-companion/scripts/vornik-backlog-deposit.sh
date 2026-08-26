#!/usr/bin/env bash
# Deposit an off-scope finding into THIS repository's backlog.
#
# WHY THIS IS CLIENT-SIDE. The process says: when you find a defect outside the
# scope of the task you are on, file it and keep going. The daemon-side
# `backlog_deposit` tool exists for that and is right for its designed caller —
# an agent inside a vornik task, on a project whose autonomy loop consumes the
# backlog one item at a time. It is the wrong tool here, for two reasons that
# are structural rather than tunable:
#
#   1. It renders ONE whitespace-flattened line with a 600-character detail cap.
#      Measured against this repo's backlog on 2026-08-26: the median top-level
#      item is 1,811 characters and 47 of 48 exceed that cap. The tables, code
#      references and reasoning that make an item actionable are exactly what a
#      flattened line destroys.
#   2. It writes a daemon-managed workspace, and the daemon must not write the
#      operator's checkout — that containment boundary is load-bearing. So the
#      deposit would land somewhere nobody reads, needing a manual promotion
#      step to reach the file that actually drives work.
#
# The client already owns the checkout, so writing it here is not a boundary
# question at all. See
# https://docs.vornik.io
#
# Usage:
#   vornik-backlog-deposit.sh --title "..." [options] < body
#   echo "body" | vornik-backlog-deposit.sh --title "..." --kind bug --priority P2
#
# Options:
#   --title   <text>   required; the item's one-line title
#   --kind    <k>      bug | feature | optimisation | inefficiency | refactor (default bug)
#   --priority <P>     P0..P3 (default P2)
#   --evidence <text>  optional; file:line or a task/exec id
#   --file    <path>   backlog file (default: nearest https://docs.vornik.io, then BACKLOG.md)
#   --dry-run          print what would be written; touch nothing
#   --allow-duplicate  deposit even if a similar title already exists
#
# Exit codes: 0 deposited (or dry-run ok), 2 usage, 3 duplicate, 4 secret-shaped
# content refused, 5 no backlog file found.
set -euo pipefail

TITLE="" KIND="bug" PRIORITY="P2" EVIDENCE="" FILE="" DRY=0 ALLOW_DUP=0
while [ $# -gt 0 ]; do
  case "$1" in
    --title)           TITLE="${2:-}"; shift 2 ;;
    --kind)            KIND="${2:-}"; shift 2 ;;
    --priority)        PRIORITY="${2:-}"; shift 2 ;;
    --evidence)        EVIDENCE="${2:-}"; shift 2 ;;
    --file)            FILE="${2:-}"; shift 2 ;;
    --dry-run)         DRY=1; shift ;;
    --allow-duplicate) ALLOW_DUP=1; shift ;;
    -h|--help)         sed -n '25,40p' "$0"; exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ -n "$TITLE" ] || { echo "error: --title is required" >&2; exit 2; }

case "$KIND" in
  bug|feature|optimisation|inefficiency|refactor) ;;
  *) echo "error: --kind must be bug|feature|optimisation|inefficiency|refactor (got '$KIND')" >&2; exit 2 ;;
esac
case "$PRIORITY" in
  P0|P1|P2|P3) ;;
  *) echo "error: --priority must be P0|P1|P2|P3 (got '$PRIORITY')" >&2; exit 2 ;;
esac

# Locate the backlog by walking up from the cwd, so the command works from any
# subdirectory of the repo — the same ergonomics `git` gives.
if [ -z "$FILE" ]; then
  _d="$PWD"
  while [ "$_d" != "/" ]; do
    if   [ -f "$_d/https://docs.vornik.io" ]; then FILE="$_d/https://docs.vornik.io"; break
    elif [ -f "$_d/BACKLOG.md" ];      then FILE="$_d/BACKLOG.md"; break
    fi
    _d="$(dirname "$_d")"
  done
fi
if [ -z "$FILE" ] || [ ! -f "$FILE" ]; then
  echo "error: no backlog file found (looked for https://docs.vornik.io then BACKLOG.md from $PWD upward)." >&2
  echo "       Pass --file <path> to name one explicitly." >&2
  exit 5
fi

BODY="$(cat)"

TITLE="$TITLE" KIND="$KIND" PRIORITY="$PRIORITY" EVIDENCE="$EVIDENCE" \
BODY="$BODY" FILE="$FILE" DRY="$DRY" ALLOW_DUP="$ALLOW_DUP" python3 - <<'PYEOF'
import os, re, sys, datetime

title    = os.environ["TITLE"].strip()
kind     = os.environ["KIND"]
priority = os.environ["PRIORITY"]
evidence = os.environ["EVIDENCE"].strip()
body     = os.environ["BODY"].rstrip()
path     = os.environ["FILE"]
dry      = os.environ["DRY"] == "1"
allowdup = os.environ["ALLOW_DUP"] == "1"

# --- secret scan -----------------------------------------------------------
# The backlog is a version-controlled document; a credential pasted into an
# evidence field is committed and pushed. This is the one guard the daemon-side
# pipeline had that hand-editing loses, so it is kept.
#
# Deliberately conservative: it matches shapes that are credentials and very
# little else. A false positive costs the author one edit; a false negative
# costs a rotation.
SECRET_PATTERNS = [
    (r'AIza[0-9A-Za-z_\-]{35}',                 "Google API key"),
    (r'sk-[A-Za-z0-9]{20,}',                    "OpenAI-style secret key"),
    (r'ghp_[A-Za-z0-9]{36}',                    "GitHub personal access token"),
    (r'gh[pousr]_[A-Za-z0-9]{36,}',             "GitHub token"),
    (r'xox[baprs]-[A-Za-z0-9-]{10,}',           "Slack token"),
    (r'-----BEGIN [A-Z ]*PRIVATE KEY-----',     "private key block"),
    (r'eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.', "JWT"),
    (r'(?i)\b(?:postgres|postgresql|mysql|mongodb)://[^\s:]+:[^\s@]+@', "connection string with password"),
]
haystack = "\n".join([title, evidence, body])
for pat, label in SECRET_PATTERNS:
    if re.search(pat, haystack):
        sys.stderr.write(
            "refused: the deposit contains something shaped like a %s.\n"
            "         The backlog is committed and pushed — redact it and retry.\n" % label)
        sys.exit(4)

# --- parse existing items --------------------------------------------------
# Understands BOTH grammars this file actually uses: `## [ ] P2 — Title (date)`
# top-level items, and `- [ ] **Title.**` bullets. The daemon-side backlogfile
# package parses only the second, which is why it sees 243 sub-notes here and
# none of the 48 real items.
src = open(path, encoding="utf-8").read()
lines = src.split("\n")

HEADING_ITEM = re.compile(r'^(#{2,6})\s+\[([ ?x!~])\]\s+(.*)$')
BULLET_ITEM  = re.compile(r'^\s*[-*]\s+\[([ ?x!~])\]\s+(.*)$')

def normalise(t):
    """Lower-case, drop a leading priority tag, strip markdown emphasis, a
    trailing (date), and all punctuation — so trivially-reworded restatements
    of the same finding compare equal.

    A bullet item in this file carries its title as a BOLD LEAD followed by
    prose — `- [ ] **Expose backlog deposit as a tool.** Hit for real on ...`.
    Normalising the whole line would compare a title against a title-plus-essay
    and never match, so the bold lead is taken as the title when there is one.
    """
    t = t.strip()
    lead = re.match(r'^\*\*(.+?)\*\*', t)
    if lead:
        t = lead.group(1)
    t = re.sub(r'^P[0-3]\s*[—\-–]\s*', '', t, flags=re.I)
    t = re.sub(r'\s*\((?:19|20)\d\d-\d\d-\d\d\)\s*$', '', t)
    t = t.replace("**", "").replace("`", "")
    t = re.sub(r'[^\w\s]+', ' ', t.lower())
    return " ".join(t.split())

existing = []          # (normalised_title, marker, line_no, raw)
last_heading_item = -1
for i, line in enumerate(lines):
    m = HEADING_ITEM.match(line)
    if m:
        existing.append((normalise(m.group(3)), m.group(2), i, line))
        last_heading_item = i
        continue
    m = BULLET_ITEM.match(line)
    if m:
        existing.append((normalise(m.group(2)), m.group(1), i, line))

# --- dedup -----------------------------------------------------------------
want = normalise(title)
if not allowdup:
    for norm, marker, lineno, raw in existing:
        if not norm or not want:
            continue
        # Exact normalised match, or one title fully containing the other —
        # the restatement case ("fix X" vs "fix X in the parser").
        if norm == want or (len(want) > 20 and (want in norm or norm in want)):
            state = {" ": "open", "?": "proposed", "~": "in-flight",
                     "x": "done", "!": "failed"}.get(marker, marker)
            sys.stderr.write(
                "duplicate: this looks like an item already in the backlog (%s, line %d):\n"
                "  %s\n"
                "Pass --allow-duplicate if it is genuinely distinct.\n"
                % (state, lineno + 1, raw.strip()[:160]))
            sys.exit(3)

# --- render ----------------------------------------------------------------
date = datetime.date.today().isoformat()
head = "## [ ] %s — %s (%s)" % (priority, title, date)

parts = [head, ""]
if body:
    parts.append(body)
    parts.append("")
if evidence:
    parts.append("Evidence: %s" % evidence)
    parts.append("")
parts.append("*(Filed by the vornik companion, kind: %s.)*" % kind)
block = "\n".join(parts)

# --- choose the insertion point --------------------------------------------
# After the LAST top-level item, and before whatever section follows it. This
# file ends with several prose sections ("## UI polish", "## Trading — FROZEN")
# that are not items; appending at EOF would bury the finding inside one.
#
# Never rewrites existing content — this only inserts. On a 5,000-line curated
# document, a clever transform that corrupts it is far worse than an item in a
# slightly suboptimal place, which the operator can move in one edit.
insert_at = len(lines)
if last_heading_item >= 0:
    for j in range(last_heading_item + 1, len(lines)):
        if re.match(r'^#{2,6}\s+', lines[j]) and not HEADING_ITEM.match(lines[j]):
            insert_at = j
            break

# Land inside the separator convention the file already uses between items.
before = lines[:insert_at]
after  = lines[insert_at:]
while before and before[-1].strip() == "":
    before.pop()
chunk = ["", "", block, "", "---", ""]

if dry:
    sys.stdout.write("dry-run: would insert at line %d of %s\n\n%s\n" % (insert_at + 1, path, block))
    sys.exit(0)

open(path, "w", encoding="utf-8").write("\n".join(before + chunk + after))
sys.stdout.write("deposited into %s at line %d:\n  %s\n" % (path, len(before) + 3, head))
PYEOF
