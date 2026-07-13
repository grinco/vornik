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

# The envelope must be valid JSON with the SessionStart hook event.
EVENT="$(printf '%s' "$OUT" | jq -r '.hookSpecificOutput.hookEventName')"
[[ "$EVENT" == "SessionStart" ]] || fail "hookEventName != SessionStart: $EVENT"

# --- Case 2: no token → the hook exits 0 and emits nothing (unconfigured
# plugin degrades gracefully; unchanged behaviour).
OUT2="$(VORNIK_COMPANION_TOKEN="" bash "$HOOK" 2>/dev/null || true)"
[[ -z "$OUT2" ]] || fail "hook must emit nothing without a token, got: $OUT2"

echo "PASS: session-start.sh emits the RAG-first directive (offline) and degrades cleanly without a token"
