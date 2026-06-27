package memory

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/secrets"
)

// Indexer ingests text content into project memory by chunking, persisting,
// and queuing chunks for async embedding.
type Indexer struct {
	cfg      Config
	repo     *Repository
	embedder *Embedder
	logger   zerolog.Logger
	metrics  *Metrics

	// secretsDetector (optional) scans content before chunking so
	// embedded/searchable chunks never carry credentials. Wired in
	// from the service container; nil means no scanning. The action
	// map controls per-checkpoint policy (Redact by default for
	// CheckpointMemory).
	secretsDetector secrets.Detector
	secretsActions  map[string]secrets.Action

	// autoStampPolicies controls whether IngestText runs a
	// post-insert UPDATE stamping default firewall policy on
	// newly-inserted chunks (those with policy_digest IS NULL).
	// Off by default to avoid double-stamp on the Pipeline path
	// (which has its own PatchPolicyByArtifact step + the
	// existing pipeline tests' UPDATE-regex mocks would
	// collide with the new stamp UPDATE). Direct callers
	// (operator correction, companion ingest) opt-in via
	// SetAutoStampPolicies(true). Lazy-backfill at retrieval
	// still produces correct firewall decisions either way.
	autoStampPolicies bool
}

// SetAutoStampPolicies enables/disables the IngestText post-
// insert firewall-policy stamp pass. Off by default. Callers
// that want their freshly-ingested chunks to land with explicit
// policy_digest + sensitivity + permitted_roles set this true
// at construction. Pipeline ingestion leaves it false so the
// existing test fixtures don't see an unexpected extra UPDATE.
func (idx *Indexer) SetAutoStampPolicies(on bool) {
	if idx == nil {
		return
	}
	idx.autoStampPolicies = on
}

// NewIndexer creates an Indexer.
func NewIndexer(cfg Config, repo *Repository, embedder *Embedder, logger zerolog.Logger) *Indexer {
	return &Indexer{
		cfg:      cfg,
		repo:     repo,
		embedder: embedder,
		logger:   logger,
	}
}

// setMetrics attaches a Metrics instance to the Indexer.
func (idx *Indexer) setMetrics(m *Metrics) { idx.metrics = m }

// SetSecrets wires the secret-leak detector and per-checkpoint action
// map. Called by the service container after the detector is built.
// A nil detector disables scanning (the existing default).
func (idx *Indexer) SetSecrets(d secrets.Detector, actions map[string]secrets.Action) {
	idx.secretsDetector = d
	idx.secretsActions = actions
}

// scanContentForSecrets applies the CheckpointMemory action to
// `content`. Default action is Redact (see DefaultCheckpoints).
// Block degrades to Redact at this checkpoint — refusing to ingest
// a tainted artifact loses signal the operator might still want to
// search; Phase 2's SECRET_LEAK failure class lands at the writer
// (result.json / artifact upload) not the indexer.
//
// Extracted into its own method so it can be tested without needing
// a real chunk repository.
func (idx *Indexer) scanContentForSecrets(projectID, taskID, sourceName, content string) string {
	if idx.secretsDetector == nil || content == "" {
		return content
	}
	findings := idx.secretsDetector.Scan([]byte(content))
	if len(findings) == 0 {
		return content
	}
	action := secrets.ResolveAction(secrets.CheckpointMemory, idx.secretsActions)
	counts := secrets.CountByType(findings)
	logEvent := idx.logger.Warn().
		Str("project_id", projectID).
		Str("task_id", taskID).
		Str("source_name", sourceName).
		Str("checkpoint", secrets.CheckpointMemory).
		Str("action", string(action)).
		Int("findings", len(findings)).
		Interface("by_type", counts)
	switch action {
	case secrets.ActionRedact:
		logEvent.Msg("memory: ingest scanned — redacting findings before chunk persist")
		return string(secrets.Redact([]byte(content), findings))
	case secrets.ActionBlock:
		logEvent.Msg("memory: ingest — BLOCK ACTION NOT YET ENFORCED, degraded to redact")
		return string(secrets.Redact([]byte(content), findings))
	default: // ActionDetect
		logEvent.Msg("memory: ingest scanned — detect-only, content left intact")
		return content
	}
}

