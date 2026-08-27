#!/usr/bin/env bash
# Regression guard: the audit truncation must not destroy JSON structure.
#
# 2026-08-26. tool_output was cut with `printf '%.4096s'`, a blind slice. For a
# JSON result that yields INVALID JSON, and internal/verifier's
# classifyAuditEntry parses the row to read the scraper's status/final_url/
# block_reason convention — so the unmarshal failed, the marker scan found
# nothing anchored, and the entry returned (zero,false), contributing NOTHING
# to the denominator. A successful fetch became invisible rather than counted.
# Measured: 2,651 of 4,481 production web_fetch rows unparseable.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$here/entrypoint.sh"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  ok: $*"; }
bad() { fail=$((fail+1)); echo "  FAIL: $*"; }

# shellcheck disable=SC1090
eval "$(sed -n '/^AUDIT_OUTPUT_BUDGET=/,/^}/p' "$ep")"

big() { python3 -c "import sys;sys.stdout.write('x'*int(sys.argv[1]))" "$1"; }

echo "--- an oversized scraper envelope stays valid JSON, keys intact ---"
body="{\"status\":200,\"final_url\":\"https://example.com/a\",\"content\":\"$(big 8000)\",\"block_reason\":\"\"}"
out="$(truncate_tool_output_for_audit "$body")"
if printf '%s' "$out" | jq -e . >/dev/null 2>&1; then ok "output is valid JSON"; else bad "output is not valid JSON: ${out:0:80}"; fi
if [ "$(printf '%s' "$out" | jq -r '.status' 2>/dev/null)" = "200" ]; then ok "status survived"; else bad "status lost"; fi
if [ "$(printf '%s' "$out" | jq -r '.final_url' 2>/dev/null)" = "https://example.com/a" ]; then ok "final_url survived"; else bad "final_url lost"; fi
if [ "$(printf '%s' "$out" | jq -r 'has("block_reason")' 2>/dev/null)" = "true" ]; then ok "block_reason survived"; else bad "block_reason lost"; fi
echo "--- it respects the budget (headroom below the daemon's 4096 cap) ---"
if [ "${#out}" -le "${AUDIT_OUTPUT_BUDGET:-3900}" ]; then
  ok "within budget (${#out} <= ${AUDIT_OUTPUT_BUDGET:-3900})"
else
  bad "over budget: ${#out}"
fi
if [ "${#out}" -le 4096 ]; then
  ok "cannot trip the daemon's blind re-cap"
else
  bad "would be re-cut blind daemon-side, undoing the fix"
fi

echo "--- the LARGEST field is shortened, whatever it is named ---"
body="{\"status\":200,\"final_url\":\"https://example.com/b\",\"payload\":\"$(big 8000)\"}"
out="$(truncate_tool_output_for_audit "$body")"
if printf '%s' "$out" | jq -e '.status == 200 and .final_url == "https://example.com/b"' >/dev/null 2>&1; then
  ok "an unfamiliar large field (payload) is shortened, small fields kept"
else
  bad "schema-agnostic truncation failed: ${out:0:100}"
fi

echo "--- a small result is untouched ---"
body='{"status":200,"final_url":"https://example.com/c","content":"short"}'
if [ "$(truncate_tool_output_for_audit "$body")" = "$body" ]; then ok "byte-identical under budget"; else bad "small result was modified"; fi
echo "--- non-JSON keeps the blind cut and stays within budget ---"
body="$(big 9000)"
out="$(truncate_tool_output_for_audit "$body")"
if [ "${#out}" -le "${AUDIT_OUTPUT_BUDGET:-3900}" ]; then ok "plain text cut to budget (${#out})"; else bad "plain text over budget: ${#out}"; fi
echo "--- a JSON ARRAY (not an object) keeps the blind cut, no crash ---"
body="[\"$(big 8000)\"]"
out="$(truncate_tool_output_for_audit "$body")"
if [ -n "$out" ] && [ "${#out}" -le "${AUDIT_OUTPUT_BUDGET:-3900}" ]; then ok "array handled"; else bad "array mishandled"; fi
echo "--- an object with NO string field does not crash ---"
body="{\"a\":1,\"b\":$(python3 -c "print('['+','.join(['1']*4000)+']')")}"
out="$(truncate_tool_output_for_audit "$body")"
if [ -n "$out" ] && [ "${#out}" -le "${AUDIT_OUTPUT_BUDGET:-3900}" ]; then ok "no-string object handled"; else bad "no-string object mishandled"; fi
echo "---"
echo "PASS: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
