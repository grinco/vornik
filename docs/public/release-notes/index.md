---
sources:
    - path: docs/release-notes
      sha256: 6076dc0ddaedb79e565ecc8ddd8ba374484baf8d651c5d0383964a38188d010f
---
# Release Notes

A curated, reverse-chronological summary of user-facing changes in vornik.
Each entry highlights what changed for you as an operator — new features,
behavior changes, and notable fixes. Internal-only changes are omitted.

!!! tip "Upgrading"
    New configuration keys are additive and default to the previous behavior,
    so upgrades generally require no config changes. Always take a backup
    before upgrading. A few releases ask you to restart the daemon to pick up
    new behavior; those are called out below.

---

## 2026.8.6

**Upgrade if you run any role on Bedrock with a declared output schema — those
steps have been failing outright.** The agent's final turn deliberately offers
no tools, but the conversation still carried tool records, and Bedrock rejects
that pairing. The step produced no result at all. Fixed by converting those
records to plain text on the final turn.

**A failed step now tells you why it failed.** Previously almost every agent
failure was recorded as "the container exited non-zero", collapsing four
distinct faults into one label. Failures are now recorded with their actual
cause — a stuck tool loop, an exhausted tool budget, a context overflow, or a
broken output rule.

**Agents are now shown the output rules they are judged against.** A role can
require that, say, a claim of passing tests is accompanied by the list of cases
checked. Those rules were enforced after the fact and never communicated, so
work was rejected for a contract the agent had never seen. The rules are now
part of the agent's instructions and are checked before it finishes, with one
chance to correct.

**An agent that repeats itself is warned before the step is ended**, and an
agent that runs out of tool calls now produces its best answer instead of
nothing. The record still shows the budget was exhausted, so this improves the
output without hiding the cause.

**A fallback model gets its own time budget.** When a primary model was
unavailable, the replacement inherited a timeout sized for the original and
could not finish in it — so failover reliably failed. It now gets a budget of
its own.

**The daemon no longer refuses to start when a container-runtime check is
merely slow**, and benchmark runs now verify the model's real context window
against the endpoint instead of trusting configuration.

No configuration changes are required. If you pin per-model context windows,
confirm they match what your endpoint actually serves.

---

## 2026.8.5

**Upgrade if your tasks ever retry — retried work has not been billed to
anything.** A workflow step's spend was stored under an id that did not name the
execution, so when a task retried, the retry overwrote the row of the attempt it
retried. On a measured ledger, 24.8% of step-spend rows were missing, and 98.9%
of those were this collision. Budget enforcement, the spend dashboard and
per-key attribution all understated by the cost of every retry. Historical rows
cannot be recovered — the figures were overwritten, not merely hidden — but new
spend records correctly from this release.

**Cached prompt tokens are counted on streamed calls.** Streamed completions were
not asking for usage, so cache hits recorded nothing. Prefix caching on
self-hosted servers was already working and simply invisible.

**New: an adoption dashboard at `/ui/insights/adoption`.** Shows how much of the
product each team has actually exercised — companion usage, RAG queries, memory
writes and channel activity, not just task counts — and names the capabilities
nobody has tried yet. Designed to be useful on deployments that have *not*
enabled auth, where everything is attributed to nobody.

**Security.** Slack interaction responses are now pinned to Slack's own host, so
a forged payload cannot redirect a response elsewhere. Go 1.26.6 picks up fixes
for 7 standard-library CVEs.

**Rebuild your agent image** (`make build-agent`). The degenerate-loop diagnostic
used to assert the context window was exhausted; a measured run found a loop at
17% of context, so an operator following that text would raise a limit that was
never the constraint. It now reports the measured figure and says plainly when
context is *not* the cause.

---

## 2026.8.4

**Upgrade if you run open-weight models on your own hardware — agentic work did
not function at all.** Vornik sent a `response_format` directive alongside its
tool definitions. Any server implementing that with guided decoding then
constrains the model to emit schema-shaped JSON, so it can never emit a tool
call. Hosted APIs tolerated the combination; self-hosted endpoints did not.
Agents answered in a few tokens of prose, the loop recorded success, and the
step then failed its output contract. The directive is now withheld whenever
tools are offered.

**Timeouts no longer assume fast inference.** New optional
`speed_aware_timeouts` and `default_step_timeout` settings; both default to
previous behavior.

**Dashboard failures are counted by when they failed**, not by when their task
was created, so a spike lands on the day it happened.

**Rebuild your agent image** (`make build-agent`) — the `response_format`,
finalization and nudge fixes live in the container, not the daemon.

---

## 2026.8.3

**Upgrade if you use Bedrock embeddings — semantic search has been silently
keyword-only.** The query vector was never computed on that transport, so hybrid
search ran with no semantic arm while `memory/stats` still reported 100% embedded
and nothing was logged. Recall changes noticeably after upgrading, and reranker
cost appears where it previously did not.

**Embedding spend now appears in your spend rollups.** It was never recorded
before, on any provider — re-embedding a mid-sized corpus is roughly $0.60–0.75 on
a hosted embedder and was reported as `$0.00`. This is not new spend; it is spend
that was always happening and never billed. Look for the role **Memory · Embedder**
on `/ui/spend`. A `~` before a token count means the provider reported no token
count and the total is derived from text length rather than measured.

**New: `vornikctl bench memory`** — a retrieval-quality benchmark with a pinned
dataset, an LLM judge and a per-run journal. It is what found the defects above,
plus two more: recall was not reproducible (ranking ties broke arbitrarily), and
the embed queue could lose chunks across a restart, leaving them permanently
unretrievable by semantic search.

**The main dashboard gains a Learning tile** — skill-store maturity and the count
of proposed skills waiting on your review, which nothing else on the landing page
surfaced.

Also: filter memory on when content *happened* rather than when it was stored,
Cohere embeddings on Bedrock, `memory reembed --only-missing` for cheap gap
repair, answer a steering checkpoint from a Slack button, and near-duplicate
detection when a skill is proposed.

Migrations apply automatically and are additive; no configuration changes.

---

## 2026.8.2

**Upgrade if you use vornik in Slack direct messages.** 2026.8.0 stopped a Slack
bot answering DMs entirely — silently, with no reply and no visible error.
**Both 2026.8.0 and 2026.8.1 are affected.**

- **Fixed: Slack direct messages were dropped when `channel_allowlist` was empty.**
  2026.8.0 correctly made an unconfigured `channel_allowlist` deny rather than
  admit every channel in your workspace. But that new check ran *before* the
  long-standing rule that a channel allow-list never applies to a direct message,
  so it answered first and the DM rule never got a chance to.

    An empty `channel_allowlist` is not a mistake — it is the normal setup for a
    bot people talk to in DMs. Slack creates a DM's channel id the first time each
    person writes to the bot, so you cannot list it in advance; `sender_allowlist`
    is what controls a DM. Every deployment set up that way lost every DM, on
    messages, `/vornik`, file uploads and voice memos alike. Because both
    allow-list checks drop a message before doing any work, nothing appeared in
    Slack and no task was created — the bot simply went quiet.

    Direct messages are now exempt from `channel_allowlist` whether it lists
    channels or is empty. This does not loosen anything: `sender_allowlist` still
    gates DMs, and it also denies by default as of 2026.8.0, so a message from
    someone you have not listed is still refused.

    **If you worked around this by setting `allow_unlisted_senders: true`, remove
    it.** On an installation with an empty `channel_allowlist`, that flag also
    admits every channel in the workspace. You never needed it for DMs: with a
    populated `sender_allowlist`, it is not consulted there.

