package api

// Propose-time near-duplicate preflight (LLD
// 2026-07-07-knowledge-skill-store-design §12.2).
//
// Before a skill is written, it is scored against the existing catalogue and a
// hit soft-blocks the write. The author then either supersedes the neighbour or
// asserts distinctness with a justification — both auditable, neither silent.

import (
	"context"

	"vornik.io/vornik/internal/persistence"
)

// SkillEmbedder is the embedding surface the dedup preflight needs. Satisfied
// by *memory.Embedder; declared here so internal/api keeps no dependency on
// internal/memory (the same separation MemoryCompanionAdapter maintains).
//
// Embed returns nil, nil when the backend is unconfigured or a call fails —
// that is the contract, not an error condition, and the preflight treats it as
// "fall back to lexical".
// projectID is a plain string rather than a memory.EmbedScope so this
// interface keeps its deliberate independence from internal/memory (see above);
// the service-layer adapter turns it into a billing scope.
type SkillEmbedder interface {
	Embed(ctx context.Context, projectID string, texts []string) ([][]float32, error)
	Model() string
}

// skillPreflightCandidates loads the set a proposed skill is compared against.
//
// INVARIANT (§12.2): this is deliberately UNSCOPED. It returns every maturity
// (including retired, so a resurrection surfaces) across every repo_scope in
// the project, plus every other project's is_global rows — which is exactly
// what List(projectID, {IncludeGlobal: true}) with no RepoScope and no
// Maturities yields.
//
// Do NOT add a RepoScope filter here to "match skill_search". That asymmetry —
// search scoped, injection unscoped — is the bug this whole slice exists to
// close: it hides from the author precisely the skills that will be injected
// alongside theirs. See §12.1(1).
func (s *Server) skillPreflightCandidates(ctx context.Context, projectID string) ([]*persistence.Skill, error) {
	return s.skillStore.List(ctx, projectID, persistence.SkillListFilter{
		IncludeGlobal: true,
	})
}

// embedSkillForPreflight computes and attaches the candidate's embedding.
//
// Returns false when no usable embedding could be produced, which is a normal
// degraded state (embedder unwired, unconfigured, or the upstream call failed).
// Callers continue with the lexical fallback; they must not surface an error.
func (s *Server) embedSkillForPreflight(ctx context.Context, sk *persistence.Skill) bool {
	if s.skillEmbedder == nil {
		return false
	}
	model := s.skillEmbedder.Model()
	if model == "" {
		return false
	}
	vecs, err := s.skillEmbedder.Embed(ctx, sk.ProjectID, []string{skillEmbeddingText(sk)})
	if err != nil || len(vecs) == 0 || len(vecs[0]) == 0 {
		return false
	}
	sk.Embedding = vecs[0]
	sk.EmbeddingModel = model
	return true
}

// backfillSkillEmbeddings lazily embeds candidates that have no vector, or one
// from a different model, and persists the result.
//
// Lazy because a migration cannot call an embedder (§12.2): the 21 rows that
// predate this feature, and any written while the embedder was down, get their
// vector the first time they participate in a preflight.
//
// Best-effort throughout. A failure to embed or persist leaves the row as it
// was and the comparison falls back to lexical for that row only — one
// un-embeddable neighbour must not stop the author's propose from completing.
func (s *Server) backfillSkillEmbeddings(ctx context.Context, model string, candidates []*persistence.Skill) {
	if s.skillEmbedder == nil || model == "" {
		return
	}
	var (
		stale []*persistence.Skill
		texts []string
	)
	for _, c := range candidates {
		if c == nil {
			continue
		}
		if len(c.Embedding) > 0 && c.EmbeddingModel == model {
			continue
		}
		stale = append(stale, c)
		texts = append(texts, skillEmbeddingText(c))
	}
	if len(stale) == 0 {
		return
	}
	// Grouped by project because this candidate set is deliberately UNSCOPED
	// (see the invariant above), so one batch spans projects — and a provider
	// call can only be billed to one. Embedding all of them under a single
	// project would bill one tenant for another's skills
	// (2026-08-12-embed-spend-attribution-design.md §8.4).
	vecs, err := s.embedByProject(ctx, stale, texts)
	if err != nil || len(vecs) != len(stale) {
		return
	}
	for i, c := range stale {
		if len(vecs[i]) == 0 {
			continue
		}
		c.Embedding = vecs[i]
		c.EmbeddingModel = model
		// Persist so the next preflight doesn't re-embed. A write failure is
		// survivable: the in-memory vector still serves THIS comparison.
		if s.skillStore != nil {
			_ = s.skillStore.SetEmbedding(ctx, c.ID, c.Embedding, model)
		}
	}
}

// embedByProject embeds one provider call per project, scattering vectors back
// into a slice positionally aligned with skills.
//
// The skill-preflight candidate set is unscoped by design, so a batch spans
// projects and a single call could not be billed truthfully. Splitting by
// project keeps each usage row honest; a single-project batch still makes
// exactly one call.
//
// Returns (nil, err) on the first failing group — the caller treats a
// length mismatch or error as "skip the backfill", which is the pre-existing
// degrade behaviour.
func (s *Server) embedByProject(ctx context.Context, skills []*persistence.Skill, texts []string) ([][]float32, error) {
	// First-seen project order keeps the call sequence deterministic; a map
	// walk would vary it run to run.
	order := make([]string, 0, 4)
	byProject := make(map[string][]int, 4)
	for i, sk := range skills {
		if _, seen := byProject[sk.ProjectID]; !seen {
			order = append(order, sk.ProjectID)
		}
		byProject[sk.ProjectID] = append(byProject[sk.ProjectID], i)
	}

	out := make([][]float32, len(skills))
	for _, projectID := range order {
		idxs := byProject[projectID]
		group := make([]string, len(idxs))
		for j, idx := range idxs {
			group[j] = texts[idx]
		}
		vecs, err := s.skillEmbedder.Embed(ctx, projectID, group)
		if err != nil {
			return nil, err
		}
		for j, idx := range idxs {
			if j < len(vecs) {
				out[idx] = vecs[j]
			}
		}
	}
	return out, nil
}

// runSkillDupePreflight scores a proposed skill against the catalogue and
// returns the matches that should soft-block the write.
//
// Order matters: the candidate is embedded first so `model` is known, then
// stale candidates are backfilled against that same model, and only then is
// scoring run — otherwise every comparison would fall back to lexical on the
// first propose after deployment.
func (s *Server) runSkillDupePreflight(ctx context.Context, candidate *persistence.Skill) ([]skillMatch, error) {
	existing, err := s.skillPreflightCandidates(ctx, candidate.ProjectID)
	if err != nil {
		return nil, err
	}
	if s.embedSkillForPreflight(ctx, candidate) {
		s.backfillSkillEmbeddings(ctx, candidate.EmbeddingModel, existing)
	}
	return findSkillDuplicates(candidate, existing), nil
}
