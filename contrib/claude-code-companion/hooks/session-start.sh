#!/usr/bin/env bash
# SessionStart hook — pulls every companion task that completed since the
# user's last session ended and emits a digest as additionalContext.
#
# Claude Code reads stdout from hooks; lines marked with the
# additionalContext convention land in the model's session prompt as
# ambient context. The hook MUST be fast (Claude Code times out long
# hooks) and MUST NOT error noisily — if the vornik daemon is offline
# or the token is wrong we silently degrade to "no digest".
#
# Configured environment:
#   VORNIK_URL              — daemon base URL (default http://localhost:8080)
#   VORNIK_COMPANION_TOKEN  — companion bearer; required
#
# The hook is deliberately a small jq pipeline so operators can read
# what it's doing without reverse-engineering a binary.

set -uo pipefail

VORNIK_URL="${VORNIK_URL:-http://localhost:8080}"
TOKEN="${VORNIK_COMPANION_TOKEN:-}"

# Degrade gracefully when the plugin isn't fully configured.
if [[ -z "$TOKEN" ]]; then
  exit 0
fi

# ----- Per-repo scope resolution (LLD migration 75) -------------------
# Walk up from $PWD looking for a git repo. If found, normalise the
# remote URL (or repo basename when no remote) to a stable token that
# the daemon can index. The token is plumbed onto recent_memory() at
# session start AND surfaced to Claude as a directive so it stamps
# repo_scope on every subsequent recall / remember / delegate.
#
# Token convention (matches the chunk-side semantics):
#   - origin = https://github.com/X/Y(.git) → "github.com/X/Y"
#   - origin = git@github.com:X/Y(.git)     → "github.com/X/Y"
#   - origin = ssh://git@host/path          → "host/path"
#   - repo with no remote → repo basename
#   - not in a git repo → current folder basename (never empty, so a
#     bare working directory still gets a stable per-folder scope
#     instead of falling through to uncategorized/NULL "none")
#
# Operator override: VORNIK_REPO_SCOPE in the shell pins a specific
# scope (handy for "*" cross-cutting or for non-git workspaces).
resolve_repo_scope() {
  if [[ -n "${VORNIK_REPO_SCOPE:-}" ]]; then
    printf '%s' "$VORNIK_REPO_SCOPE"
    return
  fi
  local remote_url
  remote_url=$(git -C "$PWD" config --get remote.origin.url 2>/dev/null) || true
  if [[ -n "$remote_url" ]]; then
    remote_url="${remote_url%.git}"
    # Strip scheme first so any embedded "user@" sits at the start.
    remote_url="${remote_url#https://}"
    remote_url="${remote_url#http://}"
    remote_url="${remote_url#ssh://}"
    # Then strip any "<user>@" prefix — git@, gitlab@, custom-bot@,
    # https://user@host — anything that's a user separator BEFORE
    # the first ":" or "/". This is what the /vornik-{upload,rag-
    # ingest} Python resolvers do; the shell side has to match so
    # session-start scope and slash-command scope can't drift.
    if [[ "$remote_url" == *"@"* ]]; then
      local before_at="${remote_url%%@*}"
      # Confirm the "@" actually precedes the first ":" or "/".
      # Without this guard, "host/path/user@artifact" would get
      # mangled (unlikely in a git remote, but cheap to protect).
      if [[ "$before_at" != *":"* && "$before_at" != *"/"* ]]; then
        remote_url="${remote_url#*@}"
      fi
    fi
    remote_url="${remote_url/://}"
    printf '%s' "$remote_url"
    return
  fi
  local toplevel
  toplevel=$(git -C "$PWD" rev-parse --show-toplevel 2>/dev/null) || true
  if [[ -n "$toplevel" ]]; then
    printf '%s' "$(basename "$toplevel")"
    return
  fi
  # Not a git repo at all (no origin, no toplevel). Fall back to the
  # current folder name so the scope is never empty — an empty scope
  # surfaces to the operator as context "none" and silently routes
  # recall/remember/delegate to project-wide. A per-folder token is a
  # better default; operators who want cross-cutting set
  # VORNIK_REPO_SCOPE="*" explicitly.
  printf '%s' "$(basename "$PWD")"
}
REPO_SCOPE=$(resolve_repo_scope)

