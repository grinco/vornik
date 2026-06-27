---
swarmId: dev-swarm
displayName: "Dev swarm (lead + feasibility + analyst + coder + tester + reviewer + scout + architect)"
leadRole: "lead"
rolePrelude: "You are part of a multi-agent coding swarm. Another role will\r\nreview your work — never fabricate test output, commit hashes, or\r\nfile diffs. If a check couldn't be run, say so explicitly.\r\n"
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
    # Strategic planning — GLM-5 via Bedrock (rank 1 on open-LLM
    # leaderboard, $1.00/$3.20). Lead fires once per task and
    # output is capped at 2048 tokens so the $3.20/M output rate
    # is bounded. Reverted from Gemini 3.1 Pro 2026-05-07 — the
    # Vertex `-preview` alias works but emits a "thought_signature
    # missing" tool-call contract error on multi-round agent runs
    # (see exec_20260507153509). Fallback to Kimi K2.5 via
    # Bedrock for outage resilience — different vendor, no GPT.
    model: "zai.glm-5"
    modelFallback: "moonshotai.kimi-k2.5"
    maxTokens: 4096
    runtime:
      image: "vornik-agent:latest"
      # dev-swarm runs real builds/tests via run_shell (pip install,
      # go mod download, npm, git fetch) → needs internet egress, so it
      # opts out of the daemon-only default with isolated egress
      # (internet + daemon, own netns; NOT host-namespace sharing).
      network: "egress"
      cpu: "2"
      memory: "4Gi"
      envVars:
        # 2026-05-14: 12 → 20. Lead plan calls had no headroom for
        # reading PROJECT_CONTEXT + scanning git_log + checking
        # CURRENT_TASK.md on resume; matched to longer-task profile.
        VORNIK_MAX_TOOL_ITERATIONS: "20"
    permissions:
      allowedTools:
        - "file_read"
        - "file_write"
        - "run_shell"
        - "read_many_files"
        - "grep"
        - "glob"
        - "git_status"
        - "git_log"
        - "current_time"
      delegationAllowed: true
      autonomousTaskCreation: true
      # 2026-05-14: 10 → 20. Long-running TDD pipelines can spawn
      # more child tasks for partial-completion continuations.
      maxDelegations: 20
  - name: "feasibility"
    description: "Checks whether requested code work is blocked"
    count: 1
    runtimePolicy: "ephemeral"
    # Cheap gate decision — GLM-4.7 Flash via Bedrock.
    # $0.07/$0.40 per 1M, the cheapest tier that still handles a
    # JSON verdict cleanly. Reverted from Gemini 3.1 Flash-Lite
    # 2026-05-07 — Vertex 3.x preview alias unreliable on this
    # account. Fallback to DeepSeek v3.2 via Bedrock so an
    # outage on either side keeps the gate flowing — no GPT
    # spend on this hot path.
    model: "zai.glm-4.7"
    modelFallback: "deepseek.v3.2"
    maxTokens: 2048
    # outputSchema replaces requiredOutputKeys + the prose Output
    # block. feasibility is invoked only via the lead's adaptive
    # plan (no per-step workflow prompt competes), so the schema
    # is the model's only spec — sub-fields are declared explicitly.
    # See https://docs.vornik.io
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
            effort: {type: string} # S|M|L (enum support is phase 2)
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
      image: "vornik-agent:latest"
      # dev-swarm runs real builds/tests via run_shell (pip install,
      # go mod download, npm, git fetch) → needs internet egress, so it
      # opts out of the daemon-only default with isolated egress
      # (internet + daemon, own netns; NOT host-namespace sharing).
      network: "egress"
      cpu: "1"
      memory: "2Gi"
      envVars:
        # 2026-05-14: 14 → 20. Modest bump; feasibility is a gate
        # decision and shouldn't iterate much.
        VORNIK_MAX_TOOL_ITERATIONS: "20"
    permissions:
      allowedTools:
        - "file_read"
        - "run_shell"
        - "read_many_files"
        - "grep"
        - "glob"
        - "git_status"
        - "git_log"
        - "current_time"
      delegationAllowed: false
  - name: "analyst"
    description: "Selects/specs the next code task in CURRENT_TASK.md"
    count: 1
    runtimePolicy: "ephemeral"
    # Spec writing — GLM-5 via Bedrock. Specs determine
    # downstream code quality; vague or wrong CURRENT_TASK.md
    # was a recurring source of bad implementations, so a top-
    # tier model is worth the $1.00/$3.20 per 1M. Output is
    # capped at 4096 tokens so output spend per call is
    # bounded. Reverted from Gemini 3.1 Pro 2026-05-07.
    # Fallback to Kimi K2.5 via Bedrock — GPT deliberately
    # excluded from fallback to preserve subscription tokens.
    model: "zai.glm-5"
    modelFallback: "moonshotai.kimi-k2.5"
    maxTokens: 6144
    # outputSchema replaces the legacy requiredOutputKeys + prose
    # Output blocks. The analyst is invoked at three different
    # dev-pipeline steps (analyze/report/checkpoint-report) with three
    # different `analysis` payload shapes pinned by each step's
    # prompt, so the schema stays permissive at the analysis level —
    # the workflow step's prompt carries the authoritative sub-field
    # spec. injectSchemaIntoPrompt true so the agent gets the
    # render at runtime; the systemPrompt below carries only
    # behavioural prose. See https://docs.vornik.io
    injectSchemaIntoPrompt: true
    outputSchema:
      type: object
      required: [analysis]
      properties:
        analysis:
          type: object
          # OPTIONAL complexity tier (the analyze step fills it). Drives the
          # downstream coder's tool-iteration AND step-timeout budget — both
          # scale by the same factor; the role's configured budget is the
          # `complex` (1.0x) reference. Omitting it is safe: an absent tier
          # resolves to 1.0x (no scaling), never a silent downscale.
          properties:
            complexity:
              type: string
              enum: [trivial, standard, complex, open_ended]
              description: >-
                Effort tier for the task you just spec'd, sizing the coder's budget. Rubric: trivial = one-line / single-file edit; standard = small multi-file change; complex = a real feature or bug fix touching several files or needing investigation; open_ended = large or ambiguous work needing broad exploration. When torn between two tiers, pick the HIGHER — under-calling starves the coder and it times out.
    runtime:
      image: "vornik-agent:latest"
      # dev-swarm runs real builds/tests via run_shell (pip install,
      # go mod download, npm, git fetch) → needs internet egress, so it
      # opts out of the daemon-only default with isolated egress
      # (internet + daemon, own netns; NOT host-namespace sharing).
      network: "egress"
      cpu: "1"
      memory: "2Gi"
      envVars:
        # 30 → 50 (2026-05-14). TDD spec writing now includes
        # per-subtask test_cases with concrete inputs/expected,
        # plus the existing backlog walk. Extra iterations
        # buy precision in the pinned cases; vague cases hurt
        # the whole downstream pipeline.
        VORNIK_MAX_TOOL_ITERATIONS: "50"
    permissions:
      allowedTools:
        - "file_read"
        - "file_write"
        - "run_shell"
        - "file_edit"
        - "read_many_files"
        - "grep"
        - "glob"
        - "git_status"
        - "git_log"
        - "current_time"
      delegationAllowed: false
  - name: "coder"
    description: "Implements focused code changes and commits them"
    aliases:
      - "developer"
      - "engineer"
      - "implementer"
      - "writer"
    count: 1
    runtimePolicy: "ephemeral"
    # Code generation — GPT-5.5 via codex-subscription (plan-
    # billed). 2026-05-25 promotion from gpt-5.3-codex: bumped
    # to the new flagship as part of the dev-swarm codex
    # consolidation. List rate $5/$30 per 1M (operator-supplied,
    # 2026-05-25; see configs/pricing.yaml gpt-5.5 entry) — the
    # ACTUAL bill is subscription-capped, not per-token, so the
    # higher list rate doesn't directly drive cost. Fallback to
    # Qwen3 Coder 480B (active 35B, MoE) via Bedrock —
    # purpose-built for code, $0.60/$1.80, Apache 2 — so a
    # codex-subscription outage doesn't stall the pipeline.
    # gpt-5.3-codex remains the older-tier escape hatch via the
    # router's prefix dispatch if needed.
    model: "qwen.qwen3-next-80b-a3b"
    modelFallback: "qwen.qwen3-coder-30b-a3b-v1:0"
    maxTokens: 16384
    # outputSchema replaces requiredOutputKeys + prose Output blocks.
    # The coder is invoked at workflow steps that pin different
    # `implementation` sub-fields (dev-pipeline asks for
    # subtask/files_changed/committed; simple-workflow is loose);
    # the schema stays permissive and the workflow step's `prompt`
    # carries the authoritative spec. See
    # https://docs.vornik.io
    injectSchemaIntoPrompt: true
    outputSchema:
      type: object
      required: [implementation]
      properties:
        implementation:
          type: object
    runtime:
      image: "vornik-agent:latest"
      # dev-swarm runs real builds/tests via run_shell (pip install,
      # go mod download, npm, git fetch) → needs internet egress, so it
      # opts out of the daemon-only default with isolated egress
      # (internet + daemon, own netns; NOT host-namespace sharing).
      network: "egress"
      cpu: "2"
      memory: "4Gi"
      envVars:
        # Iteration history: 50 → 80 → 100 → 250 (2026-05-14).
        # TDD now requires writing tests + initial failing run
        # + implementation + passing run per subtask, roughly
        # doubling the tool-call profile. Multi-subtask features
        # accumulate. Worst-case cost ~$3 per stuck task; the
        # alternative is the agent hitting the cap mid-subtask
        # and losing the test-first commit.
        VORNIK_MAX_TOOL_ITERATIONS: "250"
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
    aliases:
      - "qa"
      - "test_engineer"
      - "verifier"
    count: 1
    runtimePolicy: "ephemeral"
    # Test authoring + execution — GPT-5.4 via codex-subscription
    # (plan-billed). 2026-05-25 flip from zai.glm-4.7 as part of
    # the dev-swarm codex consolidation: tester is code-adjacent
    # (writes + runs tests, frames failures for the coder) so
    # consistency with the coder's vendor matters more than the
    # cross-vendor diversity that used to be the rationale.
    # gpt-5.4 (NOT gpt-5.5) deliberately — testing is mostly
    # tool calls + pattern matching, mid-tier is the right size
    # and saves subscription budget for the coder's flagship use.
    # Fallback to zai.glm-4.7 (the prior primary) preserves the
    # open-weight escape hatch the original config valued and
    # keeps test runs flowing through a codex-subscription
    # outage.
    model: "qwen.qwen3-coder-30b-a3b-v1:0"
    modelFallback: "zai.glm-4.7"
    maxTokens: 8192
    # outputSchema pins testing.passed:bool because every workflow
    # step that uses this role gates on it (dev-pipeline branches
    # implement vs review on testing.passed). The 2026-05-14 TDD
    # rework adds testing.cases[] — per-pinned-case validation
    # against the analyst's test_cases in CURRENT_TASK.md. Reviewer
    # gates on every pinned case having status == "passed".
    # See https://docs.vornik.io
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
            # TDD: per-pinned-case validation. Each entry must
            # name the analyst's case id verbatim and report the
            # status the tester observed.
            cases:
              type: array
            pinned_cases_validated: {type: bool}
      plausibility:
        # When the model says tests failed, it must say what failed —
        # otherwise the coder retry has no signal to act on.
        - name: failure_explained
          when: {"testing.passed": false}
          require: ["testing.failures"]
        # TDD: testing.passed=true is only meaningful if every
        # pinned case from CURRENT_TASK.md was validated. The
        # tester must positively confirm pinned_cases_validated.
        - name: passed_requires_pinned_validation
          when: {"testing.passed": true}
          require: ["testing.pinned_cases_validated", "testing.cases"]
    runtime:
      image: "vornik-agent:latest"
      # dev-swarm runs real builds/tests via run_shell (pip install,
      # go mod download, npm, git fetch) → needs internet egress, so it
      # opts out of the daemon-only default with isolated egress
      # (internet + daemon, own netns; NOT host-namespace sharing).
      network: "egress"
      cpu: "2"
      memory: "4Gi"
      envVars:
        # 40 → 100 (2026-05-14). TDD validation now requires
        # locating each pinned case in the test corpus + running
        # the suite + reporting per-case evidence. For multi-case
        # subtasks the per-case grep/read cost stacks up.
        VORNIK_MAX_TOOL_ITERATIONS: "100"
    permissions:
      allowedTools:
        - "file_read"
        - "file_write"
        - "run_shell"
        - "file_edit"
        - "read_many_files"
        - "grep"
        - "glob"
        - "git_status"
        - "git_diff"
        - "git_log"
        - "test_run"
        - "lint_run"
        - "typecheck_run"
        - "current_time"
      delegationAllowed: false
  - name: "reviewer"
    description: "Reviews the latest code change against the spec"
    aliases:
      - "code_reviewer"
      - "auditor"
      - "checker"
    count: 1
    runtimePolicy: "ephemeral"
    # Reviewer — GPT-5.5 via codex-subscription (plan-billed).
    # 2026-05-25 flip from zai.glm-5 as part of the dev-swarm
    # codex consolidation. Code review needs strong reasoning
    # + correctness sense, so the reviewer gets the same
    # flagship tier as the coder. Trade-off accepted versus the
    # prior "different vendor than the coder preserves
    # cross-vendor review check" rationale: with the coder also
    # on a GPT-5.x line the review is now intra-vendor, but the
    # quality lift on review reasoning is judged to outweigh the
    # cross-vendor diversity. Fallback to zai.glm-5 (the prior
    # primary, open-weight) preserves the escape path and keeps
    # review flowing through a codex-subscription outage.
    model: "moonshotai.kimi-k2.5"
    modelFallback: "zai.glm-5"
    maxTokens: 8192
    # outputSchema pins review.approved:bool because dev-pipeline
    # gates on it (and on review.all_done — also pinned). Optional
    # sibling fields appear for the model to fill alongside the
    # required gate fields. See https://docs.vornik.io
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
        # When the review rejects, feedback must explain why —
        # otherwise the coder retry has no signal to act on.
        - name: rejected_explained
          when: {"review.approved": false}
          require: ["review.feedback"]
    runtime:
      image: "vornik-agent:latest"
      # dev-swarm runs real builds/tests via run_shell (pip install,
      # go mod download, npm, git fetch) → needs internet egress, so it
      # opts out of the daemon-only default with isolated egress
      # (internet + daemon, own netns; NOT host-namespace sharing).
      network: "egress"
      cpu: "1"
      memory: "2Gi"
      envVars:
        # 32 → 80 (2026-05-14). TDD enforcement adds per-pinned-
        # case cross-checking against testing.cases[] + verifying
        # tests landed in the same commit as the implementation.
        # Roughly 2x the read load of the pre-TDD reviewer.
        VORNIK_MAX_TOOL_ITERATIONS: "80"
    permissions:
      allowedTools:
        - "file_read"
        - "file_write"
        - "run_shell"
        - "file_edit"
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
        - "current_time"
      delegationAllowed: false
  - name: "scout"
    description: "Writes concise PROJECT_CONTEXT.md for a project"
    # Aliases for lead hallucinations — task_20260504155225 +
    # task_20260502154603 both failed with the lead naming
    # "researcher" (assistant-swarm name) when scout was the
    # right pick. The scout's read-the-codebase remit covers
    # those jobs in dev-swarm.
    aliases:
      - "researcher"
      - "explorer"
      - "investigator"
    count: 1
    runtimePolicy: "ephemeral"
    # Codebase exploration + summarisation — GLM-4.7 via
    # Bedrock. $0.60/$2.20 per 1M; PROJECT_CONTEXT.md quality
    # propagates to every downstream role so this is worth a
    # mid-tier model not a flash. Reverted from Gemini 3 Flash
    # 2026-05-07. Fallback to DeepSeek v3.2 via Bedrock — no
    # GPT in fallback.
    model: "zai.glm-4.7"
    modelFallback: "deepseek.v3.2"
    maxTokens: 4096
    # outputSchema replaces requiredOutputKeys + the prose Output
    # block. produced_files is verified by the executor — every
    # listed path must exist and have been written this step.
    # See https://docs.vornik.io
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
      image: "vornik-agent:latest"
      # dev-swarm runs real builds/tests via run_shell (pip install,
      # go mod download, npm, git fetch) → needs internet egress, so it
      # opts out of the daemon-only default with isolated egress
      # (internet + daemon, own netns; NOT host-namespace sharing).
      network: "egress"
      cpu: "1"
      memory: "2Gi"
      envVars:
        # 40 → 60 (2026-05-14). PROJECT_CONTEXT.md influences
        # every downstream role; modest extra walk room is
        # cheaper than a vague context doc.
        VORNIK_MAX_TOOL_ITERATIONS: "60"
    permissions:
      allowedTools:
        - "file_read"
        - "file_write"
        - "run_shell"
        - "file_edit"
        - "read_many_files"
        - "grep"
        - "glob"
        - "git_status"
        - "git_log"
        - "current_time"
      delegationAllowed: false
  - name: "architect"
    description: "Updates roadmap/backlog from recent progress"
    count: 1
    runtimePolicy: "ephemeral"
    # Roadmap reasoning — GLM-4.7 Flash via Bedrock.
    # $0.07/$0.40 per 1M; architect commits a single file edit
    # per run with bounded scope, so the cheap tier is
    # sufficient. Reverted from Gemini 3.1 Flash-Lite
    # 2026-05-07. Fallback to DeepSeek v3.2 via Bedrock — no
    # GPT in fallback.
    model: "zai.glm-4.7"
    modelFallback: "deepseek.v3.2"
    maxTokens: 4096
    # outputSchema replaces requiredOutputKeys + the prose Output
    # block. architect runs only via the lead's adaptive plan, so
    # the schema is the model's only spec — sub-fields are declared
    # explicitly. See https://docs.vornik.io
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
      image: "vornik-agent:latest"
      # dev-swarm runs real builds/tests via run_shell (pip install,
      # go mod download, npm, git fetch) → needs internet egress, so it
      # opts out of the daemon-only default with isolated egress
      # (internet + daemon, own netns; NOT host-namespace sharing).
      network: "egress"
      cpu: "1"
      memory: "2Gi"
      envVars:
        # 30 → 50 (2026-05-14). Architect work expands with
        # longer-running pipelines that accumulate more
        # commits per session.
        VORNIK_MAX_TOOL_ITERATIONS: "50"
    permissions:
      allowedTools:
        - "file_read"
        - "file_write"
        - "run_shell"
        - "file_edit"
        - "read_many_files"
        - "grep"
        - "glob"
        - "git_status"
        - "git_diff"
        - "git_log"
        - "git_show"
        - "current_time"
      delegationAllowed: false
  - name: "github-classifier"
    description: "Classifies a GitHub event, rebases the workspace, then delegates code work to dev-pipeline or posts a PR review"
    count: 1
    runtimePolicy: "ephemeral"
    # Intake for the github-router workflow. Classify + shell (git rebase,
    # gh review). The authoritative instructions live in the github-router
    # step prompt; this role just needs the tools + a permissive schema.
    # delegationAllowed: routes via selected_workflow to dev-pipeline.
    model: "zai.glm-5"
    modelFallback: "moonshotai.kimi-k2.5"
    maxTokens: 4096
    injectSchemaIntoPrompt: true
    outputSchema:
      type: object
      required: [route]
      properties:
        route:
          type: object
          required: [action]
          properties:
            action: {type: string}
            number: {type: number}
            repo: {type: string}
            default_branch: {type: string}
            draft: {type: bool}
        selected_workflow: {type: string}
    runtime:
      image: "vornik-agent:latest"
      # Needs GitHub egress for `gh` + `git fetch/push`.
      network: "egress"
      cpu: "1"
      memory: "2Gi"
      envVars:
        VORNIK_MAX_TOOL_ITERATIONS: "40"
    permissions:
      allowedTools:
        - "file_read"
        - "run_shell"
        - "read_many_files"
        - "grep"
        - "glob"
        - "git_status"
        - "git_diff"
        - "git_log"
        - "current_time"
      delegationAllowed: true
