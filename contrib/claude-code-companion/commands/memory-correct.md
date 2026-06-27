---
description: Refute a wrong/stale fact in vornik RAG (and optionally store the correction)
argument-hint: "[--chunk-id ID]... [--max N] [--scope TOKEN] [<wrong claim>] [||| <correction>]"
---

# Memory-correct (soft-refute stale RAG facts via vornik)

Demote a wrong or superseded claim in this project's RAG memory so it stops
surfacing in `recall`, and (optionally) deposit the authoritative correction.
This is the programmatic equivalent of telling the dispatcher "X is wrong, it's
actually Y" — it hybrid-searches `wrong_claim`, flips the top matches to
`validation_status='refuted'` (the retrieval layer auto-excludes refuted
chunks), and, when a correction is given, stores it as a verified chunk.

Two targeting modes:
- **Claim search** — give a `wrong_claim`; the tool hybrid-searches it and
  refutes the top matches.
- **Surgical by-id** — give one or more `--chunk-id ID` (from a prior
  `/recall`); only those exact chunks are refuted. Prefer this when
  authoritative corrections already exist, because a claim search would rank
  the corrections highest and refute THEM instead of the stale chunk.

Syntax: optional leading flags, then an optional wrong claim, then an optional
`|||`-separated correction:

    /memory-correct drop the old DB legacy_db ||| legacy_db IS prod; never drop it
    /memory-correct --max 5 --scope github.com/grinco/vornik stale claim text
    /memory-correct --chunk-id 340ccc5f… --chunk-id 9750617… ||| the corrected fact
    /memory-correct refute-only claim with no replacement

Provide a `wrong_claim` OR at least one `--chunk-id`. Requires a
`memory_write`-scoped companion key.

User's arguments: `$ARGUMENTS`

!`ARGS_FILE="$(mktemp "${TMPDIR:-/tmp}/vornik-correct-args.XXXXXX")"
trap 'rm -f "$ARGS_FILE"' EXIT
cat >"$ARGS_FILE" <<'VORNIK_CORRECT_ARGS_EOF'
$ARGUMENTS
VORNIK_CORRECT_ARGS_EOF
python3 - "$ARGS_FILE" <<'PYEOF'
import sys, os, json, urllib.request, urllib.error

# Args arrive via a file written by a single-quoted heredoc so the shell never
# parses quotes, parens, backticks, or em-dashes inside them — the same hardening
# /upload and /rag-ingest use. We build the JSON body with json.dumps (which
# escapes everything) and POST it ourselves, so large/special-character
# corrections never go through a fragile inline MCP tool-call serialization.
try:
    with open(sys.argv[1], encoding="utf-8") as fh:
        raw_args = fh.read().strip()
except OSError as e:
    print(f"error: cannot read arguments file — {e}")
    sys.exit(1)
if not raw_args:
    print('usage: /memory-correct [--chunk-id ID]... [--max N] [--scope TOKEN] [<wrong claim>] [||| <correction>]')
    sys.exit(1)

# ---------- consume leading --chunk-id / --max / --scope flags ----------
max_refutes = 0
scope_arg = None
chunk_ids = []
toks = raw_args.split()
i = 0
while i < len(toks):
    if toks[i] == "--chunk-id" and i + 1 < len(toks):
        chunk_ids.append(toks[i + 1])
        i += 2
        continue
    if toks[i] == "--max" and i + 1 < len(toks):
        try:
            max_refutes = int(toks[i + 1])
        except ValueError:
            print(f"error: --max needs an integer, got {toks[i + 1]!r}")
            sys.exit(1)
        i += 2
        continue
    if toks[i] == "--scope" and i + 1 < len(toks):
        scope_arg = toks[i + 1]
        i += 2
        continue
    break
rest = " ".join(toks[i:]).strip()

# ---------- split [wrong_claim] ||| [correction] ----------
if "|||" in rest:
    wrong_claim, correction = (s.strip() for s in rest.split("|||", 1))
else:
    wrong_claim, correction = rest, ""

# Need a target: a wrong claim to search, or explicit chunk ids.
if not wrong_claim and not chunk_ids:
    print("error: provide a wrong claim, or one or more --chunk-id <id> (surgical refute)")
    print('usage: /memory-correct [--chunk-id ID]... [--max N] [--scope TOKEN] [<wrong claim>] [||| <correction>]')
    sys.exit(1)

# ---------- repo_scope: explicit flag, else env pin ----------
repo_scope = (scope_arg or os.environ.get("VORNIK_REPO_SCOPE", "")).strip()

# ---------- env: daemon URL + auth ----------
url_base = os.environ.get("VORNIK_URL", "").rstrip("/")
token = os.environ.get("VORNIK_COMPANION_TOKEN", "")
if not url_base or not token:
    print("error: VORNIK_URL and VORNIK_COMPANION_TOKEN must be set in this shell")
    print("(the same env the .mcp.json plugin config uses — see contrib/claude-code-companion/README.md)")
    sys.exit(1)
url = url_base + "/api/v1/mcp/companion"

args = {}
if chunk_ids:
    args["chunk_ids"] = chunk_ids
if wrong_claim:
    args["wrong_claim"] = wrong_claim
if correction:
    args["correction"] = correction
if max_refutes:
    args["max_refutes"] = max_refutes
if repo_scope:
    args["repo_scope"] = repo_scope

body = {"jsonrpc": "2.0", "id": 1, "method": "tools/call",
        "params": {"name": "memory_correct", "arguments": args}}
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
content = result.get("content") or []
text = content[0].get("text", "") if content else ""
if result.get("isError"):
    print(f"error: memory_correct reported failure: {text or '<no text>'}")
    sys.exit(1)
try:
    inner = json.loads(text)
except json.JSONDecodeError:
    print(text or "<no response text>")
    sys.exit(0)

n = inner.get("refuted_count", 0)
target = f"chunk_ids={chunk_ids}" if chunk_ids else f"wrong_claim={wrong_claim!r}"
print(f"Refuted {n} chunk(s) ({target}).")
for r in inner.get("refuted", []):
    score = r.get("score")
    score_str = f"  score={score:.3f}" if isinstance(score, (int, float)) and score else ""
    print(f"  - {r.get('chunk_id')}{score_str}  {r.get('source_name','')}".rstrip())
    prev = (r.get("preview") or "").replace("\n", " ")
    if prev:
        print(f"      {prev[:160]}")
cid = inner.get("correction_chunk_id")
if cid:
    print(f"Stored correction as verified chunk: {cid}")
elif correction:
    print("note: correction text was provided but no correction chunk id came back.")
if inner.get("note"):
    print(f"note: {inner['note']}")
PYEOF
`

Report the outcome to the user: how many chunks were refuted (with their
previews so the user can confirm the right ones were demoted), and whether a
correction chunk was stored. If `refuted_count` is 0, tell the user nothing
matched closely enough — they may need to rephrase `wrong_claim` to match how
the stale fact actually appears in memory (try `/recall` first to find it).
If the response says `this key lacks memory_write`, surface it verbatim and
point to the operator fix: `vornikctl companion grant --memory-write`.
