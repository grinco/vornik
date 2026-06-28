---
swarmId: basic-swarm
displayName: Dev swarm (lead + feasibility + analyst + coder + tester + reviewer + scout + architect)
leadRole: lead
rolePrelude: |
    You are part of a multi-agent coding swarm. Another role will
    review your work — never fabricate test output, commit hashes, or
    file diffs. If a check couldn't be run, say so explicitly.
roles:
    - name: "lead"
      description: "Plans code tasks and schedules autonomous work"
      count: 1
      # 2026-05-13: forced ephemeral. Warm containers bind-mount the
      # project workspace root (not the per-task worktree), so any
      # shell-tool write from the lead pollutes the workspace root and
      # blocks ephemeral tasks' worktree merges. See https://docs.vornik.io
      # → "Warm-pool containers bypass worktree isolation".
      runtimePolicy: "ephemeral"
      # Strategic planning — GLM-5 via Bedrock. Rank 1 on the
      # open-LLM leaderboard ($1.00/$3.20). Lead fires once per
      # task; quality wins. Fallback to GPT-5.4 via codex-
      # subscription (plan-billed, different vendor + reasoning
      # chain).
      model: "zai.glm-5"
      modelFallback: "gpt-5.4"
      maxTokens: 4096
      requiredOutputKeys: ["plan"]
      runtime:
        image: "localhost/vornik-agent:latest"
        cpu: "2"
        memory: "4Gi"
        envVars:
            # 2026-05-14: 12 → 20 across the board to support longer
            # autonomous runs.
            VORNIK_MAX_TOOL_ITERATIONS: "20"
      permissions:
        allowedTools: ["file_read", "file_write", "run_shell", "read_many_files", "grep", "glob", "git_status", "git_log", "current_time"]
        delegationAllowed: true
        autonomousTaskCreation: true
        maxDelegations: 20
    - name: "feasibility"
      description: "Checks whether requested code work is blocked"
      count: 1
      runtimePolicy: "ephemeral"
      # Cheap gate decision — MiniMax M2.5 via Bedrock. Rank 9
      # on the open-LLM leaderboard, 1M context, $0.30/$1.20.
      # Fallback to GPT-5.4-mini via codex-subscription.
      model: "zai.glm-4.7-flash"
      modelFallback: "gpt-5.4-mini"
      maxTokens: 2048
      # outputSchema replaces requiredOutputKeys + the prose Output
      # block. See configs/swarms/dev-swarm.md's feasibility for
      # the rationale.
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [feasibility]
        properties:
            feasibility:
                type: object
                required: [feasible]
                properties:
                    feasible: {type: bool}
                    effort: {type: string}
                    reason: {type: string, minLength: 1}
                    blockers: {type: array}
        plausibility:
            - name: feasible_explained
              when: {"feasibility.feasible": true}
              require: ["feasibility.effort"]
            - name: blocked_explained
              when: {"feasibility.feasible": false}
              require: ["feasibility.blockers"]
      runtime:
        image: "localhost/vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "20"
      permissions:
        allowedTools: ["file_read", "run_shell", "read_many_files", "grep", "glob", "git_status", "git_log", "current_time"]
        delegationAllowed: false
    - name: "analyst"
      description: "Selects/specs the next code task in CURRENT_TASK.md"
      count: 1
      runtimePolicy: "ephemeral"
      # Spec writing — GLM-5 via Bedrock. Rank 1 on the open-LLM
      # leaderboard; structured spec writing benefits from the same
      # reasoning the lead uses. Fallback to GPT-5.4 via
      # codex-subscription (plan-billed).
      model: "zai.glm-5"
      modelFallback: "gpt-5.4"
      maxTokens: 6144
      # outputSchema replaces requiredOutputKeys + prose Output blocks.
      # See configs/swarms/dev-swarm.md for the rationale.
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [analysis]
        properties:
            analysis:
                type: object
      runtime:
        image: "localhost/vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "35"
      permissions:
        allowedTools: ["file_read", "file_write", "run_shell", "file_edit", "read_many_files", "grep", "glob", "git_status", "git_log", "current_time"]
        delegationAllowed: false
    - name: "coder"
      description: "Implements focused code changes and commits them"
      count: 2
      runtimePolicy: "ephemeral"
      # Code generation — Qwen3 Coder 480B (active 35B, MoE) via
      # Bedrock. Off the general-purpose leaderboard but
      # purpose-built for code; the leaderboard's general
      # benchmarks systematically underrate code specialists.
      # $0.60/$1.80, Apache 2. Fallback to GPT-5.4 via
      # codex-subscription — different vendor, plan-billed.
      model: "qwen.qwen3-coder-480b-a35b-v1:0"
      modelFallback: "gpt-5.4"
      maxTokens: 16384
      # outputSchema replaces requiredOutputKeys + prose Output blocks.
      # See configs/swarms/dev-swarm.md's coder for the rationale.
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [implementation]
        properties:
            implementation:
                type: object
      runtime:
        image: "localhost/vornik-agent:latest"
        cpu: "2"
        memory: "4Gi"
        envVars:
            # Coder previously capped at 50; observed implementations of
            # multi-file features routinely topped 50 tool calls and the
            # task hard-failed. Raised to 80 as a bridge while the
            # checkpoint+continuation path absorbs anything beyond. The
            # cap exists to bound a confused agent's burn rate; ~80 calls
            # at $0.60 / $3.00 input/output is still under $1 per stuck
            # task, which is the right ceiling.
            # 2026-05-14: 80 → 200 to match longer-running tasks.
            VORNIK_MAX_TOOL_ITERATIONS: "200"
      permissions:
        allowedTools:
            - "current_time"
            - "file_read"
            - "file_write"
            - "file_edit"
            - "run_shell"
            - "read_many_files"
            - "grep"
            - "glob"
            - "git_status"
            - "git_diff"
            - "git_log"
            - "git_show"
            - "test_run"
            - "lint_run"
            - "typecheck_run"
        delegationAllowed: false
    - name: "tester"
      description: "Runs focused tests and reports JSON pass/fail"
      count: 1
      runtimePolicy: "ephemeral"
      # Test authoring + execution — MiniMax M2.5 via Bedrock.
      # Rank 9, 1M context fits noisy stack traces without
      # truncation. $0.30/$1.20. Fallback to GPT-5.4-mini.
      model: "zai.glm-4.7-flash"
      modelFallback: "gpt-5.4-mini"
      maxTokens: 8192
      # outputSchema pins testing.passed:bool because workflow gates
      # branch on it. See configs/swarms/dev-swarm.md's tester for
      # the rationale.
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [testing]
        properties:
            testing:
                type: object
                required: [passed]
                properties:
                    passed: {type: bool}
                    failures: {type: string}
                    ran: {type: string}
                    summary: {type: string}
                    tests_available: {type: bool}
                    manual_check: {type: string}
                    acceptance_met: {type: bool}
                    reason: {type: string}
        plausibility:
            - name: failure_explained
              when: {"testing.passed": false}
              require: ["testing.failures"]
      runtime:
        image: "localhost/vornik-agent:latest"
        cpu: "2"
        memory: "4Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "70"
      permissions:
        allowedTools: ["file_read", "file_write", "run_shell", "file_edit", "read_many_files", "grep", "glob", "git_status", "git_diff", "git_log", "test_run", "lint_run", "typecheck_run", "current_time"]
        delegationAllowed: false
    - name: "reviewer"
      description: "Reviews the latest code change against the spec"
      count: 1
      runtimePolicy: "ephemeral"
      # Reviewer — Kimi K2.5 via Bedrock. Rank 3 on the open-LLM
      # leaderboard. Different vendor from the coder (Qwen) so the
      # reviewer's reasoning chain genuinely differs; mistakes
      # that look fine inside Qwen-Coder's reasoning surface here.
      # $0.60/$3.00 (×2 reasoning), Apache 2. Fallback to GPT-5.4
      # via codex-subscription.
      model: "zai.glm-4.7-flash"
      modelFallback: "gpt-5.4"
      maxTokens: 8192
      # outputSchema pins review.approved:bool because workflow gates
      # branch on it. See configs/swarms/dev-swarm.md's reviewer
      # for the rationale.
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [review]
        properties:
            review:
                type: object
                required: [approved]
                properties:
                    approved: {type: bool}
                    all_done: {type: bool}
                    feedback: {type: string}
                    checked_commit: {type: string}
                    summary: {type: string}
                    remaining: {type: array}
        plausibility:
            - name: rejected_explained
              when: {"review.approved": false}
              require: ["review.feedback"]
      runtime:
        image: "localhost/vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "60"
      permissions:
        allowedTools: ["file_read", "file_write", "run_shell", "file_edit", "read_many_files", "grep", "glob", "git_status", "git_diff", "git_log", "git_show", "test_run", "lint_run", "typecheck_run", "current_time"]
        delegationAllowed: false
    - name: "scout"
      description: "Writes concise PROJECT_CONTEXT.md for a project"
      count: 1
      runtimePolicy: "ephemeral"
      # Codebase exploration + summarisation — MiniMax M2.5 via
      # Bedrock. Rank 9 on the leaderboard, 1M context handles
      # multi-file projects without summarisation. $0.30/$1.20.
      # Fallback to GPT-5.4-mini via codex-subscription.
      model: "zai.glm-4.7-flash"
      modelFallback: "gpt-5.4-mini"
      maxTokens: 4096
      # outputSchema replaces requiredOutputKeys + Output blocks. See
      # configs/swarms/dev-swarm.md's scout for the rationale.
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [scout, produced_files]
        properties:
            scout:
                type: object
                required: [project_context_written]
                properties:
                    project_context_written: {type: bool}
                    files_read: {type: number}
                    tech_stack: {type: string}
                    status: {type: string}
                    reason: {type: string}
            produced_files:
                type: array
        plausibility:
            - name: written_implies_files
              when: {"scout.project_context_written": true}
              require: ["produced_files"]
            - name: not_written_implies_reason
              when: {"scout.project_context_written": false}
              require: ["scout.reason"]
      runtime:
        image: "localhost/vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "50"
      permissions:
        allowedTools: ["file_read", "file_write", "run_shell", "file_edit", "read_many_files", "grep", "glob", "git_status", "git_log", "current_time"]
        delegationAllowed: false
    - name: "architect"
      description: "Updates roadmap/backlog from recent progress"
      count: 1
      runtimePolicy: "ephemeral"
      # Roadmap reasoning — GLM-4.7 via Bedrock. Rank 7 on the
      # leaderboard, multi-step reasoning at $0.60/$2.20 (cheaper
      # than glm-5 but still capable). Architect commits a single
      # file edit per run, bounded scope. Fallback to GPT-5.4-mini
      # via codex-subscription — sufficient for the bounded shape.
      model: "zai.glm-4.7-flash"
      modelFallback: "gpt-5.4-mini"
      maxTokens: 4096
      # outputSchema replaces requiredOutputKeys + Output blocks. See
      # configs/swarms/dev-swarm.md's architect for the rationale.
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [architect]
        properties:
            architect:
                type: object
                required: [committed]
                properties:
                    committed: {type: bool}
                    files_changed: {type: number}
                    commit: {type: string}
                    summary: {type: string}
                    reason: {type: string}
        plausibility:
            - name: committed_implies_sha
              when: {"architect.committed": true}
              require: ["architect.commit"]
            - name: not_committed_implies_reason
              when: {"architect.committed": false}
              require: ["architect.reason"]
      runtime:
        image: "localhost/vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "35"
      permissions:
        allowedTools: ["file_read", "file_write", "run_shell", "file_edit", "read_many_files", "grep", "glob", "git_status", "git_diff", "git_log", "git_show", "current_time"]
        delegationAllowed: false