// IngestText chunks and stores text content. Embedding is queued asynchronously.
// Returns an error only if chunk insertion into the DB fails — embedding failures
// are always async and do not propagate here.
func (idx *Indexer) IngestText(ctx context.Context, projectID, taskID, artifactID, sourceName, content string) error {
	if content == "" {
		return nil
	}

	// Secret-leak scan before chunking. Memory chunks live forever
	// and surface via similarity search — letting a credential into
	// a chunk means letting it stay there for the project's lifetime.
	content = idx.scanContentForSecrets(projectID, taskID, sourceName, content)

	chunkTokens := idx.cfg.ChunkTokens
	if chunkTokens <= 0 {
		chunkTokens = 512
	}
	chunkOverlap := idx.cfg.ChunkOverlap
	if chunkOverlap < 0 {
		chunkOverlap = 0
	}

	rawChunks := chunkText(content, chunkTokens, chunkOverlap)
	if len(rawChunks) == 0 {
		return nil
	}

	chunks := make([]MemoryChunk, 0, len(rawChunks))
	for _, rc := range rawChunks {
		id := chunkID(projectID, artifactID, sourceName, rc.Index)
		chunks = append(chunks, MemoryChunk{
			ID:          id,
			ProjectID:   projectID,
			TaskID:      taskID,
			ArtifactID:  artifactID,
			SourceName:  sourceName,
			ChunkIndex:  rc.Index,
			Content:     rc.Text,
			ContentHash: rc.Hash,
			CreatedAt:   time.Now(),
		})
	}

	start := time.Now()
	if err := idx.repo.UpsertChunks(ctx, chunks); err != nil {
		if idx.metrics != nil {
			idx.metrics.IngestErrorsTotal.WithLabelValues(projectID).Inc()
		}
		return fmt.Errorf("ingest text chunks: %w", err)
	}
	if idx.metrics != nil {
		idx.metrics.ChunksIngestedTotal.WithLabelValues(projectID).Add(float64(len(chunks)))
		idx.metrics.IngestDuration.WithLabelValues(projectID).Observe(time.Since(start).Seconds())
	}

	// Collect IDs of newly inserted chunks (those that were not duplicates).
	ids := make([]string, len(chunks))
	for i, c := range chunks {
		ids[i] = c.ID
	}
	if err := idx.repo.EnqueueForEmbedding(ctx, ids); err != nil {
		// Non-fatal — chunks are already stored; embedding will be retried.
		idx.logger.Warn().Err(err).
			Str("project_id", projectID).
			Str("source_name", sourceName).
			Msg("memory: failed to enqueue chunks for embedding")
	}

	// Stamp default policy on newly-inserted chunks (Policy-
	// Aware Memory Firewall, 2026.5.9 follow-on). Off by
	// default to keep Pipeline-path tests stable; opt-in via
	// SetAutoStampPolicies(true). The WHERE policy_digest IS
	// NULL clause in StampDefaultPoliciesForNewChunks means
	// operator-edited chunks (those that already have a digest
	// from POST /policy/chunks/{id}) keep their settings
	// across re-ingest.
	if idx.autoStampPolicies {
		idx.stampDefaultPolicies(ctx, projectID, sourceName, ids)
	}

	idx.logger.Debug().
		Str("project_id", projectID).
		Str("source_name", sourceName).
		Int("chunks", len(chunks)).
		Msg("memory: ingested text")

	return nil
}

// stampDefaultPolicies derives the Provenance source from the
// chunk's sourceName + applies DefaultPolicyForSource, then
// stamps the resulting columns onto every chunk in the batch
// that doesn't already have an explicit policy. Best-effort:
// stamp failures log + don't propagate (the chunks are already
// stored; lazy-backfill via the evaluator's
// DefaultPolicyForSource still gives correct behaviour at
// retrieval time).
func (idx *Indexer) stampDefaultPolicies(ctx context.Context, projectID, sourceName string, chunkIDs []string) {
	if len(chunkIDs) == 0 {
		return
	}
	source := provenanceFromSourceName(sourceName)
	policy := memoryfirewall.DefaultPolicyForSource(source, "")
	digest := memoryfirewall.PolicyDigest(policy)

	purposesAsStrings := make([]string, 0, len(policy.AllowedPurposes))
	for _, p := range policy.AllowedPurposes {
		purposesAsStrings = append(purposesAsStrings, string(p))
	}
	_, err := idx.repo.StampDefaultPoliciesForNewChunks(
		ctx,
		chunkIDs,
		policy.TenantID,
		string(policy.Sensitivity),
		string(policy.Provenance.Source),
		policy.Provenance.ProducerID,
		policy.Provenance.TrustLevel,
		policy.Provenance.SourceURL,
		policy.ExpiresAt,
		policy.PermittedRoles,
		purposesAsStrings,
		digest,
	)
	if err != nil {
		idx.logger.Warn().Err(err).
			Str("project_id", projectID).
			Str("source_name", sourceName).
			Msg("memory: stamp default policies failed (chunks remain on lazy-backfill path)")
	}
}