- **Fixed: the App Home tab promised channel access the bot did not have.** With
  no `channel_allowlist` configured, the **Where I answer** section said "any
  channel I have been added to" — which stopped being true in 2026.8.0, when an
  empty list started denying every channel. The first page a Slack user opens was
  telling them the bot would answer in places it silently ignored them. It now
  says what is actually configured, and what to ask an admin for.

- **Clarified: group DMs are not supported.** The direct-message exemption covers
  one-to-one DMs only. Adding the bot to a group DM produces silence rather than
  an answer; the App Home tab and the Slack guide now both say so, instead of
  leaving you to find out.

!!! tip "If a chat bot ever goes quiet"
    Both Slack allow-list checks log a warning and stop before any model is
    called, so a refused message leaves no trace in Slack and no task behind.
    Searching the daemon log for `not on installation allowlist` distinguishes an
    allow-list refusal from a genuine failure.

---

## 2026.8.1

**A security fix worth upgrading for.** Four built-in agent tools were available
to every role regardless of the tool allow-list configured for it, plus fixes for
a class of problem where a task finished successfully and its result never
reached you.

- **Security: four agent tools ignored per-role tool permissions.**
  `memory_search`, `skill_fetch`, `get_conversation_window` and
  `summarize_thread` were offered to, and usable by, every role — even roles whose
  `allowedTools` did not list them. All four are read-only (project memory,
  skills, conversation history), so this granted broader *read* access than you
  configured rather than any ability to run commands or write data. If you rely on
  role tool allow-lists to keep a role away from project memory, **upgrade**.
  Deliberately ungated tools are now recorded explicitly with a reason, so the
  distinction between "ungated on purpose" and "ungated by accident" is visible.
- **Fixed: a finished task whose result never arrived, permanently.** The record
  linking a task back to the chat it came from could be lost when a conversation
  turn ran long. Once lost, the completion message and the **Send** button both
  concluded the task had never come from a chat — and nothing ever repaired it.
  That record is now written independently of the turn's own timing.
- **Fixed: a delegated task's output was invisible on the task that requested it.**
  When a request was routed to a sub-task, the page you opened showed only the
  routing step's own small files and hid the actual deliverable. Output now
  appears on the requesting task, and can be sent from there. Artifacts stay
  strictly within their own project.
- **Fixed: an unhelpful error when a result could not be delivered.** "This task
  wasn't started from a chat channel" was shown even for tasks that plainly were.
  The message now distinguishes a task that genuinely has no chat origin from one
  whose origin record is missing, and offers a download link either way.
- **Fixed: Community Edition was told to run an Enterprise-only command.**
  `vornikctl report` pointed every reporter at `vornikctl support-report`, which
  Community Edition does not include. It now explains what Community collects and
  what to attach instead.
- **Fixed: PDF output is now guaranteed by tests.** The agent image ships the PDF
  toolchain and role prompts rely on it, but nothing verified it. A regression
  test now renders a real PDF, including non-ASCII text.

