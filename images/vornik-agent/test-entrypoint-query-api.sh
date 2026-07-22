#!/usr/bin/env bash
# Regression guard for the agent-side query_api / list_apis builtins
# (LLD 2026-07-21-query-api-task-agents-design §3). Asserts:
#   - both tools appear in the LLM tool list ONLY when the daemon API is
#     reachable (VORNIK_API_URL set) AND the role's allowedTools grant them;
#   - the handlers POST/GET to the correct project-scoped daemon endpoint with
#     the minted-key auth header + X-Execution-ID, and the query_api request
#     body carries provider/method/path/query/body (credentials never sent);
#   - a {"refusal":...} HTTP-200 body surfaces the refusal text; a non-200
#     surfaces a short error.
# Fully deterministic: curl is stubbed, no real network.
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
export VORNIK_API_URL="http://api.local"
export VORNIK_API_KEY="test-key"
export VORNIK_EXECUTION_ID="env-exec-fallback"

# Task with the two API tools granted + a project id + an execution id.
printf '%s\n' '{"config":{"permissions":{"allowedTools":["file_read","query_api","list_apis"]}},"projectId":"proj-123","workflow":{"executionId":"exec-abc"}}' > "$INPUT_FILE"

# shellcheck source=images/vornik-agent/entrypoint.sh
source "$ep"

# --- 1. Tool exposure: granted + API reachable -> both present -----------------
tools="$(tool_definitions)"
printf '%s' "$tools" | jq -e 'map(.function.name) | index("query_api") != null' >/dev/null \
  || { echo "FAIL: query_api missing when granted + VORNIK_API_URL set" >&2; exit 1; }
printf '%s' "$tools" | jq -e 'map(.function.name) | index("list_apis") != null' >/dev/null \
  || { echo "FAIL: list_apis missing when granted + VORNIK_API_URL set" >&2; exit 1; }
# Schema sanity: query_api requires provider + path; secrets warning present.
printf '%s' "$tools" | jq -e '
  map(select(.function.name=="query_api"))[0].function.parameters.required
  | (index("provider") != null) and (index("path") != null)' >/dev/null \
  || { echo "FAIL: query_api schema missing required provider/path" >&2; exit 1; }

# --- 2. Tool exposure gated by allowedTools -----------------------------------
printf '%s\n' '{"config":{"permissions":{"allowedTools":["file_read"]}},"projectId":"proj-123"}' > "$INPUT_FILE"
tools_ungranted="$(tool_definitions)"
printf '%s' "$tools_ungranted" | jq -e 'map(.function.name) | index("query_api") == null' >/dev/null \
  || { echo "FAIL: query_api leaked without an allowedTools grant" >&2; exit 1; }
printf '%s' "$tools_ungranted" | jq -e 'map(.function.name) | index("list_apis") == null' >/dev/null \
  || { echo "FAIL: list_apis leaked without an allowedTools grant" >&2; exit 1; }

# --- 3. Tool exposure gated by VORNIK_API_URL ---------------------------------
printf '%s\n' '{"config":{"permissions":{"allowedTools":["file_read","query_api","list_apis"]}},"projectId":"proj-123","workflow":{"executionId":"exec-abc"}}' > "$INPUT_FILE"
saved_api_url="$VORNIK_API_URL"
unset VORNIK_API_URL
tools_noapi="$(tool_definitions)"
printf '%s' "$tools_noapi" | jq -e 'map(.function.name) | index("query_api") == null' >/dev/null \
  || { echo "FAIL: query_api exposed with VORNIK_API_URL unset" >&2; exit 1; }
printf '%s' "$tools_noapi" | jq -e 'map(.function.name) | index("list_apis") == null' >/dev/null \
  || { echo "FAIL: list_apis exposed with VORNIK_API_URL unset" >&2; exit 1; }
export VORNIK_API_URL="$saved_api_url"

# --- curl stub: record args + stdin, emit "<body>\n<code>" --------------------
CURL_ARGS_FILE="$tmp/curl_args"
CURL_STDIN_FILE="$tmp/curl_stdin"
CURL_RESPONSE='{"body":"ok","provenance":"third_party"}'
CURL_CODE='200'
curl() {
  local args=("$@") reads_stdin=0 a
  printf '%s\n' "${args[@]}" > "$CURL_ARGS_FILE"
  for a in "${args[@]}"; do
    [ "$a" = "@-" ] && reads_stdin=1
  done
  if [ "$reads_stdin" = 1 ]; then
    cat > "$CURL_STDIN_FILE"
  else
    : > "$CURL_STDIN_FILE"
  fi
  printf '%s\n%s' "$CURL_RESPONSE" "$CURL_CODE"
}

arg_present() { grep -Fxq -- "$1" "$CURL_ARGS_FILE"; }

# --- 4. handle_query_api: URL, method, headers, body --------------------------
qa_out="$(handle_query_api '{"provider":"maps","path":"/maps/api/place","method":"POST","query":{"q":"prague"},"body":{"a":1}}' < /dev/null)"

