package graph

import (
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// ---------------------------------------------------------------------------
// responseCacheKey — the cache identity primitive. Two (model, prompt) pairs
// that should hit the same row MUST hash identically; anything that should
// produce a distinct LLM answer MUST hash differently. The NUL-delimited
// fingerprint also has to be collision-safe across field boundaries, or two
// different message shapes could alias onto one cache row and serve a stale
// answer.
// ---------------------------------------------------------------------------

func msgs(pairs ...string) []chat.Message {
	out := make([]chat.Message, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, chat.Message{Role: pairs[i], Content: pairs[i+1]})
	}
	return out
}

func TestResponseCacheKey_DeterministicForIdenticalInputs(t *testing.T) {
	m := msgs("system", "extract entities", "user", "CHUNK:\nVadim chose PostgreSQL")
	a := responseCacheKey("gpt-oss:20b", responseCachePurposeKGExtract, m)
	b := responseCacheKey("gpt-oss:20b", responseCachePurposeKGExtract, msgs(
		"system", "extract entities", "user", "CHUNK:\nVadim chose PostgreSQL"))
	if a != b {
		t.Fatalf("same inputs must hash identically: %s != %s", a, b)
	}
	// SHA-256 hex is 64 chars; guards against an empty/degenerate hash.
	if len(a) != 64 {
		t.Fatalf("expected 64-char sha256 hex, got %d chars: %q", len(a), a)
	}
}

func TestResponseCacheKey_SensitiveToModelAndPurposeAndContent(t *testing.T) {
	base := msgs("system", "sys", "user", "body")
	key := responseCacheKey("model-a", "purpose-x", base)

	cases := map[string]string{
		"different model":   responseCacheKey("model-b", "purpose-x", base),
		"different purpose": responseCacheKey("model-a", "purpose-y", base),
		"different content": responseCacheKey("model-a", "purpose-x", msgs("system", "sys", "user", "body2")),
		"different role":    responseCacheKey("model-a", "purpose-x", msgs("system", "sys", "assistant", "body")),
		"extra message":     responseCacheKey("model-a", "purpose-x", msgs("system", "sys", "user", "body", "user", "")),
	}
	for name, other := range cases {
		if other == key {
			t.Errorf("%s should change the cache key but did not", name)
		}
	}
}

func TestResponseCacheKey_NULDelimiterIsCollisionSafe(t *testing.T) {
	// Without a delimiter, concatenating ("ab","c") and ("a","bc") would
	// fingerprint identically. The NUL separator must keep these distinct
	// across BOTH the (role,content) join and the model/purpose join.
	roleSplit := responseCacheKey("m", "p", msgs("ab", "c"))
	roleSplit2 := responseCacheKey("m", "p", msgs("a", "bc"))
	if roleSplit == roleSplit2 {
		t.Error("role/content boundary not collision-safe (missing delimiter?)")
	}
	modelSplit := responseCacheKey("mp", "", msgs("user", "x"))
	modelSplit2 := responseCacheKey("m", "p", msgs("user", "x"))
	if modelSplit == modelSplit2 {
		t.Error("model/purpose boundary not collision-safe (missing delimiter?)")
	}
}

func TestResponseCacheKey_EmptyMessagesStillStable(t *testing.T) {
	a := responseCacheKey("m", "p", nil)
	b := responseCacheKey("m", "p", []chat.Message{})
	if a != b {
		t.Fatalf("nil and empty message slices must hash identically")
	}
	if len(a) != 64 {
		t.Fatalf("expected 64-char hash, got %q", a)
	}
}

// ---------------------------------------------------------------------------
// validateCandidates — the last gate before the resolver. Exercised directly
// (not through the LLM-mocked Extract path) so the span/name/type rules are
// asserted in isolation.
// ---------------------------------------------------------------------------

func TestValidateCandidates_NilAndEmptyInputs(t *testing.T) {
	if got := validateCandidates(nil, "chunk"); got != nil {
		t.Errorf("nil input must return nil, got %v", got)
	}
	if got := validateCandidates([]Candidate{}, "chunk"); got != nil {
		t.Errorf("empty input must return nil, got %v", got)
	}
}