See [docs.vornik.io](https://docs.vornik.io) for the full documentation.

---

## 2026.8.0

**Vornik can authenticate to a remote MCP server on its own.** Connect a
tool server by granting consent in the browser instead of pasting a token
that expires. Plus a batch of fixes for things that were reporting success
while doing nothing.

- **New: OAuth 2.1 for MCP servers.** A remote MCP server that requires
  authentication is now connected by clicking **Connect** on the MCP tab (or
  `vornikctl mcp connect`). Vornik discovers the server's authorization
  requirements, registers itself, runs the consent flow, and refreshes the
  token as needed — telling you when a revoked grant needs reconnecting.
  Servers that use a static header or a subprocess credential are supported
  through the same `auth:` block. Verified against the published discovery
  behaviour of eighteen well-known MCP servers.
- **New: ask another Vornik a question.** A synchronous `consult` tool lets one
  deployment put a question to another and wait for the answer.
- **New: write to memory from a conversation.** Ask the assistant to remember
  something and it will, behind an explicit confirmation step and an opt-in per
  channel. Personal-scope notes carry their own expiry.
- **New: richer Slack.** Inbound images are understood, there is a real App Home
  tab, a turn that is thinking shows that it is working, threads can be followed
  up without a mention, `/vornik` works in a DM, and the slash-command name is
  per-deployment.
- **Behaviour change — chat allowlists now fail CLOSED.** An empty
  `allowed_users` (Telegram) or `sender_allowlist` / `channel_allowlist` (Slack)
  used to admit **everyone**. Both bots are publicly addressable, so an
  unconfigured allowlist was an open door that looked like an unset field. If you
  rely on the old behaviour, set `telegram.allow_unlisted_users: true` or Slack's
  `allow_unlisted_senders: true` — deliberately, and for development only.
  Telegram logs at boot which way it will behave.
- **Fixed: the knowledge-graph extractor was discarding most of its input.** A
  reasoning model was spending its whole token allowance thinking and returning
  nothing, and an empty answer was being recorded as "found no entities" and the
  chunk marked done — so the loss was permanent and silent. Truncation is now a
  distinct, counted outcome with bounded retries, and the graph stages request a
  low reasoning effort. **If you raised the graph token cap to work around this,
  put it back:** a bigger cap makes each failure more expensive, not less likely.
  Chunks already marked extracted do not self-heal — run
  `vornikctl memory regraph --project <id>` to reclaim them.
- **Fixed: email now tells you when a job finishes.** Previously it only did so
  for tasks created a particular way, and never if the daemon restarted in
  between. The notice no longer depends on a working model to compose itself.
- **Fixed: the control-plane proposal diff.** A configuration edit could render
  as a wall of deletions it was not making, because unrelated lines got
  reformatted and the diff had no positional pairing. It also had a size limit
  below the size of a real `config.yaml`, which made whole-file edits impossible
  to apply.
- **Fixed: adding an MCP server no longer needs a restart.** The change applied
  and reloaded, but every consumer of the server list was still reading the
  boot-time configuration — so the server did not exist and could not be
  connected.
- **Fixed: GDPR erasure reaches entangled records.** A memory chunk concerning
  several people can now be redacted and re-embedded rather than reported as
  deferred.
- **Fixed:** cron mode ignored `autonomy.workflow_id`; MCP subscriptions pinned a
  transport and broke stdio servers; host-filesystem MCP tools were not treated
  as mutating; the "Test endpoint" button called an authentication-protected
  server unreachable.

!!! note "New configuration keys"
    `telegram.allow_unlisted_users`, Slack `allow_unlisted_senders`. Both default
    to the safe value. See the configuration reference.

---

## 2026.7.7

**The assistant can see.** Send it a photo and it answers about the photo. Two
legal obligations also moved from documented to enforced-in-code.

- **New: media perception.** Images sent over any channel are now understood —
  answered directly when the dispatcher's model can see them, otherwise handed
  to a dedicated vision agent. Audio is transcribed; video yields a transcript
  plus sampled keyframes. Previously a photo produced an honest "I cannot see
  this", because the pixels were being discarded before they reached the agent.
- **Behaviour change: the dispatcher model is now `minimax-m3`.** It is
  multimodal, tool-use tuned, and **cheaper** than the model it replaces. If you
  pin Bedrock models on swarm roles, check `vornikctl doctor`'s
  `model_route_coverage` after upgrading — the default provider route moved, and
  Bedrock ids are now pinned explicitly rather than relying on it.
- **New: `vornikctl erase artifact <id>`.** Performs a complete GDPR Article 17
  erasure — the extraction rows, the memory chunks derived from them, and the
  stored files. Deleting an artifact alone never removed any of that: derived
  embeddings survived and merely lost the pointer back to their source. Shows
  you exactly what will go and asks before doing it.
- **New: AI-disclosure observability.** Serve and failure counts for the EU AI
  Act Article 50 notice are now exported, so you can show the obligation is
  being met and notice if it ever stops.
- **Fixed: the disclosure record can no longer be pruned.** It is the evidence
  that the notice was served; retention now refuses to touch it outright rather
  than merely omitting it from a list.
- **New: GDPR data-subject rights.** `vornikctl subject` registers a person,
  records what is held about them, and produces an Article 15 access or
  Article 20 portability report. Identity verification is required before
  anything is produced — an export handed to whoever asked would disclose that
  person's data to a stranger. Records that also concern other people are listed
  but their content is withheld, under Article 15(4).
- **New: breach ledger.** `vornikctl incident` records a personal-data breach and
  walks the Article 33/34 obligations, with the 72-hour clock running from when
  you became *aware* rather than when the breach happened. `vornikctl doctor`
  warns while there is still a day left and errors once the window closes.
- **New: retention guidance.** The sweeper still ships off — enabling it on
  upgrade would delete data you never agreed to lose — but the doctor now tells
  you plainly that the deployment keeps personal data indefinitely, and a
  recommended profile ships alongside your config to copy from. It also warns
  when the sweeper is on with no windows, and when the memory-chunk window is
  missing: long-term memory is the longest-lived personal data in the system.
- **Fixed: a model was billing at the wrong rate.** `kimi-k3` was available but
  unpriced, so its spend accrued at the default rate — the most expensive model
  on that route was the one being mis-costed.
- **Fixed: AI disclosure now covers two surfaces that are not channels.** The
  Article 50(1) notice was served on every chat channel, but two paths reach a
  person without being a channel and so bypassed it: code-review comments the
  platform posts on pull requests, and autonomous posts to a third-party API. Both
  now carry the notice. Review comments refuse to post if the disclosure is
  unavailable rather than publishing undisclosed, and a gateway provider can be
  marked as a **publication surface** — a write whose content lacks the notice is
  refused, with the exact wording to add, so the agent corrects itself and retries.
  Publication surfaces get their own wording, because "replies in this
  conversation" is untrue of something the system authored.
- **New: `vornikctl subject erase`.** Carries out an Article 17 erasure for a
  verified request. It shows the plan and asks before destroying anything, names
  each record's treatment, and lists what is retained under an exemption with the
  legal ground — a subject told "erased" while records remain has been misled.
  The Article 17(1) ground you record decides what happens to records that also
  concern other people: under most grounds they are preserved, but under 17(1)(d)
  or (e) the controller has no discretion to retain and the record goes in full.
  Uploaded files are removed properly — the derived text, the memory built from
  it, the stored file, and the row.
  **Known limitation, stated because it affects the response a subject receives:**
  removing one person's data from a record shared with others is not yet automated.
  Those records are identified and reported, and the request is deliberately left
  open rather than closed as complete.
- **Docs: the records of processing and the DPA template were re-verified against
  the code.** Both had gone stale in the direction that understates the platform —
  the processing record still said no data-subject identifier existed, and the DPA
  told a prospective controller that erasure was available only per project or per
  record. Corrected, along with what the platform still cannot warrant.

---

## 2026.7.6

**A conversation-channels release.** The bot now keeps its bearings in email and
Slack. **Two behaviour changes are visible to your users immediately** — Slack
reply placement and the shape of inbound email in the prompt. Read those two
items before upgrading.

- **Behaviour change: Slack replies to a top-level message now land in the
  channel, not in a thread.** A message sent inside a thread still gets an
  in-thread reply, so the bot mirrors wherever the person chose to speak.
  Deliberate: Slack threads become unfindable within days, so people follow up
  in the channel anyway, and burying every answer in a thread fought that.
  Expect a slightly busier channel and far less "where did the bot say that?".
- **Fixed: Slack conversations in a channel had no memory.** Every top-level
  message opened a brand-new empty session, so the bot had no recollection of
  earlier threads — or even of the message someone typed a minute earlier. All
  top-level messages in a channel now share one continuous conversation;
  threads keep their own.
- **New: the bot can reach a channel's earlier threads.** On a channel-level
  turn it sees a short digest of the channel's recent threads — opening
  question, latest answer, turn count, date — and can pull any one of them in
  full when the digest is not enough. Reads are scoped to the caller's own
  workspace and channel. Requires PostgreSQL; on SQLite, digests cover only the
  current process lifetime.
- **Behaviour change: inbound email now carries a `From:` and `Subject:` header
  on the model's turn.** If you pin project prompts to an exact inbound email
  shape, re-check them. Other channels are unchanged.
- **Fixed: the email subject line never reached the model.** An instruction
  written only in the subject — "add these books to rag", with the books
  attached and an empty body — was silently dropped, and the bot answered as if
  no instruction had been given.
- **Fixed: the bot read its own quoted replies as somebody else's words.** Mail
  clients quote the message being replied to, and the whole trailer was handed
  to the model, which had no idea what its own address was. Reply someone said
  "4" to a numbered list and the bot asked them what they meant. Quoted
  trailers are now trimmed (Gmail, Apple Mail, Thunderbird, Roundcube, Outlook
  including localised forms, forwards, plain `>` quotes), and the bot is told
  its own address and that quoted text from it is its own earlier turn.
- **Fixed: multi-party email threads were unattributable.** With two people on a
  thread the bot could not tell their messages apart. Each turn now records its
  sender.
- **Fixed: Slack retries no longer produce duplicate replies**, and Slack slash
  commands are handled as first-class inbound.
- **Fixed: inbound Slack and GitHub webhook events could reach a channel with no
  receiver attached** and land in logs only, when observability initialisation
  rebuilt the HTTP server after startup.
- **Fixed on Community edition: operator profiles.** The operator memory page
  renders and saves — its storage and UI route were not fully wired.
- **New operator skills for the Claude Code / Codex companion:**
  `configure-vornik` (find the tree the daemon actually reads, scaffold, then
  validate → reload → confirm), `troubleshoot-vornik` (route by symptom to the
  right diagnostic instead of improvising) and `report-problem` (file an
  anonymised upstream issue, with review before submit). These were described
  under 2026.7.5 but landed after that release was cut — 2026.7.6 is the first
  release that actually contains them.

Upgrading requires no configuration changes and adds no migrations. The Slack
and email changes take effect when you restart the daemon.

---

## 2026.7.5

**A security-and-governance release.** Two full security audits — the ten-day
contribution window and then the whole tree — found and fixed 37 issues, one
critical in each pass. **Upgrading is recommended.** Alongside that, you get
per-task spend caps, parallel workflow steps, and a safety net under automated
cost tuning. This release also adds **anonymous usage telemetry, enabled by
default** — read that item before upgrading.

- **New: anonymous usage telemetry, on by default.** Vornik now reports two
  lifecycle events — a successful install, and a project creation — to
  `telemetry.vornik.io`. What goes out: the event type, the release version, an
  OS and architecture category, which Vornik-owned path created the project, and
  for a project the built-in template name (or `custom`) plus whether autonomy
  was switched on. **Nothing else.** No installation identifier, hostname,
  username, project name, path, repository, prompt, task content, config value,
  key, endpoint, model name, or error text — and no hashes of any of those. A
  build from source reports its version as `dev` rather than a commit, so the
  version can't single out one machine. The service sees your IP while handling
  the request, as any HTTPS service does; it is not part of the payload and is
  never stored alongside an event, so reports can't be linked to each other or to
  a machine. Only aggregate counts are retained.

    Turn it off at install time by answering `n` to the prompt, or
    `VORNIK_TELEMETRY=off` for an unattended install; afterwards set
    `telemetry.enabled: false`. An explicit config choice always beats the
    environment. Run `vornikctl telemetry sample` to print the effective setting
    and the exact request that *would* be sent, without sending anything. Full
    detail in the Telemetry and privacy reference.
- **Security: upgrade recommended.** A ten-day contribution audit (21 findings)
  plus a whole-tree trust-boundary pass (10 findings) plus a review of those
  fixes (6 more) — all with root-cause fixes and regression tests. The two
  critical ones: the **macOS installer** could execute arbitrary commands inside
  the provisioned VM if an attacker controlled its environment variables, and
  **archive extraction** could write outside its target directory by following a
  planted symlink. Also closed: several safety gates that silently *allowed* work
  when they couldn't read their own inputs (per-task API budgets reset by a DB
  blip, task-cost forecasts proceeding while limits were blind, taint checks
  reporting "off" when unwired), incomplete SSRF blocklists in both the daemon
  and the scraper, unguarded browser subresource/WebSocket egress, and gateway
  errors that leaked full request URLs into agent-visible messages.
