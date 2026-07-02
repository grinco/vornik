# AGENTS.md — Vornik runbook for AI agents

You are an AI agent (Claude Code, Codex, Gemini CLI, …) and your user asked
you to set up Vornik, connect it to an LLM, or work on this codebase. This
file is written for you: every step is a command plus a check. Run the
command, verify the check, and only then continue. If a check fails, stop
and triage with the step's hints — do not push on past a failed check.

**Ground rules**

1. **Ask before privileged commands.** Package installs (`sudo dnf/apt/…`)
   and `loginctl enable-linger` change the system — surface them to your
   user before running. Everything else below is user-scoped.
2. **Never echo or commit secrets.** LLM API keys and `sk-vornik-*` bearer
   tokens go only into the files named below, never into chat output, shell
   history you display, or git.
3. **Target platform is Linux with rootless Podman.** On macOS/Windows,
   stop and point your user at `docs/public/getting-started.md`.

---

## Runbook A — zero → hello world

### A1. Install the stack

One command builds the daemon + CLI (in an ephemeral container — no Go
toolchain needed), starts PostgreSQL + pgvector in a container, and runs the
daemon as a rootless `systemctl --user` service. It may `sudo`-install
missing prerequisites — warn your user first (rule 1):

```bash
curl -fsSL https://get.vornik.io | bash
# Already inside a Vornik checkout? Use it as the build context instead:
VORNIK_SKIP_FETCH=1 VORNIK_DIR="$PWD" bash deployments/podman/quickstart.sh
```

**Check:** the script ends with `Vornik is up and ready.`, and:

```bash
curl -fsS http://localhost:8080/readyz
# → {"status":"ready","checks":[{"name":"database","status":"ok"}]}
```

**If not:** `journalctl --user -u vornik -e` — the loader names any config
placeholder that is empty. Triage table: `deployments/podman/README.md`.

Binaries land in `~/.local/bin` (`vornik`, `vornikctl`). If `vornikctl` is
not found, prefix commands with `~/.local/bin/` or fix `PATH`.

### A2. Connect an LLM (the setup API)

The first-run web guide at `http://localhost:8080/ui/setup` and this API
are the same flow; as an agent, drive the API. Ask your user for their
OpenAI-compatible endpoint, model ID, and API key (rule 2), then:

```bash
SID=$(curl -fsS -X POST http://localhost:8080/api/v1/setup/session \
  -H 'Content-Type: application/json' -d '{}' | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')

# Validate first — this makes a live call to the provider:
curl -fsS -X POST "http://localhost:8080/api/v1/setup/session/$SID/validate" \
  -H 'Content-Type: application/json' \
  -d '{"endpoint":"<ENDPOINT>","api_key":"<KEY>","model":"<MODEL>"}'
```

**Check:** the response reports the endpoint and model reachable. Fix
endpoint/key/model until it does — committing a broken config wastes a
restart.

```bash
# Commit chat, then resolve the memory step (disabled is a supported,
# sensible default — enable later from /ui/setup with an embedding endpoint):
curl -fsS -X POST "http://localhost:8080/api/v1/setup/session/$SID/commit" \
  -H 'Content-Type: application/json' \
  -d '{"endpoint":"<ENDPOINT>","api_key":"<KEY>","model":"<MODEL>"}'
curl -fsS -X POST "http://localhost:8080/api/v1/setup/session/$SID/memory/commit" \
  -H 'Content-Type: application/json' -d '{"enabled":false}'
```

This writes the key to `~/.config/vornik/secrets/chat.env` (the daemon
sources that directory itself on start) and patches
`~/.config/vornik/config.yaml`. Manual fallback: set `VORNIK_CHAT_API_KEY`,
`CHAT_ENDPOINT`, `CHAT_MODEL` in `~/.config/vornik/vornik.env`.

### A3. First project + dispatcher

```bash
vornikctl init project --template personal-assistant assistant \
  --config-dir "$HOME/.config/vornik/configs"
vornikctl config reload    # loads the new project into the running registry
curl -fsS -X POST http://localhost:8080/api/v1/setup/dispatcher \
  -H 'Content-Type: application/json' -d '{"project_id":"assistant"}'
```

**Check:** `vornikctl project list` shows `assistant`, and the dispatcher
commit returns `"committed":true`.

### A4. One restart, then verify

Config from A2/A3 activates at process start:

