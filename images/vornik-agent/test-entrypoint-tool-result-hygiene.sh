#!/usr/bin/env bash
# Regression guard for agent tool-result hygiene: compact only old large tool
# bodies, preserve OpenAI tool_call_id pairing, keep UTF-8 valid, and allow
# explicit capped rehydration from .tool_results/.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$here/entrypoint.sh"
tmp="$(mktemp -d)"
cleanup() {
  rm -rf "$tmp"
}
trap cleanup EXIT

export WORKSPACE="$tmp"
export INPUT_FILE="$tmp/task.json"
export OUTPUT_FILE="$tmp/result.json"
export VORNIK_TOOL_RESULT_HYGIENE=1
export VORNIK_TOOL_RAW_COMPACT_CHARS=20
export VORNIK_TOOL_RAW_KEEP_RECENT_BATCHES=1
export VORNIK_TOOL_RAW_HEAD_CHARS=0
export VORNIK_TOOL_RAW_TAIL_CHARS=0
export VORNIK_TOOL_RESULT_MAX_BYTES=48

printf '%s\n' '{"config":{"permissions":{"allowedTools":["file_read"]}}}' > "$INPUT_FILE"

# shellcheck source=images/vornik-agent/entrypoint.sh
source "$ep"

msgs="$tmp/.messages.json"
original_msgs="$tmp/original.messages.json"
mkdir -p "$tmp/.tool_results"
old1_content="$(printf 'αβγδεζηθικλμνξοπρστυφχψω%.0s' {1..22}) middle keep this raw"
old2_content="$(printf 'second-large-old-result-abcdefghijklmnopqrstuvwxyz%.0s' {1..12})"
recent_content="$(printf 'recent-large-result-abcdefghijklmnopqrstuvwxyz%.0s' {1..12})"
printf '%s' "$old1_content" > "$tmp/.tool_results/old1.txt"
printf '%s' "$old2_content" > "$tmp/.tool_results/old2.txt"
printf '%s' "$recent_content" > "$tmp/.tool_results/recent1.txt"