- **Cap what a single task can spend.** A new per-task budget stops one runaway
  task from eating a project's allowance: the forecast at creation *refuses* a
  task that would breach its cap, ~80% emits a warning, and 100% parks the task
  for your decision (increase / reduce / abandon) instead of failing it. Spend
  accumulates across resumes. Set `default_task_budget_usd` on the project (with
  an optional per-task override and a `max_task_budget_usd` clamp) —
  **0 or unset leaves everything as it was.**
- **Workflows can fan out in parallel.** A new `parallel` step runs several
  branches at once and joins them with `all`, `quorum:<n>`, or `best_effort`,
  so a research or review stage no longer has to run serially. See
  `configs/workflows/parallel-research.md` for a worked example.
- **Memory search tells you how much to trust a result.** `memory_search` now
  returns a trust verdict (high / medium / low) computed from stored confidence,
  validation status and freshness — not from an opaque relevance score. A low
  verdict automatically widens the search, and an aged result with no expiry is
  capped at medium so a stale decision can't be presented as current.
- **A safety net under automated cost tuning.** After a cost-tuning change is
  applied, a canary compares quality and cost before and after, and
  **automatically rolls the change back** if it regressed — then holds that knob
  in a cooldown. With that guard in place you can optionally let *proven* changes
  apply without a human: only for knobs with a track record of passed canaries,
  and only when the last change there was made by a person. Off by default, and
  scoped by an explicit per-swarm allow-list that means *none* when left empty.
- **Autonomous writes can be gated on untrusted input.** vornik now records
  whether each agent step consumed untrusted content and can hold a write whose
  lineage traces back to it, with the contributing sources attached for your
  review. Defaults to **advisory** (observe-only, no behavior change) so you can
  watch it before switching a project to `enforce`.
- **Report a problem without leaking your environment.** `vornikctl report`
  builds an anonymized problem report and a prefilled GitHub issue for you to
  review before submitting — secrets, emails, home paths, IPs and hostnames are
  stripped at a single choke point. If it can't anonymize something, it refuses
  rather than guessing. The installer offers the same thing when a fresh install
  fails.
- **Fewer first-run surprises.** A pass over a clean install fixed agent-LLM
  connectivity, unwritable rootless workspaces, the daemon not surviving logout,
  and Postgres not restarting after a reboot. The bundled demo project now ships
  with autonomy **off** so a fresh install can't burn tokens unattended, and
  `vornikctl doctor` gained checks for agent-LLM reachability, upstream API-key
  validity (with a live probe), and agent-image UID mapping. Three doctor checks
  also stopped reporting *correct* setups as problems — a router-based model
  config, a secret externalized to a file rather than an env var, and a prompt
  that tells an agent **not** to use a tool it was deliberately denied.
- **Fixed: `vornikctl` couldn't read its own config on a fresh install.** Every
  config-loading command failed with `database name is required` while the daemon
  on the same host ran fine — the `${...}` placeholders in `config.yaml` are
  filled from an environment file that the daemon got from systemd and the CLI
  did not. The CLI now reads the same files (`vornik.env` and `secrets/env`
  alongside the existing `secrets/*.env`), and if a placeholder genuinely has
  nothing to resolve to, the error names the variables instead of reporting the
  field as missing.
- **Fixed: no models listed when a model-fallback map was configured.**
  `vornikctl models list` printed nothing — and the models API returned an empty
  list — for any deployment setting `chat.router.model_fallbacks`, because model
  discovery was lost inside the provider chain. The same gap blanked the
  Ollama-compatible `/api/tags` endpoint, the doctor's model-reachability check,
  and the template model picker, and quietly turned per-provider readiness probes
  into no-ops. The models API now returns `501` when nothing can enumerate models,
  so an empty list means empty rather than broken.
- **Fixed: an attached document could be reviewed from memory instead.** A
  review delegated with a file attached could end up reviewing stale recalled
  chunks rather than the attachment, because extracting the upload at submit time
  suppressed staging the file for the agent. Workflows that declare they need
  input artifacts now always get the raw file. The Claude Code and Codex
  companion plugins ship as 0.14.0 and 0.12.0 with corrected guidance; installed
  clients pick that up on their next marketplace update.
- **New: your coding assistant can now configure and troubleshoot Vornik.** Both
  companion plugins bundle an operator lifecycle triad — `configure-vornik`,
  `troubleshoot-vornik`, and the existing `report-problem` — so setup, diagnosis,
  and filing a bug each follow a guarded path over Vornik's own tooling instead
  of being improvised. `configure-vornik` leads with the configuration traps that
  fail silently: the config tree is found by a fallback chain rather than one
  environment variable, `VORNIK_CONFIGS_DIR` is ignored without complaint unless
  the directory already holds `projects/`, `swarms/`, and `workflows/`, and
  editing the wrong copy of a config is the usual reason a correct change does
  nothing. `troubleshoot-vornik` routes by symptom to the right diagnostic —
  `doctor --offline` when the daemon is down, the failure-class playbook when a
  task fails — and hands off to `report-problem` when it runs out of road.
