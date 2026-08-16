package datasubject

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Increment 4 of the D-2 rights surface: the KG binder, RESOLVE ON DEMAND.
//
// The controller ruled (2026-07-29) that the ~706 KG-derived PERSON entities
// are NOT materialised as data-subject rows. Doing so would build a permanent
// register of identified people out of LLM extraction output — increasing the
// personal data held in order to make deletion possible, which is the
// concentration risk the design flags and the pending DPIA must assess. So a
// person's entity is resolved at the moment a request actually names them.
//
// The confidence ceiling is `possible` and the design says why: the KG binder
// has false positives (a company named after a person) and false negatives (a
// nickname the extractor never linked). Everything here therefore PROPOSES and
// the operator disposes — nothing is linked without a named entity id.

// --- fakes -------------------------------------------------------------

type fakeKGIndex struct {
	entities map[string][]KGEntity // projectID -> entities
	mentions map[string][]string   // entityID -> chunk ids
	err      error
}

func (f *fakeKGIndex) FindPersonEntities(_ context.Context, projectID, name string, _ int) ([]KGEntity, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []KGEntity
	for _, e := range f.entities[projectID] {
		hay := append([]string{e.CanonicalName}, e.Aliases...)
		for _, h := range hay {
			if strings.Contains(strings.ToLower(h), strings.ToLower(name)) {
				out = append(out, e)
				break
			}
		}
	}
	return out, nil
}

func (f *fakeKGIndex) MentionChunks(_ context.Context, entityID string, limit int) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	ids := f.mentions[entityID]
	if limit > 0 && len(ids) > limit {
		return ids[:limit], nil
	}
	return ids, nil
}

func (f *fakeKGIndex) Projects(_ context.Context) ([]string, error) {
	out := make([]string, 0, len(f.entities))
	for p := range f.entities {
		out = append(out, p)
	}
	return out, nil
}

type fakeSubjectStore struct {
	subjects    map[string]Subject
	identifiers map[string][]Identifier
	links       map[string][]Link
	deleted     []string
	reassigned  []string // "from->to"
	addLinkErr  error
}

func newFakeStore() *fakeSubjectStore {
	return &fakeSubjectStore{
		subjects:    map[string]Subject{},
		identifiers: map[string][]Identifier{},
		links:       map[string][]Link{},
	}
}

func (f *fakeSubjectStore) FindSubjectByIdentifier(_ context.Context, kind, value string) (string, error) {
	for sid, ids := range f.identifiers {
		for _, id := range ids {
			if id.Kind == kind && id.Value == value {
				return sid, nil
			}
		}
	}
	return "", nil
}

func (f *fakeSubjectStore) GetSubject(_ context.Context, id string) (Subject, error) {
	s, ok := f.subjects[id]
	if !ok {
		return Subject{}, errors.New("no such subject")
	}
	return s, nil
}

func (f *fakeSubjectStore) ListIdentifiers(_ context.Context, subjectID string) ([]Identifier, error) {
	return f.identifiers[subjectID], nil
}

func (f *fakeSubjectStore) ListLinks(_ context.Context, subjectID string) ([]Link, error) {
	return f.links[subjectID], nil
}

func (f *fakeSubjectStore) AddIdentifier(_ context.Context, subjectID string, id Identifier) error {
	for _, existing := range f.identifiers[subjectID] {
		if existing.Kind == id.Kind && existing.Value == id.Value {
			return nil
		}
	}
	f.identifiers[subjectID] = append(f.identifiers[subjectID], id)
	return nil
}

func (f *fakeSubjectStore) AddLink(_ context.Context, subjectID string, l Link) error {
	if f.addLinkErr != nil {
		return f.addLinkErr
	}
	for _, existing := range f.links[subjectID] {
		if existing.Table == l.Table && existing.RowID == l.RowID {
			return nil
		}
	}
	f.links[subjectID] = append(f.links[subjectID], l)
	return nil
}

func (f *fakeSubjectStore) ReassignLinks(_ context.Context, from, to string) (int, error) {
	moved := f.links[from]
	f.links[to] = append(f.links[to], moved...)
	delete(f.links, from)
	f.reassigned = append(f.reassigned, from+"->"+to)
	return len(moved), nil
}

