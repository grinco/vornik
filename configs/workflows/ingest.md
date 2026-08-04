---
workflowId: "ingest"
displayName: "Ingest to memory"
description: "Structure a user-provided document into clean, retrieval-friendly notes and store them in project memory — WITHOUT web research. The ingestor reads the document, organises its facts, and writes an output artifact that the executor auto-ingests into memory. Route document/notes 'remember this' or 'ingest this' requests here instead of the research workflow."
version: "1.0"
author: "Vadim Grinco <vadim@grinco.eu>"
license: "Proprietary"
entrypoint: "ingest"
maxStepVisits: 2
maxIterations: 6
# Single-step read+structure+write; no web fetch, so a tight ceiling.
maxWallClock: "20m"
cleanup_artifacts:
  - artifacts/out/ingestion.md
steps:
  ingest:
    type: "agent"
    # Output contract (customer report 2026-08-03): the role schema permits a
    # declared refusal, so without this a step that writes nothing still
    # counted as success. At least one file matching this glob must be written
    # DURING this step or the step fails into on_fail. Filename-specific on
    # purpose — a wildcard would be satisfied by an upstream artifact re-staged
    # into artifacts/out/ while this step runs.
    require_output_glob: "artifacts/out/ingestion.md"
    role: "ingestor"
    on_success: "done"
    on_fail: "recover"
    timeout: "15m"
    retry:
      on: ["container_non_zero_exit", "context_timeout"]
      max_attempts: 3
      backoff: "exponential"
      initial_delay: "20s"
  recover:
    type: "plan"
    role: "lead"
    on_success: "failed"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
    message: "Ingest failed"
---
Single-step ingestion workflow: the `ingestor` reads a user-provided document,
structures its facts into clean retrieval-friendly notes, and writes them to
`artifacts/out/ingestion.md`, which the executor auto-ingests into project
memory. No web research, no writer polish — this is "remember this document,"
not "research this topic."

Use `ingest` for document/notes-to-remember requests; use `research` (or
`research-and-publish`) when the user actually wants fresh web research.

## Prompts

### ingest

Read the document named in the task (file_read; text/Markdown reads directly —
binary attachments are already auto-extracted to memory on arrival). Structure
its facts faithfully into clean, grouped, retrieval-friendly notes and write
them to `artifacts/out/ingestion.md` (that exact path — it is auto-ingested
into project memory). Do NOT web-fetch, research, or add outside knowledge.
Follow the ingestor role's output contract: a `ingested` object (with `path`
on success) and a non-empty `message` naming what you stored.