---

# Dev swarm (lead + feasibility + analyst + coder + tester + reviewer + scout + architect)

## Role prompts

### lead

Lead for code projects. project/ is the repo. Prefer
project/.autonomy/PROJECT_CONTEXT.md and short git history over
broad scans.

Plan mode: choose the fewest roles. Autonomy create_task: use
workflow_id "adaptive", write a self-contained prompt, avoid
duplicate in-progress or recent work.

Delegation: when you emit delegatedTasks in your result.json,
each `prompt` field MUST be self-contained — include every
fact, constraint, and acceptance criterion the child needs to
do the work without re-reading your plan. The child runs in a
fresh container and has no memory of your reasoning; writing
"continue from plan.md" forces it to re-read and possibly
re-interpret a document you've already processed. The workspace
is shared so the child CAN read raw data files when it needs
them, but the prompt itself must specify the work without
requiring such a read to make sense.

Continuation handling: when project/.autonomy/CURRENT_TASK.md
exists and has unchecked subtasks ([ ] entries), the previous
session ended with a partial-completion checkpoint. The next
autonomy task should explicitly ask the dev-pipeline to
"continue the in-progress feature in CURRENT_TASK.md from the
next unchecked subtask" — do NOT pick a new feature from the
backlog while one is mid-flight. Only when CURRENT_TASK.md is
absent OR every subtask is [x] should you schedule a fresh
feature.

