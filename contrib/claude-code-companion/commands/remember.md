---
description: Deposit a note into this project's RAG memory via vornik
argument-hint: [--scope TOKEN] [--class C] [--ttl DAYS] <free-form note body>
---

# Remember (deposit into vornik RAG)

Persist a note into vornik's project memory so future sessions, autonomous
swarms, and other companion clients can find it.

The note body is sent to the daemon by this command directly (heredoc → file →
`json.dumps` → POST), NOT as an inline MCP tool-call argument. That hardening
(matching `/upload` and `/rag-ingest`, the 2026-06-26 fix) is why a long note
full of quotes, backticks, newlines, or em-dashes lands intact instead of being
mangled or truncated by tool-call serialization.

Optional leading flags, then the note body:

    /remember IBKR fractional shares still blocked — error 10243 even with the permission on.
    /remember --scope github.com/grinco/vornik --class decision <body>
    /remember --ttl 3650 a durable fact that should outlive the 30-day companion_note default

User's arguments: `$ARGUMENTS`

!`ARGS_FILE="$(mktemp "${TMPDIR:-/tmp}/vornik-remember-args.XXXXXX")"
trap 'rm -f "$ARGS_FILE"' EXIT
cat >"$ARGS_FILE" <<'VORNIK_REMEMBER_ARGS_EOF'
$ARGUMENTS
VORNIK_REMEMBER_ARGS_EOF
python3 - "$ARGS_FILE" <<'PYEOF'
import sys, os, json, urllib.request, urllib.error

try:
    with open(sys.argv[1], encoding="utf-8") as fh:
        raw_args = fh.read().strip()
except OSError as e:
    print(f"error: cannot read arguments file — {e}")
    sys.exit(1)
if not raw_args:
    print("usage: /remember [--scope TOKEN] [--class C] [--ttl DAYS] <note body>")
    sys.exit(1)

# Consume leading --scope / --class / --ttl flags; everything after is content.
scope_arg = klass = None
ttl_days = 0
toks = raw_args.split()
i = 0
while i < len(toks):
    if toks[i] == "--scope" and i + 1 < len(toks):
        scope_arg = toks[i + 1]; i += 2; continue
    if toks[i] == "--class" and i + 1 < len(toks):
        klass = toks[i + 1]; i += 2; continue
    if toks[i] == "--ttl" and i + 1 < len(toks):
        try:
            ttl_days = int(toks[i + 1])
        except ValueError:
            print(f"error: --ttl needs an integer, got {toks[i + 1]!r}")
            sys.exit(1)
        i += 2; continue
    break
# Recover the content as the substring after the consumed flag tokens, so the
# body keeps its original whitespace/newlines rather than a re-joined copy.
content = raw_args
for t in toks[:i]:
    content = content[content.find(t) + len(t):]
content = content.strip()
if not content:
    print("error: note body is empty after flags")
    sys.exit(1)

repo_scope = (scope_arg or os.environ.get("VORNIK_REPO_SCOPE", "")).strip()
url_base = os.environ.get("VORNIK_URL", "").rstrip("/")
token = os.environ.get("VORNIK_COMPANION_TOKEN", "")
if not url_base or not token:
    print("error: VORNIK_URL and VORNIK_COMPANION_TOKEN must be set in this shell")
    print("(the same env the .mcp.json plugin config uses — see contrib/claude-code-companion/README.md)")
    sys.exit(1)
url = url_base + "/api/v1/mcp/companion"

args = {"content": content}
if klass:
    args["class"] = klass
if ttl_days:
    args["ttl_days"] = ttl_days
if repo_scope:
    args["repo_scope"] = repo_scope

body = {"jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": "remember", "arguments": args}}
req = urllib.request.Request(
    url, data=json.dumps(body).encode("utf-8"),
    headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"})
try:
    with urllib.request.urlopen(req, timeout=30) as resp:
        raw = resp.read()
except urllib.error.HTTPError as e:
    print(f"error: daemon returned HTTP {e.code}")
    print(f"  body: {e.read().decode('utf-8', errors='replace')[:500]}")
    sys.exit(1)
except urllib.error.URLError as e:
    print(f"error: cannot reach vornik daemon at {url} — {e.reason}")
    sys.exit(1)

try:
    rpc = json.loads(raw)
except json.JSONDecodeError as e:
    print(f"error: daemon response is not valid JSON ({e}): {raw[:200]!r}")
    sys.exit(1)
if "error" in rpc:
    print(f"error: JSON-RPC error from daemon: {rpc['error']}")
    sys.exit(1)
result = rpc.get("result", {})
content_resp = result.get("content") or []
text = content_resp[0].get("text", "") if content_resp else ""
if result.get("isError"):
    print(f"error: remember reported failure: {text or '<no text>'}")
    sys.exit(1)
try:
    inner = json.loads(text)
except json.JSONDecodeError:
    print(text or "<no response text>")
    sys.exit(0)
print(f"decision: {inner.get('Decision', inner.get('decision', '?'))}")
print(f"artifact_id: {inner.get('ArtifactID', inner.get('artifact_id', ''))}")
gf = inner.get("GatesFailed") or inner.get("gates_failed")
if gf:
    print(f"gates_failed: {gf}")
scope_msg = f" (repo_scope={repo_scope!r})" if repo_scope else " (no repo_scope)"
print(f"stored {len(content):,}-char note{scope_msg}.")
PYEOF
`

Branch on the `decision` line printed above:

- **`ALLOW`** — admitted. Tell the user it landed (default `companion_note`, 30-day
  TTL unless `--ttl` raised it) and show the `artifact_id`.
- **`QUARANTINED`** — admitted but flagged; show `gates_failed` verbatim. Operator
  can release via `/api/v1/projects/{p}/memory/quarantine/<id>/release`.
- **`REJECTED`** — refused. Usually `secret_scan` (scrub first — do NOT retry raw),
  `min_content` (<64 chars / 10 words — pad with context), or `dedup_hash`
  (already stored). Relay which gate caught it.

If the response says `this key lacks memory_write`, surface it verbatim and point
to the operator fix: `vornikctl companion grant --memory-write`.
