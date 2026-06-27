---
workflowId: "companion-rag-ingest"
displayName: "Companion: RAG ingest"
description: "Deterministic async ingestion of source documents into the project's RAG memory. The host LLM stages files as base64 INPUT artifacts (via /vornik-rag-ingest or /vornik-upload); the executor's handleSuccess hook deposits each staged input artifact DIRECTLY into RAG via the IngestText pipeline — no agent in the copy loop. Bypasses the 64 KiB-per-call cap of the synchronous remember() MCP tool."
version: "2.0.0"
entrypoint: "done"
require_input_artifacts: true
ingest_input_artifacts: true
maxWallClock: "5m"
terminals:
  done:
    status: "COMPLETED"
---

# Companion: RAG ingest

Async path for bulk-loading documents into the project's RAG memory.
The host LLM (Claude Code, etc.) delegates this workflow when it has a
larger ingest job than the synchronous `remember` MCP tool can handle
in-line — either a single payload over 64 KiB, or a batch of files
where the per-call latency adds up.

## Payload contract

The host LLM uploads source files via the companion `delegate` tool's
`inputArtifacts` parameter (or the `/vornik-upload` /
`/vornik-rag-ingest` slash commands which wrap it). The daemon
snapshots each file through the input-artifact store and folds the
resulting artifact IDs into `context.inputArtifactIDs`.

`require_input_artifacts: true` makes the daemon reject artifact-less
delegations up front — file paths in a delegate *prompt* are never
uploaded (the 2026-06-05 silent-skip incident).

## How async ingest lands chunks (deterministic — no agent)

`ingest_input_artifacts: true` tells the executor to deposit the staged
input artifacts itself. This workflow therefore has **no agent step**:
its entrypoint routes straight to the `done` terminal, and the
executor's `handleSuccess` hook (`internal/executor/workflow.go`,
`ingestInputArtifacts`) enqueues each artifact in
`context.inputArtifactIDs` onto the ingest queue by ID — the same
`IngestText` pipeline, repo-scope stamping, and gate stack
(secret_scan, dedup, near_dup_supersede, min_content, ttl_set,
claim_audit, …) that commit researcher / analyst outputs across vornik.

**Why no agent.** Earlier revisions had a weak `rag-ingester` LLM `cp`
each `artifacts/in/<file>` to `artifacts/out/` for the
`ingestOutputArtifacts` hook to pick up. The model routinely **claimed
`produced_files` it never wrote** — round-tripping large files through
its context and overflowing, or simply hallucinating success — so the
run failed its existence check and the operator had to retry or "try a
different tool" (the 2026-06 ingest incidents; the 260 KiB CHANGELOG
that failed 6×). Reading the staged artifacts by ID in Go removes the
model — and file size — from the critical path entirely.

The synchronous `remember` MCP tool remains the right surface for the
small, in-session "save this insight" case; this workflow is the
batched / large-payload path.