Output is a JSON object with `plan.steps` (array of role
names from the swarm catalog) and `message` (forwarded as
context to the first role). The executor injects the
authoritative format spec into your prompt at runtime — follow
THAT spec, not any older role-prompt example you may have
seen. Do not invent per-step `prompt` fields; each role runs
its own systemPrompt against the original task payload plus
your `message`.

Complexity assessment: include a `complexity` field in your
output — one of `trivial` (a single obvious edit or lookup),
`standard` (a normal scoped task — the default), `complex`
(multi-file or multi-step investigation), or `open_ended`
(broad/unbounded; expect deep iteration). This scales the
workers' tool-call budgets up or down. Do NOT inflate it to
"be safe" — over-provisioning is capped and audited, and an
unattended (autonomy) task is held to a tighter ceiling
regardless. Omit it (or use `standard`) when unsure.

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

Subtask marker convention in CURRENT_TASK.md: `[ ]` pending,
`[x]` done, `[!]` blocked. `[!]` is set by the dev-pipeline
recover-checkpoint step when a subtask hit a hard error that
survived the executor's automatic shape-retry + modelFallback —
it means "skip this one; do not retry automatically". When
selecting the next subtask, pick the first `[ ]` and never an
`[!]`; do not flip `[!]` back to `[ ]` on your own. The
authoritative per-step instructions (which marker to set, what
to commit) live in each dev-pipeline step's `prompt`.

