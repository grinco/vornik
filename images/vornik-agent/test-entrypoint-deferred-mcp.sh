#!/usr/bin/env bash
# Regression guard for agent-side deferred MCP exposure. The agent should send
# built-ins + tool_search while the MCP catalogue is large, then expose only
# matched MCP tools after tool_search records them.
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
export VORNIK_AGENT_DEFER_MCP_TOOLS=1
export VORNIK_AGENT_DEFER_MCP_THRESHOLD=2
export VORNIK_AGENT_TOOL_SEARCH_LIMIT=2

printf '%s\n' '{"config":{"permissions":{"allowedTools":["file_read"]}}}' > "$INPUT_FILE"

# shellcheck source=images/vornik-agent/entrypoint.sh
source "$ep"

builtins="$tmp/builtin.json"
mcp="$tmp/.mcp_tools.json"
expanded="$tmp/.expanded_mcp_tools.txt"
pinned="$tmp/.pinned_mcp_tools.txt"
tools="$tmp/.tools.json"
export MCP_TOOLS_FILE="$mcp"
export EXPANDED_MCP_TOOLS_FILE="$expanded"

tool_definitions > "$builtins"
cat > "$mcp" <<'JSON'
[
  {"type":"function","function":{"name":"mcp__mail__send","description":"Send an email message","parameters":{"type":"object"}}},
  {"type":"function","function":{"name":"mcp__calendar__create","description":"Create a calendar event","parameters":{"type":"object"}}},
  {"type":"function","function":{"name":"mcp__crm__lookup","description":"Lookup a customer record","parameters":{"type":"object"}}}
]
JSON
: > "$expanded"
: > "$pinned"

rebuild_tools_file "$builtins" "$mcp" "$expanded" "$pinned" "$tools"
jq -e 'map(.function.name) | index("tool_search") != null' "$tools" >/dev/null
jq -e 'map(.function.name) | index("mcp__mail__send") == null' "$tools" >/dev/null

search_result="$(handle_tool_search '{"query":"send email","limit":1}')"
printf '%s' "$search_result" | jq -e '.matches[0].name == "mcp__mail__send"' >/dev/null

rebuild_tools_file "$builtins" "$mcp" "$expanded" "$pinned" "$tools"
jq -e 'map(.function.name) | index("mcp__mail__send") != null' "$tools" >/dev/null
jq -e 'map(.function.name) | index("mcp__calendar__create") == null' "$tools" >/dev/null

printf '%s\n' 'mcp__calendar__create' > "$pinned"
: > "$expanded"
rebuild_tools_file "$builtins" "$mcp" "$expanded" "$pinned" "$tools"
jq -e 'map(.function.name) | index("mcp__calendar__create") != null' "$tools" >/dev/null
jq -e 'map(.function.name) | index("mcp__mail__send") == null' "$tools" >/dev/null

echo "OK: deferred MCP tool_search expands matching tools only"
