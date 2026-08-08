package api

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Calibration fixtures: the real catalogue clusters from LLD §12.0, verbatim
// descriptions and real heading structures. These are the cases the slice
// exists to catch, so they are the cases the metric is scored against.

var (
	fxInfraReview = &persistence.Skill{
		ID:          "fx-infra",
		Name:        "infra-code-review-checklist",
		Description: "WHEN reviewing infrastructure changes (detached writes, scope resolution, edition gates, BFS traversal, audit logging) before merge",
		Body: `# Infrastructure Code Review Checklist
## 1. Write durability (audit/log writes)
## 2. Failure-mode discrimination (error to reason mapping)
## 3. Scope resolution (exact-match vs tree/ancestor)
## 4. Traversal bounds (BFS/DFS over task tree)
## 5. Edition gating (multi-edition codebases)
## 6. Default-case handling (minor but recurring)
## Review output structure
## Anti-patterns to flag on sight`,
	}
	fxVerifyTests = &persistence.Skill{
		ID:          "fx-verify",
		Name:        "review-verify-claimed-tests-in-diff",
		Description: "WHEN reviewing a commit/diff that claims to add or modify tests: verify the test files are actually present in the staged diff before certifying coverage claims",
		Body: `# Verify Claimed Test Deliverables Are in the Diff
## Problem
## Procedure
## Anti-patterns
## Complementary checks`,
	}
	fxSecReview = &persistence.Skill{
		ID:          "fx-sec",
		Name:        "security-review-checklist-llm-chat-surfaces",
		Description: "When reviewing LLM-powered chat surfaces for security vulnerabilities",
		Body: `# Security Review Checklist for LLM Chat Surfaces
## Prompt injection
## Data exfiltration
## Anti-patterns`,
	}
	fxMemReview = &persistence.Skill{
		ID:          "fx-mem",
		Name:        "reviewing-ai-memory-write-designs",
		Description: "WHEN reviewing an AI agent design proposal that adds human-input-driven memory writes (chat notes, feedback loops, shared-scope stores), use these patterns to catch soft-controls-as-security-controls and compliance-after-the-fact errors.",
		Body: `# Reviewing AI Memory-Write Designs
## Soft controls are not security controls
## Compliance after the fact
## Anti-patterns`,
	}

	fxWritingDesigns = &persistence.Skill{
		ID:          "fx-writing",
		Name:        "writing-designs-and-implementation-plans",
		Description: "When producing a design/spec and/or a TDD implementation plan for a new feature or nontrivial change, before writing code — how to structure it, where to put it, and the quality bars it must clear.",
		Body: `# Writing designs and implementation plans
## Where the docs go
## Method (phases)
## Design/spec skeleton
## Implementation-plan skeleton
## Quality bars
## Verify-before-done checklist (against the actual codebase)
## Anti-patterns`,
	}
	fxAmendDesign = &persistence.Skill{
		ID:          "fx-amend",
		Name:        "amend-the-design-when-you-touch-the-surface",
		Description: "Use whenever a change lands on a surface an LLD describes, or on a surface that has no LLD — the design record must be amended in the same change, kept EE-only, and re-ingested into RAG before the work is called done.",
		Body: `# Amend the design when you touch the surface
## When it triggers
## What a good amendment contains
## Hard constraint: designs are EE IP
## Then re-ingest, or the record is only half-updated
## Also update
## Anti-patterns`,
	}

	fxSoakBacklog = &persistence.Skill{
		ID:          "fx-soak",
		Name:        "soak-then-enable-via-backlog",
		Description: "When a feature ships behind a flag with a soak/observation window before it can be turned on, schedule the post-soak enablement as an absolute-dated backlog item — via the backlog_deposit tool if available, else the project's own backlog file.",
		Body: `# Soak-then-enable via the backlog
## When to use
## The rule
## How to deposit — prefer the tool
## What the item must contain (either path)
## Acting on a due item (later sessions)
## Anti-patterns`,
	}
	fxOffScope = &persistence.Skill{
		ID:          "fx-offscope",
		Name:        "off-scope-finding-capture",
		Description: "When you notice a bug, optimisation opportunity, inefficiency, or refactor candidate OUTSIDE your current task's scope while working a dev-pipeline task — capture it with the backlog_deposit tool instead of fixing it.",
		Body: `# Off-scope finding capture
## When to deposit
## What to do
## What NOT to do
## Reviewer special case
## If the tool is unavailable`,
	}

	// Negative controls: unrelated skills that must NOT be flagged.
	fxDossier = &persistence.Skill{
		ID:          "fx-dossier",
		Name:        "person-dossier-research",
		Description: "When asked to build a profile/dossier on a specific person from limited identifiers (name, email, and/or phone) — especially inside a research or deep-research workflow.",
		Body: `# Person dossier research
## Entity resolution
## Source corroboration
## Citations and retrieval dates`,
	}
	fxDispatcher = &persistence.Skill{
		ID:          "fx-dispatcher",
		Name:        "dispatcher-workflow-selection",
		Description: "When the telegram/slack dispatcher creates a task, pick the workflow that matches the user's intent instead of defaulting to adaptive/the project default.",
		Body: `# Dispatcher workflow selection
## Match intent to workflow
## Defaults are a last resort`,
	}
)