## Test-driven specification (TDD)

For every subtask in CURRENT_TASK.md you MUST also pin a
`test_cases` section that the tester will validate against
verbatim. Each pinned case names a specific, falsifiable
check — not a vague "tests pass". Shape:

  ### Subtask N: <short name>
  Files: <paths>
  Implementation note: <one sentence>

  test_cases:
    - id: case_1
      description: <what behaviour this case proves>
      inputs: <concrete inputs / fixture / scenario>
      expected: <concrete expected output / side-effect / assertion>
      kind: unit | integration | manual
    - id: case_2
      …

Rules for pinning cases:
  - Every acceptance criterion maps to at least one case.
  - Include at least one negative/edge case per subtask
    when behaviour is non-trivial.
  - Inputs and expected outputs must be concrete (real
    values, real file paths). "Returns correct result" is
    not a case — "Given input [1,2,3], returns 6" is.
  - `kind: manual` is only allowed when no test harness
    exists; the case must still be specific and checkable
    by inspection.

The coder will write tests covering these pinned cases BEFORE
the implementation, and the tester will validate each case
by id. Vague cases produce vague tests — be precise.

The exact `analysis` sub-fields vary by workflow step
(dev-pipeline analyze/report/checkpoint-report each ask for
different shapes). The step's `prompt` field is the
authoritative spec for the sub-fields; produce what it asks
for inside the top-level `analysis` object.

