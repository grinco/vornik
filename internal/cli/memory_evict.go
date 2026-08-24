package cli

import (
	"context"
	"fmt"
	"os/user"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/storage"
)

var (
	memoryEvictProject string
	memoryEvictChunks  string
	memoryEvictScope   string
	memoryEvictReason  string
	memoryEvictConfirm bool
)

var memoryEvictCmd = &cobra.Command{
	Use:   "evict",
	Short: "Permanently delete memory chunks (GDPR-style hard eviction)",
	Long: `Permanently delete the named project_memory_chunks rows. Cascades
through memory_embed_queue + memory_embed_dlq + entity_mentions (FK
ON DELETE CASCADE). A per-chunk tombstone row lands in
memory_eviction_audit so the deletion itself is auditable (the GDPR
compliance hook — deletion without record of the deletion is itself
non-compliant).

It also removes what the evicted chunks DERIVED, which it did not
until 2026-08-21: knowledge_entities and knowledge_edges have no
foreign key to chunks, so the entities and edges built from an
evicted chunk used to survive it and stay queryable — this command
lists GDPR requests as a use case, so that was a privacy request
answered with a partial deletion. An entity a SURVIVING chunk still
reaches is kept; only what loses its last evidence goes.

The quarantined pre-ingest copy is DELETED, not detached. The
foreign key on project_memory_quarantine.released_chunk_id is
ON DELETE SET NULL, so evicting a chunk used to null the pointer and
keep the text — and that table holds content an ingest gate
REJECTED, which is disproportionately likely to be the sensitive
kind. The FK rule is unchanged (quarantine rows outlive their chunk
by design); what changed is that a deletion meant to be permanent
removes them explicitly.

Eviction is DESTRUCTIVE and IRREVERSIBLE. For "this record is wrong,
demote it in search" use the soft-refute path (vornikctl via
memory_correct dispatcher tool) instead. Use evict only for:

  - GDPR / privacy-driven "forget this" requests.
  - Cleanup of confirmed-bad records that soft-refute leaves
    cluttering the index.
  - Cascading cleanup tied to a hard-deleted source artifact.

Requires --confirm so it can't fire from a typo. --reason is
recorded on the audit row; pass a short prose justification
(e.g. "GDPR DSAR 2026-05-20-12" or "operator: confirmed wrong
ticker"). Empty --reason still records the row but flags the
operator: get-it-right-on-the-first-call gap.

Two selectors (exactly one required):
  --chunks  explicit comma-separated chunk IDs
  --scope   every chunk under a repo_scope; --scope="" targets the
            UNTAGGED (NULL) bucket — e.g. memories ingested before
            scopes existed. Running without --confirm prints the
            match count and refuses (a built-in dry run).

Examples:
  # explicit IDs:
  vornikctl memory evict --project assistant \
      --chunks mc_abc123,mc_def456 \
      --reason "GDPR DSAR 2026-05-20-12" --confirm

  # preview, then evict the untagged (pre-scope) bucket:
  vornikctl memory evict -p acme --scope ""            # dry run (count only)
  vornikctl memory evict -p acme --scope "" \
      --reason "pre-scope cleanup" --confirm

  # evict everything under a specific (wrong/dead) scope:
  vornikctl memory evict -p acme --scope github.com/old/repo --confirm

Note: the project filter is the IDOR guard. Chunk IDs that exist
under a different project will be silently ignored — the command
reports the count actually deleted so an operator who pastes the
wrong IDs notices the discrepancy.`,
	RunE: runMemoryEvict,
}

func init() {
	memoryEvictCmd.Flags().StringVarP(&memoryEvictProject, "project", "p", "", "Project ID (required)")
	memoryEvictCmd.Flags().StringVar(&memoryEvictChunks, "chunks", "", "Comma-separated chunk IDs to evict (one of --chunks / --scope)")
	memoryEvictCmd.Flags().StringVar(&memoryEvictScope, "scope", "",
		`Evict EVERY chunk under this repo_scope (one of --chunks / --scope). Pass --scope="" for the untagged (NULL) bucket — e.g. pre-scope memories.`)
	memoryEvictCmd.Flags().StringVar(&memoryEvictReason, "reason", "", "Audit-trail reason recorded on the tombstone row")
	memoryEvictCmd.Flags().BoolVar(&memoryEvictConfirm, "confirm", false, "REQUIRED safety gate — refuses to delete without this")
	_ = memoryEvictCmd.MarkFlagRequired("project")
	memoryCmd.AddCommand(memoryEvictCmd)
}

