package datasubject

// Increment 4 of the D-2 rights surface: the knowledge-graph binder, the third
// and widest of the three (design §4.2), RESOLVED ON DEMAND.
//
// THE CONTROLLER DECISION THIS IMPLEMENTS (2026-07-29). The alternative was to
// materialise the deployment's ~706 KG-derived PERSON entities as data-subject
// rows, so every named third party had a standing subject to erase. That was
// rejected: it would build a permanent register of identified people out of LLM
// extraction output — increasing the personal data held in order to make
// deletion possible. That is the concentration risk the design itself flags and
// what the pending DPIA (D-3b, Art 35) has to assess rather than assume away. A
// proportionality judgement under Art 25, not an engineering pick.
//
// So nothing here scans, indexes or stores anything until a request actually
// names someone. Resolution runs against a subject that already exists because
// a real person asked, and it ends with links the erasure and export executors
// already know how to act on.
//
// PROPOSE, NEVER PRESUME. The source ceiling is `possible` (datasubject.go:80)
// and the design is explicit about why: this binder has false positives (a
// company named after a person — "Doe Industries" for Jane Doe) and false
// negatives (a nickname the extractor never linked). Candidates() therefore
// proposes and the operator disposes; Bind() acts only on an entity id a human
// named. An auto-linked false positive on the erasure path deletes a third
// party's data, which is a breach committed while honouring a right.
//
// see LLD § https://docs.vornik.io
// §4.2 binder 3, §5.1 (why this is best-effort and says so)

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// KGEntity is a knowledge-graph PERSON entity, reduced to what resolution needs.
type KGEntity struct {
	ID            string
	ProjectID     string
	CanonicalName string
	Aliases       []string
}

// KGBindingState says what already holds this entity, which decides what
// Bind is allowed to do with it.
type KGBindingState string

const (
	// KGUnbound — no subject claims this entity. Bind links it.
	KGUnbound KGBindingState = "unbound"
	// KGBoundHere — this subject already claims it. Bind tops up any mentions
	// added since, which makes re-running safe.
	KGBoundHere KGBindingState = "bound_here"
	// KGBoundToPlaceholder — a synthetic `kg:<id>` subject minted by the chat
	// memory-write path (dispatcher linkOneKGEntity) claims it. That row is a
	// binding with no identified person behind it; adoption folds it into this
	// subject.
	KGBoundToPlaceholder KGBindingState = "placeholder"
	// KGConflict — another IDENTIFIED subject claims it. Never resolved
	// automatically: two identified people claiming one entity is a question
	// about people, and guessing merges one person's data into another's.
	KGConflict KGBindingState = "conflict"
)

// KGCandidate is a proposal: this entity might denote this subject.
type KGCandidate struct {
	Entity       KGEntity
	MatchedOn    string // the name or alias that matched, so the operator can judge
	MentionCount int
	State        KGBindingState
	// BoundSubjectID/Name are set for placeholder and conflict states.
	BoundSubjectID   string
	BoundSubjectName string
}

// KGBindResult reports what a bind actually did.
type KGBindResult struct {
	EntityID    string
	State       KGBindingState
	LinksAdded  int
	LinksMoved  int
	AdoptedFrom string
	// MentionsTruncated is retained for wire compatibility with older clients.
	// Current binds fetch every distinct mention and therefore leave it false.
	MentionsTruncated bool
}

// KGIndex is the read side of the knowledge graph.
type KGIndex interface {
	// FindPersonEntities returns PERSON entities in projectID whose canonical
	// name or an alias matches name.
	FindPersonEntities(ctx context.Context, projectID, name string, limit int) ([]KGEntity, error)
	// MentionChunks returns the memory-chunk ids the entity is mentioned in.
	// A positive limit caps the result; zero requests every distinct chunk.
	MentionChunks(ctx context.Context, entityID string, limit int) ([]string, error)
	// Projects lists the project ids holding a knowledge graph.
	Projects(ctx context.Context) ([]string, error)
}

// KGResolveStore is the subject-axis write surface resolution needs.
type KGResolveStore interface {
	FindSubjectByIdentifier(ctx context.Context, kind, value string) (string, error)
	GetSubject(ctx context.Context, id string) (Subject, error)
	ListIdentifiers(ctx context.Context, subjectID string) ([]Identifier, error)
	ListLinks(ctx context.Context, subjectID string) ([]Link, error)
	AddIdentifier(ctx context.Context, subjectID string, id Identifier) error
	AddLink(ctx context.Context, subjectID string, l Link) error
	// ReassignLinks moves every link from one subject to another, returning how
	// many moved. Used only by adoption.
	ReassignLinks(ctx context.Context, fromSubjectID, toSubjectID string) (int, error)
	// DeleteSubject removes a subject and its identifiers.
	DeleteSubject(ctx context.Context, id string) error
}