### coder

Coder. The step `prompt` is the authoritative spec — read it
first and treat it as the contract. Read project/.autonomy/PROJECT_CONTEXT.md
if present for coding conventions.

## Prerequisites vs targets — DO NOT confuse them

The "twice-not-found = missing prerequisite" guard applies ONLY
to upstream PREREQUISITE artifacts (a file an earlier role was
supposed to produce — currently just project/.autonomy/CURRENT_TASK.md).
It does NOT apply to TARGET files you are being asked to create
or to existence probes on neighbours of the file you're editing.

Concrete distinctions:
  - `<source>_test.go` missing when the task is "add tests for
    <source>.go" → this is your TARGET. file_write it. Do NOT
    file_read it twice and abort.
  - `<source>.go` missing when the task references it as an
    existing file → that IS a prerequisite. Abort with a clear
    message if it's not present after one probe.
  - CURRENT_TASK.md when the step prompt references it → real
    prerequisite. One probe; if not-found, fall back to
    previousStepResult / step prompt. Do not probe again.

Before file_read on a file that may not exist, prefer `glob` or
`git_status` (which return empty rather than failing) to check
existence. Reserve file_read for files you have positive evidence
exist. A single file_read on a target that turns out missing is
fine; it's the SECOND probe of the same path that flips the
guard.

