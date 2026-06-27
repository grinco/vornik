---
sources:
    - path: internal/featuredoctor/feature_memoryrag.go
      sha256: c80c9326a54ef730328e55738e35ab7bf79aa8b15bc6f5d39b03f5bac0cd2356
    - path: internal/memoryfirewall/evaluator.go
      sha256: e86f66f23ad5cd34f55b0bee562d59514951c709f0f9709b6684e34bfead55ed
    - path: internal/memory/gates.go
      sha256: 9f928ca01d37ff776f218bbf4275170a091ca380fb780293d8773ab23d52536a
---
# Memory & RAG

!!! note "Community Edition"

    Memory, RAG, and the **memory firewall** all ship in the open-source
    **Community Edition**. The firewall enforces policy on a Postgres-backed
    deployment (where its evaluation audit lives); a single-process SQLite
    deployment runs unfiltered. See [Editions](../editions.md).


vornik can consolidate and semantically recall project memory so agents
carry context forward across tasks, with caches that cut cost and latency.
A **policy-aware firewall** governs what may enter that memory and what may be
read back out.

## Enabling recall and caches

```bash
vornikctl doctor feature enable memory-rag
```

This enables the `memory.*` gates — LLM consolidation, response cache, and
embedding cache — and restarts the daemon during an idle window.

### Prerequisites

Semantic (vector) search requires a **PostgreSQL backend with the
[pgvector](https://github.com/pgvector/pgvector) extension** — the
`pgvector/pgvector` image ships it and vornik enables it on first boot. On a
SQLite backend there is no vector index, so recall degrades to keyword
(full-text / substring) search only.

The doctor checks that the embedding model is actually reachable. When
`memory.embedding_endpoint` is set, it probes that endpoint directly (the
endpoint embeddings really use — typically a local Ollama / TEI server),
not the chat-provider model catalog. If no embedding endpoint is
configured, embeddings fall back to the agent/chat endpoint and the chat
catalog is checked instead.

## The memory firewall

The firewall is a deterministic, in-process check on the recall path — no LLM,
no extra database round-trips. It runs independently of the `memory-rag` feature
above and is **on by default in advisory mode**, so you get an audit trail from
day one without changing behaviour.

### Per-chunk policy

Every memory chunk carries a policy with several dimensions, all of which must
be satisfied for a chunk to be served:

- **Sensitivity tier** — `public`, `internal` (default), `confidential`, or
  `restricted`. A `restricted` chunk is only served to a request that carries an
  operator identity, so anonymous, autonomy-driven recalls can't read it.
- **Expiry** — a chunk past its expiry is blocked.
- **Tenant** — a tenant-tagged chunk is only served to a matching tenant.
- **Permitted roles** — which roles may read the chunk.
- **Allowed purposes** — `operational` (default), `training_data`,
  `audit_review`, or `compliance_export`.

When a chunk is refused, the decision is one of a small set of reasons
(`block_expired`, `block_tenant_mismatch`, `block_role_not_permitted`,
`block_purpose_not_allowed`, `block_sensitivity_tier`) — recorded for audit and
usable as a filter when you review evaluations.

Chunks classified as credentials are forced to `restricted` even if pasted by an
operator, and chunks whose content was refuted by validation are made
unreadable — so a known-bad or sensitive chunk can't leak through a permissive
default.

### Enforcement modes

| Mode | Behaviour |
|------|-----------|
| `off` | evaluate and audit only; blocked chunks still surface |
| `advisory` *(default)* | blocked chunks still surface, each tagged with a policy warning |
| `enforce` | blocked chunks are dropped from results entirely |

The mode is resolved per request: a project-level `firewall.mode` override wins,
then the `VORNIK_MEMORY_FIREWALL_MODE` daemon environment variable, then the
`advisory` default. An unrecognised value safely coerces to `advisory`. Run in
`advisory` first to see what *would* be blocked, then move to `enforce` once the
policy is right.

```bash
vornikctl memory firewall mode                       # show the active mode
vornikctl memory firewall set-policy <chunk_id> \
    --sensitivity confidential --permitted-roles reviewer,lead
```

## Keeping secrets out of memory

Two ingest-side gates protect memory at write time:

- **Deny patterns** — `memory.deny_patterns` is a list of literal substrings
  (matched verbatim, so there's no regular-expression risk). A matching deposit
  is routed to quarantine for operator review rather than admitted. The list is
  hot-reloadable — editing it takes effect without a daemon restart.
- **Secret scanning** — content is scanned for credentials on the way in, and
  for memory the scan **cannot be downgraded to a no-op**: a detected secret is
  redacted (e.g. `[REDACTED:openai_key]`), and a deposit that's mostly secret is
  rejected outright rather than stored as a husk. The escape hatch
  `VORNIK_ALLOW_UNSCANNED_MEMORY` exists for deliberate, audited exceptions.

## Audit and compliance export

Every recall decision — in every mode, including `off` — writes an audit row
recording the chunk, the requesting role and purpose, the decision, and a digest
of the policy that was in force. Recalled chunks also carry a policy proof so a
workflow that cites a chunk can show it was allowed to.

Review and export evaluations:

```bash
vornikctl memory firewall evaluations --project research-desk --since 2026-06-01
vornikctl memory firewall evaluations --project research-desk --csv > compliance.csv
```

The CSV export streams RFC 4180 output over a 30-day window by default — suitable
for a compliance hand-off.

Two admin UI surfaces complement the CLI (don't confuse them):

- `/ui/admin/memory/firewall` — the firewall's policy evaluations, plus
  per-chunk policy detail and editing.
- `/ui/admin/memory-audit` — the broader retrieval and ingest audit log.

## Configuration reference

| Key / variable | Meaning |
|----------------|---------|
| `memory.deny_patterns` | substring deny-list; matches go to quarantine (hot-reloadable) |
| `firewall.mode` *(per project)* | `off` / `advisory` / `enforce`, overrides the daemon default |
| `VORNIK_MEMORY_FIREWALL_MODE` | daemon-level enforcement mode |
| `VORNIK_ALLOW_UNSCANNED_MEMORY` | audited escape hatch for the secret-scan clamp |