// TestSkillLexicalFallbackSeparation pins the DEGRADED path's behaviour.
//
// This is not the acceptance gate — the lexical metric already failed that
// (LLD §12.2), which is why cosine is the primary. What this asserts is the
// weaker property the fallback must still hold: intra-cluster pairs outscore
// unrelated controls, so an embedder outage degrades the guard rather than
// silently disabling it. The real cosine calibration needs a live embedder and
// lives in the integration lane.
func TestSkillLexicalFallbackSeparation(t *testing.T) {
	type pair struct {
		name string
		a, b *persistence.Skill
	}
	intra := []pair{
		{"review: infra x verify-tests", fxInfraReview, fxVerifyTests},
		{"review: infra x security", fxInfraReview, fxSecReview},
		{"review: infra x memory-designs", fxInfraReview, fxMemReview},
		{"review: verify-tests x security", fxVerifyTests, fxSecReview},
		{"review: verify-tests x memory-designs", fxVerifyTests, fxMemReview},
		{"review: security x memory-designs", fxSecReview, fxMemReview},
		{"design: writing x amend", fxWritingDesigns, fxAmendDesign},
		{"backlog: soak x off-scope", fxSoakBacklog, fxOffScope},
	}
	negative := []pair{
		{"control: dossier x dispatcher", fxDossier, fxDispatcher},
		{"control: dossier x infra-review", fxDossier, fxInfraReview},
		{"control: dispatcher x soak-backlog", fxDispatcher, fxSoakBacklog},
	}

	best := func(p pair) float64 {
		d, h := scoreSkillPair(p.a.Name, p.a.Description, p.a.Body, p.b.Name, p.b.Description, p.b.Body)
		m := d
		if h > m {
			m = h
		}
		t.Logf("%-40s desc=%.3f head=%.3f max=%.3f", p.name, d, h, m)
		return m
	}

	intraMin, negMax := 1.0, 0.0
	for _, p := range intra {
		if s := best(p); s < intraMin {
			intraMin = s
		}
	}
	for _, p := range negative {
		if s := best(p); s > negMax {
			negMax = s
		}
	}
	t.Logf("weakest intra-cluster %.3f / strongest control %.3f / fallback threshold %.3f",
		intraMin, negMax, skillDupeLexicalThreshold)

	if intraMin <= negMax {
		t.Errorf("lexical fallback does not separate: weakest intra-cluster %.3f <= strongest control %.3f", intraMin, negMax)
	}
	if skillDupeLexicalThreshold > intraMin {
		t.Errorf("fallback threshold %.3f is above the weakest intra-cluster pair %.3f — the fallback would flag nothing",
			skillDupeLexicalThreshold, intraMin)
	}
	if skillDupeLexicalThreshold <= negMax {
		t.Errorf("fallback threshold %.3f is at or below the strongest control %.3f — the fallback would flag noise",
			skillDupeLexicalThreshold, negMax)
	}
}