```bash
systemctl --user restart vornik
curl -fsS http://localhost:8080/readyz        # ready again (poll ~30s)
vornikctl doctor                              # feature readiness incl. chat
curl -fsS http://localhost:8080/api/v1/setup/status
# → "fresh_install":false — onboarding is complete
```

### A5. Hello world

```bash
vornikctl task submit -p assistant --prompt "Say hello and introduce yourself."
# Poll the returned task ID (first run pulls/warms the agent container — allow a few minutes):
vornikctl task get <taskId> -p assistant
vornikctl task tail <taskId> -p assistant
```

**Check:** the task reaches `COMPLETED` and `task get` shows the agent's
answer. Show your user the result and the chat UI:
`http://localhost:8080/ui/projects/assistant/chat`.

---

## Runbook B — wire yourself in (companion + RAG)

Vornik can serve **you**: an MCP endpoint for delegating async work to
swarm agents and a per-project RAG memory you can `remember`/`recall`
across sessions. Set it up for yourself after Runbook A.

### B1. Capability check

```bash
curl -fsS http://localhost:8080/api/v1/capabilities
```

**Check:** `features.companion-v1` and `features.companion-mcp` are `true`.

### B2. Companion project + scoped key

```bash
vornikctl init project --template companion "companion-$USER" \
  --config-dir "$HOME/.config/vornik/configs"
vornikctl config reload

vornikctl companion grant \
  --project="companion-$USER" \
  --client=claude-code \
  --label="$(hostname)/$USER" \
  --workflows=companion-architectural-review,companion-doc-review,companion-research-gather,companion-report-summarize,companion-rag-ingest \
  --budget-usd=25 \
  --memory-all
```

**Check:** the grant prints a `sk-vornik-…` secret — it is shown once.
Store it per rule 2 (next step), never in your transcript.

### B3. Install the plugin

The Claude Code plugin ships in this repo (`contrib/claude-code-companion`;
Codex adapter: `contrib/codex-companion`). Have your user add to their
shell profile:

```bash
export VORNIK_URL="http://localhost:8080"
export VORNIK_COMPANION_TOKEN="<the sk-vornik-… secret>"
```

Then, inside Claude Code:

```
/plugin marketplace add <path-to-this-checkout>
/plugin install vornik-companion@vornik
```

Full install options and the MCP wiring:
`contrib/claude-code-companion/README.md`.

### B4. RAG round-trip smoke test

You can verify the memory path over plain HTTP before a session reload —
the plugin's MCP endpoint is stateless JSON-RPC:

```bash
MCP="$VORNIK_URL/api/v1/mcp/companion"
AUTH="Authorization: Bearer $VORNIK_COMPANION_TOKEN"

# remember a canary…
curl -fsS -X POST "$MCP" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "jsonrpc":"2.0","id":1,"method":"tools/call",
  "params":{"name":"remember","arguments":{
    "content":"AGENTS.md smoke canary: companion RAG round-trip works on this install.",
    "tags":["agents-md-smoke"]}}}'

# …recall it (embedding is async — if the first recall misses, wait ~5s and retry once)
curl -fsS -X POST "$MCP" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "jsonrpc":"2.0","id":2,"method":"tools/call",
  "params":{"name":"recall","arguments":{"query":"AGENTS.md smoke canary"}}}'
```

**Check:** remember returns `"decision":"ALLOW"`; recall's hits include the
canary. Clean up after yourself so the canary never pollutes real recalls:

```bash
curl -fsS -X POST "$MCP" -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "jsonrpc":"2.0","id":3,"method":"tools/call",
  "params":{"name":"memory_correct","arguments":{
    "wrong_claim":"AGENTS.md smoke canary: companion RAG round-trip works on this install.",
    "max_refutes":1}}}'
```

After the plugin is installed, start a fresh session: your SessionStart
digest will list finished delegations and recent project memory, and the
`recall`/`remember`/`delegate` tools are available natively.

---

## Runbook C — working on the codebase

```bash
make build             # go build (daemon + CLI)
make test              # unit tests (-short)
make test-integration  # needs PostgreSQL — see README "Integration Tests"
make lint              # golangci-lint + LLD contract lint; run before every commit
```

Conventions that will get your PR rejected if skipped: tests first (TDD —
each bugfix carries a regression test that fails pre-fix), every touched
file keeps unit coverage, and `go test ./...` (not just `go build`) before
claiming a rename/deletion is safe. Start with `https://docs.vornik.io` and
`https://docs.vornik.io` for how the pieces fit; user-facing docs live in
`docs/public/`.
