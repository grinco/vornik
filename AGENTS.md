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

# Mint a key scoped to YOUR CLI. Native companion plugins exist for Claude Code
# and Codex only, so set --client to match: claude-code OR codex. If you are
# Codex, also pass --repo-scope=<canonical git remote, e.g. github.com/ORG/REPO>:
# Codex has no SessionStart scope injector, so the key default backstops any
# memory call that omits repo_scope (keeps chunks from landing NULL-scoped).
vornikctl companion grant \
  --project="companion-$USER" \
  --client=claude-code \
  --label="$(hostname)/$USER" \
  --workflows=companion-architectural-review,companion-doc-review,companion-research-gather,companion-report-summarize,companion-rag-ingest \
  --budget-usd=25 \
  --memory-all \
  --skill-all
```

`--memory-all` grants RAG `remember`/`recall`; `--skill-all` grants the
knowledge-skill store (`skill_read`/`write`/`admin`) — both are the
recommended default for a companion project so you can capture reusable
procedures as **knowledge skills** and approve them without a second grant.
Drop `--skill-admin` (use `--skill-read --skill-write`) if you want proposals
to require a separate operator approval; drop `--skill-all` entirely for a
delegate-only key.

**Check:** the grant prints a `sk-vornik-…` secret — it is shown once.
Store it per rule 2 (next step), never in your transcript.

### B3. Install the plugin (Claude Code / Codex only)

Native companion plugins are packaged for **Claude Code** and **Codex** only.
First export the connection env in your shell profile (both clients):

```bash
export VORNIK_URL="http://localhost:8080"
export VORNIK_COMPANION_TOKEN="<the sk-vornik-… secret>"
```

**Claude Code** — the plugin ships in this repo (`contrib/claude-code-companion`).
Inside the CLI:

```
/plugin marketplace add <path-to-this-checkout>
/plugin install vornik-companion@vornik
```

Full options and MCP wiring: `contrib/claude-code-companion/README.md`.

**Codex** — the adapter ships in `contrib/codex-companion`. Register the
companion MCP endpoint:

```bash
codex mcp add vornik \
  --url http://localhost:8080/api/v1/mcp/companion \
  --bearer-token-env-var VORNIK_COMPANION_TOKEN
```

Codex has no slash-command / SessionStart layer, so there is no `/plugin`
step and no auto session digest — call the `mcp__vornik__*` tools directly
(`catalog`, `recall`, `delegate`, `remember`, …). Full options:
`contrib/codex-companion/README.md`.

**Any other CLI** (Gemini CLI, opencode, …): there is **no** native companion
plugin — packaging covers Claude Code and Codex only. Skip this step and drive
the companion over plain HTTP (B4); the `remember` / `recall` / `delegate`
methods all work as stateless JSON-RPC, so every CLI can use them without a
plugin.

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

Once wired in, start a fresh session. On **Claude Code** the plugin's
SessionStart digest lists finished delegations and recent project memory, and
`recall`/`remember`/`delegate` are available as native tools. On **Codex** the
`mcp__vornik__*` tools are available natively (no session digest — call
`recent_memory` yourself to catch up). On any **other CLI**, keep using the
HTTP calls above.

### B5. Capture a knowledge skill (with `--skill-all`)

RAG memory is for **facts**; a **knowledge skill** is a reusable **procedure**
the agent should apply — a troubleshooting flow, a deploy sequence, a
non-obvious fix. Once an operator approves it, vornik injects it into swarm
roles AND serves it to every companion client, so you write it once and it's
available everywhere.

On **Claude Code**: `/skill-propose`, `/skill-search`, `/skill-approve`,
`/skill-set-global`. On **Codex**: the `skill_propose` / `skill_search` /
`skill_get` / `skill_set_global` MCP tools (see the `knowledge` skill). A
proposal lands as a `draft` and does not fire until approved (`skill_admin`).

**Cross-project (global) skills.** By default a skill only injects into its
home project. Mark it **global** — `skill_propose global:true`, or
`/skill-set-global <id>` / `vornikctl knowledge set-global <id>` — and it
injects into **every** project's roles, so a procedure captured in your
`companion-$USER` project reaches the autonomy roles (e.g. `janka`,
`assistant`) too. Global drafts are labelled "affects ALL projects" wherever
they're reviewed.

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

---

## Runbook D — updating an existing install

For a host installed via Runbook A, **upgrade in place** with the update
script — do NOT re-run the install one-liner to update. It rebuilds the
binaries in the same ephemeral golang container the quickstart uses (no host
Go), takes a DB dump + binary/config backup first, and prints exact rollback
commands.

```bash
cd ~/vornik/deployments/podman
./vornik-update.sh --check                  # is a newer tag available? (changes nothing)
./vornik-update.sh                          # upgrade to the newest tag (asks to confirm)
./vornik-update.sh --ref origin/main        # or track the tip of main
./vornik-update.sh --yes                    # skip the prompt (automation)
```

**Check:** the run ends with `Upgrade complete.` and prints `/readyz : ready`
plus the `DB migr : vN -> vM` bump.

```bash
curl -fsS http://localhost:8080/readyz      # ready again (poll ~30s)
vornikctl doctor
```

**If not:** the script prints ROLLBACK commands (restore the `*.prev` binaries
from `~/vornik-upgrade-backup-<UTC>/` and `git checkout` the prior commit) and
tails the journal. Full step-by-step + the optional daily "update available"
timer: `deployments/podman/UPDATING.md`.

**Ground rule 1 still applies:** the cutover restarts the service (interrupting
any running task); the script warns first unless you pass `--yes`.
