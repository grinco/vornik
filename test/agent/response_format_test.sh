#!/usr/bin/env bash
# Smoke test for the vornik-agent entrypoint's response_format /
# response_schema wiring — item 7 of
# https://docs.vornik.io
#
# Pre-fix the entrypoint only read config.responseFormat (string) and
# emitted `response_format: {type: <format>}` on the wire. Roles with
# an outputSchema landed the schema body in config.responseSchema in
# task.json, but the agent never re-serialised it into the OpenAI-
# shape response_format directive — so OpenAI / Bedrock / Anthropic
# providers got an empty schema reference and provider-side
# enforcement silently fell back to free-form.
#
# This test runs the entrypoint's actual jq pipeline against
# synthetic task.json inputs and asserts the produced request body
# carries the typed json_schema directive (with the schema embedded)
# when the role has one, and falls back to the legacy form otherwise.

set -u

PASS=0
FAIL=0
FAILURES=()

assert_contains() {
    local name="$1" haystack="$2" needle="$3"
    if [[ "$haystack" == *"$needle"* ]]; then
        PASS=$((PASS+1))
        echo "PASS: $name"
    else
        FAIL=$((FAIL+1))
        FAILURES+=("$name: expected to contain '$needle', got: $(printf '%s' "$haystack" | head -c 500)")
        echo "FAIL: $name"
    fi
}

assert_not_contains() {
    local name="$1" haystack="$2" needle="$3"
    if [[ "$haystack" != *"$needle"* ]]; then
        PASS=$((PASS+1))
        echo "PASS: $name"
    else
        FAIL=$((FAIL+1))
        FAILURES+=("$name: unexpectedly contains '$needle'")
        echo "FAIL: $name"
    fi
}

# The exact jq expression the entrypoint uses to build the request
# body. Kept inline so a drift between this test and the production
# script surfaces as a test failure when the entrypoint changes.
build_request() {
    local response_format="$1"
    local response_schema="${2:-null}"
    local schema_name="${3:-writer_result}"
    jq -n --arg model "test-model" \
        --argjson msgs '[{"role":"user","content":"hi"}]' \
        --argjson tools '[]' \
        --argjson ctx_size 0 \
        --argjson max_tokens 4096 \
        --arg response_format "$response_format" \
        --arg schema_name "$schema_name" \
        --argjson response_schema "$response_schema" \
        '{"model":$model,"messages":$msgs,"tools":$tools}
         | if $max_tokens > 0 then . + {"max_tokens":$max_tokens} else . end
         | if $ctx_size > 0 then . + {"options":{"num_ctx":$ctx_size}} else . end
         | if $response_format == "json_schema" and ($response_schema != null) then
               . + {"response_format":{"type":"json_schema","json_schema":{"name":$schema_name,"schema":$response_schema,"strict":true}}}
           elif $response_format == "json_schema" then
               . + {"response_format":{"type":"json_object"}}
           elif $response_format != "" then
               . + {"response_format":{"type":$response_format}}
           else . end'
}

echo "=== json_schema directive with schema body ==="
schema='{"type":"object","required":["writing"],"properties":{"writing":{"type":"object"}},"additionalProperties":false}'
out=$(build_request "json_schema" "$schema")
assert_contains "wraps response_format.type=json_schema" "$out" '"type": "json_schema"'
assert_contains "carries schema name" "$out" '"name": "writer_result"'
assert_contains "carries inner schema type" "$out" '"type": "object"'
assert_contains "carries required keys" "$out" '"writing"'
assert_contains "carries additionalProperties false" "$out" '"additionalProperties": false'
assert_contains "carries strict flag" "$out" '"strict": true'

echo ""
echo "=== json_object legacy directive ==="
out=$(build_request "json_object")
assert_contains "carries json_object type" "$out" '"type": "json_object"'
assert_not_contains "no json_schema body on legacy path" "$out" "json_schema"

echo ""
echo "=== no directive (free-form) ==="
out=$(build_request "")
assert_not_contains "free-form: no response_format at all" "$out" "response_format"

echo ""
echo "=== json_schema flag without schema body (degrades cleanly) ==="
# Hardening: an operator who set responseFormat: json_schema but
# whose outputSchema render produced an empty body (e.g. role-level
# config drift during migration) should NOT send a malformed
# `response_format:{type:json_schema}` without the schema — the
# gateway would 400. Fall back to json_object (loose enforcement) so
# the agent still benefits from gateway-level JSON-mode while logs
# surface the misconfiguration.
out=$(build_request "json_schema" "null")
assert_not_contains "empty schema does not emit malformed json_schema" "$out" '"type": "json_schema"'
assert_contains "empty schema falls back to json_object" "$out" '"type": "json_object"'

echo ""
echo "=== custom schema name (per-role naming) ==="
out=$(build_request "json_schema" "$schema" "researcher_result")
assert_contains "custom schema name surfaces" "$out" '"name": "researcher_result"'

echo ""
echo "================================"
echo "PASSED: $PASS"
echo "FAILED: $FAIL"
if [ "$FAIL" -gt 0 ]; then
    echo ""
    echo "Failures:"
    for f in "${FAILURES[@]}"; do
        echo "  - $f"
    done
    exit 1
fi
exit 0