arg_present "http://api.local/api/v1/projects/proj-123/api/query" \
  || { echo "FAIL: query_api did not build the project-scoped /api/query URL" >&2; cat "$CURL_ARGS_FILE" >&2; exit 1; }
{ arg_present "-X" && grep -Fxq -- "POST" "$CURL_ARGS_FILE"; } \
  || { echo "FAIL: query_api did not issue a POST" >&2; exit 1; }
arg_present "X-API-Key: test-key" \
  || { echo "FAIL: query_api did not send the minted-key X-API-Key header" >&2; exit 1; }
arg_present "X-Execution-ID: exec-abc" \
  || { echo "FAIL: query_api did not send X-Execution-ID from the task" >&2; exit 1; }
jq -e '.provider=="maps" and .path=="/maps/api/place" and .method=="POST" and .query.q=="prague" and .body.a==1' "$CURL_STDIN_FILE" >/dev/null \
  || { echo "FAIL: query_api request body malformed" >&2; cat "$CURL_STDIN_FILE" >&2; exit 1; }
# No credential ever appears in the outgoing body.
grep -qi "api.key\|token\|secret\|bearer" "$CURL_STDIN_FILE" \
  && { echo "FAIL: query_api body contains a credential-looking field" >&2; exit 1; }
# Success unwraps the envelope: the LLM gets the inner .body (already
# redacted+capped server-side), not the {body,provenance,...} wrapper.
[ "$qa_out" = "ok" ] \
  || { echo "FAIL: query_api did not unwrap the response body (got: $qa_out)" >&2; exit 1; }

# The server embeds its truncation marker INSIDE .body (capToolResultBytes);
# the handler must NOT append a second one — the agent should see exactly one.
CURL_RESPONSE='{"body":"partial\n\n[truncated: response exceeded 100 bytes]","provenance":"third_party","truncated":true}'
trunc_out="$(handle_query_api '{"provider":"maps","path":"/x"}' < /dev/null)"
marker_count="$(printf '%s' "$trunc_out" | grep -c '\[truncated:' || true)"
[ "$marker_count" = "1" ] \
  || { echo "FAIL: expected exactly one (server-embedded) truncation marker, got $marker_count (out: $trunc_out)" >&2; exit 1; }
case "$trunc_out" in
  *"narrow your query to see more"*) echo "FAIL: entrypoint appended its own duplicate truncation marker" >&2; exit 1 ;;
esac

# --- 5. handle_query_api: refusal (HTTP 200) surfaces the text ----------------
CURL_RESPONSE='{"refusal":"provider not on this project allowlist"}'
CURL_CODE='200'
refusal_out="$(handle_query_api '{"provider":"evil","path":"/x"}' < /dev/null)"
[ "$refusal_out" = "provider not on this project allowlist" ] \
  || { echo "FAIL: query_api did not surface refusal text (got: $refusal_out)" >&2; exit 1; }

# --- 6. handle_query_api: non-200 surfaces a short error ----------------------
CURL_RESPONSE='{"error":{"code":"FORBIDDEN"}}'
CURL_CODE='403'
err_out="$(handle_query_api '{"provider":"maps","path":"/x"}' < /dev/null)"
case "$err_out" in
  *"HTTP 403"*) ;;
  *) echo "FAIL: query_api non-200 did not surface HTTP 403 (got: $err_out)" >&2; exit 1 ;;
esac

# --- 7. handle_list_apis: GET, URL with ?query=, headers ----------------------
CURL_RESPONSE='{"providers":[{"provider":"maps"}],"count":1}'
CURL_CODE='200'
la_out="$(handle_list_apis '{"query":"maps"}' < /dev/null)"
arg_present "http://api.local/api/v1/projects/proj-123/api/providers?query=maps" \
  || { echo "FAIL: list_apis did not build the providers URL with ?query=" >&2; cat "$CURL_ARGS_FILE" >&2; exit 1; }
grep -Fxq -- "-X" "$CURL_ARGS_FILE" \
  && { echo "FAIL: list_apis should be a GET (no -X)" >&2; exit 1; }
arg_present "X-API-Key: test-key" \
  || { echo "FAIL: list_apis did not send X-API-Key" >&2; exit 1; }
arg_present "X-Execution-ID: exec-abc" \
  || { echo "FAIL: list_apis did not send X-Execution-ID" >&2; exit 1; }
[ "$la_out" = "$CURL_RESPONSE" ] \
  || { echo "FAIL: list_apis did not return the server response verbatim" >&2; exit 1; }

# --- 8. handle_list_apis: no filter -> bare providers URL ---------------------
la_bare="$(handle_list_apis '{}' < /dev/null)"
arg_present "http://api.local/api/v1/projects/proj-123/api/providers" \
  || { echo "FAIL: list_apis without a filter did not hit the bare providers URL" >&2; exit 1; }
[ -n "$la_bare" ] || { echo "FAIL: list_apis returned empty" >&2; exit 1; }

echo "OK: query_api/list_apis exposed under gating and POST/GET the project endpoints correctly"
