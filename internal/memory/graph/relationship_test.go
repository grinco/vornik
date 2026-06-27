package graph

import (
	"context"
	"strings"
	"testing"
)

func TestRelationship_HappyPath(t *testing.T) {
	chunk := "ACME quoted $9,500 for the new ledger build, beating Globex's offer."
	body := `[
	  {"from":"kent-ACME","to":"kent-PRICE","predicate":"QUOTED_PRICE","evidence":"ACME quoted $9,500"},
	  {"from":"kent-ACME","to":"kent-GLOBEX","predicate":"CHOSEN_OVER","evidence":"beating Globex's offer"}
	]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "fake")

	ents := []ResolvedEntity{
		{ID: "kent-ACME", Type: "VENDOR", CanonicalName: "ACME"},
		{ID: "kent-GLOBEX", Type: "VENDOR", CanonicalName: "Globex"},
		{ID: "kent-PRICE", Type: "PRICE", CanonicalName: "$9,500"},
	}
	got, m, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 proposals, got %d (%+v)", len(got), got)
	}
	if m.ProposedKept != 2 || m.ProposedDropped != 0 {
		t.Errorf("metrics off: %+v", m)
	}
}

func TestRelationship_DropsUnknownEntityIDs(t *testing.T) {
	chunk := "Vadim chose PostgreSQL 16 for the new ledger service."
	body := `[
	  {"from":"kent-VADIM","to":"kent-MYSTERY","predicate":"DEPENDS_ON","evidence":"PostgreSQL 16"},
	  {"from":"kent-VADIM","to":"kent-PG","predicate":"DEPENDS_ON","evidence":"PostgreSQL 16"}
	]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "")

	ents := []ResolvedEntity{
		{ID: "kent-VADIM", Type: "PERSON", CanonicalName: "Vadim"},
		{ID: "kent-PG", Type: "TECHNOLOGY", CanonicalName: "PostgreSQL 16"},
	}
	got, m, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 || got[0].To != "kent-PG" {
		t.Fatalf("expected 1 kept proposal pointing at kent-PG, got %+v", got)
	}
	if m.ProposedDropped != 1 {
		t.Errorf("expected 1 drop for unknown id, got metrics %+v", m)
	}
}

func TestRelationship_DropsUnknownPredicate(t *testing.T) {
	chunk := "Vadim hates the legacy Mongo cluster."
	body := `[{"from":"kent-V","to":"kent-M","predicate":"HATES","evidence":"Vadim hates the legacy Mongo"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "")

	ents := []ResolvedEntity{
		{ID: "kent-V", Type: "PERSON", CanonicalName: "Vadim"},
		{ID: "kent-M", Type: "TECHNOLOGY", CanonicalName: "Mongo"},
	}
	got, m, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 proposals after dropping unknown predicate, got %+v", got)
	}
	if m.ProposedDropped != 1 {
		t.Errorf("expected drop count 1, got %+v", m)
	}
}

func TestRelationship_DropsHallucinatedEvidence(t *testing.T) {
	chunk := "Vadim chose PostgreSQL 16 for the new ledger service."
	// "for the legacy mainframe" is not in the chunk.
	body := `[{"from":"kent-V","to":"kent-PG","predicate":"DEPENDS_ON","evidence":"for the legacy mainframe"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "")

	ents := []ResolvedEntity{
		{ID: "kent-V", Type: "PERSON", CanonicalName: "Vadim"},
		{ID: "kent-PG", Type: "TECHNOLOGY", CanonicalName: "PostgreSQL 16"},
	}
	got, m, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 proposals after dropping hallucinated evidence, got %+v", got)
	}
	if m.ProposedDropped != 1 {
		t.Errorf("expected drop count 1, got %+v", m)
	}
}

func TestRelationship_DropsSelfLoop(t *testing.T) {
	chunk := "ACME, also doing business as ACME Corp, signed the contract."
	body := `[{"from":"kent-ACME","to":"kent-ACME","predicate":"RELATES_TO","evidence":"ACME, also doing business as ACME Corp"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "")

	ents := []ResolvedEntity{{ID: "kent-ACME", Type: "VENDOR", CanonicalName: "ACME"}}
	got, _, err := re.Extract(context.Background(), chunk, ents)
	// len(entities) == 1 short-circuits before LLM; let's exercise the
	// drop path with two entities of the same id (impossible in
	// practice but exercises the validator branch).
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got != nil {
		t.Errorf("single-entity chunk returned proposals: %+v", got)
	}
}

func TestRelationship_NormalisesSymmetricEdgeOrdering(t *testing.T) {
	chunk := "ACME and Globex are both vendors we evaluated."
	body := `[
	  {"from":"kent-Z","to":"kent-A","predicate":"RELATES_TO","evidence":"ACME and Globex are both vendors"}
	]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "")

	ents := []ResolvedEntity{
		{ID: "kent-A", Type: "VENDOR", CanonicalName: "ACME"},
		{ID: "kent-Z", Type: "VENDOR", CanonicalName: "Globex"},
	}
	got, _, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 || got[0].From != "kent-A" || got[0].To != "kent-Z" {
		t.Errorf("expected (kent-A → kent-Z) after symmetric normalise, got %+v", got)
	}
}

