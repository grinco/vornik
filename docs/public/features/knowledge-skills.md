# Knowledge skills

!!! note "Community Edition"

    The knowledge-skill store, capture, approval over Telegram/Slack, and
    injection into swarm roles are all in the free **Community Edition**.
    The web-UI review inbox is an Enterprise convenience.

A **knowledge skill** is a piece of reusable, instructional know-how — a
troubleshooting flow, a non-obvious fix, a deploy/verify sequence — captured
once and then applied automatically by future agent runs. It is the
`SKILL.md` shape (a short frontmatter + a Markdown body) that an agent reads
and follows.

Knowledge skills are **daemon-owned**: authored from any client (Claude Code,
Codex) and, once approved, served to your vornik swarm roles *and* every
companion client. They are distinct from `SWARM-SKILL.md` capability skills
(a packaged workflow + roles you install) and share no storage with project
RAG memory — memory holds *facts*, skills hold *procedures*.

## The loop

```
propose (draft) → operator approves → active → injected into roles → matures on use
```

1. **Propose.** A skill can be created by:
   - You, explicitly: `/skill-propose` in Claude Code (or `skill_propose`
     over MCP in Codex).
   - Your companion session, automatically: when you and the model work out
     something durable and reusable, it proposes a draft on its own
     (vornik never sees your transcript — only the resulting proposal).
   - A completed vornik swarm task: a lightweight distiller reviews it and,
     if there's reusable know-how, proposes a draft.

   Every proposal lands as a **draft** and never takes effect on its own.

2. **Approve (the human gate).** An operator reviews drafts and approves or
   rejects them. Approving activates the skill; rejecting retires it. No
   skill is ever auto-activated — this is the safety valve, because an
   active skill injects as trusted guidance into swarm roles.

3. **Inject.** Active (and trusted) skills relevant to a role are added to
   that role's context when a task runs, so the agent applies your captured
   procedures without rediscovering them.

4. **Mature.** vornik tracks how a skill performs: a skill that keeps helping
   is promoted `active → trusted`; one that keeps getting corrected or goes
   unused decays to `retired`. Promotion and decay never *activate* anything
   — they only reorder already-approved skills.

## Approving skills

Pick whichever surface suits you — they all apply the same decision:

- **Telegram / Slack.** A periodic "N skills to review" message arrives with
  **Approve / Reject** buttons. Tap to decide — no key handling. Any allowed
  operator can approve. This is the Community-Edition path.
- **Companion command.** `/skill-list maturity=draft` to see pending drafts,
  then `/skill-approve <id>` (needs a key with the `skill_admin` capability).
- **Web UI (Enterprise).** The **Admin → Skills** page (`/ui/admin/skills`)
  is a full skills browser across all projects, with maturity tabs
  (Pending review / Active / Trusted / Retired / All) and per-skill usage
  counters. Pending drafts carry Approve / Reject; any skill carries a
  Make global / Make project-only toggle; active/trusted skills can be
  retired.

## Client tools

Available in Claude Code (as `/skill-*` commands) and Codex (as `skill_*`
MCP tools):

| Tool | Purpose | Capability |
|---|---|---|
| `skill_propose` | propose a draft | `skill_write` |
| `skill_search` | find active/trusted skills | `skill_read` |
| `skill_get` | read a skill's full body | `skill_read` |
| `skill_list` | list skills by maturity | `skill_read` |
| `skill_approve` | promote a draft to active | `skill_admin` |
| `skill_reject` | retire a skill | `skill_admin` |
| `skill_set_global` | set/clear cross-project reach | `skill_admin` |

## Cross-project (global) skills

By default a knowledge skill is **project-scoped** — it injects only into its
home project's roles. Mark a skill **global** and it injects into **every**
project's roles instead, so a procedure captured once (say, from your
`companion-<you>` project) reaches your autonomy roles across projects.

- **Create global:** `skill_propose` with `global: true` (`/skill-propose`
  documents the flag). Honored only on a fresh create.
- **Promote/demote an existing skill** from any of three surfaces, all of
  which flip the same flag without changing the skill's maturity:
    - **Companion:** `/skill-set-global <id>` (Claude Code) or the
      `skill_set_global` MCP tool (Codex).
    - **CLI:** `vornikctl knowledge set-global <id>` /
      `vornikctl knowledge set-project <id>`.
    - **Web UI (Enterprise):** the **Make global / Make project-only**
      buttons on the **Admin → Skills** inbox.

Because an approved global skill fires in every project, every review surface
labels a global draft **"affects ALL projects"** so the approver decides its
blast radius knowingly. Marking a skill global is a `skill_admin` action and
only affects a skill in the key's own project. Non-global skills stay isolated
to their project.

## Capabilities & access

Skill access is gated per companion key by three capabilities —
`skill_read`, `skill_write`, and `skill_admin` (the approval gate,
deliberately opt-in so a client can propose but not self-approve). Grant them
either way:

- **CLI:** `vornikctl companion grant --project <p> --client claude-code
  --skill-all` (or `--skill-read` / `--skill-write` / `--skill-admin`
  individually; `--skill-write` implies `--skill-read`).
- **Web UI (Enterprise):** **Admin → Keys & access** (`/ui/admin/keys`) has a
  per-key capability toggle for memory + skill flags — grant or revoke a
  role in the browser, no re-mint required.

Capabilities are read live on every request, so a grant takes effect on the
key's next call.