---

# Dev swarm (lead + feasibility + analyst + coder + tester + reviewer + scout + architect)

## Role prompts

### lead

Lead for code projects. project/ is the repo. Prefer
PROJECT_CONTEXT.md and short git history over broad scans.

Plan mode: output only the required plan JSON; choose the
fewest roles. Autonomy create_task: use workflow_id "adaptive",
write a self-contained prompt, avoid duplicate in-progress or
recent work.

Continuation handling: when project/.autonomy/CURRENT_TASK.md exists and
has unchecked subtasks ([ ] entries), the previous session
ended with a partial-completion checkpoint. The next autonomy
task should explicitly ask the dev-pipeline to "continue the
in-progress feature in CURRENT_TASK.md from the next unchecked
subtask" — do NOT pick a new feature from the backlog while
one is mid-flight. Only when CURRENT_TASK.md is absent OR
every subtask is [x] should you schedule a fresh feature.

Output is a JSON object with `plan.steps` (array of role names from the swarm catalog) and `message` (forwarded to the first role). The executor injects the authoritative format spec at runtime; follow that spec.

### feasibility

Feasibility assessor. Read PROJECT_CONTEXT.md, relevant backlog
lines, and `cd project && git log --oneline -12`. Check direct
dependencies only.

### analyst

Analyst. Read PROJECT_CONTEXT.md if present, then the
referenced backlog or requested feature. Write
project/.autonomy/CURRENT_TASK.md with: feature, acceptance criteria,
real files to change, short implementation plan. Verify file
paths with `find project -maxdepth 3 -type f -not -path '*/.git/*'`
when unsure.

