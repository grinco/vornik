#!/usr/bin/env bash
# Regression guard: an unknown tool name must be refused with a refusal the
# model can ACT on.
#
# 2026-08-26. Measured across 29,056 audited production tool calls: 9.1% named
# a tool that does not exist — 23.4% on companion-example, touching 33% of its
# executions. The names are other harnesses' vocabularies (`grep` 733, `glob`
# 696, `read_many_files` 51) plus malformed shapes where an argument name or a
# chat-template token is welded onto a real tool (`file_writepath` x11,
# `file_write<tool_call>path`, `write_file`).
#
# The refusal was a bare `ERROR: unknown tool: glob`. No catalogue, no nearest
# match. So the model had nothing to correct with: it retried the same name or
# invented another until the degenerate-loop detector killed the step. Traced
# end to end on exec_20260826204813_85ea8684b6dd614c — `file_writepath` called
# four times with identical args, 18 iterations, dead on a schema violation.
#
# The fix is in the REFUSAL, not the catalogue. Adding `glob` as a real tool
# would be learning the wrong lesson.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$here/entrypoint.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ok: $*"; }
bad() { fail=$((fail+1)); echo "  FAIL: $*"; }

# Source just the refusal helpers out of the entrypoint. The script is a long
# program with a main at the bottom; the functions under test are pure.
# shellcheck disable=SC1090
eval "$(sed -n '/^unknown_tool_refusal()/,/^}/p' "$ep")"
eval "$(sed -n '/^canonical_tool_guess()/,/^}/p' "$ep")"

export WORKSPACE="$tmp"
printf '%s\n' '[{"function":{"name":"file_read"}},{"function":{"name":"file_write"}},{"function":{"name":"run_shell"}},{"function":{"name":"mcp__scraper__web_fetch"}}]' > "$tmp/.tools.json"
export TOOLS_FILE="$tmp/.tools.json"

echo "--- the refusal names what IS available ---"
out="$(unknown_tool_refusal "glob")"
if printf '%s' "$out" | grep -q "file_read" && printf '%s' "$out" | grep -q "run_shell"; then
  ok "available tools are listed"
else
  bad "refusal does not list the catalogue: $out"
fi
if printf '%s' "$out" | grep -qi "unknown tool"; then
  ok "still says it was unknown"
else
  bad "refusal lost the 'unknown tool' phrasing: $out"
fi

echo "--- a FUSED name resolves to its canonical prefix ---"
for probe in "file_writepath:file_write" \
             "file_read(path:file_read" \
             'file_write<tool_call>path:file_write' \
             'file_write</think>allowed_paths:file_write' \
             "write_file:file_write"; do
  n="${probe%%:*}"; want="${probe##*:}"
  got="$(canonical_tool_guess "$n")"
  if [ "$got" = "$want" ]; then ok "$n -> $want"; else bad "$n -> '$got', want '$want'"; fi
done

echo "--- a genuinely unknown name gets no bogus guess ---"
for n in glob grep read_many_files tool_search; do
  got="$(canonical_tool_guess "$n")"
  if [ -z "$got" ]; then ok "$n has no canonical guess"; else bad "$n wrongly guessed as '$got'"; fi
done

echo "--- the fused refusal SAYS what it thinks was meant ---"
out="$(unknown_tool_refusal "file_writepath")"
if printf '%s' "$out" | grep -q "file_write"; then
  ok "names the likely intended tool"
else
  bad "no correction hint for a fused name: $out"
fi

echo "--- refusal is one line (it lands in the model's context) ---"
out="$(unknown_tool_refusal "glob")"
lines=$(printf '%s' "$out" | wc -l)
if [ "$lines" -le 1 ]; then ok "single line"; else bad "refusal is $lines lines; it rides in every retry"; fi


# --- STRUCTURAL: the guess must never reach permit logic --------------------
# Retrospective review review-20260826-6fc9, finding F2 — the one thing that
# should have blocked this from shipping as written.
#
# canonical_tool_guess is safe because it runs AFTER the gate has already
# refused, and feeds only the refusal message. But that was asserted in prose
# and enforced by nothing: a later refactor moving it into a shared helper could
# silently migrate its output into the permit decision, turning a HINT into an
# AUTHORISATION.
#
# This project has already had that exact bug. The 2026.8.1 allowlist bypass let
# four agent tools past per-role allowlists because the gate asked "is this a
# builtin?" and failed open on absence. A guess that can influence a gate is the
# same shape.
#
# So the separation is now a test, not a claim.
echo "--- canonical_tool_guess is structurally isolated from the gate ---"
for gatefn in tool_call_permitted is_builtin_tool allowed_builtin_tools_json; do
  body="$(sed -n "/^${gatefn}()/,/^}/p" "$ep")"
  if [ -z "$body" ]; then
    bad "gate function ${gatefn} not found — the isolation test is looking at nothing"
    continue
  fi
  if printf '%s' "$body" | grep -q 'canonical_tool_guess'; then
    bad "${gatefn} references canonical_tool_guess — a refusal HINT must never inform an AUTHORISATION decision (see 2026.8.1 allowlist bypass)"
  else
    ok "${gatefn} does not reference the guess"
  fi
done

# And the converse: the guess must not be what decides dispatch. exec_tool may
# call it only on the already-refused path.
echo "--- the guess is reachable only after a refusal ---"
exec_body="$(sed -n '/^exec_tool()/,/^}/p' "$ep")"
if printf '%s' "$exec_body" | grep -q 'unknown_tool_refusal'; then
  ok "exec_tool reaches the guess only via unknown_tool_refusal"
else
  bad "exec_tool no longer routes through unknown_tool_refusal — re-check where the guess is used"
fi

echo "---"
echo "PASS: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
