---
sources:
    - path: docs/release-notes
      sha256: bea5a0143d6f4e500be50a2c49ff41c9b848a7380fa09462379da37bb453f542
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