func TestValidateCandidates_TrimsNameAndDropsWhitespaceOnly(t *testing.T) {
	content := "Vadim met here"
	in := []Candidate{
		{Type: persistence.EntityTypePerson, Name: "  Vadim  "},
		{Type: persistence.EntityTypePerson, Name: "   "}, // whitespace-only → drop
	}
	out := validateCandidates(in, content)
	if len(out) != 1 {
		t.Fatalf("expected 1 surviving candidate, got %d", len(out))
	}
	if out[0].Name != "Vadim" {
		t.Errorf("name must be trimmed, got %q", out[0].Name)
	}
}

func TestValidateCandidates_DropsUnknownTypeKeepsKnown(t *testing.T) {
	content := "x"
	in := []Candidate{
		{Type: "ANIMAL", Name: "cat"},                        // unknown → drop
		{Type: persistence.EntityTypeTechnology, Name: "Go"}, // known → keep
	}
	out := validateCandidates(in, content)
	if len(out) != 1 || out[0].Name != "Go" {
		t.Fatalf("only the known-type candidate should survive, got %+v", out)
	}
}

func TestValidateCandidates_DoesNotDedupRepeatedSpans(t *testing.T) {
	// validateCandidates is a per-candidate filter, NOT a dedup pass: the
	// resolver is what merges identical surfaces. Two identical in-chunk
	// candidates must both survive so the orchestrator records both mentions.
	content := "PostgreSQL and PostgreSQL again"
	in := []Candidate{
		{Type: persistence.EntityTypeTechnology, Name: "PostgreSQL", CharStart: 0, CharEnd: 10},
		{Type: persistence.EntityTypeTechnology, Name: "PostgreSQL", CharStart: 15, CharEnd: 25},
	}
	out := validateCandidates(in, content)
	if len(out) != 2 {
		t.Fatalf("same-chunk repeats must NOT be deduped here, got %d", len(out))
	}
}

func TestValidateCandidates_ClampsSurfaceToActualSubstring(t *testing.T) {
	content := "Vadim chose PostgreSQL"
	in := []Candidate{
		// Surface disagrees with the actual span text → clamp to substring.
		{Type: persistence.EntityTypeTechnology, Name: "PostgreSQL", CharStart: 12, CharEnd: 22, Surface: "MySQL"},
	}
	out := validateCandidates(in, content)
	if len(out) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(out))
	}
	if out[0].Surface != "PostgreSQL" {
		t.Errorf("surface must clamp to chunk[start:end], got %q", out[0].Surface)
	}
}

func TestValidateCandidates_OutOfRangeSpanResetButEntityKept(t *testing.T) {
	content := "short"
	in := []Candidate{
		{Type: persistence.EntityTypeFact, Name: "fact", CharStart: 3, CharEnd: 99, Surface: "junk"},
		{Type: persistence.EntityTypeFact, Name: "neg", CharStart: -1, CharEnd: 2, Surface: "junk"},
		{Type: persistence.EntityTypeFact, Name: "rev", CharStart: 4, CharEnd: 2, Surface: "junk"}, // end<start
	}
	out := validateCandidates(in, content)
	if len(out) != 3 {
		t.Fatalf("entities with bad spans are kept (span reset), got %d", len(out))
	}
	for _, c := range out {
		if c.CharStart != 0 || c.CharEnd != 0 || c.Surface != "" {
			t.Errorf("out-of-range span must reset to 0/0/\"\", got %+v", c)
		}
	}
}

// ---------------------------------------------------------------------------
// isKnownEntityType — closed vocabulary. Assert the FULL accepted set plus the
// case-sensitivity contract (the prompt emits upper-case; lower-case is a
// model error and must be rejected, not silently coerced).
// ---------------------------------------------------------------------------