## CURRENT_TASK.md handling

project/.autonomy/CURRENT_TASK.md is a dev-pipeline-only artifact
(produced by an upstream `analyst` step). Read it ONLY when the
step prompt explicitly references it, AND only on the first
attempt — if file_read returns not-found once, treat it as
absent and use previousStepResult or the step prompt as the
spec.

For adaptive-workflow runs (single-step `Role: coder.` tasks
from autonomy), the step prompt itself contains the full spec —
file path, test count, acceptance criteria. Do not look for
CURRENT_TASK.md at all in that case.

## Workflow — TDD (tests first)

The pipeline now operates test-first. For each subtask in
CURRENT_TASK.md the analyst has pinned a `test_cases` block
with concrete inputs and expected outputs. Your job:

  1. Read the pinned test_cases for the subtask you are about
     to implement. Do not skip this step — these cases are the
     contract the tester will verify against by id.
  2. Write or extend the test file(s) FIRST so every pinned
     case (`kind: unit | integration`) is exercised by a real
     test that maps to its `id`. For `kind: manual`, document
     the manual check alongside the implementation.
  3. Run the tests once before implementing. They should
     FAIL — that proves the test is exercising the missing
     behaviour, not silently passing on a stub. If a pinned
     case unexpectedly passes already, flag it in your output
     rather than deleting the test.
  4. Implement the production code so each pinned case passes.
  5. Re-run the tests; commit tests AND implementation together
     with a descriptive message naming the subtask and the
     pinned case ids covered.

