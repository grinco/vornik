---
name: vornik-docs
description: |
  Teaches Claude where Vornik's documentation lives, so it answers questions
  about Vornik from the docs rather than from guesswork. Use this skill
  whenever the user asks how a Vornik feature works, what a config key does,
  which flag a command takes, whether Vornik can do something, or asks for a
  doc link — and before answering any "how does Vornik do X" question from
  memory. Vornik's surface is large and changes fast, so a plausible-sounding
  invented config key or CLI flag is the most common failure mode here.
  Prefer the shipped CLI's own help and the published site map below over
  recall; cite the page you used; and say plainly when the docs do not cover
  something instead of filling the gap.
---

# Finding things in Vornik's documentation

Vornik's surface is large — a daemon, a CLI, a registry format, a memory
subsystem, channels, and a companion plugin — and it moves between releases. The
failure mode this skill exists to prevent is a confident answer containing a
config key or CLI flag that does not exist. Those are expensive: the user pastes
it into a config, gets no error (unknown keys are frequently ignored), and the
thing silently does nothing.

**So: look it up, and cite what you looked at.**

## Order of preference

1. **The installed CLI itself**, when the user has one. It is the only source that
   cannot be out of date relative to their deployment:

   ```
   vornikctl --help
   vornikctl <command> --help
   vornikctl doctor            # resolved health, per-check remediations
   ```

2. **A local checkout**, if there is one. `docs/public/` is the source for the
   published site, so `docs/public/reference/configuration.md` is the config
   reference. Grep it — this is faster and more reliable than fetching.

3. **The published site**, <https://docs.vornik.io>. A `docs/public/<path>.md`
   file maps to `https://docs.vornik.io/<path>/`.

4. **Your own memory — last.** Use it to decide *where to look*, not as the
   answer. If you cannot point at a source, say so.

## Site map

Use this to route; do not invent paths outside it.

| Looking for | Go to |
| --- | --- |
| What Vornik is, core model | `concepts/index`, `concepts/architecture` |
| Installing, first run | `getting-started/index` |
| **Expected shape of a healthy install** | `reference/reference-architecture` |
| **Every config key** | `reference/configuration` |
| **Every CLI command and flag** | `reference/vornikctl` |
| Projects, swarms, workflows, LLM controls | `guides/workflows-and-llm-controls` |
| Chat channels | `guides/conversation-channels`, `integrations/telegram`, `integrations/slack`, `integrations/email` |
| MCP tools | `guides/mcp-tools`, `integrations/mcp` |
| Artifacts, deliverables | `guides/artifacts-and-delivery` |
| Scheduled / recurring work | `guides/autonomy` |
| Human-in-the-loop gates | `guides/approvals` |
| Failure handling, retries, recovery | `guides/recovery`, `features/self-healing` |
| Model cost, caching | `guides/cost-and-caching` |
| Secrets handling | `guides/secrets` |
| Logs, metrics, tracing | `guides/observability` |
| Retention, storage | `guides/storage-and-retention` |
| Health checks | `guides/feature-doctor` |
| Evaluations, benchmarking | `guides/evals` |
| Multi-node | `guides/clustering`, `features/cluster` |
| Building automations from prose | `guides/automation-composer` |
| Work across projects | `guides/cross-project` |
| Memory / RAG | `features/memory-rag` |
| API keys, scoping | `features/auth` |
| Air-gapped / no-egress operation | `features/zero-egress` |
| The companion plugin | `features/companion` |
| Knowledge skills | `features/knowledge-skills` |
| Config/tuning proposals for approval | `features/control-plane` |
| Learned remediations | `features/instinct` |
| GDPR, DPA, AI Act, transparency | `compliance`, `ai-transparency` |
| Something broke | `troubleshooting/index` |
| What changed in a release | `release-notes/index` |

## Where the docs deliberately stop

Do not expect these on the public site, and do not infer them:

- **Internal design documents.** Low-level design docs are not published. If a
  question needs design rationale that isn't on the site, say the public docs
  don't cover it rather than reconstructing intent.
- **Enterprise-only detail** may be thinner in the Community documentation than
  the deployment the user is running. If a feature seems undocumented, check
  whether it is an edition difference before calling it a docs gap.
- **Sizing and capacity numbers.** Deliberately absent: the reference
  architecture states shape, not hardware.

## Answering well

- **Cite the page.** One link or path per claim, so the user can check you.
- **Quote config keys and flags exactly**, from the reference page or `--help`.
  Never adjust one to look more consistent with the others.
- **Prefer the user's own deployment as the authority.** For "is this on?", the
  answer is what their daemon resolved, not what the docs say is possible —
  `vornikctl doctor feature` beats a docs quote every time.
- **Say when the docs don't cover it.** An honest gap is useful; an invented key
  costs the user a debugging session. If they need it anyway, `report-problem`
  files a documentation gap.

## Related skills

- Setting something up → `configure-vornik`
- Checking an install against the reference shape → `validate-install`
- Something is broken → `troubleshoot-vornik`
- A real defect or a docs gap worth filing → `report-problem`
