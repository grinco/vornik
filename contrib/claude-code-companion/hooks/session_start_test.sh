#!/usr/bin/env bash
# Test harness for session-start.sh (LLD 2026-07-12-companion-rag-first-
# guidance §5). Runs the hook with the daemon unreachable (curls fail
# silently, as they do in a headless/offline session) and asserts the emitted
# additionalContext still carries the static RAG-first directive — the whole
# point of shipping it as a default is that it works without the network.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOOK="$HERE/session-start.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }

# --- Case 1: token set, daemon unreachable, repo scope pinned. The hook must
# emit a JSON envelope whose additionalContext contains the RAG-first section
# (silent-skip on the tools/list + recent_memory curls; the static directive
# is always present).
OUT="$(
  VORNIK_COMPANION_TOKEN="test-token" \
  VORNIK_URL="http://127.0.0.1:1" \
  VORNIK_REPO_SCOPE="github.com/grinco/vornik" \
  bash "$HOOK" 2>/dev/null
)"

[[ -n "$OUT" ]] || fail "hook emitted nothing with a token set"

# additionalContext must be present and carry the RAG-first heading, the
# recall-first rule, the trigger list, and the worked example.
CTX="$(printf '%s' "$OUT" | jq -r '.hookSpecificOutput.additionalContext')"
[[ -n "$CTX" && "$CTX" != "null" ]] || fail "no additionalContext in envelope: $OUT"

grep -q "vornik-companion: RAG-first" <<<"$CTX" || fail "RAG-first heading missing from additionalContext"
grep -q "recall\` BEFORE you start reading code" <<<"$CTX" || fail "recall-before-code rule missing"
grep -q "Trigger list" <<<"$CTX" || fail "trigger list missing"
grep -q "Worked example" <<<"$CTX" || fail "worked example missing"
grep -q "authoritative design record" <<<"$CTX" || fail "authoritative-trust contract missing"
# The repo-scope directive is a standing directive, planted on every path
# (LLD 2026-07-12 §9).
grep -q "vornik-companion: repo scope auto-detected" <<<"$CTX" || fail "repo-scope directive missing from the normal path"
grep -q 'repo_scope: "github.com/grinco/vornik"' <<<"$CTX" || fail "repo-scope directive does not name the pinned scope"

# The envelope must be valid JSON with the SessionStart hook event.
EVENT="$(printf '%s' "$OUT" | jq -r '.hookSpecificOutput.hookEventName')"
[[ "$EVENT" == "SessionStart" ]] || fail "hookEventName != SessionStart: $EVENT"

# --- Case 2: no token → the hook exits 0 and emits nothing (unconfigured
# plugin degrades gracefully; unchanged behaviour).
OUT2="$(VORNIK_COMPANION_TOKEN="" bash "$HOOK" 2>/dev/null || true)"
[[ -z "$OUT2" ]] || fail "hook must emit nothing without a token, got: $OUT2"

# --- Case 3: --refresh (the `compact` matcher, plugin 0.19.0). The digest is
# skipped; EVERY standing directive is re-planted. Regression for 0.19.0, which
# re-planted RAG-first and skill capture and dropped repo scope because that
# block was nested inside build_digest() (LLD 2026-07-12 §9).
OUT3="$(
  VORNIK_COMPANION_TOKEN="test-token" \
  VORNIK_URL="http://127.0.0.1:1" \
  VORNIK_REPO_SCOPE="github.com/grinco/vornik" \
  bash "$HOOK" --refresh 2>/dev/null
)"
[[ -n "$OUT3" ]] || fail "refresh emitted nothing"
CTX3="$(printf '%s' "$OUT3" | jq -r '.hookSpecificOutput.additionalContext')"
[[ -n "$CTX3" && "$CTX3" != "null" ]] || fail "no additionalContext in refresh envelope: $OUT3"
grep -q "context refreshed after compaction" <<<"$CTX3" || fail "refresh banner missing"
grep -q "vornik-companion: repo scope auto-detected" <<<"$CTX3" || fail "refresh dropped the repo-scope directive"
grep -q 'repo_scope: "github.com/grinco/vornik"' <<<"$CTX3" || fail "refresh repo-scope directive does not name the pinned scope"
grep -q "vornik-companion: RAG-first" <<<"$CTX3" || fail "refresh dropped the RAG-first directive"
grep -q "capture reusable know-how as skills" <<<"$CTX3" || fail "refresh dropped the skill-capture directive"
# The delegation digest is deliberately NOT reprinted on a refresh.
if grep -q "delegation(s) finished since your last session" <<<"$CTX3"; then
  fail "refresh reprinted the delegation digest"
fi

echo "PASS: session-start.sh emits every standing directive on both paths, skips the digest on --refresh, and degrades cleanly without a token"
