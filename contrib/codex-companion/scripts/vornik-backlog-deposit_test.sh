#!/usr/bin/env bash
# Tests for vornik-backlog-deposit.sh.
#
# The two guards this script exists to preserve are the secret scan and dedup —
# they are what hand-editing a backlog loses. Most of what follows tests those,
# plus the one property that matters more than any feature: on a 5,000-line
# curated document, the script must only ever INSERT, never rewrite.
#
# Run: bash contrib/claude-code-companion/scripts/vornik-backlog-deposit_test.sh
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
SH="$HERE/vornik-backlog-deposit.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ok: $*"; }
bad() { fail=$((fail+1)); echo "  FAIL: $*"; }

fixture() {
  f="$TMP/$1.md"
  cat > "$f" <<'EOF'
# Backlog

Preamble prose.

---

## [ ] P1 — An existing open item (2026-08-01)

Some detail.

- [ ] **A nested sub-note.** Not a top-level item.

---

## [x] P2 — Something already done (2026-08-02)

Done detail.

---

## Trading — FROZEN

Trailing prose section, not an item.
EOF
  printf '%s' "$f"
}

run() { printf '%s' "${BODY:-body}" | bash "$SH" --file "$1" "${@:2}" 2>&1; }

# --- deposits land, and land in the items region --------------------------
echo "--- a new finding is deposited before the trailing prose section ---"
f="$(fixture basic)"
out="$(run "$f" --title "A brand new finding")"; rc=$?
if [ $rc -eq 0 ] && grep -q "A brand new finding" "$f"; then
  ok "item written"
else
  bad "rc=$rc out=$out"
fi
# It must NOT be buried inside the trailing prose section.
if [ "$(grep -n 'A brand new finding' "$f" | cut -d: -f1)" -lt "$(grep -n '^## Trading' "$f" | cut -d: -f1)" ]; then
  ok "inserted before the trailing non-item section"
else
  bad "the finding landed inside '## Trading — FROZEN'"
fi

# --- INSERT ONLY: existing content is byte-identical ----------------------
echo "--- existing content is never rewritten ---"
f="$(fixture insertonly)"
before="$(cat "$f")"
run "$f" --title "Another finding" >/dev/null
# Every original line must still be present, in order.
if printf '%s' "$before" | while IFS= read -r l; do grep -qxF -- "$l" "$f" || exit 1; done; then
  ok "every original line survived"
else
  bad "the script rewrote existing content"
fi

# --- dedup ----------------------------------------------------------------
echo "--- dedup against an existing OPEN item ---"
f="$(fixture dupopen)"
out="$(run "$f" --title "An existing open item")"; rc=$?
[ $rc -eq 3 ] && ok "refused as duplicate (rc=3)" || bad "rc=$rc, want 3 — out=$out"

echo "--- dedup ignores the priority tag and the trailing date ---"
f="$(fixture duptag)"
out="$(run "$f" --title "P1 — An existing open item (2026-08-01)")"; rc=$?
[ $rc -eq 3 ] && ok "normalisation stripped the tag and date" || bad "rc=$rc, want 3"

echo "--- dedup catches a DONE item too (do not re-file completed work) ---"
f="$(fixture dupdone)"
out="$(run "$f" --title "Something already done")"; rc=$?
[ $rc -eq 3 ] && ok "refused against a [x] item" || bad "rc=$rc, want 3"

echo "--- dedup also sees nested bullet items ---"
f="$(fixture dupbullet)"
out="$(run "$f" --title "A nested sub-note")"; rc=$?
[ $rc -eq 3 ] && ok "bullet items participate in dedup" || bad "rc=$rc, want 3"

echo "--- --allow-duplicate overrides ---"
f="$(fixture dupover)"
out="$(run "$f" --title "An existing open item" --allow-duplicate)"; rc=$?
[ $rc -eq 0 ] && ok "override deposits anyway" || bad "rc=$rc, want 0 — out=$out"

echo "--- a genuinely different title is NOT a duplicate ---"
f="$(fixture nodup)"
out="$(run "$f" --title "Totally unrelated subject matter here")"; rc=$?
[ $rc -eq 0 ] && ok "distinct titles pass" || bad "rc=$rc, want 0 — out=$out"

# --- secret scan ----------------------------------------------------------
echo "--- secret-shaped content is refused ---"
for probe in \
  "AIzaSyA1234567890abcdefghijklmnopqrstuvw" \
  "ghp_012345678901234567890123456789012345" \
  "xoxb-123456789012-abcdefghijkl" \
  "postgresql://user:hunter2@db.internal:5432/app" ; do
  f="$(fixture "secret")"
  out="$(BODY="see $probe for context" run "$f" --title "Finding with a credential")"; rc=$?
  if [ $rc -eq 4 ]; then ok "refused: ${probe:0:12}…"; else bad "rc=$rc for ${probe:0:12}… want 4"; fi
done

echo "--- a private key block in EVIDENCE is refused too ---"
f="$(fixture secretev)"
out="$(run "$f" --title "Finding" --evidence "-----BEGIN RSA PRIVATE KEY-----")"; rc=$?
[ $rc -eq 4 ] && ok "evidence field is scanned" || bad "rc=$rc, want 4"

echo "--- ordinary prose mentioning tokens is NOT refused ---"
f="$(fixture nofalsepos)"
out="$(BODY='The OAuth access token expired and the call returned 401.' run "$f" --title "Ordinary finding about tokens")"; rc=$?
[ $rc -eq 0 ] && ok "no false positive on prose about credentials" || bad "rc=$rc, want 0 — out=$out"

# --- structure is preserved (the reason this is client-side) --------------
echo "--- multi-line body with a table survives intact ---"
f="$(fixture structure)"
BODY='Measured:

| finding | value |
|---|---|
| rows | 2741 |

Second paragraph.'
printf '%s' "$BODY" | bash "$SH" --file "$f" --title "A finding with structure" >/dev/null 2>&1
if grep -q '^| rows | 2741 |$' "$f" && grep -q '^Second paragraph.$' "$f"; then
  ok "table and paragraphs preserved (the daemon path would flatten these)"
else
  bad "structure was lost"
fi

# --- validation -----------------------------------------------------------
echo "--- argument validation ---"
f="$(fixture validate)"
out="$(run "$f" --title "x" --kind nonsense)"; rc=$?
[ $rc -eq 2 ] && ok "bad --kind rejected" || bad "rc=$rc, want 2"
out="$(run "$f" --title "x" --priority P9)"; rc=$?
[ $rc -eq 2 ] && ok "bad --priority rejected" || bad "rc=$rc, want 2"
out="$(printf 'b' | bash "$SH" --file "$f" 2>&1)"; rc=$?
[ $rc -eq 2 ] && ok "missing --title rejected" || bad "rc=$rc, want 2"
out="$(printf 'b' | bash "$SH" --file "$TMP/nope.md" --title "x" 2>&1)"; rc=$?
[ $rc -eq 5 ] && ok "missing backlog file reported (rc=5)" || bad "rc=$rc, want 5"

echo "--- --dry-run touches nothing ---"
f="$(fixture dryrun)"
sum_before="$(cksum < "$f")"
run "$f" --title "Would be deposited" --dry-run >/dev/null
[ "$(cksum < "$f")" = "$sum_before" ] && ok "file unchanged" || bad "dry-run modified the file"

echo "---"
echo "PASS: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
