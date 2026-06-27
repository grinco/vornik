---
description: Upload local files as input artifacts + delegate ANY vornik workflow (RAG ingest, document architectural-review, data-validation, …) in one shot, without burning client tokens on file content
allowed-tools: Bash
argument-hint: <workflow-id> "<prompt>" [--scope <token>] <path1> [path2 ...]
---

# Upload files + delegate to vornik

The user wants to delegate a vornik task that needs file attachments — the
files live on this machine (e.g. macOS laptop) but vornik runs elsewhere,
so the agent container can't reach the paths directly.

This command is workflow-generic — the first arg is any workflow ID, not
just an ingest path. It is the canonical way to get a **document reviewed**:
`companion-architectural-review` (v1.1.0+) reviews the STAGED input artifact
as its source of truth and refuses to review project memory, so a doc review
must attach the file here rather than naming a path in a delegate prompt (the
agent container can't read your repo, and a RAG-only review surfaces stale,
contradictory chunks). Example:

```
/upload companion-architectural-review "Review this design doc for architectural issues." https://docs.vornik.io
```

This slash command's bash does the read + base64 + POST locally (in this
shell, not via the model), so the file bytes never appear in your token
stream. The model only sees the bash output below: a task_id confirmation.

User's arguments: `$ARGUMENTS`