// provenanceFromSourceName maps the Indexer's sourceName
// convention into the firewall's ProvenanceSource enum. The
// mapping is best-effort heuristic — sourceName values come
// from many places in the codebase and don't follow a strict
// schema. ProvenanceUnknown is the safe fallthrough.
func provenanceFromSourceName(s string) memoryfirewall.ProvenanceSource {
	switch {
	case strings.HasPrefix(s, "operator_correction_"):
		return memoryfirewall.ProvenanceOperatorCorrection
	case strings.HasPrefix(s, "chat_turn_") || s == "chat_turn":
		return memoryfirewall.ProvenanceChatTurn
	case strings.HasPrefix(s, "self_consolidate") || s == "consolidate":
		return memoryfirewall.ProvenanceSelfConsolidated
	case strings.HasPrefix(s, "external_fetch_") || strings.HasPrefix(s, "web_"):
		return memoryfirewall.ProvenanceExternalFetch
	case strings.HasPrefix(s, "companion_") || strings.HasPrefix(s, "remember_"):
		return memoryfirewall.ProvenanceCompanionRemember
	case strings.HasPrefix(s, "extracted_") || strings.HasPrefix(s, "ingest_"):
		return memoryfirewall.ProvenanceIngestedArtifact
	case strings.HasPrefix(s, "task_output_") || strings.HasPrefix(s, "workflow_"):
		return memoryfirewall.ProvenanceWorkflowOutput
	default:
		// Most task-driven ingests use the workflow-output
		// shape even without the prefix. Default to that
		// rather than Unknown so existing chunks land on
		// sensible defaults out of the box.
		return memoryfirewall.ProvenanceWorkflowOutput
	}
}

// IngestFile reads a file from disk and calls IngestText.
func (idx *Indexer) IngestFile(ctx context.Context, projectID, taskID, artifactID, sourceName, filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read file %s: %w", filePath, err)
	}
	return idx.IngestText(ctx, projectID, taskID, artifactID, sourceName, string(data))
}

// ExtractedSection is one document section handed to
// IngestExtractedSections. SourceName is the operator-visible label
// surfaced in memory_search results — typically a short string like
// "Schema Coaching · Chapter 4" so a citation reads naturally.
type ExtractedSection struct {
	SectionID  string
	SourceName string
	Content    string
}

// IngestExtractedSections chunks each extracted section into project
// memory and stamps the provenance pointers (extracted_document_id,
// section_id) on every resulting chunk. Returns the total chunk
// count across all sections, ignoring per-section duplicate skips
// (UPSERT semantics dedupe on content_hash). Errors from any single
// section abort the batch — best-effort retry is the caller's job.
//
// Why a separate entry from IngestText: legacy IngestText is a hot
// path with no spare optional args; rather than introduce a variadic-
// param surface, isolating extracted-document ingest in its own
// method keeps both paths' contracts clear.
func (idx *Indexer) IngestExtractedSections(ctx context.Context, projectID, taskID, sourceArtifactID, extractedDocumentID string, sections []ExtractedSection) (int, error) {
	if idx == nil || idx.repo == nil {
		return 0, nil
	}
	if extractedDocumentID == "" {
		return 0, fmt.Errorf("extracted document id is required")
	}
	chunkTokens := idx.cfg.ChunkTokens
	if chunkTokens <= 0 {
		chunkTokens = 512
	}
	chunkOverlap := idx.cfg.ChunkOverlap
	if chunkOverlap < 0 {
		chunkOverlap = 0
	}

	totalChunks := 0
	for _, sec := range sections {
		if sec.Content == "" || sec.SectionID == "" {
			continue
		}
		content := idx.scanContentForSecrets(projectID, taskID, sec.SourceName, sec.Content)
		rawChunks := chunkText(content, chunkTokens, chunkOverlap)
		if len(rawChunks) == 0 {
			continue
		}
		chunks := make([]MemoryChunk, 0, len(rawChunks))
		for _, rc := range rawChunks {
			id := chunkID(projectID, sourceArtifactID, sec.SectionID, rc.Index)
			chunks = append(chunks, MemoryChunk{
				ID:                             id,
				ProjectID:                      projectID,
				TaskID:                         taskID,
				ArtifactID:                     sourceArtifactID,
				SourceName:                     sec.SourceName,
				ChunkIndex:                     rc.Index,
				Content:                        rc.Text,
				ContentHash:                    rc.Hash,
				CreatedAt:                      time.Now(),
				DerivedFromExtractedDocumentID: extractedDocumentID,
				DerivedFromSectionID:           sec.SectionID,
			})
		}
		if err := idx.repo.UpsertChunks(ctx, chunks); err != nil {
			return totalChunks, fmt.Errorf("upsert chunks for section %q: %w", sec.SectionID, err)
		}
		ids := make([]string, len(chunks))
		for i, c := range chunks {
			ids[i] = c.ID
		}
		if err := idx.repo.EnqueueForEmbedding(ctx, ids); err != nil {
			// Non-fatal — chunks are stored; embedding retried later.
			idx.logger.Warn().Err(err).
				Str("project_id", projectID).
				Str("section_id", sec.SectionID).
				Msg("memory: failed to enqueue extracted-section chunks for embedding")
		}
		totalChunks += len(chunks)
	}

	idx.logger.Info().
		Str("project_id", projectID).
		Str("extracted_document_id", extractedDocumentID).
		Int("sections", len(sections)).
		Int("chunks", totalChunks).
		Msg("memory: ingested extracted document sections")
	return totalChunks, nil
}

