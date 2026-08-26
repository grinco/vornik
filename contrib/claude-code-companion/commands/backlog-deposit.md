---
description: File an off-scope finding into this repo's backlog, with dedup and a secret scan
argument-hint: "[--kind K] [--priority P] [--evidence E] <title>"
---

# Backlog deposit (this repository's backlog)

Capture a defect, inefficiency or missing capability you found **outside the
scope of the task you are on** — without derailing that task. File it, say one
line about it, and go back to what you were doing.

This writes the repository's own backlog (`https://docs.vornik.io`, found by walking
up from the cwd). It is deliberately **client-side**: the daemon must not write
your checkout, and the daemon-side `backlog_deposit` tool renders one
whitespace-flattened line capped at 600 characters — against a backlog whose
median item is ~1,800 characters, that would destroy the tables and code
references that make an item actionable. See
`https://docs.vornik.io`.

Two guards run that hand-editing loses:

- **Dedup** against every existing item — both `## [ ] P2 — Title` headings and
  `- [ ] **Bold lead.**` bullets, ignoring priority tags and trailing dates. It
  also matches items already `[x]` done, so completed work is not re-filed.
- **A secret scan** over title, body and evidence. The backlog is committed and
  pushed; a credential in an evidence field is a rotation.

Write the BODY as you would write the item — multiple paragraphs, tables, code
references. Structure is preserved.

    /backlog-deposit --kind bug --priority P2 A truncated tool_output hides a successful fetch
    /backlog-deposit --kind feature --priority P3 --evidence internal/api/foo.go:120 Add a resolved-config dump

Exit codes: 3 = duplicate (re-run with `--allow-duplicate` if genuinely
distinct), 4 = secret-shaped content refused, 5 = no backlog file found.

User's arguments: `$ARGUMENTS`

**How to use this command.** The bash below only parses the arguments and shows
you the target. YOU then call the script with the body on stdin, because the
body is prose you are writing — it cannot come from `$ARGUMENTS`:

```
printf '%s\n' "<your multi-paragraph body>" | \
  "${CLAUDE_PLUGIN_ROOT}/scripts/vornik-backlog-deposit.sh" \
    --title "<title>" --kind <kind> --priority <P> [--evidence "<e>"]
```

Run it with `--dry-run` first if you want to see the rendered item and the
insertion point before touching the file.

!`BACKLOG=""; d="$PWD"
while [ "$d" != "/" ]; do
  if [ -f "$d/https://docs.vornik.io" ]; then BACKLOG="$d/https://docs.vornik.io"; break;
  elif [ -f "$d/BACKLOG.md" ]; then BACKLOG="$d/BACKLOG.md"; break; fi
  d="$(dirname "$d")"
done
if [ -z "$BACKLOG" ]; then
  echo "No backlog file found from $PWD upward (looked for https://docs.vornik.io, BACKLOG.md)."
  echo "Pass --file <path> to the script explicitly."
else
  echo "Target backlog: $BACKLOG"
  echo "Open items: $(grep -c '^## \[ \]' "$BACKLOG" 2>/dev/null || echo 0) top-level, $(grep -cE '^\s*[-*]\s+\[ \]' "$BACKLOG" 2>/dev/null || echo 0) bullets"
fi
`

The bash above only locates the backlog. Call the script yourself with the body
on stdin, as shown above.