// defaultMaxMentions is the legacy-named cap on entity candidates returned for
// one name search. Mention linking itself is complete and is never capped.
const defaultMaxMentions = 5000

// KGResolver resolves a data subject's knowledge-graph entities on demand.
type KGResolver struct {
	Store KGResolveStore
	Index KGIndex
	// MaxMentions is retained for compatibility and caps entity candidates per
	// name search; 0 uses defaultMaxMentions. It does not cap mention linking.
	MaxMentions int
}

func (r *KGResolver) maxMentions() int {
	if r.MaxMentions > 0 {
		return r.MaxMentions
	}
	return defaultMaxMentions
}

// PlaceholderSubjectName is the display name the chat memory-write path gives a
// subject it mints for a resolved KG entity (dispatcher linkOneKGEntity).
//
// It lives here rather than in the dispatcher because BOTH sides depend on the
// same string: the writer to mint it, adoption to recognise it. If they
// diverged, every chat-created subject would present as an un-adoptable
// conflict and one person would keep two subject rows for good.
func PlaceholderSubjectName(entityID string) string { return "kg:" + entityID }

// IsPlaceholderSubject reports whether s is the synthetic subject for entityID
// — a binding with no identified person behind it, as opposed to someone an
// operator has named.
func IsPlaceholderSubject(s Subject, entityID string) bool {
	return s.DisplayName == PlaceholderSubjectName(entityID)
}