func TestRelationship_DedupsRepeatedTriples(t *testing.T) {
	chunk := "Vadim picked PostgreSQL 16 — we then standardised on PostgreSQL 16."
	body := `[
	  {"from":"kent-V","to":"kent-PG","predicate":"DEPENDS_ON","evidence":"Vadim picked PostgreSQL 16"},
	  {"from":"kent-V","to":"kent-PG","predicate":"DEPENDS_ON","evidence":"standardised on PostgreSQL 16"}
	]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "")

	ents := []ResolvedEntity{
		{ID: "kent-V", Type: "PERSON", CanonicalName: "Vadim"},
		{ID: "kent-PG", Type: "TECHNOLOGY", CanonicalName: "PostgreSQL 16"},
	}
	got, m, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 deduped proposal, got %d (%+v)", len(got), got)
	}
	if m.ProposedDropped != 1 {
		t.Errorf("expected 1 drop for dup, got %+v", m)
	}
}

func TestRelationship_FewerThanTwoEntitiesShortCircuits(t *testing.T) {
	fp := &fakeProvider{}
	re := NewRelationshipExtractor(fp, "")

	got, _, err := re.Extract(context.Background(), "any chunk", []ResolvedEntity{
		{ID: "only-one", Type: "PERSON", CanonicalName: "Solo"},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for single-entity chunk, got %+v", got)
	}
	if fp.calls.Load() != 0 {
		t.Errorf("expected no LLM call when < 2 entities, got %d", fp.calls.Load())
	}
}

func TestRelationship_AcceptsObjectWrapping(t *testing.T) {
	chunk := "Vadim picked PostgreSQL 16."
	body := `{"edges":[{"from":"kent-V","to":"kent-PG","predicate":"DEPENDS_ON","evidence":"Vadim picked PostgreSQL 16"}]}`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "")

	ents := []ResolvedEntity{
		{ID: "kent-V", Type: "PERSON", CanonicalName: "Vadim"},
		{ID: "kent-PG", Type: "TECHNOLOGY", CanonicalName: "PostgreSQL 16"},
	}
	got, _, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("object-wrapped parse failed: %+v", got)
	}
}

func TestIsKnownPredicate(t *testing.T) {
	if !isKnownPredicate("DEPENDS_ON") {
		t.Error("DEPENDS_ON should be known")
	}
	if isKnownPredicate("ENJOYS") {
		t.Error("ENJOYS should be unknown")
	}
}

func TestIsSymmetricPredicate(t *testing.T) {
	if !isSymmetricPredicate("RELATES_TO") {
		t.Error("RELATES_TO should be symmetric")
	}
	if isSymmetricPredicate("OWNED_BY") {
		t.Error("OWNED_BY should be directional")
	}
}

func TestRelationship_BuildsUserMessage(t *testing.T) {
	captured := &capturingProvider{reply: "[]"}
	re := NewRelationshipExtractor(captured, "")
	chunk := "Vadim chose PostgreSQL 16."
	ents := []ResolvedEntity{
		{ID: "kent-V", Type: "PERSON", CanonicalName: "Vadim"},
		{ID: "kent-PG", Type: "TECHNOLOGY", CanonicalName: "PostgreSQL 16"},
	}
	_, _, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(captured.userMsg, "ENTITIES:") || !strings.Contains(captured.userMsg, "CHUNK:") {
		t.Errorf("user message missing labels: %q", captured.userMsg)
	}
	if !strings.Contains(captured.userMsg, "kent-V") || !strings.Contains(captured.userMsg, "kent-PG") {
		t.Errorf("entity ids missing from prompt: %q", captured.userMsg)
	}
}

// 2026-05-25 audit found 48% of knowledge_entities on the live DB
// had zero edges. Per-chunk forensics traced ~33% of those isolated
// entities to relationship proposals dropped at the evidence-
// substring check on cosmetic differences the LLM routinely
// introduces (smart quotes, NFD-normalised diacritics, collapsed
// whitespace). The fix normalises both sides before falling back
// to the substring check. These tests pin the new behaviour
// against future regression.

func TestRelationship_AcceptsSmartQuotesInEvidence(t *testing.T) {
	// Chunk uses straight ASCII quotes; LLM emits smart quotes.
	// Pre-fix this dropped at the validator's substring check.
	chunk := `ACME chose "PostgreSQL 16" for the new ledger.`
	body := `[{"from":"kent-A","to":"kent-PG","predicate":"DEPENDS_ON","evidence":"chose “PostgreSQL 16”"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "")

	ents := []ResolvedEntity{
		{ID: "kent-A", Type: "VENDOR", CanonicalName: "ACME"},
		{ID: "kent-PG", Type: "TECHNOLOGY", CanonicalName: "PostgreSQL 16"},
	}
	got, m, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 proposal kept after smart-quote normalisation, got %d (drops=%d)", len(got), m.ProposedDropped)
	}
}

