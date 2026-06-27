---
sources:
    - path: internal/ui/task_conversation.go
      sha256: fcb55e7db3be5cfb3f5ef7a49723ad3c720d44782965aadcf4ce507fa369eb2c
    - path: internal/registry/project.go
      sha256: a6b13685371ebdecfddfa9870a8ae7fc37653f83374519c7d9cb9dc6ec0aeb82
---
# Approvals & human-in-the-loop

vornik is built to run unattended, but you decide where a human has to step in.
There are two distinct gates — one inside a workflow, one at the point an
autonomous task is created — plus a way for an agent to stop and ask you a
question mid-run. All of them surface in one place: the operator **Inbox**.

## Approval steps in a workflow

A workflow can include an `approval` step. When an execution reaches it, the run
**pauses** and the task enters an *awaiting-approval* state — no agent runs, and
the workflow will not advance until you approve. It also won't auto-resume on a
daemon restart: an approval pause is deliberately a human decision.

Declare it like any other step:

```yaml
steps:
  - id: build
    type: agent
    role: engineer
    on_success: review
  - id: review
    type: approval        # pauses here until an operator approves
    on_success: deploy
  - id: deploy
    type: agent
    role: release
```

Use this to put a checkpoint before anything irreversible — a deploy, an
outbound send, a destructive change.

## Approval-gated autonomy

For [autonomous projects](autonomy.md), you can require that *every* task the
project creates for itself be approved before it runs. Set it on the project:

```yaml
autonomy:
  enabled: true
  requireApproval: true
```

Tasks then land in an awaiting-approval state instead of being queued. A
daemon-level watchdog cancels approvals left unanswered too long
(`autonomy.approval_timeout_hours`, default 96), so a forgotten task doesn't sit
forever.

## Agents that stop to ask

Separately from approvals, an agent can reach a point where it needs a decision
or information from you — it emits a **checkpoint** and the task enters an
*awaiting-input* state. A decision checkpoint presents options as buttons; an
open question waits for your answer. Reply from the CLI:

```bash
vornikctl task answer <taskId> --project my-project \
    --checkpoint <checkpointId> --choice "Option A"
# or provide free-text:
vornikctl task answer <taskId> --project my-project \
    --checkpoint <checkpointId> --content "Use the staging bucket, not prod."
```

You can also steer a running task with `vornikctl task directive`, pause/resume
it, or amend its brief — see the [vornikctl reference](../reference/vornikctl.md).

## Acting on approvals

Approvals and questions are actioned in the Web UI, where they're collected in
the **Inbox** ("what needs me") — ranked so the things blocking work come first:
tasks needing approval, then tasks needing input, then failures. Each item links
to the task, where the **Approve** / **Reject** buttons (for approval-gated
tasks) and the **Answer** action (for checkpoints) live.

> Approving or rejecting a *task* is a Web UI action (or a steering reply from a
> connected chat channel) — there is no `vornikctl approve` command for tasks.
> (`vornikctl workflow-proposals approve` is a different feature — it approves
> architect-proposed workflow changes, not tasks.)

When a task does fail, see [Recovering failed work](recovery.md) for retrying,
rerunning from a step, and structured recovery.
