#!/usr/bin/env bash
# replay-audit-journal.sh — backfill audit events that were 4xx-journaled
# while VORNIK_BROKER_DAEMON_API_KEY was missing or wrong.
#
# BACKGROUND
#   The broker's AuditWriter journals every event that cannot be posted to the
#   daemon (network error, 5xx backoff exhaustion, or 4xx rejection). When the
#   API key was absent every POST returned 401, so orders/fills were recorded
#   by the broker but never landed in the daemon's Postgres tables. This script
#   replays those lines now that the key is configured.
#
# USAGE
#   export VORNIK_BROKER_DAEMON_URL="http://host.containers.internal:8080"
#   export VORNIK_BROKER_DAEMON_API_KEY="<your-key>"
#   # Dry-run (default): prints what would be sent without POSTing.
#   bash replay-audit-journal.sh
#   # Apply for real:
#   APPLY=1 bash replay-audit-journal.sh
#
# REQUIREMENTS
#   - jq (for JSON field extraction)
#   - sudo (the journal is root-owned at the host path; the script reads it
#     via `sudo cat` — you will be prompted for your password if sudo is not
#     already cached)
#
# JOURNAL PATHS
#   Host:      ~/.local/share/vornik-broker/audit.journal  (root-owned)
#   Container: /var/lib/vornik-broker/audit.journal
#
# IDEMPOTENCY
#   The daemon enforces uniqueness on (project_id, idempotency_key) extracted
#   from the payload, so replaying the same line twice is safe. Re-running
#   this script after a partial run will produce 409/200 responses for
#   already-applied events; those are expected and harmless.
#
# DRY-RUN DEFAULT
#   Set APPLY=1 to actually POST. Without it the script only prints what it
#   would send.

set -euo pipefail

JOURNAL_HOST_PATH="${VORNIK_BROKER_AUDIT_JOURNAL_HOST_PATH:-${HOME}/.local/share/vornik-broker/audit.journal}"
DAEMON_URL="${VORNIK_BROKER_DAEMON_URL:?VORNIK_BROKER_DAEMON_URL must be set}"
API_KEY="${VORNIK_BROKER_DAEMON_API_KEY:?VORNIK_BROKER_DAEMON_API_KEY must be set}"
APPLY="${APPLY:-0}"

# Verify dependencies.
if ! command -v jq &>/dev/null; then
    echo "ERROR: jq is required but not found in PATH" >&2
    exit 1
fi

echo "=== Broker audit journal replay ==="
echo "Journal:   ${JOURNAL_HOST_PATH}"
echo "Daemon:    ${DAEMON_URL}"
echo "Dry-run:   $([ "${APPLY}" = "1" ] && echo "NO (APPLY=1)" || echo "YES (set APPLY=1 to send)")"
echo ""

# Read the root-owned journal via sudo.
echo "Reading journal (may prompt for sudo password)..."
journal_content="$(sudo cat "${JOURNAL_HOST_PATH}" 2>/dev/null || true)"

if [ -z "${journal_content}" ]; then
    echo "Journal is empty or does not exist at ${JOURNAL_HOST_PATH}."
    exit 0
fi

total=0
ok=0
skipped=0
failed=0

while IFS= read -r line; do
    [ -z "${line}" ] && continue

    # Each line is an AuditEvent JSON object: {endpoint, payload, idempotency_key}
    endpoint="$(echo "${line}" | jq -r '.endpoint // empty')"
    payload="$(echo "${line}" | jq -c '.payload // empty')"
    idempotency_key="$(echo "${line}" | jq -r '.idempotency_key // empty')"

    if [ -z "${endpoint}" ] || [ -z "${payload}" ]; then
        echo "  [SKIP] malformed line (missing endpoint or payload): ${line}"
        ((skipped++)) || true
        continue
    fi

    total=$((total + 1))
    target_url="${DAEMON_URL}${endpoint}"

    if [ "${APPLY}" != "1" ]; then
        echo "  [DRY-RUN] POST ${target_url}  idempotency_key=${idempotency_key}"
        ((ok++)) || true
        continue
    fi

    http_status="$(
        curl -s -o /dev/null -w "%{http_code}" \
            -X POST "${target_url}" \
            -H "Content-Type: application/json" \
            -H "Authorization: Bearer ${API_KEY}" \
            -d "${payload}"
    )"

    case "${http_status}" in
        2??)
            echo "  [OK]   ${http_status} POST ${target_url}  idempotency_key=${idempotency_key}"
            ((ok++)) || true
            ;;
        409)
            echo "  [DUP]  ${http_status} POST ${target_url}  idempotency_key=${idempotency_key} (already applied, safe to ignore)"
            ((ok++)) || true
            ;;
        *)
            echo "  [FAIL] ${http_status} POST ${target_url}  idempotency_key=${idempotency_key}"
            ((failed++)) || true
            ;;
    esac

done <<<"${journal_content}"

echo ""
echo "=== Summary ==="
echo "Total lines: ${total}"
if [ "${APPLY}" = "1" ]; then
    echo "Succeeded:   ${ok}"
    echo "Failed:      ${failed}"
else
    echo "Would POST:  ${ok}  (re-run with APPLY=1 to apply)"
fi
echo "Skipped:     ${skipped}  (malformed lines)"

if [ "${failed}" -gt 0 ]; then
    echo ""
    echo "WARNING: ${failed} lines failed. Check VORNIK_BROKER_DAEMON_URL and VORNIK_BROKER_DAEMON_API_KEY and retry." >&2
    exit 1
fi
