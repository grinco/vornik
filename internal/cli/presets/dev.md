---
swarmId: "__TEMPLATE__"
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
      runtimePolicy: "warm"
      maxTokens: 2048
      requiredOutputKeys: ["plan"]
      runtime:
        image: "vornik-agent:latest"
        cpu: "2"
        memory: "4Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "12"
      permissions:
        allowedTools: ["file_read", "file_write", "run_shell", "read_many_files", "grep", "glob", "git_status", "git_log", "current_time"]
        delegationAllowed: true
        autonomousTaskCreation: true
        maxDelegations: 10
    - name: "feasibility"
      description: "Checks whether requested code work is blocked"
      count: 1
      runtimePolicy: "ephemeral"
      maxTokens: 2048
      # outputSchema replaces requiredOutputKeys + the prose Output
      # block. See configs/swarms/dev-swarm.yaml's feasibility for
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
        image: "vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "14"
      permissions:
        allowedTools: ["file_read", "run_shell", "read_many_files", "grep", "glob", "git_status", "git_log", "current_time"]
        delegationAllowed: false
    - name: "analyst"
      description: "Selects/specs the next code task in CURRENT_TASK.md"
      count: 1
      runtimePolicy: "ephemeral"
      maxTokens: 4096
      # outputSchema replaces requiredOutputKeys + prose Output blocks.
      # See configs/swarms/dev-swarm.yaml for the rationale.
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [analysis]
        properties:
            analysis:
                type: object
      runtime:
        image: "vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "20"
      permissions:
        allowedTools: ["file_read", "file_write", "run_shell", "file_edit", "read_many_files", "grep", "glob", "git_status", "git_log", "current_time"]
        delegationAllowed: false
    - name: "coder"
      description: "Implements focused code changes and commits them"
      count: 2
      runtimePolicy: "ephemeral"
      maxTokens: 8192
      # outputSchema replaces requiredOutputKeys + prose Output blocks.
      # See configs/swarms/dev-swarm.yaml's coder for the rationale.
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [implementation]
        properties:
            implementation:
                type: object
      runtime:
        image: "vornik-agent:latest"
        cpu: "2"
        memory: "4Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "50"
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
      maxTokens: 4096
      # outputSchema pins testing.passed:bool because workflow gates
      # branch on it. See configs/swarms/dev-swarm.yaml's tester for
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
        image: "vornik-agent:latest"
        cpu: "2"
        memory: "4Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "28"
      permissions:
        allowedTools: ["file_read", "file_write", "run_shell", "file_edit", "read_many_files", "grep", "glob", "git_status", "git_diff", "git_log", "test_run", "lint_run", "typecheck_run", "current_time"]
        delegationAllowed: false
    - name: "reviewer"
      description: "Reviews the latest code change against the spec"
      count: 1
      runtimePolicy: "ephemeral"
      maxTokens: 4096
      # outputSchema pins review.approved:bool because workflow gates
      # branch on it. See configs/swarms/dev-swarm.yaml's reviewer
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
        image: "vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "24"
      permissions:
        allowedTools: ["file_read", "file_write", "run_shell", "file_edit", "read_many_files", "grep", "glob", "git_status", "git_diff", "git_log", "git_show", "test_run", "lint_run", "typecheck_run", "current_time"]
        delegationAllowed: false
    - name: "scout"
      description: "Writes concise PROJECT_CONTEXT.md for a project"
      count: 1
      runtimePolicy: "ephemeral"
      maxTokens: 4096
      # outputSchema replaces requiredOutputKeys + the prose Output
      # block. See configs/swarms/dev-swarm.yaml's scout for the
      # rationale.
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [scout]
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
        plausibility:
            - name: not_written_implies_reason
              when: {"scout.project_context_written": false}
              require: ["scout.reason"]
      runtime:
        image: "vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "28"
      permissions:
        allowedTools: ["file_read", "file_write", "run_shell", "file_edit", "read_many_files", "grep", "glob", "git_status", "git_log", "current_time"]
        delegationAllowed: false
    - name: "architect"
      description: "Updates roadmap/backlog from recent progress"
      count: 1
      runtimePolicy: "ephemeral"
      maxTokens: 4096
      # outputSchema replaces requiredOutputKeys + the prose Output
      # block. See configs/swarms/dev-swarm.yaml's architect for the
      # rationale.
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
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "20"
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

### architect

Architect. Read PROJECT_CONTEXT.md to locate roadmap/backlog,
read only relevant sections, and check
`cd project && git log --oneline -12`. Mark done only when
commits support it. Update roadmap/backlog and commit.
