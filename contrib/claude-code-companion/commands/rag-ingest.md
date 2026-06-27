---
description: Bulk-ingest local files into vornik's project RAG memory via the companion-rag-ingest workflow, without burning client tokens on file content
allowed-tools: Bash
argument-hint: [--scope <token>] [--tag <label>] [--max-file-bytes <n>] <path1> [path2 ...]
---

# Bulk-ingest local files into vornik RAG memory

The user wants to deposit local file content into the project's RAG
memory at scale — the synchronous `remember()` MCP tool caps at 64 KiB
per call and burns CLIENT tokens, so this slash command stages each
file as an `inputArtifact` (base64 POST, below) and delegates the
`companion-rag-ingest` workflow, which lands chunks through the full
`IngestText` pipeline (gate stack, embedding, classifier).

The base64 POST means the file bytes never enter THIS (client) model's
token stream — you only see a `task_id`. As of the 2026-06-11 workflow
rewrite the ingest is **deterministic and agent-free**: the
`companion-rag-ingest` workflow has no agent step, and the executor's
`handleSuccess` hook ingests each staged input artifact directly by ID
through the `IngestText` pipeline (`ingest_input_artifacts: true`). This
removed the LLM from the copy loop entirely — a weak `rag-ingester`
model used to claim it had copied files it never wrote (round-tripping
large files through its context, or hallucinating success), which made
ingests fail and forced operators to retry or "try a different tool"
(the 260 KiB CHANGELOG that failed 6×). File size and model quality are
now irrelevant to whether the copy lands.

- The only hard ceiling is the **600 KiB total MCP body** (base64
  inflates raw bytes ~1.33x). A `--max-file-bytes <n>` per-file sanity
  cap (default 512 KiB) is also enforced; raise it if you have a single
  large doc under the total cap.
- **One doc per task** is still reasonable hygiene for large/multi-file
  sets — it isolates a transient failure (network, store) to one file
  rather than the whole batch — but the per-file copy is no longer a
  failure source.

User's arguments: `$ARGUMENTS`

