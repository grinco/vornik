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
# Scores this workflow's DECLARED output obligation — the
# require_output_glob on the `ingest` step — as met/declared (2026-08-19).
# Needed here specifically because this workflow HAS a recover hop: task
# status would then measure "did recovery work", not "did we break ingest"
# (coverage design 7.3). contract_satisfaction is unmet-or-met regardless of
# which terminal the workflow reached. It measures DELIVERY, not correctness:
# notes that are valid but substantively empty still score 1.0. See
# https://docs.vornik.io
qualityScoring:
  kind: "contract_satisfaction"
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
    # Retry these classes. Attempts/backoff/delay are deliberately
    # OMITTED so they inherit the executor defaults: this block never
    # executed before 2026-08-27, so its former "max_attempts: 5" was
    # never in force (the real behaviour was infraRetryMaxAttempts=6).
    # Writing 5 here would be a silent retune disguised as a fix.
    retry:
      on: ["unclassified", "llm_call_failed", "container_start_failed", "container_wait_failed", "container_killed", "context_timeout"]
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
