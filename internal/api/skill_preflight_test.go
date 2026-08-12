package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// stubSkillEmbedder returns a fixed vector per text, so preflight behaviour can
// be exercised without a live embedding backend. A text absent from vectors
// embeds to nil, which models the partial-failure case.
type stubSkillEmbedder struct {
	model   string
	vectors map[string][]float32
	calls   int
	fail    bool
}

func (e *stubSkillEmbedder) Model() string { return e.model }

func (e *stubSkillEmbedder) Embed(_ context.Context, _ string, texts []string) ([][]float32, error) {
	e.calls++
	if e.fail {
		// Mirrors the real contract: nil, nil on failure, never an error.
		return nil, nil
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.vectors[t]
	}
	return out, nil
}

func preflightBlocked(t *testing.T, out string) (bool, []skillMatch) {
	t.Helper()
	var r struct {
		Blocked bool         `json:"blocked"`
		Matches []skillMatch `json:"matches"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("unmarshal propose result: %v", err)
	}
	return r.Blocked, r.Matches
}

func mustPropose(t *testing.T, s *Server, key *persistence.APIKey, args map[string]any) string {
	t.Helper()
	out, err := s.companionToolSkillPropose(context.Background(), key, rawArgs(t, args))
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	return out
}

// TestPreflightIgnoresRepoScope is the regression test for LLD §12.1(1) — the
// root cause of the whole catalogue drift.
//
// skill_search filters by repo_scope; the executor's injection does not. An
// author in scope A therefore cannot SEE a skill pinned to scope B, while that
// skill is injected alongside theirs anyway. The preflight must query the
// unscoped candidate set.
//
// If someone "tidies up" the preflight to reuse skill_search's scoping, this
// test is what fails.
func TestPreflightIgnoresRepoScope(t *testing.T) {
	s := newSkillTestServer(t)
	key := skillKey("proj-a", true, true)
	ctx := context.Background()

	// An existing skill pinned to a DIFFERENT repo scope.
	existing := &persistence.Skill{
		ID: "sk-other-scope", ProjectID: "proj-a", RepoScope: "github.com/acme/other",
		Name: "review-checklist-alpha", Description: "WHEN reviewing infrastructure changes before merge",
		Body: "# Alpha\n## Anti-patterns", Maturity: persistence.SkillMaturityActive, Version: 1,
	}
	if err := s.skillStore.Create(ctx, existing); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out := mustPropose(t, s, key, map[string]any{
		"name":        "review-checklist-beta",
		"description": "WHEN reviewing infrastructure changes before merge",
		"body":        "# Beta\n## Anti-patterns",
		"repo_scope":  "github.com/acme/mine",
	})
	blocked, matches := preflightBlocked(t, out)
	if !blocked {
		t.Fatal("preflight did not block on a candidate in another repo_scope — this is exactly the blind spot that produced the catalogue drift")
	}
	if len(matches) == 0 || matches[0].ID != existing.ID {
		t.Errorf("matches = %+v, want the cross-scope skill %s", matches, existing.ID)
	}
}

// TestPreflightSeesOtherProjectsGlobals: a global skill authored elsewhere is
// injected into this project's roles, so duplicating it is just as real a
// mistake as duplicating a local one (§12.2 invariant).
func TestPreflightSeesOtherProjectsGlobals(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()

	global := &persistence.Skill{
		ID: "sk-global-elsewhere", ProjectID: "proj-elsewhere", RepoScope: "*",
		Name: "review-checklist-alpha", Description: "WHEN reviewing infrastructure changes before merge",
		Body: "# Alpha\n## Anti-patterns", Maturity: persistence.SkillMaturityTrusted,
		Version: 1, IsGlobal: true,
	}
	if err := s.skillStore.Create(ctx, global); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out := mustPropose(t, s, skillKey("proj-a", true, true), map[string]any{
		"name":        "review-checklist-beta",
		"description": "WHEN reviewing infrastructure changes before merge",
		"body":        "# Beta\n## Anti-patterns",
	})
	blocked, matches := preflightBlocked(t, out)
	if !blocked {
		t.Fatal("preflight did not block on another project's GLOBAL skill, which injects here too")
	}
	if matches[0].ID != global.ID {
		t.Errorf("matched %s, want %s", matches[0].ID, global.ID)
	}
}

// TestPreflightSurfacesRetiredResurrection: retired rows stay in the candidate
// set so re-proposing something previously rejected is visible, not silent.
func TestPreflightSurfacesRetiredResurrection(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	retired := &persistence.Skill{
		ID: "sk-retired", ProjectID: "proj-a", Name: "review-checklist-alpha",
		Description: "WHEN reviewing infrastructure changes before merge",
		Body:        "# Alpha\n## Anti-patterns", Maturity: persistence.SkillMaturityRetired, Version: 1,
	}
	if err := s.skillStore.Create(ctx, retired); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out := mustPropose(t, s, skillKey("proj-a", true, true), map[string]any{
		"name":        "review-checklist-beta",
		"description": "WHEN reviewing infrastructure changes before merge",
		"body":        "# Beta\n## Anti-patterns",
	})
	if blocked, _ := preflightBlocked(t, out); !blocked {
		t.Error("a retired near-duplicate did not surface — resurrecting a rejected skill should be a visible decision")
	}
}

// TestConfirmDistinctRequiresJustification: an empty justification must not let
// the author past. Without this the disposition is a reflex bypass (§12.2).
func TestConfirmDistinctRequiresJustification(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	seedNearDuplicate(t, s)

	// Whitespace-only is not a justification.
	out, err := s.companionToolSkillPropose(ctx, skillKey("proj-a", true, true), rawArgs(t, map[string]any{
		"name":             "review-checklist-beta",
		"description":      "WHEN reviewing infrastructure changes before merge",
		"body":             "# Beta\n## Anti-patterns",
		"confirm_distinct": "   ",
	}))
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if blocked, _ := preflightBlocked(t, out); !blocked {
		t.Error("whitespace-only confirm_distinct was accepted as a justification")
	}
}

// TestConfirmDistinctStoresJustification: the bypass is recorded, so skill_audit
// can answer "why do these two both exist".
func TestConfirmDistinctStoresJustification(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	seedNearDuplicate(t, s)

	const why = "narrower trigger: only fires on infra diffs, not all reviews"
	out := mustPropose(t, s, skillKey("proj-a", true, true), map[string]any{
		"name":             "review-checklist-beta",
		"description":      "WHEN reviewing infrastructure changes before merge",
		"body":             "# Beta\n## Anti-patterns",
		"confirm_distinct": why,
	})
	if blocked, _ := preflightBlocked(t, out); blocked {
		t.Fatal("confirm_distinct with a justification was still blocked")
	}
	stored, err := s.skillStore.GetByID(ctx, proposeID(t, out))
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.DistinctJustification != why {
		t.Errorf("justification = %q, want %q", stored.DistinctJustification, why)
	}
}

// TestSupersedesPreservesPriorBody is the retention guard from the round-2
// review: superseding must write a NEW row and retire the target, never
// overwrite it. §6 binds approval to a body hash, so an approved body has to
// stay recoverable.
func TestSupersedesPreservesPriorBody(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	target := seedNearDuplicate(t, s)
	const originalBody = "# Alpha\n## Anti-patterns"

	out := mustPropose(t, s, skillKey("proj-a", true, true), map[string]any{
		"name":        "review-checklist-beta",
		"description": "WHEN reviewing infrastructure changes before merge",
		"body":        "# Beta\n## Anti-patterns",
		"supersedes":  target.ID,
	})
	newID := proposeID(t, out)
	if newID == target.ID {
		t.Fatal("supersedes overwrote the target in place; it must create a new row")
	}

	prior, err := s.skillStore.GetByID(ctx, target.ID)
	if err != nil {
		t.Fatalf("superseded skill is unreadable: %v", err)
	}
	if prior.Body != originalBody {
		t.Errorf("superseded body = %q, want it preserved as %q", prior.Body, originalBody)
	}
	if prior.Maturity != persistence.SkillMaturityRetired {
		t.Errorf("superseded maturity = %q, want retired", prior.Maturity)
	}
	fresh, err := s.skillStore.GetByID(ctx, newID)
	if err != nil {
		t.Fatalf("GetByID(new): %v", err)
	}
	if fresh.SupersedesID != target.ID {
		t.Errorf("supersedes_id = %q, want %q", fresh.SupersedesID, target.ID)
	}
}

// TestSupersedesRejectsCrossProject: superseding another project's skill would
// let a project-scoped key retire a row it cannot otherwise touch.
func TestSupersedesRejectsCrossProject(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	foreign := &persistence.Skill{
		ID: "sk-foreign", ProjectID: "proj-other", Name: "foreign",
		Description: "d", Body: "# f", Maturity: persistence.SkillMaturityActive, Version: 1,
	}
	if err := s.skillStore.Create(ctx, foreign); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := s.companionToolSkillPropose(ctx, skillKey("proj-a", true, true), rawArgs(t, map[string]any{
		"name": "mine", "description": "d", "body": "# m", "supersedes": foreign.ID,
	}))
	if err == nil || !strings.Contains(err.Error(), "another project") {
		t.Errorf("err = %v, want a cross-project refusal", err)
	}
}

// TestProposeRejectsBothDispositions: they are opposite answers to the same
// question, so accepting both would leave the intent ambiguous.
func TestProposeRejectsBothDispositions(t *testing.T) {
	s := newSkillTestServer(t)
	_, err := s.companionToolSkillPropose(context.Background(), skillKey("proj-a", true, true),
		rawArgs(t, map[string]any{
			"name": "x", "description": "d", "body": "# b",
			"supersedes": "sk-1", "confirm_distinct": "because",
		}))
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("err = %v, want a both-dispositions refusal", err)
	}
}

// TestPreflightUsesEmbeddingsWhenAvailable: with a working embedder, a pair
// that shares no vocabulary must still be caught. This is the case the lexical
// metric provably cannot see (LLD §12.2 calibration table).
func TestPreflightUsesEmbeddingsWhenAvailable(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()

	existing := &persistence.Skill{
		ID: "sk-embedded", ProjectID: "proj-a", Name: "alpha-trigger",
		Description: "completely unrelated wording", Body: "# Alpha",
		Maturity: persistence.SkillMaturityActive, Version: 1,
		Embedding: []float32{1, 0, 0}, EmbeddingModel: "stub-v1",
	}
	if err := s.skillStore.Create(ctx, existing); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s.skillEmbedder = &stubSkillEmbedder{
		model: "stub-v1",
		vectors: map[string][]float32{
			"beta-trigger\nnothing lexically in common": {0.99, 0.01, 0},
		},
	}

	out := mustPropose(t, s, skillKey("proj-a", true, true), map[string]any{
		"name": "beta-trigger", "description": "nothing lexically in common", "body": "# Beta",
	})
	blocked, matches := preflightBlocked(t, out)
	if !blocked {
		t.Fatal("semantically-identical skills were not flagged despite a working embedder")
	}
	if matches[0].Reason != reasonSimilarEmbedding {
		t.Errorf("reason = %q, want %q", matches[0].Reason, reasonSimilarEmbedding)
	}
}

// TestPreflightDegradesWhenEmbedderFails: an embedder outage must not block
// authoring. The propose completes; the guard falls back to lexical.
func TestPreflightDegradesWhenEmbedderFails(t *testing.T) {
	s := newSkillTestServer(t)
	s.skillEmbedder = &stubSkillEmbedder{model: "stub-v1", fail: true}

	out, err := s.companionToolSkillPropose(context.Background(), skillKey("proj-a", true, true),
		rawArgs(t, map[string]any{
			"name": "solo-skill", "description": "nothing else resembles this", "body": "# Solo",
		}))
	if err != nil {
		t.Fatalf("an embedder outage must never fail a propose, got: %v", err)
	}
	if blocked, _ := preflightBlocked(t, out); blocked {
		t.Error("propose was blocked with no real duplicate present")
	}
}

// TestBackfillPersistsEmbeddings: a row embedded during preflight is written
// back so the next propose doesn't re-embed the whole catalogue.
func TestBackfillPersistsEmbeddings(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()

	bare := &persistence.Skill{
		ID: "sk-bare", ProjectID: "proj-a", Name: "bare-skill",
		Description: "unembedded neighbour", Body: "# Bare",
		Maturity: persistence.SkillMaturityActive, Version: 1,
	}
	if err := s.skillStore.Create(ctx, bare); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s.skillEmbedder = &stubSkillEmbedder{
		model: "stub-v1",
		vectors: map[string][]float32{
			"bare-skill\nunembedded neighbour": {0, 1, 0},
			"fresh-skill\nsomething new":       {1, 0, 0},
		},
	}

	mustPropose(t, s, skillKey("proj-a", true, true), map[string]any{
		"name": "fresh-skill", "description": "something new", "body": "# Fresh",
	})

	got, err := s.skillStore.GetByID(ctx, bare.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.Embedding) == 0 || got.EmbeddingModel != "stub-v1" {
		t.Errorf("neighbour was not backfilled: embedding=%v model=%q", got.Embedding, got.EmbeddingModel)
	}
}

// TestBackfillDoesNotDisturbMaturity: re-embedding is derived-data maintenance.
// It must never send an approved skill back to draft.
func TestBackfillDoesNotDisturbMaturity(t *testing.T) {
	s := newSkillTestServer(t)
	ctx := context.Background()
	trusted := &persistence.Skill{
		ID: "sk-trusted", ProjectID: "proj-a", Name: "trusted-skill",
		Description: "already approved", Body: "# T",
		Maturity: persistence.SkillMaturityTrusted, Version: 3,
	}
	if err := s.skillStore.Create(ctx, trusted); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s.skillEmbedder = &stubSkillEmbedder{
		model: "stub-v1",
		vectors: map[string][]float32{
			"trusted-skill\nalready approved": {0, 1, 0},
			"fresh-skill\nsomething new":      {1, 0, 0},
		},
	}
	mustPropose(t, s, skillKey("proj-a", true, true), map[string]any{
		"name": "fresh-skill", "description": "something new", "body": "# Fresh",
	})

	got, err := s.skillStore.GetByID(ctx, trusted.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Maturity != persistence.SkillMaturityTrusted {
		t.Errorf("maturity = %q, want trusted — a backfill must not require re-approval", got.Maturity)
	}
	if got.Version != 3 {
		t.Errorf("version = %d, want 3 unchanged", got.Version)
	}
}

// seedNearDuplicate installs a skill the lexical fallback will flag, and
// returns it.
func seedNearDuplicate(t *testing.T, s *Server) *persistence.Skill {
	t.Helper()
	sk := &persistence.Skill{
		ID: "sk-alpha", ProjectID: "proj-a", Name: "review-checklist-alpha",
		Description: "WHEN reviewing infrastructure changes before merge",
		Body:        "# Alpha\n## Anti-patterns",
		Maturity:    persistence.SkillMaturityActive, Version: 1,
	}
	if err := s.skillStore.Create(context.Background(), sk); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return sk
}