func TestIsKnownEntityType_AcceptsEntireClosedSet(t *testing.T) {
	known := []string{
		persistence.EntityTypePerson, persistence.EntityTypeVendor,
		persistence.EntityTypeProduct, persistence.EntityTypeDecision,
		persistence.EntityTypeEvent, persistence.EntityTypeDate,
		persistence.EntityTypePrice, persistence.EntityTypeLocation,
		persistence.EntityTypeTechnology, persistence.EntityTypeFact,
		persistence.EntityTypeOther,
	}
	for _, k := range known {
		if !isKnownEntityType(k) {
			t.Errorf("%q must be a known entity type", k)
		}
	}
}

func TestIsKnownEntityType_RejectsUnknownAndWrongCase(t *testing.T) {
	for _, bad := range []string{"", "person", "Person", "PERSON ", "ANIMAL", "ORG"} {
		if isKnownEntityType(bad) {
			t.Errorf("%q must be rejected (case-sensitive, closed set)", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// validateProposals — edge integrity at the relationship stage. Exercised
// directly to pin drop-reason precedence, predicate case-folding, and the
// cross-direction collapse of symmetric predicates.
// ---------------------------------------------------------------------------

func ents(ids ...string) []ResolvedEntity {
	out := make([]ResolvedEntity, len(ids))
	for i, id := range ids {
		out[i] = ResolvedEntity{ID: id, Type: persistence.EntityTypeOther, CanonicalName: id}
	}
	return out
}

func TestValidateProposals_NilInputReturnsNonNilReasonMap(t *testing.T) {
	kept, dropped, by := validateProposals(nil, ents("a", "b"), "chunk")
	if kept != nil || dropped != 0 {
		t.Fatalf("nil input: want (nil,0), got (%v,%d)", kept, dropped)
	}
	if by == nil {
		t.Error("byReason map must be non-nil even on empty input")
	}
}

func TestValidateProposals_SelfLoopGuardBeatsUnknownEndpoint(t *testing.T) {
	// from == to short-circuits as a self-loop BEFORE the known-endpoint
	// check runs, even when the id is absent from the entity set.
	in := []EdgeProposal{{From: "ghost", To: "ghost", Predicate: persistence.PredicateRelatesTo, Evidence: "x"}}
	_, dropped, by := validateProposals(in, ents("a", "b"), "ghost evidence x")
	if dropped != 1 || by[DropReasonSelfLoop] != 1 {
		t.Fatalf("expected 1 self_loop drop, got dropped=%d by=%v", dropped, by)
	}
	if by[DropReasonUnknownFrom] != 0 {
		t.Error("self-loop must be detected before the unknown-endpoint check")
	}
}

func TestValidateProposals_EmptyEndpointBeatsSelfLoop(t *testing.T) {
	// Two empty endpoints are technically equal, but the empty check is
	// evaluated first — assert the precedence is empty_endpoint, not self_loop.
	in := []EdgeProposal{{From: "  ", To: "", Predicate: persistence.PredicateRelatesTo, Evidence: "x"}}
	_, dropped, by := validateProposals(in, ents("a"), "x")
	if dropped != 1 || by[DropReasonEmptyEndpoint] != 1 {
		t.Fatalf("expected empty_endpoint precedence, got dropped=%d by=%v", dropped, by)
	}
}

func TestValidateProposals_PredicateCaseAndWhitespaceFolded(t *testing.T) {
	// A lower-case, padded predicate must be upper-cased + trimmed and then
	// accepted against the closed catalog — the model frequently lower-cases.
	in := []EdgeProposal{{From: "a", To: "b", Predicate: "  relates_to  ", Evidence: "ev"}}
	kept, dropped, _ := validateProposals(in, ents("a", "b"), "some ev here")
	if dropped != 0 || len(kept) != 1 {
		t.Fatalf("folded predicate must be accepted, got kept=%d dropped=%d", len(kept), dropped)
	}
	if kept[0].Predicate != persistence.PredicateRelatesTo {
		t.Errorf("predicate must be normalised to upper, got %q", kept[0].Predicate)
	}
}

func TestValidateProposals_SymmetricCollapsesAcrossDirection(t *testing.T) {
	// (b RELATES_TO a) and (a RELATES_TO b) are the same symmetric edge.
	// The first is reordered to a|b; the second is then a duplicate triple.
	in := []EdgeProposal{
		{From: "b", To: "a", Predicate: persistence.PredicateRelatesTo, Evidence: "ev"},
		{From: "a", To: "b", Predicate: persistence.PredicateRelatesTo, Evidence: "ev"},
	}
	kept, dropped, by := validateProposals(in, ents("a", "b"), "ev present")
	if len(kept) != 1 {
		t.Fatalf("symmetric pair must collapse to one edge, got %d", len(kept))
	}
	if kept[0].From != "a" || kept[0].To != "b" {
		t.Errorf("symmetric edge must be lexicographically ordered, got %s->%s", kept[0].From, kept[0].To)
	}
	if dropped != 1 || by[DropReasonDuplicateTriple] != 1 {
		t.Errorf("second direction must drop as duplicate_triple, got dropped=%d by=%v", dropped, by)
	}
}

func TestValidateProposals_DirectionalPredicateNotCollapsed(t *testing.T) {
	// OWNED_BY is directional: (a,b) and (b,a) are two distinct edges and
	// must both survive (no symmetric reordering, no dedup).
	in := []EdgeProposal{
		{From: "a", To: "b", Predicate: persistence.PredicateOwnedBy, Evidence: "ev"},
		{From: "b", To: "a", Predicate: persistence.PredicateOwnedBy, Evidence: "ev"},
	}
	kept, dropped, _ := validateProposals(in, ents("a", "b"), "ev here")
	if len(kept) != 2 || dropped != 0 {
		t.Fatalf("directional edges must both survive, got kept=%d dropped=%d", len(kept), dropped)
	}
}

// ---------------------------------------------------------------------------
// normaliseForMatch — the cosmetic-difference canonicaliser behind
// evidenceInChunk. Assert each declared mapping produces the SAME normalised
// output for the variant and its ASCII spelling (the property the fallback
// substring match relies on).
// ---------------------------------------------------------------------------

func TestNormaliseForMatch_VariantsCanonicaliseToASCII(t *testing.T) {
	cases := []struct {
		name, variant, ascii string
	}{
		{"curly double quotes", "“quoted”", `"quoted"`},
		{"curly single quotes", "‘it’s’", `'it's'`},
		{"low-9 + prime quotes", "„X″", `"X"`},
		{"em and en dash", "a—b–c", "a-b-c"},
		{"minus sign", "5−3", "5-3"},
		{"ellipsis expands", "wait…", "wait..."},
		{"collapsed whitespace", "two   spaces\tand\ntabs", "two spaces and tabs"},
		{"leading/trailing trimmed", "   padded   ", "padded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normaliseForMatch(tc.variant)
			if got != tc.ascii {
				t.Errorf("normalise(%q) = %q, want %q", tc.variant, got, tc.ascii)
			}
			// The ASCII spelling must be a fixed point of the mapping.
			if again := normaliseForMatch(tc.ascii); again != tc.ascii {
				t.Errorf("ASCII form not stable: normalise(%q) = %q", tc.ascii, again)
			}
		})
	}
}

func TestNormaliseForMatch_NFCComposesDiacritics(t *testing.T) {
	// Built from explicit code points so the source bytes are
	// unambiguous regardless of editor Unicode normalisation.
	decomposed := "Caf\u0065\u0301" // e + combining acute U+0301 (NFD)
	precomposed := "Caf\u00e9"      // \u00e9 precomposed (NFC)
	if decomposed == precomposed {
		t.Fatal("test setup: the two spellings must differ before normalisation")
	}
	if normaliseForMatch(decomposed) != normaliseForMatch(precomposed) {
		t.Error("NFD and NFC spellings of the same word must normalise equal")
	}
}

func TestNormaliseForMatch_EmptyStringPassthrough(t *testing.T) {
	if got := normaliseForMatch(""); got != "" {
		t.Errorf("empty input must return empty, got %q", got)
	}
}