!`ARGS_FILE="$(mktemp "${TMPDIR:-/tmp}/vornik-upload-args.XXXXXX")"
trap 'rm -f "$ARGS_FILE"' EXIT
cat >"$ARGS_FILE" <<'VORNIK_UPLOAD_ARGS_EOF'
$ARGUMENTS
VORNIK_UPLOAD_ARGS_EOF
python3 - "$ARGS_FILE" <<'PYEOF'
import sys, os, json, base64, shlex, urllib.request, urllib.error

# ---------- parse args ----------
# The prompt arrives via a file written by a single-quoted heredoc, so the
# shell never parses quotes, parens, or em-dashes inside it. (2026-06-26 bug:
# a raw ARGUMENTS value interpolated inside a double-quoted shell word broke
# the command with a shell syntax error before Python ran.) shlex then splits
# the reconstructed argument line exactly as a shell word-split would, so the
# documented workflow / quoted-prompt / paths shape still works.
try:
    with open(sys.argv[1], encoding="utf-8") as _fh:
        raw_args = _fh.read()
except OSError as e:
    print(f"error: cannot read arguments file — {e}")
    sys.exit(1)
try:
    tokens = shlex.split(raw_args)
except ValueError as e:
    print(f"error: failed to parse arguments — {e}")
    print('usage: /upload <workflow-id> "<prompt>" [--scope <token>] <path1> [...]')
    sys.exit(1)
if len(tokens) < 3:
    print('error: need at least <workflow-id> "<prompt>" <path>')
    print('usage: /upload <workflow-id> "<prompt>" [--scope <token>] <path1> [...]')
    sys.exit(1)

# Positional shape: <workflow> <prompt> [flags...] <paths...>
# Flags accepted: --scope <token>
workflow, prompt = tokens[0], tokens[1]
i = 2
scope_arg = None
paths = []
while i < len(tokens):
    t = tokens[i]
    if t == "--scope":
        if i + 1 >= len(tokens):
            print("error: --scope requires a value")
            sys.exit(1)
        scope_arg = tokens[i + 1]
        i += 2
        continue
    paths.append(t)
    i += 1
if not paths:
    print("error: at least one file path is required")
    sys.exit(1)

# ---------- repo_scope resolution ----------
# Mirrors hooks/session-start.sh so the slash command behaves the same
# as recall()/remember()/delegate() do at session start:
#   1. explicit --scope wins
#   2. else $VORNIK_REPO_SCOPE (operator pin)
#   3. else auto-detect from cwd's git remote.origin.url
#   4. else repo toplevel basename (local-only repo)
#   5. else current folder basename (not a git repo — never empty, so
#      the scope never degrades to "none"/project-wide)
def resolve_scope():
    if scope_arg is not None:
        return scope_arg.strip()
    env_pin = os.environ.get("VORNIK_REPO_SCOPE", "").strip()
    if env_pin:
        return env_pin
    import subprocess
    try:
        out = subprocess.run(
            ["git", "config", "--get", "remote.origin.url"],
            capture_output=True, text=True, timeout=2, check=False,
        )
        url = out.stdout.strip()
    except (subprocess.SubprocessError, FileNotFoundError):
        return os.path.basename(os.getcwd())
    if not url:
        # No remote — fall back to repo basename for the rare local-only
        # case, then to the current folder name when this isn't a git
        # repo at all. Mirrors session-start.sh's resolve_repo_scope.
        try:
            top = subprocess.run(
                ["git", "rev-parse", "--show-toplevel"],
                capture_output=True, text=True, timeout=2, check=False,
            )
            base = os.path.basename(top.stdout.strip())
            return base or os.path.basename(os.getcwd())
        except (subprocess.SubprocessError, FileNotFoundError):
            return os.path.basename(os.getcwd())
    # Normalise to the token shape session-start.sh produces.
    for suf in (".git",):
        if url.endswith(suf):
            url = url[:-len(suf)]
    # Strip scheme first so any embedded "user@" sits at the start.
    for pre in ("https://", "http://", "ssh://"):
        if url.startswith(pre):
            url = url[len(pre):]
    # Then strip any "<user>@" prefix — covers git@, gitlab@,
    # https://user@host shapes, and any future provider that ships a
    # different username convention. The "@" must come before the
    # first ":" or "/" to actually be a user separator (otherwise it's
    # inside the path).
    at_idx = url.find("@")
    if at_idx > 0:
        first_sep = min(
            (i for i in (url.find(":"), url.find("/")) if i != -1),
            default=-1,
        )
        if first_sep == -1 or at_idx < first_sep:
            url = url[at_idx + 1:]
    # host:user/repo → host/user/repo
    if ":" in url and "/" not in url.split(":", 1)[0]:
        url = url.replace(":", "/", 1)
    return url

repo_scope = resolve_scope()

# ---------- read + encode files ----------
# Companion MCP body limit is 1 MiB (companion_mcp.go: companionMCPMaxBodyBytes).
# Base64 inflates 1.33x; leave ~25% headroom for JSON envelope, prompt, etc.
SOFT_LIMIT_BYTES = 600 * 1024

files = []
total_bytes = 0
for p in paths:
    p_abs = os.path.abspath(os.path.expanduser(p))
    if not os.path.exists(p_abs):
        print(f"error: path does not exist: {p}")
        sys.exit(1)
    if not os.path.isfile(p_abs):
        print(f"error: not a regular file (directories not supported in v1): {p}")
        sys.exit(1)
    try:
        with open(p_abs, "rb") as fh:
            data = fh.read()
    except OSError as e:
        print(f"error: cannot read {p}: {e}")
        sys.exit(1)
    total_bytes += len(data)
    files.append({
        "name": os.path.basename(p_abs),
        "content": base64.b64encode(data).decode("ascii"),
    })

if total_bytes > SOFT_LIMIT_BYTES:
    print(f"error: total file size {total_bytes:,} bytes exceeds the {SOFT_LIMIT_BYTES:,}-byte")
    print("soft limit (companion MCP caps the JSON body at 1 MiB; base64 inflates 1.33x).")
    print("Either trim the file set or upload in batches across multiple /upload calls.")
    sys.exit(1)

# ---------- env: daemon URL + auth ----------
url_base = os.environ.get("VORNIK_URL", "").rstrip("/")
token = os.environ.get("VORNIK_COMPANION_TOKEN", "")
if not url_base or not token:
    print("error: VORNIK_URL and VORNIK_COMPANION_TOKEN must be set in this shell")
    print("(the same env the .mcp.json plugin config uses — see contrib/claude-code-companion/README.md)")
    sys.exit(1)
url = url_base + "/api/v1/mcp/companion"

# ---------- JSON-RPC call to mcp__vornik__delegate ----------
delegate_args = {
    "workflow": workflow,
    "prompt": prompt,
    "inputArtifacts": files,
    # Skip auto-extract on upload — the workflow we're delegating to
    # is itself an ingest path. Auto-extract would chunk + ingest the
    # file at upload time AND mark it as already-extracted, which
    # makes the agent skip staging the raw file (see B-10 backlog).
    # The right default for a file-upload-then-workflow shape.
    "skip_auto_extract": True,
}
if repo_scope:
    delegate_args["repo_scope"] = repo_scope

body = {
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/call",
    "params": {"name": "delegate", "arguments": delegate_args},
}
req = urllib.request.Request(
    url,
    data=json.dumps(body).encode("utf-8"),
    headers={
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json",
    },
)
try:
    with urllib.request.urlopen(req, timeout=30) as resp:
        raw = resp.read()
except urllib.error.HTTPError as e:
    print(f"error: daemon returned HTTP {e.code}")
    msg = e.read().decode("utf-8", errors="replace")[:500]
    print(f"  body: {msg}")
    sys.exit(1)
except urllib.error.URLError as e:
    print(f"error: cannot reach vornik daemon at {url} — {e.reason}")
    sys.exit(1)

# ---------- unwrap the JSON-RPC envelope ----------
try:
    rpc = json.loads(raw)
except json.JSONDecodeError as e:
    print(f"error: daemon response is not valid JSON ({e}): {raw[:200]!r}")
    sys.exit(1)
if "error" in rpc:
    print(f"error: JSON-RPC error from daemon: {rpc['error']}")
    sys.exit(1)
result = rpc.get("result", {})
if result.get("isError"):
    text = (result.get("content") or [{}])[0].get("text", "<no text>")
    print(f"error: delegate tool reported failure: {text}")
    sys.exit(1)
content = result.get("content") or []
if not content or "text" not in content[0]:
    print(f"error: malformed delegate response: {raw[:300]!r}")
    sys.exit(1)
try:
    inner = json.loads(content[0]["text"])
except json.JSONDecodeError:
    # Older daemon versions may return plain text; pass through verbatim.
    print(content[0]["text"])
    sys.exit(0)

# ---------- success ----------
print(f"Uploaded {len(files)} file(s), {total_bytes:,} bytes total.")
scope_msg = f" (repo_scope={repo_scope!r})" if repo_scope else " (no repo_scope)"
print(f"Delegated to workflow={workflow!r} on project={inner.get('project', '?')!r}{scope_msg}.")
print(f"task_id: {inner.get('task_id')}")
print(f"status:  {inner.get('status')}")
hint = inner.get("eta_hint")
if hint:
    print(f"next:    {hint}")
PYEOF
`

## What just happened

The bash above already did the upload + delegate end-to-end. Read the
output and report back to the user with:

- The `task_id` (so they can poll it).
- A one-line summary of what was queued (workflow ID + file count).
- The next step (`/status <task_id>` if they want to poll
  themselves, or just walk away — the SessionStart hook will surface
  the result digest next time).

Do NOT call `mcp__vornik__delegate` again. The bash already did it; a
second call would duplicate the task. If the bash output starts with
`error:`, surface the error verbatim and stop.