func (f *fakeSubjectStore) DeleteSubject(_ context.Context, id string) error {
	delete(f.subjects, id)
	delete(f.identifiers, id)
	delete(f.links, id)
	f.deleted = append(f.deleted, id)
	return nil
}

// realSubject/placeholderSubject build the two shapes the collision rules turn on.
func (f *fakeSubjectStore) realSubject(id, name string) {
	f.subjects[id] = Subject{ID: id, DisplayName: name}
}

func (f *fakeSubjectStore) placeholderFor(entityID string) {
	const id = "ds_ph"
	f.subjects[id] = Subject{ID: id, DisplayName: PlaceholderSubjectName(entityID)}
	f.identifiers[id] = []Identifier{{
		Kind: KindKGEntity, Value: entityID,
		Source: SourceKGExtraction, Confidence: ConfidencePossible,
	}}
}

func testResolver(store *fakeSubjectStore, idx *fakeKGIndex) *KGResolver {
	return &KGResolver{Store: store, Index: idx, MaxMentions: 100}
}

// --- candidates --------------------------------------------------------

func TestKGResolver_CandidatesMatchNameAndAliases(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	idx := &fakeKGIndex{
		entities: map[string][]KGEntity{
			"janka": {
				{ID: "ent_7f3", ProjectID: "janka", CanonicalName: "Jane Doe"},
				{ID: "ent_2b8", ProjectID: "janka", CanonicalName: "Doe Industries"},
				{ID: "ent_555", ProjectID: "janka", CanonicalName: "Someone Else"},
			},
			"headmatch": {
				{ID: "ent_9a1", ProjectID: "headmatch", CanonicalName: "J. Doe", Aliases: []string{"Jane Doe"}},
			},
		},
		mentions: map[string][]string{
			"ent_7f3": {"chunk_a", "chunk_b"},
			"ent_2b8": {"chunk_c"},
			"ent_9a1": {"chunk_d"},
		},
	}
	got, err := testResolver(store, idx).Candidates(context.Background(), "ds_1", nil, nil)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	byID := map[string]KGCandidate{}
	for _, c := range got {
		byID[c.Entity.ID] = c
	}
	if _, ok := byID["ent_555"]; ok {
		t.Error("an entity matching no name of the subject was proposed")
	}
	if _, ok := byID["ent_2b8"]; ok {
		t.Error("\"Doe Industries\" does not contain the subject's name and must not be proposed for it")
	}
	if c := byID["ent_7f3"]; c.MentionCount != 2 {
		t.Errorf("mention count = %d, want 2", c.MentionCount)
	}
	if c := byID["ent_9a1"]; c.MatchedOn != "Jane Doe" {
		t.Errorf("alias match recorded as %q, want the matched name", c.MatchedOn)
	}
	for _, c := range got {
		if c.State != KGUnbound {
			t.Errorf("candidate %s state = %q, want unbound", c.Entity.ID, c.State)
		}
	}
}

// The binder's KNOWN false positive, from the design: an entity that carries a
// person's name but is not that person. A broad operator-supplied name surfaces
// it, and the resolver must NOT filter it out on a heuristic — hiding it would
// hide exactly the judgement this step exists to ask the operator for, and a
// heuristic good enough to drop a company is good enough to drop a real person.
func TestKGResolver_CandidatesDoNotFilterLikelyFalsePositives(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	idx := &fakeKGIndex{entities: map[string][]KGEntity{"janka": {
		{ID: "ent_7f3", ProjectID: "janka", CanonicalName: "Jane Doe"},
		{ID: "ent_2b8", ProjectID: "janka", CanonicalName: "Doe Industries"},
	}}}
	got, err := testResolver(store, idx).Candidates(context.Background(), "ds_1", []string{"Doe"}, nil)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	byID := map[string]KGCandidate{}
	for _, c := range got {
		byID[c.Entity.ID] = c
	}
	if _, ok := byID["ent_2b8"]; !ok {
		t.Error("a broad name search must surface \"Doe Industries\" for the operator to reject, " +
			"not filter it silently")
	}
	if c := byID["ent_2b8"]; c.MatchedOn != "Doe" {
		t.Errorf("MatchedOn = %q; the operator needs to see WHICH name matched to judge it", c.MatchedOn)
	}
}

