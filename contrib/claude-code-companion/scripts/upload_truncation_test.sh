#!/usr/bin/env bash
# A backtick in a slash-command argument must not be able to truncate the
# command's shell block.
#
# 2026-09-15: a backtick anywhere in the /upload prompt silently aborted the
# command — no upload, no delegation, and an output that still ended with the
# command file's own "What just happened" instructions, so it read as a
# success. An agent skimming for the happy path reported a delegation that
# never occurred (task_20260915043346_95a4571c078bdde4 succeeded only after the
# backticks were removed from the same prompt).
#
# MEASURED 2026-09-17 by invoking the real command with a backtick-bearing
# prompt: the harness substitutes $ARGUMENTS first, then scans for the inline
# !`...` placeholder, so the FIRST backtick in the substituted text closes the
# block. bash reported "here-document ... delimited by end-of-file" and the
# whole remainder of the file — the rest of the script AND the trailing
# instructions — was emitted as literal text. That is the entire disguise,
# confirmed end to end rather than inferred.
#
# THE FIX is the documented FENCED form (```! … ```), delimited by a closing
# fence LINE instead of by a backtick, so a backtick in an argument is ordinary
# text. Every command that interpolates $ARGUMENTS into a shell block uses it.
#
# The VORNIK_UPLOAD_END marker is kept as the second line of defence: a line
# consisting of three backticks in an argument would still close a fence, and
# that residual must not be able to look like a success either.
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cmd="$here/../commands/upload.md"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1" >&2; fails=$((fails+1)); }

# --- THE CAUSE: no command may interpolate $ARGUMENTS into an inline block.
for f in "$here"/../commands/*.md; do
  grep -q 'ARGUMENTS' "$f" || continue
  grep -q '^!`' "$f" || continue
  fail "$(basename "$f") still uses the inline block — a backtick in an argument truncates it"
done
if [ "$fails" -eq 0 ]; then
  pass "no command interpolates arguments into an inline block"
fi
for f in "$here"/../commands/*.md; do
  grep -q 'ARGUMENTS' "$f" && grep -q '^```!' "$f" \
    && pass "$(basename "$f") uses the fenced form"
done

# --- THE DISGUISE: /upload's markers. Extract the fenced block and run it.
awk '/^```!$/{f=1; next} f&&/^```$/{exit} f{print}' "$cmd" > "$tmp/block.sh"
[ -s "$tmp/block.sh" ] || { echo "FAIL - could not extract the fenced block" >&2; exit 1; }

grep -q 'VORNIK_UPLOAD_BEGIN' "$tmp/block.sh" || fail "block does not print a begin marker"
grep -q 'VORNIK_UPLOAD_END' "$tmp/block.sh"   || fail "block does not print an end marker"

# The end marker must be the LAST thing the block does, or it can print while
# the work is still unfinished.
last="$(grep -vE '^[[:space:]]*$' "$tmp/block.sh" | tail -1)"
case "$last" in
  *VORNIK_UPLOAD_END*) pass "the end marker is the block's last statement" ;;
  *) fail "the end marker is not last (last line: $last)" ;;
esac

# A complete run prints BOTH markers even when the work itself fails: END means
# "ran to completion", NOT "succeeded", so the error: guard keeps its job.
# shellcheck disable=SC2016  # $ARGUMENTS is the LITERAL placeholder the harness
# substitutes; expanding it here is exactly what must not happen.
sed 's/\$ARGUMENTS/some-workflow "a plain prompt" \/dev\/null/' "$tmp/block.sh" > "$tmp/clean.sh"
out="$(env -u VORNIK_URL -u VORNIK_COMPANION_TOKEN bash "$tmp/clean.sh" 2>&1)"
case "$out" in
  *VORNIK_UPLOAD_BEGIN*) pass "complete run prints the begin marker" ;;
  *) fail "complete run printed no begin marker: $out" ;;
esac
case "$out" in
  *VORNIK_UPLOAD_END*) pass "complete run prints the end marker" ;;
  *) fail "complete run printed no end marker: $out" ;;
esac
case "$out" in
  *error:*) pass "a failing run still reports error: (the existing guard)" ;;
  *) fail "expected an error: line with no credentials set: $out" ;;
esac

# --- a backtick in the prompt must no longer cut anything: under the fenced
# rule it is ordinary text, so the whole script survives.
# shellcheck disable=SC2016  # literal placeholder, as above.
sed 's/\$ARGUMENTS/some-workflow "review the `file_write` tool" \/dev\/null/' "$tmp/block.sh" > "$tmp/tick.sh"
out="$(env -u VORNIK_URL -u VORNIK_COMPANION_TOKEN bash "$tmp/tick.sh" 2>&1)"
case "$out" in
  *VORNIK_UPLOAD_END*) pass "a backtick in the prompt no longer truncates the block" ;;
  *) fail "a backtick still truncates the block: $out" ;;
esac

# --- the residual: a line of three backticks closes the fence early. The
# marker must still catch it, which is the whole reason it is kept.
head -3 "$tmp/block.sh" > "$tmp/cut.sh"
out="$(env -u VORNIK_URL -u VORNIK_COMPANION_TOKEN bash "$tmp/cut.sh" 2>&1)"
case "$out" in
  *VORNIK_UPLOAD_BEGIN*) pass "fence-cut run still prints the begin marker" ;;
  *) fail "fence-cut run printed no begin marker: $out" ;;
esac
case "$out" in
  *VORNIK_UPLOAD_END*) fail "fence-cut run printed the END marker — it cannot detect a cut: $out" ;;
  *) pass "fence-cut run does NOT print the end marker" ;;
esac
case "$out" in
  *task_id:*) fail "fence-cut run printed a task_id line: $out" ;;
  *) pass "fence-cut run prints no task_id" ;;
esac

# --- the command file must TELL the model the marker is load-bearing, or the
# marker is only a fact nobody reads.
grep -q 'VORNIK_UPLOAD_END' "$cmd" || fail "the command file never mentions the marker to the model"
if grep -q 'check the output ends with' "$cmd"; then
  pass "the command file instructs the model to check the marker"
else
  fail "the command file does not tell the model to check the marker"
fi

echo
[ "$fails" -eq 0 ] || { echo "$fails case(s) failed" >&2; exit 1; }
echo "all cases passed"