// MarkVerifiedByArtifact flips chunks from unverified → verified
// for role_of_record producers (Phase 4). See repository.go for
// the rule details.
func (idx *Indexer) MarkVerifiedByArtifact(ctx context.Context, projectID, artifactID, validatorRole string) error {
	if idx == nil || idx.repo == nil {
		return nil
	}
	return idx.repo.MarkVerifiedByArtifact(ctx, projectID, artifactID, validatorRole)
}

// SupersedeBySameSource marks older chunks superseded when a fresh
// chunk lands for the same (project_id, content_class, source_name,
// task_id). Supersession is task-scoped so independent tasks writing
// the same filename don't clobber each other's chunks. epochID is the
// causing epoch, recorded as restore provenance (migration 89) so a
// rollback of that epoch un-supersedes the prior version.
func (idx *Indexer) SupersedeBySameSource(ctx context.Context, projectID, contentClass, sourceName, taskID, newArtifactID, epochID string) (int, error) {
	if idx == nil || idx.repo == nil {
		return 0, nil
	}
	return idx.repo.SupersedeBySameSource(ctx, projectID, contentClass, sourceName, taskID, newArtifactID, epochID)
}

// PatchPolicyByArtifact updates per-policy columns for every chunk
// the indexer just wrote for one artifact. See repository.go for
// the SQL details + the legacy→unverified flip rule. Pipeline
// calls this after a successful IngestText so the chunks emerge
// with the policy metadata the gates derived (class, ttl, etc).
func (idx *Indexer) PatchPolicyByArtifact(ctx context.Context, projectID, artifactID, contentClass string, confidence float32, producerRole, ingestExecutionID string, expiresAt *time.Time, repoScope string) error {
	if idx == nil || idx.repo == nil {
		return nil
	}
	return idx.repo.PatchPolicyByArtifact(ctx, projectID, artifactID, contentClass, confidence, producerRole, ingestExecutionID, expiresAt, repoScope)
}

// PatchScopeByArtifact stamps repo_scope (migration 75) on every
// chunk produced for a given artifact. Used by the executor's
// ingestOutputArtifacts hook: after IngestText lands the chunks with
// repo_scope=NULL, the executor reads the scope out of the task's
// payload and calls this to retag them. Idempotent; pass empty
// scope for a no-op. Nil-safe at every layer.
func (idx *Indexer) PatchScopeByArtifact(ctx context.Context, projectID, artifactID, repoScope string) error {
	if idx == nil || idx.repo == nil {
		return nil
	}
	if repoScope == "" {
		return nil
	}
	return idx.repo.PatchScopeByArtifact(ctx, projectID, artifactID, repoScope)
}

// chunkID derives a stable, unique ID for a chunk.
func chunkID(projectID, artifactID, sourceName string, chunkIndex int) string {
	// Escape the delimiter (and the escape char itself) within each segment
	// so a colon embedded in a field can't shift the boundary and alias two
	// distinct tuples — e.g. ("a","b:c",…) vs ("a:b","c",…), which both
	// collapsed to "a:b:c:…" before. Colon/backslash-free inputs — the normal
	// case (ULID artifact ids + system-controlled source names) — escape to
	// themselves, so existing chunk ids are byte-identical: no re-ingest churn.
	esc := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		return strings.ReplaceAll(s, ":", `\:`)
	}
	raw := fmt.Sprintf("%s:%s:%s:%d", esc(projectID), esc(artifactID), esc(sourceName), chunkIndex)
	h := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", h[:16]) // 32-char hex
}