- **Behavior changes to note.** A `web_fetch` of a page with a blocked or
  unresolvable third-party asset now **succeeds** and lists what it refused in a
  new `denied_subresources` field, instead of failing the whole fetch. Inbound
  email that fails delivery five polls in a row is dropped with an error log
  rather than retried forever (this is what stops one bad message from blocking
  your inbox). If you override the macOS installer's quickstart URL it must now
  be `https://` and have a published `.sha256` alongside it.


---

## 2026.7.4

**A model-resilience release.** Your non-swarm LLM paths now fail over and
recover automatically, tool budgets got safer defaults and clearer warnings, and
model-health is fully visible in the doctor.

- **Automatic model fallback for non-swarm paths.** A new
  `chat.router.model_fallbacks` map (primary model → fallback model) lets the
  chat/dispatcher/autonomy/wizard path and the built-in workers (narrator,
  memory consolidation, titler, classifier, judge, …) fail over to a configured
  twin when a model's circuit opens — and return to the primary automatically
  when it recovers. Previously only swarm-role agents had this; an upstream
  outage would stall the rest. Empty map = unchanged behavior.
- **Model health is fully observable.** `vornik doctor` now reports **both**
  circuit breakers — the chat-router breaker (`model_circuits`) and the
  agent-container breaker (`agent_model_circuits`) — as separate checks, so an
  open circuit is visible where you look. The troubleshooting guide explains the
  two breakers and why an open agent circuit can leave the chat one showing
  closed.
- **Tool-budget refinements.** An operator-started task keeps its full tool
  budget across a crash-and-resume (instead of dropping to the tighter
  autonomous cap); startup warns when a role's tool-budget config won't take
  effect (a warm role, or a role with no base limit); and the `standard`
  complexity tier now downscales to 0.5× of a role's configured budget (the base
  is reserved for genuinely complex work) — **note this behavior change** if you
  relied on `standard` running at the full configured budget.
- **Scheduled updates.** Reminders can now run a task on a cadence and deliver
  its outcome, not just static text — ask the bot for a recurring digest or
  weekly plan ("every Monday at 7, plan my week") and it will spawn the task
  and message you the result when it's done. Pause and resume it from chat, the
  CLI, or the web UI's reminders table, and edit a recurring reminder's cadence
  or project in place from chat instead of recreating it. A slow run never
  overlaps itself, and an interrupted daemon can't lose or double-send an
  outcome. See [vornikctl reminders](../reference/vornikctl.md) and
  [Observability](../guides/observability.md) for the new metrics.