func TestCosineNotComparableReturnsNegativeOne(t *testing.T) {
	cases := map[string][2][]float32{
		"empty left":         {nil, {1, 2}},
		"empty right":        {{1, 2}, nil},
		"dimension mismatch": {{1, 2, 3}, {1, 2}},
		"zero vector":        {{0, 0}, {1, 1}},
	}
	for name, c := range cases {
		if got := cosine(c[0], c[1]); got != -1 {
			t.Errorf("%s: cosine = %v, want -1 (not comparable must be distinguishable from orthogonal)", name, got)
		}
	}
}

func TestCosineScoresKnownVectors(t *testing.T) {
	if got := cosine([]float32{1, 0}, []float32{1, 0}); got < 0.999 {
		t.Errorf("identical vectors scored %v, want ~1", got)
	}
	if got := cosine([]float32{1, 0}, []float32{0, 1}); got > 0.001 || got < -0.001 {
		t.Errorf("orthogonal vectors scored %v, want ~0", got)
	}
	if got := cosine([]float32{1, 0}, []float32{-1, 0}); got > -0.999 {
		t.Errorf("opposite vectors scored %v, want ~-1", got)
	}
}

// TestEmbeddingComparabilityRequiresSameModel: vectors from different models
// occupy different spaces. Comparing them yields a confident meaningless
// number, which is worse than falling back to lexical.
func TestEmbeddingComparabilityRequiresSameModel(t *testing.T) {
	a := &persistence.Skill{Embedding: []float32{1, 0}, EmbeddingModel: "model-a"}
	b := &persistence.Skill{Embedding: []float32{1, 0}, EmbeddingModel: "model-b"}
	same := &persistence.Skill{Embedding: []float32{1, 0}, EmbeddingModel: "model-a"}
	unset := &persistence.Skill{Embedding: []float32{1, 0}}

	if skillsAreEmbeddingComparable(a, b) {
		t.Error("different embedding models reported comparable")
	}
	if skillsAreEmbeddingComparable(a, unset) {
		t.Error("row with no recorded model reported comparable")
	}
	if !skillsAreEmbeddingComparable(a, same) {
		t.Error("same model reported not comparable")
	}
}

// TestFindSkillDuplicatesPrefersEmbeddingOverLexical: when both rows carry
// comparable vectors, the semantic score decides — a pair that lexical would
// miss entirely must still be flagged.
func TestFindSkillDuplicatesPrefersEmbeddingOverLexical(t *testing.T) {
	candidate := &persistence.Skill{
		ID: "new", Name: "alpha-trigger", Description: "wholly different wording",
		Body: "# Alpha", Embedding: []float32{1, 0, 0}, EmbeddingModel: "m",
	}
	existing := []*persistence.Skill{{
		ID: "old", Name: "beta-trigger", Description: "nothing lexically shared",
		Body: "# Beta", Embedding: []float32{0.99, 0.01, 0}, EmbeddingModel: "m",
		Maturity: "active",
	}}
	got := findSkillDuplicates(candidate, existing)
	if len(got) != 1 {
		t.Fatalf("got %d matches, want 1 — cosine should have caught what lexical cannot", len(got))
	}
	if got[0].Reason != reasonSimilarEmbedding {
		t.Errorf("reason = %q, want %q", got[0].Reason, reasonSimilarEmbedding)
	}
}