jq -n \
  --arg old1 "$old1_content" \
  --arg old2 "$old2_content" \
  --arg recent "$recent_content" \
  '[
    {"role":"system","content":"sys"},
    {"role":"user","content":"work"},
    {"role":"assistant","content":null,"tool_calls":[{"id":"old1","type":"function","function":{"name":"file_read","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"old1","content":$old1},
    {"role":"assistant","content":null,"tool_calls":[{"id":"old2","type":"function","function":{"name":"mcp__x__fetch","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"old2","content":$old2},
    {"role":"assistant","content":null,"tool_calls":[{"id":"recent1","type":"function","function":{"name":"run_shell","arguments":"{}"}},{"id":"recent2","type":"function","function":{"name":"file_read","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"recent1","content":$recent},
    {"role":"tool","tool_call_id":"recent2","content":"small"}
  ]' > "$msgs"
cp "$msgs" "$original_msgs"

before_hash="$(sha256sum "$tmp/.tool_results/old1.txt" | awk '{print $1}')"
compact_old_tool_results "$msgs"
after_hash="$(sha256sum "$tmp/.tool_results/old1.txt" | awk '{print $1}')"
[ "$before_hash" = "$after_hash" ]

deterministic_msgs="$tmp/deterministic.messages.json"
cp "$original_msgs" "$deterministic_msgs"
compact_old_tool_results "$deterministic_msgs"
cmp -s "$msgs" "$deterministic_msgs" || {
  echo "FAIL: identical message history did not compact deterministically" >&2
  exit 1
}

jq -e 'length == 9' "$msgs" >/dev/null
jq -e '[.[] | select(.role=="assistant" and has("tool_calls"))] | length == 3' "$msgs" >/dev/null
jq -e '
  [.[]
   | select(.role=="assistant" and has("tool_calls"))
   | .tool_calls[].id] as $ids
  | [.[]
     | select(.role=="tool")
     | .tool_call_id] as $tools
  | (($ids | length) == ($tools | length))
    and all($ids[]; . as $id | ([$tools[] | select(. == $id)] | length) == 1)
' "$msgs" >/dev/null

jq -e '.[3].content | startswith("[tool result compacted: tool_call_id=old1")' "$msgs" >/dev/null
jq -e '.[3].content | contains("αβγ") and contains("φχψω")' "$msgs" >/dev/null
jq -e '.[5].content | startswith("[tool result compacted: tool_call_id=old2")' "$msgs" >/dev/null
jq -e --arg recent "$recent_content" '.[7].content == $recent' "$msgs" >/dev/null
jq -e '.[8].content == "small"' "$msgs" >/dev/null

keep_two_msgs="$tmp/keep-two.json"
cp "$original_msgs" "$keep_two_msgs"
VORNIK_TOOL_RAW_KEEP_RECENT_BATCHES=2
TOOL_RAW_KEEP_RECENT_BATCHES=2
compact_old_tool_results "$keep_two_msgs"
jq -e '.[3].content | startswith("[tool result compacted: tool_call_id=old1")' "$keep_two_msgs" >/dev/null
jq -e --arg old2 "$old2_content" '.[5].content == $old2' "$keep_two_msgs" >/dev/null
jq -e --arg recent "$recent_content" '.[7].content == $recent' "$keep_two_msgs" >/dev/null
VORNIK_TOOL_RAW_KEEP_RECENT_BATCHES=1
TOOL_RAW_KEEP_RECENT_BATCHES=1

keep_clamp_msgs="$tmp/keep-clamp.json"
cp "$original_msgs" "$keep_clamp_msgs"
VORNIK_TOOL_RAW_KEEP_RECENT_BATCHES=0
TOOL_RAW_KEEP_RECENT_BATCHES=0
compact_old_tool_results "$keep_clamp_msgs"
jq -e '.[3].content | startswith("[tool result compacted: tool_call_id=old1")' "$keep_clamp_msgs" >/dev/null
jq -e '.[5].content | startswith("[tool result compacted: tool_call_id=old2")' "$keep_clamp_msgs" >/dev/null
jq -e --arg recent "$recent_content" '.[7].content == $recent' "$keep_clamp_msgs" >/dev/null
VORNIK_TOOL_RAW_KEEP_RECENT_BATCHES=1
TOOL_RAW_KEEP_RECENT_BATCHES=1

rehydrated="$(handle_tool_result_read '{"tool_call_id":"old1"}')"
case "$rehydrated" in
  αβγ*) ;;
  *) echo "FAIL: rehydrated body did not start with raw content" >&2; exit 1 ;;
esac
case "$rehydrated" in
  *"[... truncated at 48 bytes by tool_result_read"*) ;;
  *) echo "FAIL: rehydrated body did not include truncation marker" >&2; exit 1 ;;
esac
direct_read="$(exec_tool file_read '{"path":".tool_results/old1.txt"}')"
case "$direct_read" in
  "ERROR: .tool_results is only readable through tool_result_read") ;;
  *) echo "FAIL: direct file_read of .tool_results was not blocked" >&2; exit 1 ;;
esac

threshold_msgs="$tmp/threshold.json"
jq -n \
  '[
    {"role":"assistant","content":null,"tool_calls":[{"id":"edge","type":"function","function":{"name":"file_read","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"edge","content":"12345678901234567890"},
    {"role":"assistant","content":null,"tool_calls":[{"id":"new","type":"function","function":{"name":"file_read","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"new","content":"new"}
  ]' > "$threshold_msgs"
compact_old_tool_results "$threshold_msgs"
jq -e '.[1].content == "12345678901234567890"' "$threshold_msgs" >/dev/null

head_tail_noop_msgs="$tmp/head-tail-noop.json"
jq -n \
  '[
    {"role":"assistant","content":null,"tool_calls":[{"id":"wide_preview","type":"function","function":{"name":"file_read","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"wide_preview","content":"abcdefghijklmnopqrstuvwxyz12345"},
    {"role":"assistant","content":null,"tool_calls":[{"id":"new","type":"function","function":{"name":"file_read","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"new","content":"new"}
  ]' > "$head_tail_noop_msgs"
VORNIK_TOOL_RAW_HEAD_CHARS=20
VORNIK_TOOL_RAW_TAIL_CHARS=20
TOOL_RAW_HEAD_CHARS=20
TOOL_RAW_TAIL_CHARS=20
compact_old_tool_results "$head_tail_noop_msgs"
jq -e '.[1].content == "abcdefghijklmnopqrstuvwxyz12345"' "$head_tail_noop_msgs" >/dev/null
VORNIK_TOOL_RAW_HEAD_CHARS=0
VORNIK_TOOL_RAW_TAIL_CHARS=0
TOOL_RAW_HEAD_CHARS=0
TOOL_RAW_TAIL_CHARS=0

unsafe_id_msgs="$tmp/unsafe-id.json"
jq -n \
  --arg old2 "$old2_content" \
  '[
    {"role":"assistant","content":null,"tool_calls":[{"id":"../unsafe","type":"function","function":{"name":"file_read","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"../unsafe","content":$old2},
    {"role":"assistant","content":null,"tool_calls":[{"id":"new","type":"function","function":{"name":"file_read","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"new","content":"new"}
  ]' > "$unsafe_id_msgs"
compact_old_tool_results "$unsafe_id_msgs"
jq -e --arg old2 "$old2_content" '.[1].content == $old2' "$unsafe_id_msgs" >/dev/null

cp "$original_msgs" "$tmp/disabled.json"
VORNIK_TOOL_RESULT_HYGIENE=0
TOOL_RESULT_HYGIENE=0
compact_old_tool_results "$tmp/disabled.json"
cmp -s "$original_msgs" "$tmp/disabled.json" || {
  echo "FAIL: disabled hygiene mutated message history" >&2
  exit 1
}
VORNIK_TOOL_RESULT_HYGIENE=1
TOOL_RESULT_HYGIENE=1

cp "$original_msgs" "$tmp/zero-threshold.json"
VORNIK_TOOL_RAW_COMPACT_CHARS=0
TOOL_RAW_COMPACT_CHARS=0
compact_old_tool_results "$tmp/zero-threshold.json"
cmp -s "$original_msgs" "$tmp/zero-threshold.json" || {
  echo "FAIL: zero compaction threshold mutated message history" >&2
  exit 1
}
VORNIK_TOOL_RAW_COMPACT_CHARS=20
TOOL_RAW_COMPACT_CHARS=20

cp "$original_msgs" "$tmp/invalid-threshold.json"
VORNIK_TOOL_RAW_COMPACT_CHARS=not-a-number
TOOL_RAW_COMPACT_CHARS=not-a-number
compact_old_tool_results "$tmp/invalid-threshold.json"
jq -e --arg old1 "$old1_content" '.[3].content == $old1' "$tmp/invalid-threshold.json" >/dev/null

VORNIK_TOOL_RAW_COMPACT_CHARS=20
TOOL_RAW_COMPACT_CHARS=20

cp "$original_msgs" "$tmp/negative-threshold.json"
VORNIK_TOOL_RAW_COMPACT_CHARS=-1
TOOL_RAW_COMPACT_CHARS=-1
compact_old_tool_results "$tmp/negative-threshold.json"
cmp -s "$original_msgs" "$tmp/negative-threshold.json" || {
  echo "FAIL: negative compaction threshold mutated message history" >&2
  exit 1
}
VORNIK_TOOL_RAW_COMPACT_CHARS=20
TOOL_RAW_COMPACT_CHARS=20

echo "OK: tool-result hygiene preserves transcript structure and rehydrates raw results"