- **macOS install.** The one-liner `curl -fsSL https://get.vornik.io | bash`
  now works on **macOS** as well as Linux — the same command; the script detects
  your OS. On macOS it provisions a small Linux VM and runs the stack inside it,
  so the zero-egress agent isolation is preserved exactly as on Linux. See
  [Getting started](../getting-started.md#macos-inside-a-linux-vm).
- **Control-plane: apply tuning changes without waiting for an idle window.** A
  *value-only* workflow edit — e.g. reclaiming an over-provisioned step timeout —
  now applies even while tasks that use the workflow are in flight; only
  *structural* changes still wait for an idle window. Recommendations that go
  stale (their config target changed since they were drafted) now **auto-retire**
  instead of lingering as un-appliable, and the detector re-files a fresh one.
- **Control-plane hub is tidier.** The proposals inbox hides closed
  (rejected / rolled-back) rows by default and paginates; the skills browser
  hides retired skills by default; system-retired rows carry a distinct label so
  they don't read as human rejections; and long MCP/dispatcher tool descriptions
  now collapse behind a toggle.
- **Safer rollback.** Rolling back an applied change is refused when a later
  change overwrote the same target (or it was hand-edited), so a rollback can no
  longer silently clobber newer state — roll back the newer change first. The
  hub hides the rollback button on a proposal that's been superseded.


---

## 2026.7.3

**A hardening + security release.** The headline is a full security pass over
the code base, alongside autonomy workspace-state hygiene, a manifest-driven
config installer, wizard composition convergence, an Ollama Cloud chat
provider, and a broad fix batch.

- **Security hardening.** A sweep of the static-analysis backlog: path handling
  for user-supplied project/swarm/workflow identifiers is centralised on one
  confinement primitive; e-mail envelope addresses reject control-character
  injection; result pre-allocations are bounded; a webhook echo is HTML-escaped;
  admin redirects are constrained to same-origin; and session/chat cookies set
  `Secure` automatically when served over HTTPS (without breaking plain-HTTP
  deployments). Additional trust-scope and config-path hardening also landed.
- **Autonomy workspace hygiene.** Failed/cancelled tasks no longer strand
  tracked-file residue in the shared workspace clone; the prelude auto-commit
  now attributes honestly (`backlog: marker checkpoint` / `rescue: stranded
  tracked changes`) instead of blaming the next task. **Scripts grepping the
  old `auto-commit: workspace-root prelude` message must update.** A new
  `vornik_executor_residue_discard_total{project}` metric flags workspace
  contract violations.
- **RAG-ingest quality gating.** A task's output is ingested into memory only
  once the producing task succeeds, so failed runs no longer deposit garbage;
  `vornikctl memory purge-producer-failed` retro-cleans older residue. Backlog
  items are marked done at the success terminal, not at dispatch.
- **Agent LLM circuit breaker.** Per-agent model calls now get the same
  circuit-breaker / fast fail-over as the chat router.
- **Manifest-driven config tree.** A single manifest drives what installs into
  the deployed config tree, with drift-checking across all deployable subtrees.
- **Ollama Cloud provider.** A new chat sub-provider for Ollama's hosted
  open-weight models, separate from the local Ollama route; pricing refreshed.
- **Wizard convergence.** The project wizard normalises and repairs
  compositions (dropping undeclared params, defaulting required ones) and
  retains the last good build.
- **Quality-of-life.** Progressive-disclosure skill injection; friendlier
  internal-role labels on the spend dashboards; the Fix-It Doctor link now works
  on every deployment; an inbox mobile pass; and a turnkey in-place update
  script for Community quickstart installs.


---

## 2026.7.2

**The operator-experience release: describe an automation in plain language,
watch tasks narrate themselves, and repair failures from a guided chat.** This
cycle is built for operators who never want to touch YAML — plus a
control-plane copilot, a learning loop for knowledge skills, and an
autonomous backlog-to-PR pipeline for those who do.

- **Automation Composer.** Describe what you want automated in chat and the
  composer assembles a validated project — roles, workflow, schedule —
  grounded in your connected MCP servers and models, with a Plan/Graph/YAML
  preview before anything is committed. Ships **disabled by default**
  (`composer.enabled`) while quality gates soak; the classic wizard remains
  the default.
- **Narrated execution.** Running tasks tell their story on the task pages,
  and can push progress to the chat channel that started them (opt-in per
  project). Completion is deliverable-first: output artifacts appear as cards
  you can send straight to chat.
- **Fix-It Doctor.** Failed tasks open into a repair chat that gathers the
  failure context and proposes fixes; every applied action is deny-by-default,
  audited, and rollback-able.
- **"My requests" home.** A new Outcome Inbox groups your work into request
  cards with status rollups and inline actions for anything awaiting you — and
  it is now the default home page for non-admin users.
- **Guided Integrations Hub** (Community). Set up Telegram, Slack, email,
  GitHub App, and MCP integrations through guided forms that **probe the
  credentials before saving**, with recheck and assisted troubleshooting.
- **Control-plane copilot** (Enterprise console). Detectors watch for tunable
  timeouts, failure-rate regressions, and self-healing incidents, and raise
  **proposals** you can inspect, apply (gated, atomic, rollback-able — live
  where non-disruptive), or withdraw, from the web console or
  `vornikctl control-plane`. A diagnose-from-logs engine turns incidents into
  ready-to-apply config changes.
- **Knowledge skills learn.** Skills gathered from past work now carry usage
  signals and mature (or decay) automatically, with distilled new-skill
  proposals reviewed from Telegram, Slack, or the web inbox — and skills can
  be promoted to **global**, applying across every project.
- **Autonomous dev loop.** Agents can deposit work items into a per-project
  backlog, and autonomy drives open items to **draft pull requests** through
  an adversarial review loop; failed items are blocked visibly instead of
  silently skipped.
- **Model-health circuit breaker.** Unhealthy upstream models trip a breaker
  and tasks fail over immediately instead of grinding through the retry
  ladder. **On by default** with conservative thresholds; tune via
  `chat.router.health.*`.
- **Credential carryover.** Tools that mint credentials during a task can have
  them captured (opt-in per tool) and surfaced to you at completion —
  redaction stays fail-closed throughout.
- **Telegram task control.** `/tasks`, `/status`, `/cancel`, `/retry`, plus
  button-driven recovery steering and tap-to-copy command rendering.
- **Community contribution: AWS Bedrock embeddings.** Native Bedrock embedding
  support for memory/RAG — the first inbound community contribution.
- **Quality of life.** Config saves never hang (invalid saves are rejected;
  restart-required changes show a banner), a unified Artifacts panel, paused
  tasks can be cancelled, per-MCP-server timeouts, and mobile layout fixes.

!!! note "Restart to pick up new behavior"
    The model-health circuit breaker and the new detectors start with the
    daemon; restart after upgrading. All new configuration keys are additive.

---

## 2026.7.1

**A polish release: leaner large tool outputs, an end-to-end project-creation
experience, and clearer edition boundaries.** This follow-up to 2026.7.0 keeps
big tool payloads out of the model's context, finishes the guided
project-creation flow, and tightens the Community/Enterprise line.

- **Large tool outputs stay out of the context window.** The daemon can now hold
  a large MCP tool result and hand the model a compact handle instead of the
  full payload, and the per-result size cap is configurable (default raised to
  256 KiB). A new image-encoding helper fetches and downscales an image to an
  inline data URI, hardened against SSRF.
- **End-to-end project creation.** Project templates gained list/multiselect
  parameters, dynamic option sources, and conflict-free ID suggestions, with new
  ready-made purpose templates (code reviewer, tool assistant, report pipeline,
  docs↔RAG sync). A per-project **readiness page** (`/ui/projects/{id}/setup`)
  runs config, schedule, secrets, MCP, model, and smoke checks and flags what is
  still incomplete, and the conversational **project wizard** now composes a
  project turn by turn from your live MCP servers and models, showing a summary
  before it commits.
- **Clearer edition boundaries.** Community Edition returns a typed
  "Enterprise-only" response on Enterprise-gated admin routes, companion API-key
  minting is available in Community, and the Memory Firewall and project
  configuration are now Community features (cross-project context remains
  Enterprise).
- **Security & correctness.** Fixes for a cross-project access-control gap in the
  UI, a server-side request-forgery path, and several query-efficiency
  improvements on hot paths.
- **Onboarding fixes.** The setup flow now correctly recognizes router- and
  CLI-based chat providers as configured, clearing spurious "not configured" and
  session-authentication errors on the setup and wizard pages.

!!! note "Restart to pick up new behavior"
    The configurable tool-result cap and media-handle behavior take effect after
    a daemon restart.

---

## 2026.7.0

**A renamed, edition-aware platform with an AI-first install and a hardened
memory/chat core.** This release completes the rename to **vornik**, introduces
a clean Community/Enterprise edition seam, publishes the docs site, and lands a
broad reliability pass across memory/RAG, chat providers, onboarding, and
deployment.

!!! warning "This release renames the product"
    `swarmd` is now **vornik** and `swarmctl` is now **vornikctl**. Service
    names, command names, config paths, environment variables, and dashboards
    changed. Review your automation for the `swarmd` → `vornik` migration before
    upgrading, and always take a backup first.

- **Vornik rename, end to end.** Source, commands, environment variables,
  metrics, deployment files, Helm chart, Grafana dashboards, configs, and docs
  moved from `swarmd` to `vornik`; `swarmctl` is now `vornikctl`.
- **Edition-aware builds.** The codebase now has a clean Community/Enterprise
  seam: every feature carries a CE/EE tag (shown in the docs and enforced in
  code), and the Community Edition builds and ships independently under
  AGPL-3.0.
- **Published documentation.** Public docs are now served at
  <https://docs.vornik.io>, with per-page edition markers and expanded
  onboarding, architecture, configuration, CLI, security, and support material.
- **AI-first install.** `AGENTS.md` is now a runbook a coding agent can execute
  and verify end to end (install → LLM key → first task → its own persistent
  memory); the getting-started path makes the AI-assisted install the preferred
  route (short link: `agents.vornik.io`).
- **Sharper memory recall.** Multi-term recall now uses a strict full-text query
  with a relaxed fallback and all-term matches ranked first; a reranker-gated,
  round-isolated retrieval path improves context assembly, scoped to
  non-interactive use so interactive recall stays fast. By-id correction is now
  the preferred way to fix a stale or wrong memory.
- **More resilient chat.** Long conversations are summarized instead of dropped
  when context overflows, chat providers share a tuned HTTP transport, and
  persistent-timeout handling engages provider fallback faster.
- **Companion & MCP.** A first-party Codex companion plugin joins the Claude
  companion; companion memory gained a `memory_correct` tool (including a
  surgical by-id refute mode) plus input-validation and reliability hardening.
- **Deployment quickstarts.** A one-command, pgvector-backed local playground
  and a hardened installer (clean re-checkout, safe cleanup, literal DB port);
  PostgreSQL + pgvector is the recommended backend for memory/RAG.
- **Fixes & hardening.** Rate-limit minute/hour buckets no longer cross-sum;
  no-JavaScript pages render one consistent color scheme; Postgres integration
  tests honor `POSTGRES_PORT`.

!!! note "Restart required"
    Restart the daemon after upgrading to pick up the new behavior.

Enterprise-edition capabilities (packaging and installers, SSO and admin
surfaces, clustering, and more) also advanced this release — see the
[Editions matrix](../editions.md) for what's included where.

---

## 2026.6.1

**A more visual control plane, safer to expose and support.** This release
consolidates a large body of work: the web UI's capability tracks, a
security/operability hardening pass, git-over-HTTPS workspace access, and
content provenance.

- **Edit workflows as a graph.** A new graph view (`/ui/workflows/{id}/graph`)
  lets you wire steps visually — add/delete nodes, draw success/fail/gate edges,
  set the entrypoint — with every change validated and hot-reloaded like the
  form and YAML editors.
- **Bulk actions & clone.** Multi-select on the Tasks list (bulk Cancel / Retry /
  Close) and the Executions list (bulk Cancel); clone a workflow to a new id from
  the editor.
- **Insight area.** A **Trends** page (daily throughput, success rate, judge
  abstain rate, recovery, and LLM spend) and a **Tool budget** page (actual
  tool-use vs the configured complexity-tier budget, now with advisory
  over/under-provisioning flags).
- **Clone and push to a project workspace over HTTPS.** A project's git-backed
  workspace can be cloned and pushed to with a per-project API key — no shell on
  the box. Exposure is opt-in per project (`Project.Git.Enabled` +
  `server.public_base_url`); push requires a key issued with `--allow-push`
  (keys are read-only by default) and is blocked while a task holds the
  workspace, so a push can't race a running job. Manage it from the new project
  Git-access panel or `vornikctl key --allow-push`.
- **Centralised log forwarding.** A new subsystem ships vornik's structured logs
  and audit events to an external HTTP webhook (bearer-token) and/or syslog (TLS
  with a pinned CA). Forwarding is scope-filtered, best-effort, and never blocks
  the daemon; an empty or ship-all scope is refused.
- **Run safely behind a reverse proxy.** Trusted-proxy real-client-IP resolution
  (`server.real_ip`) recovers the true client IP from a configurable header —
  trusted only when the immediate peer is in your `trusted_proxies` allowlist —
  so brute-force lockout, rate limiting, and audit no longer collapse every user
  to the proxy's address. Off by default.
- **One-command support bundle.** `vornikctl support-report --task <id>` (or
  `--since <window>`) produces a single, **redacted-by-default**, self-contained
  diagnostic archive — lifecycle, audit, LLM usage, conversation, container and
  daemon logs, redacted config, a doctor diagnosis, and version/health — so you
  no longer hand-assemble evidence for a support thread.
- **Smarter content redaction.** The output guard now knows whether scanned
  content is first-party (produced by vornik) or third-party, so vornik's own
  output is no longer over-redacted while untrusted content and secrets still
  are.
- **Structured recovery steering.** When a step fails, the lead can be offered a
  typed recovery decision — re-route the workflow, fall back to a role's
  configured model, retry, or skip — and successful recoveries are recorded so
  they surface as a recovery trend.
- **Outbound email attachments.** The email channel now sends replies as
  `multipart/mixed`, delivering `send_artifact` outputs as attachments on the
  threaded reply. File delivery is now channel-agnostic and shared with Telegram.
- **More `vornikctl doctor` checks.** `model_health` (flags a model that is
  failing or returning degenerate output and recommends its fallback),
  `config_crlf` (detects — and with `--fix` repairs — CRLF line endings that
  cause phantom config drift), `model_route_coverage` (every role's model
  resolves to a route and has pricing), and `scraper_profile_freshness` (warns
  on stale scraper login sessions).
- **Clustering / fleet visibility.** Lightweight multi-node awareness:
  heartbeat/relay logging and metrics, a probe library driven by an
  expected-endpoints config, and an opt-in, leader-gated endpoint monitor with
  operator alerting.

Notable fixes: a **graceful restart no longer fails in-flight work** and
**bounded graceful shutdown** force-closes stuck connections so a long-lived
chat request can no longer hang a restart into a `SIGABRT` (pair with
`TimeoutStopSec=90s`); the Bedrock adapter recovers SDK panics on
cancel-during-shutdown; document extraction is hardened against zip-bombs, XXE,
and OCR hangs; schema-config saves normalise CRLF→LF (with a CI gate) to stop
phantom config drift; the multi-step "file does not exist" artifact-handoff bug
is resolved with store-backed staging and an end-to-end regression guard; and
first-eval-after-reload chat no longer emits a spurious "LLM error". Under the
hood, recovery exits are now recorded and several previously design-only
observability metrics are emitted.

!!! note "Upgrade"
    Restart the daemon to pick up the new behavior. Add `TimeoutStopSec=90s` to
    the systemd unit so the bounded shutdown has room to drain. Log forwarding,
    the support report, real-IP resolution, and git-over-HTTPS workspace access
    are all opt-in — existing deployments are unaffected until you configure
    them.

---

## 2026.6.0

**Fix GitHub issues end-to-end.** Label an issue (or open a pull request) and
vornik can plan it, make the change across subtasks, review its own diff, and
open a **draft pull request** for you to approve — hands-off, with the daemon
doing the branch push and the review posting. Every automated PR is a draft, so
nothing merges without you.

Other highlights:

- **Self-improving workflows.** When a workflow starts failing or getting
  expensive, vornik can propose a repair, **replay-test it against real past
  runs**, score it against the current version, and apply it only when you
  approve — nothing changes on its own.
- **The swarm learns from itself.** A new continuous-learning layer mines
  reusable, confidence-scored fixes from past runs and surfaces them as advice
  on failed tasks and to the planner. Advisory and opt-in.
- **Refreshed control-plane UI.** A new translucent design with icon-rail
  navigation, first-class Swarms / Workflows / Executions list pages, and
  tidier per-page layouts.
- **Feature Doctor.** A new command tells you which optional features are
  enabled, ready, or blocked — and can turn one on for you, with a backup,
  validation, and automatic rollback if anything is wrong.
- **Sign in with GitHub.** Optional GitHub login for the web UI with per-project
  access scoping, plus the groundwork for single sign-on.
- **More model choices.** Free-tier hosted models with automatic fallback, and
  per-role tool budgets that scale to task complexity so simple tasks stay cheap
  and hard ones get more room to work.
- **Public documentation** is now live at docs.vornik.io.
- **Safer web fetching.** The research browser follows shortened links (e.g.
  map short-URLs) to their real destination before applying your host
  allowlist, and refuses to reach private or loopback network addresses.
- **Security & safety hardening.** A broad audit-remediation pass across access
  controls and memory-access policy.

!!! note "Restart after upgrade"
    A few of these are read from the daemon config at startup, so restart the
    daemon after upgrading to pick them up.

---

## 2026.5.9

The conversational **project setup wizard** now works end-to-end: start a new
project from a guided chat, resume or cancel a draft, and have it created and
running just like a template-based project. You can pin the wizard to its own
model independently of your default chat model.

Other highlights:

- **Official packages and binaries.** Releases now ship prebuilt `vornik` and
  `vornikctl` binaries (Linux amd64 and arm64) plus RPM and DEB packages,
  attached to each GitHub release.
- **Stronger default network isolation for agents.** New installs default to a
  zero-egress agent network: agents reach vornik for model and tool access but
  cannot reach the internet. Roles that genuinely need outbound access can opt
  in per role.
- **More resilient chat under rate limits.** The model client now honors
  rate-limit retry hints and backs off automatically instead of failing the
  request.
- **Chat history loads on page open.** The web chat view now shows your prior
  conversation immediately instead of starting blank.

!!! note "Structured output is model-dependent"
    Features that need strictly structured model output (such as the wizard)
    are most reliable on models that honor structured-output requests. The
    wizard tolerates models that reply in prose, so it stays usable regardless.

## 2026.5.8

Two operator-facing capabilities landed alongside a large stability pass:

- **Counterfactual replay.** Re-run any past task with a single variable
  changed — a different model or a different prompt — and compare the original
  and the new run side by side, including cost, latency, and per-step
  differences. Available from the command line and the web UI.
- **Memory policy controls.** Every memory item now carries policy metadata
  (sensitivity, provenance, expiry, and access scope), and every retrieval is
  recorded for audit. Three modes let you choose how strictly policies are
  enforced: off, advisory (the default — nothing is blocked, but everything is
  logged), or enforce. The default is safe to adopt without changing existing
  workflows.

This release also closed a broad set of reliability and security fixes across
task execution, scheduling, memory, and inbound channels.

## 2026.5.7

A large **user-experience release**.

- **New landing page** with at-a-glance tiles (active tasks, active chats,
  today's spend, next autonomy run) and a cross-project activity feed.
- **Project template gallery.** Browse a catalog of starter projects in the web
  UI (or via `vornikctl init project --template <slug>`) and create one from a
  simple form, filterable by domain.
- **Per-project home page** with a readable summary card, autonomy status, and
  an effective data-retention panel that shows your actual pruning windows.
- **Friendlier error messages.** Failed tasks now show a plain-language banner
  explaining what went wrong, with the technical detail tucked underneath.
- **Retry a failed task from the step that failed**, directly in the UI.
- **Smarter memory recall**, including optional date-range filtering ("what did
  we discuss last week") and a resilience cascade so memory search keeps working
  even if part of the search backend hiccups.
- **High-availability friendliness.** If you run more than one vornik instance,
  polling channels now coordinate so a single inbound message is handled once,
  not duplicated.

!!! note "Optional new settings"
    The client request timeout is now configurable for long-running commands,
    and projects gain optional `description` and retention settings that surface
    on the new home page.

## 2026.5.6

A guardrails and memory-quality hardening release.

- **Runaway-loop protection for adaptive workflows.** A misconfigured router
  can no longer spawn an endless chain of child tasks; routing now caps its
  depth and fails clearly instead of looping. If a workflow previously relied on
  a silent fallback, review your candidate-workflow list (see the behavior note
  below).
- **More reliable memory classification**, including an optional background pass
  that keeps newly ingested memory labeled without manual cleanup.

!!! warning "Behavior change"
    Adaptive routing no longer silently falls back to your project's default
    workflow when a step picks an out-of-list choice — it now fails loudly. If
    you depended on the old fallback, tighten your candidate list or disable
    strict mode. Restart the daemon to pick up the new behavior.

## 2026.5.5

A memory-quality release focused on retrieval precision.

- **Better recall** through a set of ranking improvements (richer embedding
  context, reranking, result diversification, and time-to-live enforcement) that
  surface more relevant memory without any change to how you use it.
- **New memory maintenance commands**: reclassify, re-embed (for model
  upgrades), and list prune candidates.

```bash
vornikctl memory reclassify --project <project>
vornikctl memory reembed     --project <project>
vornikctl memory prune-candidates --project <project>
```

## 2026.5.4

A reliability release.

- **Native AWS Bedrock support** for chat and agents, removing a translation
  hop for Bedrock-hosted models.
- **Sturdier scheduling and recovery.** Interrupted tasks recover faster,
  models that get stuck producing the wrong output shape are stopped instead of
  burning budget, and lease handling under heavy load is more robust.
- **Model-quality visibility.** New dashboards surface which models are
  producing malformed output and how often retries recover, so you can spot a
  poorly behaving model and swap it.

!!! note "Upgrade"
    Restart the daemon after upgrading. If you build the agent image yourself,
    rebuild it to pick up this release's runtime improvements.

## 2026.5.3

A reliability and output-correctness release.

- **Declarative output schemas.** Agent roles can declare the exact shape of
  their output in one place, eliminating a whole class of "valid output, wrong
  shape" failures. `vornikctl doctor` flags mismatches before they cause a
  failed run.
- **Security hardening** across container isolation, secret detection, webhook
  authentication, and API input validation.

!!! note "Upgrade"
    Restart the daemon to activate the new schema enforcement.

## 2026.5.2

A reliability and observability release.

- **Per-task post-mortem explainer.** Failed tasks now include a plain-language
  paragraph explaining why they failed, alongside the step outcomes and audit
  trail.
- **Security fixes**, including requiring authentication on the web UI subtree
  and closing a cross-project access path.

## 2026.5.1

Cancelled tasks now show their accumulated LLM cost in the UI, and autonomy's
duplicate-suppression now matches by topic similarity rather than exact text.

## 2026.5.0

The **trustworthy-output** release. A multi-phase hallucination-detection
pipeline now runs across agents, chat replies, and the autonomy loop, flagging
unsupported claims (a quoted URL that was never fetched, a referenced file that
was never produced, a numeric claim with no source) and either failing the step
for retry or surfacing the finding in the UI.

Also in this release: multimodal task input (image uploads through chat), an
additional first-class chat provider, secret detection and redaction at every
persistence point, and a larger operator UI (spend deep-dive, live tool-call
audit stream, routing-decision panel).

---

## Earlier releases (2026.4.x)

The 2026.4 series built vornik from its first stable release into a capable
multi-agent platform. Notable user-facing milestones:

- **2026.4.14** — A typed agent tool set (`file_edit`, `grep`, `glob`, and
  typed git inspection tools) with per-role allowlists; live task-log tailing
  in the CLI and UI; signed webhook ingress with audit history; a project
  onboarding wizard; and a browser-based project YAML editor with validation.
- **2026.4.13** — Direct-API access to subscription-billed model backends
  (no CLI subprocess), a multi-backend chat router, a headless-browser fetch
  capability for agents, and an expanded `vornikctl doctor` with new schema,
  security, cost, and hygiene checks.
- **2026.4.12** — Per-task cost drill-down, local-timezone budgets, tool-call
  rate limits, a per-project data-retention sweeper, and `vornikctl backup` /
  `restore`.
- **2026.4.11** — First-class **LLM spend tracking**: per-step token and cost
  metrics, a per-project spend panel (24h / 7d / 30d / month-to-date with a
  per-role breakdown), and soft/hard USD budgets enforced before work starts.
  Introduced "effective cost" (spend per successful step) so a cheap-but-flaky
  model stops looking cheap. See [Cost and caching](../guides/cost-and-caching.md).
- **2026.4.10** — Per-user channel project scoping enforced on every action,
  per-project tool servers reached through the daemon (no per-agent
  credentials), and a deterministic git-backed workspace so "task completed"
  reliably means "output was saved."
- **2026.4.9** — Per-task git worktree isolation, a swarm-agnostic adaptive
  workflow that works with any swarm, a runtime autopilot on/off toggle, and
  lenient registry loading so one broken project no longer takes down the rest.
- **2026.4.8** — Per-role model overrides applied correctly to warm containers,
  an expanded `vornikctl doctor`, and additional conversation session controls.
- **2026.4.7** — **First stable release.** The autonomous development pipeline,
  durable task queue and scheduler, the multi-step workflow engine, the
  server-rendered web dashboard, and the conversational bot interface.

---

For configuration details on anything mentioned here, see
[Configuration reference](../reference/configuration.md) and the
[Guides](../guides/index.md).