func TestRelationship_AcceptsStraightQuotesWhenChunkHasSmart(t *testing.T) {
	// Inverse direction: chunk has smart quotes, LLM emits
	// straight quotes. Same normalisation path.
	chunk := "ACME chose “PostgreSQL 16” for the new ledger."
	body := `[{"from":"kent-A","to":"kent-PG","predicate":"DEPENDS_ON","evidence":"chose \"PostgreSQL 16\""}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "")

	ents := []ResolvedEntity{
		{ID: "kent-A", Type: "VENDOR", CanonicalName: "ACME"},
		{ID: "kent-PG", Type: "TECHNOLOGY", CanonicalName: "PostgreSQL 16"},
	}
	got, _, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 proposal kept after reverse-direction quote normalisation, got %d", len(got))
	}
}

func TestRelationship_AcceptsCollapsedWhitespaceInEvidence(t *testing.T) {
	// Chunk has a multi-space / newline span; LLM emits the
	// quote with whitespace collapsed to single spaces. Common
	// pattern on long chunks where the LLM normalises wrapping.
	chunk := "ACME quoted\n  $9,500\nfor the new ledger build."
	body := `[{"from":"kent-A","to":"kent-P","predicate":"QUOTED_PRICE","evidence":"ACME quoted $9,500 for the new ledger build"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "")

	ents := []ResolvedEntity{
		{ID: "kent-A", Type: "VENDOR", CanonicalName: "ACME"},
		{ID: "kent-P", Type: "PRICE", CanonicalName: "$9,500"},
	}
	got, _, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 proposal kept after whitespace-collapse normalisation, got %d", len(got))
	}
}

func TestRelationship_AcceptsNFDDecomposedDiacritics(t *testing.T) {
	// Chunk has the precomposed é (U+00E9); LLM emits the
	// decomposed e + combining acute (U+0065 U+0301). Visually
	// identical, byte-distinct.
	chunk := "Café Globex signed the deal with ACME."
	// "Café" with NFD-decomposed é:
	body := `[{"from":"kent-A","to":"kent-G","predicate":"RELATES_TO","evidence":"Café Globex signed the deal with ACME"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "")

	ents := []ResolvedEntity{
		{ID: "kent-A", Type: "VENDOR", CanonicalName: "ACME"},
		{ID: "kent-G", Type: "VENDOR", CanonicalName: "Café Globex"},
	}
	got, _, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 proposal kept after NFC/NFD normalisation, got %d", len(got))
	}
}

func TestRelationship_AcceptsEmDashAsHyphenInEvidence(t *testing.T) {
	// Chunk has an ASCII hyphen; LLM emits an em-dash. Same
	// normalisation set.
	chunk := "ACME-Globex partnership signed the deal."
	body := `[{"from":"kent-A","to":"kent-G","predicate":"RELATES_TO","evidence":"ACME—Globex partnership signed the deal"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "")

	ents := []ResolvedEntity{
		{ID: "kent-A", Type: "VENDOR", CanonicalName: "ACME"},
		{ID: "kent-G", Type: "VENDOR", CanonicalName: "Globex"},
	}
	got, _, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 proposal kept after dash normalisation, got %d", len(got))
	}
}