// TestFindSkillDuplicatesFallsBackWhenUnembedded: an un-embedded row (the lazy
// backfill state, or an embedder outage) must still be compared, not skipped.
func TestFindSkillDuplicatesFallsBackWhenUnembedded(t *testing.T) {
	candidate := &persistence.Skill{
		ID: "new", Name: "review-checklist-alpha",
		Description: "WHEN reviewing infrastructure changes before merge",
		Body:        "# Alpha\n## Anti-patterns",
	}
	existing := []*persistence.Skill{{
		ID: "old", Name: "review-checklist-beta",
		Description: "WHEN reviewing infrastructure changes before merge",
		Body:        "# Beta\n## Anti-patterns", Maturity: "active",
	}}
	got := findSkillDuplicates(candidate, existing)
	if len(got) == 0 {
		t.Fatal("un-embedded rows were not compared at all — an embedder outage must degrade, not disable")
	}
	if got[0].Reason == reasonSimilarEmbedding {
		t.Errorf("reason = %q, want a lexical reason for un-embedded rows", got[0].Reason)
	}
}

func TestJaccardEmptySetsScoreZero(t *testing.T) {
	if got := jaccard(map[string]struct{}{}, map[string]struct{}{}); got != 0 {
		t.Errorf("two empty sets scored %v, want 0 — an empty description must never read as a perfect match", got)
	}
}

func TestSkillHeadingsNormalisesNumbering(t *testing.T) {
	bare := skillHeadings("## Root cause")
	sectioned := skillHeadings("## §12.1 Root cause")
	numbered := skillHeadings("## 12.1 Root cause")
	if jaccard(bare, sectioned) != 1 {
		t.Errorf("'## §12.1 Root cause' did not normalise to '## Root cause': %v vs %v", sectioned, bare)
	}
	if jaccard(bare, numbered) != 1 {
		t.Errorf("'## 12.1 Root cause' did not normalise to '## Root cause': %v vs %v", numbered, bare)
	}
}

func TestSkillHeadingsEmptyBodyDegrades(t *testing.T) {
	if got := skillHeadings(""); len(got) != 0 {
		t.Errorf("empty body produced headings %v, want none", got)
	}
	// Malformed markdown must not panic or error — it degrades to no headings.
	if got := skillHeadings("#no space\n####### seven hashes\n   \n**bold**"); len(got) != 0 {
		t.Errorf("malformed markdown produced headings %v, want none", got)
	}
}

// TestFindSkillDuplicatesFlagsNameCollisionAcrossScope guards the case that
// produced the two moltbook-engagement rows: identical name, different scope,
// which the (project, repo_scope, name) natural key permits silently.
func TestFindSkillDuplicatesFlagsNameCollisionAcrossScope(t *testing.T) {
	candidate := &persistence.Skill{
		ID: "new", Name: "shared-name", RepoScope: "github.com/acme/a",
		Description: "totally different words here", Body: "# Nothing alike",
	}
	existing := []*persistence.Skill{{
		ID: "old", Name: "shared-name", RepoScope: "github.com/acme/b",
		Description: "no overlap whatsoever", Body: "# Unrelated", Maturity: "active",
	}}
	got := findSkillDuplicates(candidate, existing)
	if len(got) != 1 {
		t.Fatalf("got %d matches, want 1", len(got))
	}
	if got[0].Reason != reasonNameOtherScope {
		t.Errorf("reason = %q, want %q", got[0].Reason, reasonNameOtherScope)
	}
}

// TestFindSkillDuplicatesIgnoresSelf: re-proposing an existing skill by id must
// not report the skill as a duplicate of itself.
func TestFindSkillDuplicatesIgnoresSelf(t *testing.T) {
	s := &persistence.Skill{ID: "same", Name: "x", Description: "y", Body: "# z"}
	if got := findSkillDuplicates(s, []*persistence.Skill{s}); len(got) != 0 {
		t.Errorf("skill matched itself: %+v", got)
	}
}