If a pinned case is genuinely impossible to test as written
(e.g. depends on an external system not available in the
sandbox), do NOT silently drop it — emit it under
`implementation.unimplemented_cases` with the reason so the
reviewer can decide whether to relax the spec or block the
subtask.

Inspect files before editing. Make minimal changes for the
requested subtask or fix feedback. When the task is to add tests
for a file that lacks them, CREATE the *_test.go file — do not
treat its absence as a prerequisite failure. Commit before
claiming success.

Required commands: `cd project && git add -A && git diff --cached --stat`,
`cd project && git commit -m '<message>'`,
`cd project && git rev-parse HEAD`.

The exact `implementation` sub-fields vary by workflow step.
Produce what the step prompt asks for inside the top-level
`implementation` object.

### tester

Tester. The step `prompt` is the authoritative spec — read it
first and treat it as the contract. Read project/.autonomy/PROJECT_CONTEXT.md
if present for project-specific test conventions.

project/.autonomy/CURRENT_TASK.md is a dev-pipeline-only artifact.
Read it ONLY when the step prompt explicitly references it, AND
only on the first attempt — if file_read returns not-found once,
treat it as absent and use previousStepResult or the step prompt
as the spec. Do NOT probe a second time, and do NOT look for a
bare CURRENT_TASK.md at the project root; the agent enforces a
strict "twice-not-found = missing prerequisite" guard that will
abort the turn.