func TestRelationship_StillDropsGenuinelyDifferentEvidence(t *testing.T) {
	// Negative test: the fix must NOT loosen so much that
	// hallucinated evidence sneaks through. A quote that says
	// something genuinely different from anything in the chunk
	// still drops, even after normalisation.
	chunk := "ACME chose PostgreSQL 16 for the new ledger."
	body := `[{"from":"kent-A","to":"kent-PG","predicate":"DEPENDS_ON","evidence":"ACME rejected PostgreSQL for the legacy mainframe"}]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "")

	ents := []ResolvedEntity{
		{ID: "kent-A", Type: "VENDOR", CanonicalName: "ACME"},
		{ID: "kent-PG", Type: "TECHNOLOGY", CanonicalName: "PostgreSQL 16"},
	}
	got, m, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("genuinely-different evidence must still drop; got %+v", got)
	}
	if m.ProposedDropped != 1 {
		t.Errorf("expected drop count 1, got %d", m.ProposedDropped)
	}
}

func TestEvidenceInChunk_StrictSubstringFastPath(t *testing.T) {
	// Defence: the strict-substring path stays the dominant
	// branch for the common case (no normalisation needed).
	// The helper must not perversely reject a strict match.
	cases := []struct {
		name  string
		chunk string
		ev    string
		want  bool
	}{
		{"exact match", "ACME quoted $9,500.", "ACME quoted $9,500", true},
		{"empty evidence", "anything", "", false},
		{"absent substring", "ACME signed.", "rejected", false},
		{"smart quote chunk vs straight ev", "“hello” world", `"hello" world`, true},
		{"straight chunk vs smart ev", `"hello" world`, "“hello” world", true},
		{"collapse multispace", "hello   world", "hello world", true},
		{"em-dash vs hyphen", "A—B", "A-B", true},
		{"NFD vs NFC", "Café", "Café", true},
		{"genuinely different", "hello world", "goodbye world", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evidenceInChunk(tc.chunk, tc.ev)
			if got != tc.want {
				t.Errorf("evidenceInChunk(%q, %q) = %v, want %v",
					tc.chunk, tc.ev, got, tc.want)
			}
		})
	}
}

// Per-drop-reason attribution. Earlier tests pin the WHAT
// (kept vs dropped count); these pin the WHY (which validation
// rule fired). Each test exercises ONE rule in isolation so the
// label value is unambiguous. Sum of DropsByReason values
// across all tests equals ProposedDropped for the same input.

func TestRelationship_DropsByReason_EmptyEndpoint(t *testing.T) {
	body := `[{"from":"","to":"kent-B","predicate":"RELATES_TO","evidence":"x"}]`
	got := dropReasonsFor(t, body, "x x", "kent-A", "kent-B")
	if got[DropReasonEmptyEndpoint] != 1 {
		t.Errorf("expected empty_endpoint=1, got map=%v", got)
	}
}

func TestRelationship_DropsByReason_SelfLoop(t *testing.T) {
	body := `[{"from":"kent-A","to":"kent-A","predicate":"RELATES_TO","evidence":"ACME exists"}]`
	got := dropReasonsFor(t, body, "ACME exists", "kent-A", "kent-B")
	if got[DropReasonSelfLoop] != 1 {
		t.Errorf("expected self_loop=1, got map=%v", got)
	}
}

func TestRelationship_DropsByReason_UnknownFrom(t *testing.T) {
	body := `[{"from":"kent-MYSTERY","to":"kent-B","predicate":"RELATES_TO","evidence":"ACME exists"}]`
	got := dropReasonsFor(t, body, "ACME exists", "kent-A", "kent-B")
	if got[DropReasonUnknownFrom] != 1 {
		t.Errorf("expected unknown_from=1, got map=%v", got)
	}
}

func TestRelationship_DropsByReason_UnknownTo(t *testing.T) {
	body := `[{"from":"kent-A","to":"kent-MYSTERY","predicate":"RELATES_TO","evidence":"ACME exists"}]`
	got := dropReasonsFor(t, body, "ACME exists", "kent-A", "kent-B")
	if got[DropReasonUnknownTo] != 1 {
		t.Errorf("expected unknown_to=1, got map=%v", got)
	}
}

func TestRelationship_DropsByReason_UnknownPredicate(t *testing.T) {
	body := `[{"from":"kent-A","to":"kent-B","predicate":"DEDICATED_TO","evidence":"ACME exists"}]`
	got := dropReasonsFor(t, body, "ACME exists", "kent-A", "kent-B")
	if got[DropReasonUnknownPredicate] != 1 {
		t.Errorf("expected unknown_predicate=1, got map=%v", got)
	}
}

func TestRelationship_DropsByReason_EmptyEvidence(t *testing.T) {
	body := `[{"from":"kent-A","to":"kent-B","predicate":"RELATES_TO","evidence":""}]`
	got := dropReasonsFor(t, body, "ACME exists", "kent-A", "kent-B")
	if got[DropReasonEmptyEvidence] != 1 {
		t.Errorf("expected empty_evidence=1, got map=%v", got)
	}
}

func TestRelationship_DropsByReason_EvidenceNotInChunk(t *testing.T) {
	body := `[{"from":"kent-A","to":"kent-B","predicate":"RELATES_TO","evidence":"fabricated quote"}]`
	got := dropReasonsFor(t, body, "ACME and Globex signed.", "kent-A", "kent-B")
	if got[DropReasonEvidenceNotInChunk] != 1 {
		t.Errorf("expected evidence_not_in_chunk=1, got map=%v", got)
	}
}

func TestRelationship_DropsByReason_DuplicateTriple(t *testing.T) {
	// Two proposals with the same (from, predicate, to) — first
	// kept, second dropped as duplicate.
	body := `[
	  {"from":"kent-A","to":"kent-B","predicate":"RELATES_TO","evidence":"ACME exists"},
	  {"from":"kent-A","to":"kent-B","predicate":"RELATES_TO","evidence":"ACME exists"}
	]`
	got := dropReasonsFor(t, body, "ACME exists", "kent-A", "kent-B")
	if got[DropReasonDuplicateTriple] != 1 {
		t.Errorf("expected duplicate_triple=1, got map=%v", got)
	}
}

func TestRelationship_DropsByReason_SumEqualsTotal(t *testing.T) {
	// Mixed-reason batch — assert byReason sums to
	// ProposedDropped. Critical invariant: the per-reason
	// breakdown must not over- or under-count vs the
	// aggregate.
	body := `[
	  {"from":"","to":"kent-B","predicate":"RELATES_TO","evidence":"ACME"},
	  {"from":"kent-A","to":"kent-A","predicate":"RELATES_TO","evidence":"ACME"},
	  {"from":"kent-A","to":"kent-B","predicate":"NOT_A_PREDICATE","evidence":"ACME"},
	  {"from":"kent-A","to":"kent-B","predicate":"RELATES_TO","evidence":"fabricated"}
	]`
	fp := &fakeProvider{replies: []reply{{content: body}}}
	re := NewRelationshipExtractor(fp, "")
	ents := []ResolvedEntity{
		{ID: "kent-A", Type: "VENDOR", CanonicalName: "ACME"},
		{ID: "kent-B", Type: "VENDOR", CanonicalName: "B"},
	}
	_, m, err := re.Extract(context.Background(), "ACME", ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	sum := 0
	for _, v := range m.DropsByReason {
		sum += v
	}
	if sum != m.ProposedDropped {
		t.Errorf("DropsByReason sum %d != ProposedDropped %d (map=%v)",
			sum, m.ProposedDropped, m.DropsByReason)
	}
	if m.ProposedDropped != 4 {
		t.Errorf("expected 4 total drops, got %d", m.ProposedDropped)
	}
}

// dropReasonsFor is a focused helper for the per-reason tests.
// Constructs a 2-entity setup (kent-A, kent-B), runs Extract,
// and returns the DropsByReason map for assertion.
func dropReasonsFor(t *testing.T, replyBody, chunk, aID, bID string) map[string]int {
	t.Helper()
	fp := &fakeProvider{replies: []reply{{content: replyBody}}}
	re := NewRelationshipExtractor(fp, "")
	ents := []ResolvedEntity{
		{ID: aID, Type: "VENDOR", CanonicalName: "ACME"},
		{ID: bID, Type: "VENDOR", CanonicalName: "B"},
	}
	_, m, err := re.Extract(context.Background(), chunk, ents)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if m.DropsByReason == nil {
		t.Fatalf("DropsByReason must be non-nil even when empty")
	}
	return m.DropsByReason
}

func TestNormaliseForMatch_Idempotent(t *testing.T) {
	// Applying normalisation twice produces the same result as
	// once. Important property for the helper — otherwise the
	// strict→normalised fallback could thrash on edge cases.
	inputs := []string{
		"",
		"plain ASCII",
		"“quotes”  and —dashes—",
		"Café — “result”",
		"   leading and trailing   ",
	}
	for _, s := range inputs {
		once := normaliseForMatch(s)
		twice := normaliseForMatch(once)
		if once != twice {
			t.Errorf("not idempotent: %q → %q → %q", s, once, twice)
		}
	}
}