func TestKGResolver_CandidatesUseExtraNames(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	idx := &fakeKGIndex{
		entities: map[string][]KGEntity{"janka": {
			{ID: "ent_nick", ProjectID: "janka", CanonicalName: "Janey"},
		}},
	}
	// The design's §5.1 false negative: a nickname the extractor never linked
	// to the canonical name. The operator supplies it; that is the backstop.
	got, err := testResolver(store, idx).Candidates(context.Background(), "ds_1", []string{"Janey"}, nil)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 || got[0].Entity.ID != "ent_nick" {
		t.Fatalf("operator-supplied name did not reach the search: %+v", got)
	}
}

func TestKGResolver_CandidatesClassifyBindingState(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	store.realSubject("ds_other", "Jane D.")
	store.identifiers["ds_other"] = []Identifier{{
		Kind: KindKGEntity, Value: "ent_conflict",
		Source: SourceKGExtraction, Confidence: ConfidencePossible,
	}}
	store.placeholderFor("ent_placeholder")
	store.identifiers["ds_1"] = []Identifier{{
		Kind: KindKGEntity, Value: "ent_mine",
		Source: SourceKGExtraction, Confidence: ConfidencePossible,
	}}
	idx := &fakeKGIndex{entities: map[string][]KGEntity{"janka": {
		{ID: "ent_free", ProjectID: "janka", CanonicalName: "Jane Doe"},
		{ID: "ent_mine", ProjectID: "janka", CanonicalName: "Jane Doe (work)"},
		{ID: "ent_placeholder", ProjectID: "janka", CanonicalName: "Jane Doe (chat)"},
		{ID: "ent_conflict", ProjectID: "janka", CanonicalName: "Jane Doe (other)"},
	}}}
	got, err := testResolver(store, idx).Candidates(context.Background(), "ds_1", []string{"Jane Doe"}, nil)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	want := map[string]KGBindingState{
		"ent_free":        KGUnbound,
		"ent_mine":        KGBoundHere,
		"ent_placeholder": KGBoundToPlaceholder,
		"ent_conflict":    KGConflict,
	}
	for _, c := range got {
		if w := want[c.Entity.ID]; c.State != w {
			t.Errorf("%s state = %q, want %q", c.Entity.ID, c.State, w)
		}
	}
	for _, c := range got {
		if c.State == KGConflict && c.BoundSubjectName != "Jane D." {
			t.Errorf("a conflict must name the other subject, got %q", c.BoundSubjectName)
		}
	}
}

// A subject with the same display name in two projects must not have its
// candidates deduplicated across them: they are different entities and the
// operator may want one and not the other.
func TestKGResolver_CandidatesSpanProjectsAndCanBeNarrowed(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	idx := &fakeKGIndex{entities: map[string][]KGEntity{
		"janka":     {{ID: "ent_a", ProjectID: "janka", CanonicalName: "Jane Doe"}},
		"headmatch": {{ID: "ent_b", ProjectID: "headmatch", CanonicalName: "Jane Doe"}},
	}}
	all, err := testResolver(store, idx).Candidates(context.Background(), "ds_1", nil, nil)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want a candidate per project, got %d", len(all))
	}
	one, err := testResolver(store, idx).Candidates(context.Background(), "ds_1", nil, []string{"janka"})
	if err != nil {
		t.Fatalf("Candidates(project): %v", err)
	}
	if len(one) != 1 || one[0].Entity.ProjectID != "janka" {
		t.Errorf("--project did not narrow the search: %+v", one)
	}
}

// --- binding -----------------------------------------------------------

