---
swarmId: "__TEMPLATE__"
displayName: Basic code swarm (lead + coder + reviewer)
leadRole: lead
rolePrelude: |
    You are part of a multi-agent swarm. Assume another role will
    review your output before it affects the user — never guess when
    you can check, and never edit files outside the task workspace.
roles:
    - name: "lead"
      description: "Plans implementation, delegates to coder / reviewer"
      count: 1
      runtimePolicy: "warm"
      maxTokens: 2048
      # requiredOutputKeys enforces the plan shape so downstream gates
      # never see undefined fields. Keep the list tight — one key per
      # role is usually enough; expand once the failure-class dashboard
      # shows output-shape drift.
      requiredOutputKeys: ["plan"]
      runtime:
        image: "vornik-agent:latest"
        cpu: "2"
        memory: "4Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "12"
      permissions:
        allowedTools: ["file_read", "run_shell", "read_many_files", "grep", "glob", "git_status", "git_log", "current_time"]
        delegationAllowed: true
        autonomousTaskCreation: true
        maxDelegations: 5
    - name: "coder"
      description: "Implements the requested change and commits"
      count: 1
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
            VORNIK_MAX_TOOL_ITERATIONS: "40"
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
    - name: "reviewer"
      description: "Reviews the last change against the spec"
      count: 1
      runtimePolicy: "ephemeral"
      maxTokens: 4096
      # outputSchema pins review.approved:bool because the common
      # reviewer gate "review.approved == true" branches on it. See
      # configs/swarms/dev-swarm.yaml's reviewer for the rationale.
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
                    feedback: {type: string}
                    checked_commit: {type: string}
                    summary: {type: string}
        plausibility:
            - name: rejected_explained
              when: {"review.approved": false}
              require: ["review.feedback"]
      runtime:
        image: "vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "18"
      permissions:
        allowedTools:
            - "current_time"
            - "file_read"
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
---

# Basic code swarm (lead + coder + reviewer)

## Role prompts

### lead

Technical lead. Produce a concise implementation plan using
only needed steps. Prefer existing context and short git/file
checks over broad scans. Output a single JSON object with a
"plan" array of role assignments.

Output is a JSON object with `plan.steps` (array of role names from the swarm catalog) and `message` (forwarded to the first role). The executor injects the authoritative format spec at runtime; follow that spec.

### coder

Software engineer. Implement only the requested change.
Inspect files before editing. Run relevant checks. Commit
before claiming success — `cd project && git rev-parse HEAD`
MUST succeed with a non-empty sha.

The exact `implementation` sub-fields are dictated by the
workflow step's `prompt` field; produce what it asks for
inside the top-level `implementation` object.

### reviewer

Code reviewer. Read CURRENT_TASK.md (if present) then inspect
the last commit with `cd project && git show --stat HEAD` and
the changed files. Report approved/rejected plus specific
feedback.
