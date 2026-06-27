---
swarmId: "__TEMPLATE__"
displayName: Research swarm (lead + researcher + writer)
leadRole: lead
rolePrelude: |
    You are part of a research swarm. Cite sources explicitly — "I
    searched and found…" is not a source. When you use memory_search,
    quote the source file. When you fetch a URL, include the fetched
    URL in your output.
roles:
    - name: "lead"
      description: "Plans research and writing tasks"
      count: 1
      runtimePolicy: "warm"
      maxTokens: 2048
      requiredOutputKeys: ["plan"]
      runtime:
        image: "vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "12"
      permissions:
        # The lead delegates to researcher/writer; memory_search belongs
        # on the researcher, not here. Keeps the lead allowlist narrow
        # and clears role_prompt_sanity's "never mentions it" lint.
        allowedTools: ["file_read", "read_many_files", "grep", "glob", "current_time"]
        delegationAllowed: true
        autonomousTaskCreation: true
        maxDelegations: 6
    - name: "researcher"
      description: "Gathers sourced facts and writes artifacts/out/research.md"
      count: 1
      runtimePolicy: "ephemeral"
      maxTokens: 6144
      # outputSchema replaces requiredOutputKeys + Output blocks. The
      # research preset is lighter than assistant-swarm (no
      # produced_files declared) — see configs/swarms/assistant-swarm.yaml
      # for the production-shipped variant.
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [research]
        properties:
            research:
                type: object
                required: [written]
                properties:
                    written: {type: bool}
                    sources: {type: array}
                    summary: {type: string}
                    reason: {type: string}
        plausibility:
            - name: not_written_implies_reason
              when: {"research.written": false}
              require: ["research.reason"]
      runtime:
        image: "vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "28"
      permissions:
        allowedTools: ["file_read", "file_write", "run_shell", "read_many_files", "grep", "glob", "memory_search", "current_time", "mcp__*"]
        delegationAllowed: false
    - name: "writer"
      description: "Turns research into a polished deliverable"
      count: 1
      runtimePolicy: "ephemeral"
      maxTokens: 6144
      # outputSchema is the single source of truth for this role's
      # result.json shape — see configs/swarms/assistant-swarm.yaml's
      # writer for the matching block in the production-shipped
      # swarm. autonomy / extractResultMessage reads `message` so it
      # is required + non-empty.
      injectSchemaIntoPrompt: true
      outputSchema:
        type: object
        required: [writing, message]
        properties:
            writing:
                type: object
                required: [written]
                properties:
                    written: {type: bool}
                    path: {type: string}
                    summary: {type: string}
                    reason: {type: string}
            message:
                type: string
                minLength: 1
        plausibility:
            - name: written_implies_path
              when: {"writing.written": true}
              require: ["writing.path"]
            - name: not_written_implies_reason
              when: {"writing.written": false}
              require: ["writing.reason"]
      runtime:
        image: "vornik-agent:latest"
        cpu: "1"
        memory: "2Gi"
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "24"
      permissions:
        # Writer reads research.md produced by the researcher and
        # composes the final deliverable — no memory_search needed.
        # run_shell is required for pandoc-based format conversion
        # (md → pdf/html/docx/…); markdown remains the canonical
        # primary output.
        allowedTools: ["file_read", "file_write", "run_shell", "read_many_files", "grep", "glob", "current_time"]
        delegationAllowed: false
---

# Research swarm (lead + researcher + writer)

## Role prompts

### lead

Lead for a research project. Produce the smallest valid plan
for the requested task. Prefer one researcher run feeding one
writer run over many small round-trips.

Output is a JSON object with `plan.steps` (array of role names from the swarm catalog) and `message` (forwarded to the first role). The executor injects the authoritative format spec at runtime; follow that spec.

### researcher

Researcher. Gather only information needed for the task.
Prefer primary or reputable sources. Avoid rereading known
material — use memory_search first.

Web: use mcp__scraper__web_fetch when available. Respect
rate limits; if a portal blocks the scan, record the failure
and move on — do NOT retry or rotate headers.

Write exactly one file: artifacts/out/research.md with summary,
key facts, source URLs/names, caveats, useful raw notes.

### writer

Writer. Read artifacts/out/research.md (produced by the
researcher). Produce a polished deliverable that cites the
research file for every factual claim. No hedging boilerplate
("as an AI…") — operators forward these verbatim.

Output formats:
  - Default: write artifacts/out/<deliverable-name>.md
    (markdown is the canonical source — ALWAYS produce
    it, even when the user asks for another format).
  - If the user requests another format (PDF, HTML, DOCX,
    EPUB, RTF, ODT, …), convert the markdown via pandoc
    in run_shell AFTER writing the .md file. The agent
    image ships pandoc + weasyprint; no LaTeX toolchain,
    so pass `--pdf-engine=weasyprint` for PDF. Examples:
      pandoc artifacts/out/foo.md \
        --pdf-engine=weasyprint \
        -o artifacts/out/foo.pdf
      pandoc artifacts/out/foo.md --standalone \
        -o artifacts/out/foo.html
      pandoc artifacts/out/foo.md -o artifacts/out/foo.docx
      pandoc artifacts/out/foo.md -o artifacts/out/foo.epub
  - `writing.path` points to the canonical markdown file;
    converted artefacts live alongside it in artifacts/out/.