// Candidates proposes the KG entities that might denote this subject.
//
// names widens the search beyond the subject's display name — the design's
// §5.1 backstop for the binder's false negatives, where the operator supplies a
// nickname the extractor never linked. projects narrows it; empty means every
// project holding a graph.
//
// Nothing is written. Every match is returned, including ones that are probably
// wrong: filtering a "Doe Industries" out on a heuristic would hide from the
// operator exactly the judgement this step exists to ask them for.
func (r *KGResolver) Candidates(ctx context.Context, subjectID string, names, projects []string) ([]KGCandidate, error) {
	subject, err := r.Store.GetSubject(ctx, subjectID)
	if err != nil {
		return nil, fmt.Errorf("load subject %s: %w", subjectID, err)
	}
	search := searchNames(subject, names)
	if len(search) == 0 {
		return nil, fmt.Errorf("subject %s has no display name and no --name was given: "+
			"there is nothing to search the knowledge graph for", subjectID)
	}
	if len(projects) == 0 {
		projects, err = r.Index.Projects(ctx)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
	}

	seen := map[string]bool{}
	var out []KGCandidate
	for _, project := range projects {
		for _, name := range search {
			found, err := r.Index.FindPersonEntities(ctx, project, name, r.maxMentions())
			if err != nil {
				return nil, fmt.Errorf("search %q in %s: %w", name, project, err)
			}
			for _, e := range found {
				if seen[e.ID] {
					continue
				}
				seen[e.ID] = true
				cand := KGCandidate{Entity: e, MatchedOn: name}
				if cand.MentionCount, err = r.mentionCount(ctx, e.ID); err != nil {
					return nil, err
				}
				if err := r.classify(ctx, subjectID, &cand); err != nil {
					return nil, err
				}
				out = append(out, cand)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Entity.ProjectID != out[j].Entity.ProjectID {
			return out[i].Entity.ProjectID < out[j].Entity.ProjectID
		}
		return out[i].Entity.ID < out[j].Entity.ID
	})
	return out, nil
}

func (r *KGResolver) mentionCount(ctx context.Context, entityID string) (int, error) {
	chunks, err := r.Index.MentionChunks(ctx, entityID, 0)
	if err != nil {
		return 0, fmt.Errorf("mentions of %s: %w", entityID, err)
	}
	return len(chunks), nil
}

// classify fills in a candidate's binding state — who, if anyone, already
// claims this entity.
func (r *KGResolver) classify(ctx context.Context, subjectID string, cand *KGCandidate) error {
	owner, err := r.Store.FindSubjectByIdentifier(ctx, KindKGEntity, cand.Entity.ID)
	if err != nil {
		return fmt.Errorf("look up owner of %s: %w", cand.Entity.ID, err)
	}
	switch owner {
	case "":
		cand.State = KGUnbound
		return nil
	case subjectID:
		cand.State = KGBoundHere
		return nil
	}
	cand.BoundSubjectID = owner
	ownerSubject, err := r.Store.GetSubject(ctx, owner)
	if err != nil {
		return fmt.Errorf("load owning subject %s: %w", owner, err)
	}
	cand.BoundSubjectName = ownerSubject.DisplayName
	placeholder, err := r.isAdoptablePlaceholder(ctx, ownerSubject, cand.Entity.ID)
	if err != nil {
		return err
	}
	if placeholder {
		cand.State = KGBoundToPlaceholder
	} else {
		cand.State = KGConflict
	}
	return nil
}

// isAdoptablePlaceholder reports whether a subject is nothing but the chat
// path's binding for this entity.
//
// The name alone is not enough. A `kg:` subject that has since acquired other
// identifiers is one an operator has been working on — folding it away would
// destroy that work — so it is treated as a real subject and refused.
func (r *KGResolver) isAdoptablePlaceholder(ctx context.Context, s Subject, entityID string) (bool, error) {
	if !IsPlaceholderSubject(s, entityID) {
		return false, nil
	}
	ids, err := r.Store.ListIdentifiers(ctx, s.ID)
	if err != nil {
		return false, fmt.Errorf("list identifiers of %s: %w", s.ID, err)
	}
	for _, id := range ids {
		if id.Kind != KindKGEntity || id.Value != entityID {
			return false, nil
		}
	}
	return true, nil
}

// KGBindRequest is one bind: which entity, for which subject, under the same
// search terms the preview ran with.
//
// Names/Projects are carried rather than re-derived because Bind re-checks that
// the entity is a candidate, and an entity found through an operator-supplied
// nickname would not be re-found from the display name alone — the bind would
// refuse the very entity the preview had just offered.
type KGBindRequest struct {
	SubjectID string
	EntityID  string
	Names     []string
	Projects  []string
	// Adopt authorises folding a placeholder subject in. It does NOT authorise
	// resolving a conflict between two identified people.
	Adopt bool
}

// Bind links a named entity's mentions to the subject.
//
// req.EntityID must be one Candidates proposed — resolution never acts on a
// name, only on an id a human picked off the preview.
func (r *KGResolver) Bind(ctx context.Context, req KGBindRequest) (KGBindResult, error) {
	subjectID, entityID := req.SubjectID, req.EntityID
	res := KGBindResult{EntityID: entityID}
	entity, err := r.findEntity(ctx, req)
	if err != nil {
		return res, err
	}
	cand := KGCandidate{Entity: entity}
	// Re-classified here rather than carried from the preview: the preview can
	// be minutes old and a bind acts on the state it finds, not the one it was
	// shown. Same discipline as re-querying identifiers at redaction time.
	if err := r.classify(ctx, subjectID, &cand); err != nil {
		return res, err
	}
	res.State = cand.State

	switch cand.State {
	case KGConflict:
		return res, fmt.Errorf(
			"entity %s is already bound to subject %s (%q): two identified people cannot both "+
				"claim one knowledge-graph entity, and merging them would disclose one person's "+
				"data to the other. Resolve by hand — --adopt does not override this",
			entityID, cand.BoundSubjectID, cand.BoundSubjectName)
	case KGBoundToPlaceholder:
		if !req.Adopt {
			return res, fmt.Errorf(
				"entity %s is already bound to placeholder subject %s (%q), created when a chat note "+
					"named this person: pass --adopt to move its links onto %s and remove the "+
					"placeholder, so one person does not keep two subject rows",
				entityID, cand.BoundSubjectID, cand.BoundSubjectName, subjectID)
		}
		moved, adoptErr := r.adopt(ctx, subjectID, cand.BoundSubjectID, entityID)
		if adoptErr != nil {
			return res, adoptErr
		}
		res.LinksMoved = moved
		res.AdoptedFrom = cand.BoundSubjectID
	}

	if err := r.recordIdentifier(ctx, subjectID, entityID); err != nil {
		return res, err
	}
	added, truncated, err := r.linkMentions(ctx, subjectID, entity)
	res.LinksAdded = added
	res.MentionsTruncated = truncated
	if err != nil {
		// The count rides on the error: a partial bind that reported nothing
		// would look to the operator like a bind that did nothing.
		return res, fmt.Errorf("%w (linked %d rows before stopping)", err, added)
	}
	return res, nil
}

// findEntity re-reads the entity from the graph, refusing an id the search for
// this subject would not have proposed. Stops a mistyped id minting a
// kg_entity identifier that points at nothing.
func (r *KGResolver) findEntity(ctx context.Context, req KGBindRequest) (KGEntity, error) {
	cands, err := r.Candidates(ctx, req.SubjectID, req.Names, req.Projects)
	if err != nil {
		return KGEntity{}, err
	}
	for _, c := range cands {
		if c.Entity.ID == req.EntityID {
			return c.Entity, nil
		}
	}
	return KGEntity{}, fmt.Errorf(
		"entity %s is not among the candidates for subject %s — re-run 'subject resolve-kg' without "+
			"--entity to see the current proposals (pass the same --name flags the preview ran with "+
			"if the entity is under a name this subject is not recorded with)",
		req.EntityID, req.SubjectID)
}

// adopt folds a placeholder subject into the real one.
//
// ORDER IS THE CRASH CONTRACT. Links move first, then the identifier lands on
// the real subject, then the placeholder goes. A crash after the move leaves
// the links on the real subject and a placeholder holding nothing, so a re-run
// moves zero and completes — the operation converges, and at no point are
// links stranded on a subject that no longer answers to the entity.
func (r *KGResolver) adopt(ctx context.Context, toSubjectID, fromSubjectID, entityID string) (int, error) {
	moved, err := r.Store.ReassignLinks(ctx, fromSubjectID, toSubjectID)
	if err != nil {
		return 0, fmt.Errorf("move links from %s: %w", fromSubjectID, err)
	}
	if err := r.recordIdentifier(ctx, toSubjectID, entityID); err != nil {
		return moved, err
	}
	if err := r.Store.DeleteSubject(ctx, fromSubjectID); err != nil {
		return moved, fmt.Errorf("remove placeholder %s (its %d links are already on %s): %w",
			fromSubjectID, moved, toSubjectID, err)
	}
	return moved, nil
}

// recordIdentifier records the kg_entity identifier, so a later resolve reuses
// this binding instead of re-proposing the same entity. Idempotent: the store's
// identifier key is (subject, kind, value).
func (r *KGResolver) recordIdentifier(ctx context.Context, subjectID, entityID string) error {
	// Through the binder, so the confidence ceiling is applied in one place.
	b, err := BindKGExtraction(entityID, "", "", ConfidencePossible)
	if err != nil {
		return fmt.Errorf("bind identifier for %s: %w", entityID, err)
	}
	for _, id := range b.Identifiers {
		if err := r.Store.AddIdentifier(ctx, subjectID, id); err != nil {
			return fmt.Errorf("record identifier %s: %w", entityID, err)
		}
	}
	return nil
}

// linkMentions writes a link for the entity ROW and one for every chunk it is
// mentioned in.
//
// The row first, and deliberately: it is the densest single piece of personal
// data in the set (the subject's canonical name plus every alias the extractor
// learned), so a run that fails part-way has covered it rather than left it for
// a retry that may never come.
func (r *KGResolver) linkMentions(ctx context.Context, subjectID string, e KGEntity) (added int, truncated bool, err error) {
	entityBinding, err := BindKGEntityRow(e.ID, e.ProjectID, ConfidencePossible)
	if err != nil {
		return 0, false, fmt.Errorf("bind entity row %s: %w", e.ID, err)
	}
	for _, l := range entityBinding.Links {
		if lErr := r.Store.AddLink(ctx, subjectID, l); lErr != nil {
			return 0, false, fmt.Errorf("link entity row %s: %w", e.ID, lErr)
		}
		added++
	}

	// Fetch every distinct mention. The former ceiling fetched the same first
	// page on every retry, so an entity with N+1 mentions could never have its
	// final row linked even though the CLI instructed the operator to rerun.
	// The repository streams the SQL rows; this slice is bounded by the actual
	// number of distinct chunks for one entity rather than an arbitrary page.
	chunks, err := r.Index.MentionChunks(ctx, e.ID, 0)
	if err != nil {
		return 0, false, fmt.Errorf("mentions of %s: %w", e.ID, err)
	}
	truncated = false
	for _, chunkID := range chunks {
		b, bErr := BindKGExtraction(e.ID, chunkID, e.ProjectID, ConfidencePossible)
		if bErr != nil {
			return added, truncated, fmt.Errorf("bind link %s/%s: %w", e.ID, chunkID, bErr)
		}
		for _, l := range b.Links {
			if lErr := r.Store.AddLink(ctx, subjectID, l); lErr != nil {
				return added, truncated, fmt.Errorf("link %s: %w", chunkID, lErr)
			}
		}
		added++
	}
	return added, truncated, nil
}

// searchNames is the set of names to search the graph for: the subject's
// display name plus anything the operator supplied, de-duplicated
// case-insensitively and with blanks dropped.
func searchNames(s Subject, extra []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range append([]string{s.DisplayName}, extra...) {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		key := strings.ToLower(n)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, n)
	}
	return out
}