!`ARGS_FILE="$(mktemp "${TMPDIR:-/tmp}/vornik-ragingest-args.XXXXXX")"
trap 'rm -f "$ARGS_FILE"' EXIT
cat >"$ARGS_FILE" <<'VORNIK_RAGINGEST_ARGS_EOF'
$ARGUMENTS
VORNIK_RAGINGEST_ARGS_EOF
python3 - "$ARGS_FILE" <<'PYEOF'
import sys, os, json, base64, shlex, urllib.request, urllib.error

# Args arrive via a file written by a single-quoted heredoc so the shell never
# parses quotes, parens, or em-dashes in a path or tag. (2026-06-26 bug: a raw
# ARGUMENTS value inside a double-quoted shell word broke on special chars
# before Python ran.) shlex then word-splits the reconstructed line as a shell would.
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
    print('usage: /rag-ingest [--scope <token>] [--tag <label>] <path1> [...]')
    sys.exit(1)

scope_arg = None
tag_arg = "default"
# Per-file sanity cap. Since the 2026-06-11 workflow rewrite the executor
# ingests staged input artifacts deterministically by ID (no agent copy),
# so file size no longer affects whether the copy lands — the real ceiling
# is the 600 KiB total MCP body below. This per-file cap is just a sanity
# bound (a single file can't exceed the
# total body anyway); raise with --max-file-bytes for a large lone doc.
per_file_limit = 512 * 1024
paths = []
i = 0
while i < len(tokens):
    t = tokens[i]
    if t == "--max-file-bytes":
        if i + 1 >= len(tokens):
            print("error: --max-file-bytes requires a value")
            sys.exit(1)
        try:
            per_file_limit = int(tokens[i + 1])
        except ValueError:
            print("error: --max-file-bytes must be an integer number of bytes")
            sys.exit(1)
        if per_file_limit <= 0:
            print("error: --max-file-bytes must be positive")
            sys.exit(1)
        i += 2
        continue
    if t == "--scope":
        if i + 1 >= len(tokens):
            print("error: --scope requires a value")
            sys.exit(1)
        scope_arg = tokens[i + 1]
        i += 2
        continue
    if t == "--tag":
        if i + 1 >= len(tokens):
            print("error: --tag requires a value")
            sys.exit(1)
        tag_arg = tokens[i + 1]
        i += 2
        continue
    paths.append(t)
    i += 1

if not paths:
    print("error: at least one file or directory path is required")
    print('usage: /rag-ingest [--scope <token>] [--tag <label>] <path1> [...]')
    sys.exit(1)

# Expand directories to their contained files (one level deep — recurse
# only when the user explicitly passes nested paths). Skips dotfiles
# because RAG ingest of .git/etc. is never what the user wants here.
def collect(path):
    p_abs = os.path.abspath(os.path.expanduser(path))
    if not os.path.exists(p_abs):
        print(f"error: path does not exist: {path}")
        sys.exit(1)
    if os.path.isfile(p_abs):
        return [p_abs]
    if os.path.isdir(p_abs):
        out = []
        for root, dirs, files in os.walk(p_abs):
            # Skip dotted directories in-place so we don't descend into them.
            dirs[:] = [d for d in dirs if not d.startswith(".")]
            for f in files:
                if f.startswith("."):
                    continue
                out.append(os.path.join(root, f))
        if not out:
            print(f"error: directory contains no ingestible files: {path}")
            sys.exit(1)
        return out
    print(f"error: not a regular file or directory: {path}")
    sys.exit(1)

expanded = []
for p in paths:
    expanded.extend(collect(p))

# repo_scope resolution mirrors /upload exactly.
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
        # No remote → repo toplevel basename; not a git repo → current
        # folder basename. Never empty, so the scope can't degrade to
        # "none"/project-wide. Mirrors session-start.sh + /upload.
        try:
            top = subprocess.run(
                ["git", "rev-parse", "--show-toplevel"],
                capture_output=True, text=True, timeout=2, check=False,
            )
            base = os.path.basename(top.stdout.strip())
            return base or os.path.basename(os.getcwd())
        except (subprocess.SubprocessError, FileNotFoundError):
            return os.path.basename(os.getcwd())
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
    if ":" in url and "/" not in url.split(":", 1)[0]:
        url = url.replace(":", "/", 1)
    return url

repo_scope = resolve_scope()

# Same 600 KiB soft cap as /upload (companion MCP body is 1 MiB
# raw; base64 inflates 1.33x; ~25% headroom for JSON envelope).
SOFT_LIMIT_BYTES = 600 * 1024

files = []
total_bytes = 0
seen_names = {}
for p_abs in expanded:
    try:
        with open(p_abs, "rb") as fh:
            data = fh.read()
    except OSError as e:
        print(f"error: cannot read {p_abs}: {e}")
        sys.exit(1)
    if len(data) > per_file_limit:
        print(f"error: {os.path.basename(p_abs)} is {len(data):,} bytes, over the "
              f"{per_file_limit:,}-byte per-file sanity cap.")
        print("Ingest is deterministic (the executor reads staged artifacts by ID since the")
        print("2026-06-11 rewrite), so size no longer affects the copy — but a single file still")
        print("can't exceed the 600 KiB total MCP body. Raise the cap with --max-file-bytes <n>")
        print("(up to the total-body limit) or ingest the file alone.")
        sys.exit(1)
    total_bytes += len(data)
    # Collisions: when a directory walk hits multiple files of the same
    # basename, disambiguate with parent-dir prefix. Without this, the
    # rag-ingester would overwrite its OUTPUT artifacts mid-loop.
    base = os.path.basename(p_abs)
    if base in seen_names:
        parent = os.path.basename(os.path.dirname(p_abs))
        base = f"{parent}__{base}"
    seen_names[base] = True
    files.append({"name": base, "content": base64.b64encode(data).decode("ascii")})

if total_bytes > SOFT_LIMIT_BYTES:
    print(f"error: total file size {total_bytes:,} bytes exceeds the {SOFT_LIMIT_BYTES:,}-byte")
    print("soft limit (companion MCP caps the JSON body at 1 MiB; base64 inflates 1.33x).")
    print(f"Trim the file set ({len(files)} file(s) collected) or split across multiple")
    print("/rag-ingest calls.")
    sys.exit(1)

url_base = os.environ.get("VORNIK_URL", "").rstrip("/")
token = os.environ.get("VORNIK_COMPANION_TOKEN", "")
if not url_base or not token:
    print("error: VORNIK_URL and VORNIK_COMPANION_TOKEN must be set in this shell")
    print("(the same env the .mcp.json plugin config uses — see contrib/claude-code-companion/README.md)")
    sys.exit(1)
url = url_base + "/api/v1/mcp/companion"

delegate_args = {
    "workflow": "companion-rag-ingest",
    # The tag is embedded in the prompt as the canonical token
    # "tag=<value>" on its own line. The rag-ingester role's prompt
    # parses this token; delegate doesn't currently plumb structured
    # "payload.tag" through to context.payload, and the round-trip
    # would add a generic key-injection surface for a one-shot field.
    # Keeping it in the prompt is simpler and traceable in the audit
    # log. Inline-code backticks intentionally avoided in this
    # comment block: the surrounding markdown invocation wraps the
    # whole Python source in a single backtick code-span, so a stray
    # backtick here closes that span early and breaks heredoc parsing
    # (real incident, 2026-05-28).
    "prompt": f"Ingest {len(files)} file(s) into project RAG memory.\ntag={tag_arg}",
    "inputArtifacts": files,
    # Critical: skip_auto_extract MUST be True. Auto-extract would
    # chunk + ingest the file at upload time AND mark it as already-
    # extracted, which makes the rag-ingester skip staging the raw
    # file (the B-10 failure mode). For this workflow specifically,
    # the WHOLE POINT is for the rag-ingester to see the raw file
    # in its container and copy it to artifacts/out/.
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
    print(content[0]["text"])
    sys.exit(0)

print(f"Staged {len(files)} file(s), {total_bytes:,} bytes total (tag={tag_arg!r}).")
scope_msg = f" (repo_scope={repo_scope!r})" if repo_scope else " (no repo_scope)"
print(f"Delegated companion-rag-ingest on project={inner.get('project', '?')!r}{scope_msg}.")
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
- A one-line summary of what was queued (file count + tag).
- The next step (`/status <task_id>` if they want to poll;
  otherwise the SessionStart hook will surface the ingest digest next
  time).

Do NOT call `mcp__vornik__delegate` again. The bash already did it; a
second call would duplicate the task. If the bash output starts with
`error:`, surface the error verbatim and stop.