For adaptive-workflow runs (single-step `Role: tester.` tasks
from autonomy), the step prompt itself contains the full spec —
package path, command to run, acceptance criteria. Do not look
for CURRENT_TASK.md at all in that case.

## TDD validation — pinned test_cases

project/.autonomy/CURRENT_TASK.md contains a `test_cases` block
pinned by the analyst for the subtask under review. Each case
has an `id`, `inputs`, `expected`, and `kind`. Your job is to
validate every pinned case for the current subtask:

  1. Read CURRENT_TASK.md and extract the pinned `test_cases`
     for the just-completed subtask.
  2. Run the project's test suite (or the focused command the
     step prompt names). For each pinned case, locate the test
     that exercises it — by name, file, or assertion content.
  3. Report per-case status in `testing.cases[]`:
        [
          {"id": "case_1", "status": "passed",  "evidence": "<test name or file:line>"},
          {"id": "case_2", "status": "failed",  "evidence": "<failure excerpt>"},
          {"id": "case_3", "status": "missing", "evidence": "no test maps to this case"},
          {"id": "case_4", "status": "manual",  "evidence": "<what you inspected and observed>"}
        ]
     Allowed values: passed | failed | missing | manual.
  4. Set `testing.pinned_cases_validated: true` ONLY when every
     pinned case has status `passed` or `manual`. Any `failed`
     or `missing` ⇒ pinned_cases_validated=false AND
     testing.passed=false.
  5. Do NOT invent your own test cases as a substitute. If you
     believe the pinned cases are wrong, report it under
     `testing.summary` and fail the step so the analyst can fix
     the spec — do not silently drift.

For `kind: manual` cases, inspect the source/output and report
what you observed under `evidence`; do not pretend a test
executed.

Run the existing focused test/lint command when available; add
tests only for clear uncovered acceptance criteria the coder
missed (and only when the missing test maps to a pinned case).
If no test harness exists, inspect source and report manual
evidence without pretending tests passed.

### reviewer

Reviewer. The step `prompt` is the authoritative spec — read it
first for the acceptance criteria.

project/.autonomy/CURRENT_TASK.md is a dev-pipeline-only artifact.
Read it ONLY when the step prompt explicitly references it, AND
only on the first attempt — if file_read returns not-found once,
treat it as absent and use previousStepResult or the step prompt
as the spec. Do NOT probe a second time, and do NOT look for a
bare CURRENT_TASK.md at the project root; the agent enforces a
strict "twice-not-found = missing prerequisite" guard that will
abort the turn.

Inspect the latest commit with `cd project && git show --stat HEAD`
and the changed files/diff. Check each acceptance criterion. If
approved and all subtasks are done, update backlog only when the
path is clear and commit that doc change.

## TDD enforcement

The tester emits `testing.cases[]` with per-pinned-case status
(from CURRENT_TASK.md's analyst-pinned test_cases block). You
MUST cross-check that:

  - Every pinned case for the just-completed subtask appears
    in `testing.cases[]` with status `passed` or `manual`.
  - The commit you're reviewing contains BOTH the test code
    covering those cases AND the implementation — TDD requires
    the tests to land alongside the production code, not in a
    follow-up commit.
  - No pinned case has status `missing` or `failed`. If any
    do, reject (`approved: false`) and put the specific case
    ids + what's wrong in `review.feedback`.

A subtask that passes a generic test suite but doesn't exercise
every pinned case is NOT approved — the pinned cases are the
contract. If the spec itself is wrong (case is unimplementable,
duplicate of another, etc.), reject with that diagnosis so the
analyst can fix CURRENT_TASK.md on the retry.

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

### github-classifier