# Resolve the MCP server URL. We hit the JSON-RPC tools/call surface
# directly rather than going through Claude — the hook isn't an MCP
# client of its own.
MCP_URL="$VORNIK_URL/api/v1/mcp/companion"

# build_digest emits the digest's markdown to stdout. Wrapped in a
# function so the caller can capture it once at the end and emit the
# v2-style JSON envelope ({hookSpecificOutput: {hookEventName:
# "SessionStart", additionalContext: <markdown>}}) that Claude Code
# v2.1+ requires to render in the welcome banner. Pre-refactor (raw
# stdout) was reaching the model's context but not the banner — see
# learning-output-style hook for the canonical reference shape.
#
# Inside the function, "return" replaces the old "exit 0" — function
# returns terminate this digest cleanly without killing the parent's
# trailing envelope-emit step.
build_digest() {
  # Build the JSON-RPC envelope for tools/call name=list with limit=20.
  local payload
  payload=$(cat <<'EOF'
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": {
    "name": "list",
    "arguments": { "limit": 20 }
  }
}
EOF
  )

  # 5s connect timeout, 10s total. If the daemon is slow, drop the
  # digest — Claude Code is already waiting on us.
  local response
  response=$(curl -sS \
    --connect-timeout 5 \
    --max-time 10 \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $TOKEN" \
    -X POST "$MCP_URL" \
    -d "$payload" 2>/dev/null) || return 0

  # Pull the tool's structured text out of the JSON-RPC envelope.
  # tools/call returns { result: { content: [{ type: "text", text: "..." }] } }
  local text
  text=$(printf '%s' "$response" | jq -r '.result.content[0].text // empty' 2>/dev/null)
  if [[ -z "$text" ]]; then
    return 0
  fi

  # The text is itself a JSON document. Filter to terminal tasks
  # only — in-flight work isn't actionable in a SessionStart digest,
  # the user can `/peek` to see those.
  local completed count
  completed=$(printf '%s' "$text" | jq -c '[.tasks[] | select(.status == "COMPLETED" or .status == "FAILED")]' 2>/dev/null)
  count=$(printf '%s' "$completed" | jq 'length' 2>/dev/null)

  if [[ -n "$count" && "$count" != "0" ]]; then
    echo "## vornik-companion: ${count} delegation(s) finished since your last session"
    echo
    printf '%s' "$completed" | jq -r '.[] |
      "- **\(.status)** \(.workflow) — `\(.task_id)` (created \(.created_at))"' 2>/dev/null
    echo
    echo "Pull any of these with \`/vornik-companion:result <task_id>\`. Use \`/vornik-companion:peek\` for the full list including in-flight work."
    echo
  fi

  # ----- Project memory digest (LLD 22 Phase 2) -----------------------
  # Best-effort: ask the daemon for the most recently-touched RAG
  # entries in this key's project. The host LLM opens the session
  # knowing what the project just learned, without having to call
  # recall() manually.
  #
  # Silent-skip on every failure mode: key lacks memory_read, daemon
  # returns an error, jq parse fails, etc. The delegations digest
  # above is the load-bearing output; this is enrichment.
  local recent_payload recent_response recent_error recent_text recent_entries recent_count
  recent_payload=$(jq -n --arg scope "$REPO_SCOPE" '{
    jsonrpc: "2.0",
    id: 2,
    method: "tools/call",
    params: {
      name: "recent_memory",
      arguments: (
        { limit: 5 }
        + (if $scope == "" then {} else { repo_scope: $scope } end)
      )
    }
  }')
  recent_response=$(curl -sS \
    --connect-timeout 5 \
    --max-time 10 \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $TOKEN" \
    -X POST "$MCP_URL" \
    -d "$recent_payload" 2>/dev/null) || recent_response=""

  if [[ -n "$recent_response" ]]; then
    recent_error=$(printf '%s' "$recent_response" | jq -r '.result.isError // false' 2>/dev/null)
    if [[ "$recent_error" != "true" ]]; then
      recent_text=$(printf '%s' "$recent_response" | jq -r '.result.content[0].text // empty' 2>/dev/null)
      if [[ -n "$recent_text" ]]; then
        recent_entries=$(printf '%s' "$recent_text" | jq -c '.entries // []' 2>/dev/null)
        recent_count=$(printf '%s' "$recent_entries" | jq 'length' 2>/dev/null)
        if [[ -n "$recent_count" && "$recent_count" != "0" ]]; then
          if [[ -n "$REPO_SCOPE" ]]; then
            echo "## vornik-companion: project memory — last ${recent_count} chunk(s) in repo \`${REPO_SCOPE}\`"
          else
            echo "## vornik-companion: project memory — last ${recent_count} chunk(s) (no repo scope detected)"
          fi
          echo
          printf '%s' "$recent_entries" | jq -r '.[] |
            "- **\(.content_class // "unclassified")** \(.source_name) (\(.created_at))\n  \(.snippet)"' 2>/dev/null
          echo
          echo "Drill in with \`/vornik-companion:recall <query>\`."
        fi
      fi
    fi
  fi

  # Emit the scope directive AFTER the digests so Claude reads the
  # context first then the rule. Without this nudge the host LLM has
  # no way to know that calls without repo_scope will pollute other
  # repos' RAG. We make it a hard rule in the prose so Claude treats
  # it as a load-bearing instruction, not a suggestion. We emit the
  # directive even when both digests were empty — the operator may
  # still issue a manual recall in the new session and we want the
  # scope rule active anyway.
  if [[ -n "$REPO_SCOPE" ]]; then
    cat <<EOF

## vornik-companion: repo scope auto-detected

You are in the **\`${REPO_SCOPE}\`** repo. The companion project's RAG
serves multiple repos for this operator — when you call any of:
  - \`mcp__vornik__recall\`
  - \`mcp__vornik__remember\`
  - \`mcp__vornik__recent_memory\`
  - \`mcp__vornik__delegate\`
…pass \`repo_scope: "${REPO_SCOPE}"\` so deposits land tagged and
recalls don't surface unrelated repos' chunks.

Pass \`repo_scope: "*"\` instead when a deposit / recall genuinely
spans repos (e.g. an operator-wide note about their toolchain).
Omit \`repo_scope\` entirely (the rare opt-out) only when you
explicitly want project-wide search regardless of repo.
EOF
  fi
}

# Capture the digest's markdown into a single variable, then emit
# one JSON envelope at the end. Claude Code v2.1+ reads this envelope
# from stdout; the additionalContext field is what shows in the
# welcome banner AND lands in the model's context. Raw markdown to
# stdout still reaches context (proven 2026-05-28) but doesn't
# render in the banner.
DIGEST=$(build_digest)

# ----- Knowledge-skill self-authoring directive (learning-loop E2) ----
# Vornik cannot see this session's transcript — only the MCP calls this
# client makes. So capturing reusable know-how has to be client-driven.
# Plant the directive at session start (reliable — the model carries it
# through the whole session) rather than a fragile stop-hook re-prompt.
# The proposed skill is a DRAFT; an operator approves it (e.g. from a
# Telegram review prompt) before it activates. The transcript never
# leaves this machine — only the skill_propose call (the draft) does.
# NOTE: the heredoc lives in a function rather than directly inside a
# $(cat <<EOF ...) command substitution. bash 3.2 — still /bin/bash on macOS —
# scans for the closing paren of $( ) WITHOUT honoring heredoc bodies, so a
# prose apostrophe ("it's") inside the body is read as an opening quote and the
# whole script dies with "unexpected EOF while looking for matching `'''". A
# fresh macOS install hit exactly that on 2026-08-04 (line 333). Same safe
# shape as build_digest() above.
emit_skill_directive() {
  cat <<EOF

## vornik: capture reusable know-how as skills

When you and the operator work out a **durable, reusable, project-relevant
procedure** this session — a troubleshooting flow, a non-obvious fix, a
deploy/verify sequence worth doing the same way next time — proactively
capture it as a vornik knowledge skill:

1. First \`mcp__vornik__skill_search\` (repo_scope \`${REPO_SCOPE}\`) to
   avoid duplicating an existing skill.
2. If it's genuinely new, call \`mcp__vornik__skill_propose\` with a
   kebab-case \`name\`, a one-line \`description\` (WHEN to apply it), a
   Markdown \`body\` (the actual steps/checks/anti-patterns), and
   \`repo_scope: "${REPO_SCOPE}"\`.

It lands as a **draft** and does NOT take effect until an operator
approves it — so propose freely, but only for things genuinely worth
reusing. Do NOT propose trivial, one-off, or secret-bearing content.
EOF
}
SKILL_DIRECTIVE=$(emit_skill_directive)
DIGEST=$(printf '%s\n%s' "$DIGEST" "$SKILL_DIRECTIVE")

# ----- RAG-first directive (LLD 2026-07-12-companion-rag-first-guidance) -----
# The "recall before spelunking code" rule used to live only in the operator's
# per-user CLAUDE.md, so it didn't ship with the plugin and didn't help other
# operators. Plant it here as a session-start default. It is a PREFERENCE, not
# an unconditional hook — trivial literal lookups skip it. The trust boundary
# is class-scoped (design/spec/decision chunks are authoritative; other content
# is a hypothesis) so a poisoned chunk from a failed task isn't asserted as fact.
# Function-wrapped for the same bash 3.2 reason as emit_skill_directive:
# "doesn'''t" in the body below would otherwise break the parse.
emit_rag_first_directive() {
  cat <<'EOF'

## vornik-companion: RAG-first — recall BEFORE reading code on design questions

For any **design / architecture / roadmap / "how does X work" / "why is it
built this way"** question — or a bug whose symptom or incident you suspect has
been seen before — call `recall` BEFORE you start reading code. Treat
**design / spec / decision-class** chunks (the LLDs in project memory) as the
authoritative design record; read code only to verify a named file/flag/
function a recalled note points at still matches (RAG freezes facts at write
time). **Other content** (research, incident notes, companion notes) is a
hypothesis to verify against current state — do not assert it as fact. Before
trusting a recalled chunk as design truth, check its class/provenance.

Trigger list (recall first):
- "How does <subsystem> work?" / "Why is <feature> built this way?"
- roadmap, status, architecture, or design-decision questions
- a bug that doesn't reproduce, a surprise behavior, or an error that looks
  like a known incident
- before a deep code dive (>2-3 tool calls into one subsystem — a package,
  module, or related file group under `internal/` or `src/`)

Worked example: asked to fix a daemon bug, `recall "<subsystem> design"` returns
the LLD naming the seam (file:line) + the rationale; you then read ONLY that
file to confirm the symbol still exists, instead of reverse-engineering the
architecture from a dozen files. The project-memory digest above (recent
chunks) is a hint: if a recent chunk looks relevant, recall its topic.

This is a preference, not a gate. If you know the exact file path and the query
is a literal string search (grep/cat), skip recall. Anything involving
architecture, history, or design rationale → recall first. For a failing unit
test that points at a typo in a single file, fix directly — the RAG-first rule
is for questions whose answer lives in design intent, not code syntax.
EOF
}
RAG_FIRST_DIRECTIVE=$(emit_rag_first_directive)
DIGEST=$(printf '%s\n%s' "$DIGEST" "$RAG_FIRST_DIRECTIVE")

if [[ -n "$DIGEST" ]]; then
  jq -n --arg ctx "$DIGEST" '{
    hookSpecificOutput: {
      hookEventName: "SessionStart",
      additionalContext: $ctx
    }
  }'
fi