func runMemoryEvict(cmd *cobra.Command, args []string) error {
	if strings.TrimSpace(memoryEvictProject) == "" {
		return fmt.Errorf("--project is required")
	}
	// Exactly one of --chunks / --scope. --scope is "changed" even when
	// set to "" (the untagged/NULL bucket), so Changed() — not the value
	// — is the mode signal.
	scopeMode := cmd.Flags().Changed("scope")
	if scopeMode == (strings.TrimSpace(memoryEvictChunks) != "") {
		return fmt.Errorf("specify exactly one of --chunks or --scope")
	}

	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = backend.Close() }()
	db, err := requirePostgresDB(backend, "memory evict")
	if err != nil {
		return err
	}

	repo := memory.NewRepository(db)

	// Resolve the chunk IDs to evict from whichever selector was given.
	var chunkIDs []string
	var target string
	if scopeMode {
		scopeIsNull := strings.TrimSpace(memoryEvictScope) == ""
		scopeLabel := memoryEvictScope
		if scopeIsNull {
			scopeLabel = "(untagged / NULL)"
		}
		target = fmt.Sprintf("scope %s", scopeLabel)
		chunkIDs, err = repo.ChunkIDsByScope(ctx, memoryEvictProject, memoryEvictScope, scopeIsNull)
		if err != nil {
			return fmt.Errorf("resolve scope: %w", err)
		}
		fmt.Printf("%d chunk(s) match %s in project %q\n", len(chunkIDs), target, memoryEvictProject)
	} else {
		chunkIDs = splitMemoryEvictChunks(memoryEvictChunks)
		target = fmt.Sprintf("%d explicit chunk id(s)", len(chunkIDs))
	}

	if len(chunkIDs) == 0 {
		fmt.Println("(nothing to evict)")
		return nil
	}
	// --confirm is the destructive-action gate. Checking it AFTER the
	// scope resolution means the refusal message can report the count
	// the operator is about to delete (a built-in dry run for --scope).
	if !memoryEvictConfirm {
		return fmt.Errorf("eviction is destructive and irreversible — re-run with --confirm to evict %d chunk(s) (%s)", len(chunkIDs), target)
	}

	// The Corrector wraps the repo. Searcher is nil because evict
	// doesn't search — it works on explicit chunk IDs.
	corrector := memory.NewCorrector(repo, nil)

	res, err := corrector.HardEvict(ctx, memoryEvictProject, chunkIDs, memoryEvictReason, currentOperatorIdentity())
	if err != nil {
		return fmt.Errorf("evict: %w", err)
	}
	audit := res.Audit

	fmt.Printf("evicted %d of %d requested chunks under project %q\n", len(audit), len(chunkIDs), memoryEvictProject)
	if len(audit) < len(chunkIDs) {
		fmt.Println("(non-deleted IDs were stale, wrong-project, or already evicted)")
	}
	// Printed unconditionally, zeros included. These rows outlived evictions
	// reported as complete until 2026-08-21; the eviction tombstones record the
	// chunks, and this line is the only place the derived rows are accounted for.
	fmt.Printf("derived data: %d knowledge entit(ies), %d graph edge(s), %d quarantined "+
		"pre-ingest cop(ies), %d cached embedding(s)\n",
		res.Derived.Entities, res.Derived.Edges, res.QuarantinedCopiesDeleted,
		res.EmbeddingCacheKeysDeleted)
	for _, row := range audit {
		hashShort := row.ContentHash
		if len(hashShort) > 12 {
			hashShort = hashShort[:12]
		}
		fmt.Printf("  - %s (class=%s, role=%s, hash=%s)\n",
			row.ChunkID, row.ContentClass, row.ProducerRole, hashShort)
	}
	return nil
}

// splitMemoryEvictChunks tokenises the comma-separated --chunks
// flag and drops empty fragments. Exposed for testing.
func splitMemoryEvictChunks(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// currentOperatorIdentity returns the OS username for the
// evicted_by audit field. Falls back to "unknown" if /etc/passwd is
// unreadable (shouldn't happen in normal deployments). Kept thin so
// the eviction call site doesn't grow a deps tree just to stamp an
// audit row.
func currentOperatorIdentity() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}