The exact `analysis` sub-fields vary by workflow step. The
step's `prompt` field is the authoritative spec for the
sub-fields; produce what it asks for inside the top-level
`analysis` object.

### coder

Coder. Read PROJECT_CONTEXT.md if present and
project/.autonomy/CURRENT_TASK.md or previousStepResult. Inspect files
before editing. Make minimal changes for the requested subtask
or fix feedback. Commit before claiming success.

Required commands: `cd project && git add -A && git diff --cached --stat`,
`cd project && git commit -m '<message>'`,
`cd project && git rev-parse HEAD`.

The exact `implementation` sub-fields vary by workflow step.
The step's `prompt` field is the authoritative spec; produce
what it asks for inside the top-level `implementation` object.

### tester

Tester. Read CURRENT_TASK.md and PROJECT_CONTEXT.md if present.
Run the existing focused test/lint command when available; add
tests only for clear uncovered acceptance criteria. If no test
harness exists, inspect source and report manual evidence
without pretending tests passed.

### reviewer

Reviewer. Read CURRENT_TASK.md, then inspect latest commit with
`cd project && git show --stat HEAD` and changed files/diff.
Check each acceptance criterion. If approved and all subtasks
are done, update backlog only when the path is clear and commit
that doc change.

### scout

Scout. Only document the existing project; do not implement
requested features. List files with
`find project -maxdepth 3 -type f -not -path '*/.git/*' | head -80`,
read key config/docs/source files, and
`cd project && git log --oneline -12`.

Write project/.autonomy/PROJECT_CONTEXT.md with: Overview, Tech Stack,
Project Structure, Build & Test, Conventions, Key Files, Current
State. Keep it factual and compact; cite observed files.

ALWAYS list the files you wrote in `produced_files` at the top
level — the executor verifies each path exists.

### architect

Architect. Read PROJECT_CONTEXT.md to locate roadmap/backlog,
read only relevant sections, and check
`cd project && git log --oneline -12`. Mark done only when
commits support it. Update roadmap/backlog and commit.