func TestKGResolver_BindUnboundEntityLinksEveryMention(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	idx := &fakeKGIndex{
		entities: map[string][]KGEntity{"janka": {{ID: "ent_7f3", ProjectID: "janka", CanonicalName: "Jane Doe"}}},
		mentions: map[string][]string{"ent_7f3": {"chunk_a", "chunk_b", "chunk_c"}},
	}
	res, err := testResolver(store, idx).Bind(context.Background(), KGBindRequest{SubjectID: "ds_1", EntityID: "ent_7f3"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	// Three chunks plus the entity row itself.
	if res.LinksAdded != 4 {
		t.Errorf("LinksAdded = %d, want 4 (3 chunks + the entity row)", res.LinksAdded)
	}
	var links []Link
	for _, l := range store.links["ds_1"] {
		if l.Table == TableProjectMemoryChunks {
			links = append(links, l)
		}
	}
	if len(links) != 3 {
		t.Fatalf("want 3 chunk links, got %d", len(links))
	}
	for _, l := range links {
		if l.Confidence != ConfidencePossible {
			t.Errorf("confidence %q — a KG extraction may never claim more than possible", l.Confidence)
		}
		if l.Source != SourceKGExtraction {
			t.Errorf("source %q, want kg_extraction", l.Source)
		}
		// A chunk naming this person routinely concerns others too. Asserting
		// exclusivity would authorise deleting their data on this erasure.
		if l.Exclusivity != SharedRow {
			t.Errorf("exclusivity %q, want shared", l.Exclusivity)
		}
		if l.ProjectID != "janka" {
			t.Errorf("project %q, want janka", l.ProjectID)
		}
	}
	// The identifier makes the entity discoverable from the subject, so a
	// later resolve reuses this binding instead of proposing it again.
	ids := store.identifiers["ds_1"]
	if len(ids) != 1 || ids[0].Kind != KindKGEntity || ids[0].Value != "ent_7f3" {
		t.Errorf("kg_entity identifier not recorded: %+v", ids)
	}
}

// The entity ROW is itself personal data — the closed table set says so in as
// many words ("a PERSON entity IS data about that person"), and it holds the
// subject's canonical name and every alias the extractor learned. Linking only
// the chunks would leave an Art 17 erasure reporting success while a row
// literally named after the subject survives.
func TestKGResolver_BindLinksTheEntityRowItself(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	idx := &fakeKGIndex{
		entities: map[string][]KGEntity{"janka": {{ID: "ent_7f3", ProjectID: "janka", CanonicalName: "Jane Doe"}}},
		mentions: map[string][]string{"ent_7f3": {"chunk_a"}},
	}
	if _, err := testResolver(store, idx).Bind(context.Background(),
		KGBindRequest{SubjectID: "ds_1", EntityID: "ent_7f3"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	var entityLink *Link
	for i, l := range store.links["ds_1"] {
		if l.Table == TableKnowledgeEntities {
			entityLink = &store.links["ds_1"][i]
		}
	}
	if entityLink == nil {
		t.Fatal("no link to knowledge_entities: the subject's own entity row would survive an erasure")
	}
	if entityLink.RowID != "ent_7f3" || entityLink.ProjectID != "janka" {
		t.Errorf("entity link points at %s/%s", entityLink.ProjectID, entityLink.RowID)
	}
	// EXCLUSIVE, unlike the chunk links: a PERSON entity is about exactly one
	// person, so it is deleted in full rather than redacted — you cannot redact
	// someone out of their own entity row.
	if entityLink.Exclusivity != ExclusiveRow {
		t.Errorf("entity link exclusivity = %q, want exclusive", entityLink.Exclusivity)
	}
	if entityLink.Confidence != ConfidencePossible || entityLink.Source != SourceKGExtraction {
		t.Errorf("entity link claims %s/%s; the KG ceiling applies to it too",
			entityLink.Source, entityLink.Confidence)
	}
}

func TestKGResolver_BindIsIdempotent(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	idx := &fakeKGIndex{
		entities: map[string][]KGEntity{"janka": {{ID: "ent_7f3", ProjectID: "janka", CanonicalName: "Jane Doe"}}},
		mentions: map[string][]string{"ent_7f3": {"chunk_a", "chunk_b"}},
	}
	r := testResolver(store, idx)
	if _, err := r.Bind(context.Background(), KGBindRequest{SubjectID: "ds_1", EntityID: "ent_7f3"}); err != nil {
		t.Fatalf("first Bind: %v", err)
	}
	// A second run picks up chunks ingested since the first.
	idx.mentions["ent_7f3"] = append(idx.mentions["ent_7f3"], "chunk_c")
	res, err := r.Bind(context.Background(), KGBindRequest{SubjectID: "ds_1", EntityID: "ent_7f3"})
	if err != nil {
		t.Fatalf("second Bind: %v", err)
	}
	if len(store.links["ds_1"]) != 4 {
		t.Errorf("re-binding duplicated links: %d (want 3 chunks + the entity row)",
			len(store.links["ds_1"]))
	}
	if res.State != KGBoundHere {
		t.Errorf("second bind state = %q, want bound_here", res.State)
	}
}

func TestKGResolver_BindRefusesAnEntityOwnedByAnotherRealSubject(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	store.realSubject("ds_other", "Jane D.")
	store.identifiers["ds_other"] = []Identifier{{
		Kind: KindKGEntity, Value: "ent_x", Source: SourceKGExtraction, Confidence: ConfidencePossible,
	}}
	idx := &fakeKGIndex{
		entities: map[string][]KGEntity{"janka": {{ID: "ent_x", ProjectID: "janka", CanonicalName: "Jane Doe"}}},
		mentions: map[string][]string{"ent_x": {"chunk_a"}},
	}
	_, err := testResolver(store, idx).Bind(context.Background(), KGBindRequest{SubjectID: "ds_1", EntityID: "ent_x", Adopt: true})
	if err == nil {
		t.Fatal("binding an entity owned by another identified person must be refused")
	}
	// --adopt must not override this: two identified people claiming one
	// entity is a human question, and guessing merges two people's data.
	for _, want := range []string{"ds_other", "Jane D."} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name the other subject (%s)", err.Error(), want)
		}
	}
	if len(store.links["ds_1"]) != 0 {
		t.Error("a refused bind wrote links anyway")
	}
}

func TestKGResolver_BindPlaceholderRequiresAdopt(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	store.placeholderFor("ent_7f3")
	store.links["ds_ph"] = []Link{
		{Table: TableProjectMemoryChunks, RowID: "chunk_old", ProjectID: "janka",
			Source: SourceKGExtraction, Confidence: ConfidencePossible, Exclusivity: SharedRow},
	}
	idx := &fakeKGIndex{
		entities: map[string][]KGEntity{"janka": {{ID: "ent_7f3", ProjectID: "janka", CanonicalName: "Jane Doe"}}},
		mentions: map[string][]string{"ent_7f3": {"chunk_new"}},
	}
	r := testResolver(store, idx)
	_, err := r.Bind(context.Background(), KGBindRequest{SubjectID: "ds_1", EntityID: "ent_7f3"})
	if err == nil {
		t.Fatal("adopting a placeholder without --adopt must be refused")
	}
	if !strings.Contains(err.Error(), "--adopt") {
		t.Errorf("refusal %q does not tell the operator how to proceed", err.Error())
	}
	if len(store.deleted) != 0 {
		t.Error("a refused bind deleted a subject")
	}
}

func TestKGResolver_BindAdoptsPlaceholder(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	store.placeholderFor("ent_7f3")
	store.links["ds_ph"] = []Link{
		{Table: TableProjectMemoryChunks, RowID: "chunk_old", ProjectID: "janka",
			Source: SourceKGExtraction, Confidence: ConfidencePossible, Exclusivity: SharedRow},
	}
	idx := &fakeKGIndex{
		entities: map[string][]KGEntity{"janka": {{ID: "ent_7f3", ProjectID: "janka", CanonicalName: "Jane Doe"}}},
		mentions: map[string][]string{"ent_7f3": {"chunk_old", "chunk_new"}},
	}
	res, err := testResolver(store, idx).Bind(context.Background(), KGBindRequest{SubjectID: "ds_1", EntityID: "ent_7f3", Adopt: true})
	if err != nil {
		t.Fatalf("Bind --adopt: %v", err)
	}
	if res.AdoptedFrom != "ds_ph" {
		t.Errorf("AdoptedFrom = %q, want ds_ph", res.AdoptedFrom)
	}
	if res.LinksMoved != 1 {
		t.Errorf("LinksMoved = %d, want 1", res.LinksMoved)
	}
	// The placeholder held the chunk written by the chat path; the real
	// subject must end up with it, or an Art 15 export for this person
	// silently misses everything the chat path recorded.
	rows := map[string]bool{}
	for _, l := range store.links["ds_1"] {
		rows[l.RowID] = true
	}
	if !rows["chunk_old"] || !rows["chunk_new"] {
		t.Errorf("adopted subject is missing rows: %+v", store.links["ds_1"])
	}
	if _, still := store.subjects["ds_ph"]; still {
		t.Error("the placeholder subject survived adoption — one person, two subject rows")
	}
	if len(store.deleted) != 1 || store.deleted[0] != "ds_ph" {
		t.Errorf("deleted = %v, want [ds_ph]", store.deleted)
	}
}

// A subject named kg:<id> that has acquired OTHER identifiers is no longer a
// placeholder — someone has been working on it. Deleting it would destroy that
// work, so it is treated as a real subject and refused.
func TestKGResolver_BindRefusesAPlaceholderThatGrewIdentifiers(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	store.placeholderFor("ent_7f3")
	store.identifiers["ds_ph"] = append(store.identifiers["ds_ph"], Identifier{
		Kind: "email", Value: "jane@example.com",
		Source: SourceOperatorAsserted, Confidence: ConfidenceCertain,
	})
	idx := &fakeKGIndex{
		entities: map[string][]KGEntity{"janka": {{ID: "ent_7f3", ProjectID: "janka", CanonicalName: "Jane Doe"}}},
		mentions: map[string][]string{"ent_7f3": {"chunk_a"}},
	}
	_, err := testResolver(store, idx).Bind(context.Background(), KGBindRequest{SubjectID: "ds_1", EntityID: "ent_7f3", Adopt: true})
	if err == nil {
		t.Fatal("a kg: subject carrying other identifiers must not be silently deleted")
	}
	if len(store.deleted) != 0 {
		t.Errorf("deleted %v despite the refusal", store.deleted)
	}
}

func TestKGResolver_BindRefusesAnEntityTheSearchNeverProposed(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	idx := &fakeKGIndex{entities: map[string][]KGEntity{"janka": {
		{ID: "ent_7f3", ProjectID: "janka", CanonicalName: "Jane Doe"},
	}}}
	_, err := testResolver(store, idx).Bind(context.Background(), KGBindRequest{SubjectID: "ds_1", EntityID: "ent_typo"})
	if err == nil {
		t.Fatal("binding an unknown entity id must fail rather than create a dangling identifier")
	}
}

// Regression: the old ceiling always fetched the same first page. Re-running
// could therefore never reach mention N+1, despite the CLI promising it would.
func TestKGResolver_BindCoversEveryMentionBeyondTheOldCeiling(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	many := make([]string, 5)
	for i := range many {
		many[i] = string(rune('a'+i)) + "_chunk"
	}
	idx := &fakeKGIndex{
		entities: map[string][]KGEntity{"janka": {{ID: "ent_7f3", ProjectID: "janka", CanonicalName: "Jane Doe"}}},
		mentions: map[string][]string{"ent_7f3": many},
	}
	r := &KGResolver{Store: store, Index: idx, MaxMentions: 3}
	res, err := r.Bind(context.Background(), KGBindRequest{SubjectID: "ds_1", EntityID: "ent_7f3"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if res.MentionsTruncated {
		t.Error("a complete paged/all-row bind must not report permanent truncation")
	}
	if res.LinksAdded != 6 {
		t.Errorf("LinksAdded = %d, want all 5 chunks plus the entity row", res.LinksAdded)
	}
}

// A partial bind must not look like a clean one: the error carries how far it
// got, so the operator knows rows were linked before it stopped.
func TestKGResolver_BindSurfacesAPartialFailure(t *testing.T) {
	store := newFakeStore()
	store.realSubject("ds_1", "Jane Doe")
	idx := &fakeKGIndex{
		entities: map[string][]KGEntity{"janka": {{ID: "ent_7f3", ProjectID: "janka", CanonicalName: "Jane Doe"}}},
		mentions: map[string][]string{"ent_7f3": {"chunk_a"}},
	}
	store.addLinkErr = errors.New("db down")
	_, err := testResolver(store, idx).Bind(context.Background(), KGBindRequest{SubjectID: "ds_1", EntityID: "ent_7f3"})
	if err == nil {
		t.Fatal("a failing AddLink must surface")
	}
}

func TestPlaceholderSubjectName(t *testing.T) {
	if got := PlaceholderSubjectName("ent_7f3"); got != "kg:ent_7f3" {
		t.Errorf("PlaceholderSubjectName = %q, want kg:ent_7f3", got)
	}
	// The chat memory-write path (dispatcher tool_remember) mints exactly this
	// shape. If the two ever diverge, adoption stops recognising placeholders
	// and every chat-created subject becomes an un-mergeable conflict.
	if !IsPlaceholderSubject(Subject{DisplayName: "kg:ent_7f3"}, "ent_7f3") {
		t.Error("the chat path's subject name is not recognised as a placeholder")
	}
	if IsPlaceholderSubject(Subject{DisplayName: "Jane Doe"}, "ent_7f3") {
		t.Error("a named subject must never be treated as a placeholder")
	}
}
