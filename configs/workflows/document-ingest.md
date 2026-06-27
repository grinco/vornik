---
workflowId: "document-ingest"
displayName: "Document ingest (deterministic, no-LLM)"
description: "Deterministic ingest of operator-uploaded files into project RAG memory. Two no-LLM system steps: rag.extract dispatches each input artifact to its MIME-matched extractor (text / markdown / pdf / epub / html / audio / image); rag.index chunks the extracted sections into project_memory_chunks. Bypasses the rag-ingester agent role for files whose content the extractor pipeline can already handle — zero LLM tokens spent on the mechanical work."
version: "1.0.0"
entrypoint: "extract"
maxStepVisits: 1
maxIterations: 4
maxWallClock: "15m"
steps:
  extract:
    type: "system"
    handler: "rag.extract"
    on_success: "index"
    on_fail: "failed"
    timeout: "10m"
  index:
    type: "system"
    handler: "rag.index"
    on_success: "done"
    on_fail: "failed"
    timeout: "5m"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
    message: "document-ingest failed — see step outcomes for extract / index"
---

# Document ingest

Deterministic, no-LLM ingest path for operator-uploaded files. Pairs
with the existing `companion-rag-ingest` workflow:

| Workflow | LLM cost | Best for |
|----------|----------|----------|
| `document-ingest` | **zero LLM tokens** (system steps only) | files whose MIME has a registered extractor (text / markdown / pdf / epub / html / audio / image) |
| `companion-rag-ingest` | one agent step per ingest task | files needing custom transformation, large batches with custom tagging, formats the extractor pipeline doesn't yet support |

For markdown / plain text the two produce equivalent chunks; pick
`document-ingest` whenever you don't need the rag-ingester agent's
discretion. The B-7 close-out post-mortem (2026-05-28) is the
canonical "the agent burned 60 iterations globbing for paths"
incident.

## Payload contract

Same as `companion-rag-ingest`: the host LLM uploads sources via
the companion `delegate` tool's `inputArtifacts` parameter (or
the `/vornik-upload` / `/vornik-rag-ingest` slash commands). The
executor stages each artifact, then `rag.extract` reads
`context.inputArtifactIDs` and dispatches each artifact to the
MIME-matched extractor.

Set `skip_auto_extract: true` on the `delegate` call so the
upload pipeline doesn't double-ingest the file via
`tryAutoExtract` before this workflow runs.

## Step shapes

### rag.extract

Reads `task.payload.context.inputArtifactIDs[]`. For each artifact:

1. Load the `artifacts` row (project-scoped).
2. Resolve the MIME via `MimeType`; fall back to
   `MimeFromFilename` when the column is nil.
3. Look up the extractor via the registry (`extractor.Registry.For`).
4. Run extraction via `extractor.Runner.Run` — persists the
   `extracted_documents` row plus the on-disk
   `metadata.json` / `outline.json` / `sections/*.md` layout.
5. Emit one `{artifact_id, extracted_document_id, section_count}`
   entry per artifact in the step result.

Errors (no extractor, missing artifact, extractor crash) route to
`failed` via `on_fail`.

### rag.index

Reads the previous step's `extracted` list. For each entry:

1. Load the `extracted_documents` row by ID.
2. Parse `outline.json` (already on disk from rag.extract).
3. Read each section's markdown from
   `<storage>/sections/<section_id>.md`.
4. Call `Indexer.IngestExtractedSections` — the standard memory
   pipeline (gates, dedup, embedding queue) handles the rest.

Emits `{chunks: N, documents: M}` so the operator-facing task
detail can show a one-line summary.

## Why not summarize?

The original LLD's `extract → index → summarize` arc includes an
optional LLM summary step. v1 ships the deterministic core only;
operators wanting a summary can chain a follow-on `summarize`
task that calls the existing writer role. Keeps the
"zero-LLM-tokens for the mechanical work" promise pure.
