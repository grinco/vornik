package datasubject

import "fmt"

// Art 17 erasure — the decision layer (design §4.6, §5.3).
//
// This file decides WHAT should happen to each linked row. Execution — the
// artifact cascade, redact-and-re-embed, the row deletes — is separate, because
// the decision is the part that is irreversible if wrong and it is the part worth
// being pure and exhaustively tested.
//
// The organising idea is that the ERASURE GROUND is load-bearing, not
// bookkeeping. Five of the six Art 17(1) grounds leave the controller some
// discretion about how to honour the request over a row that also concerns
// somebody else; two do not. A design that defaulted to redaction for every
// shared row would be substituting a policy preference for a legal requirement,
// so the ground is captured at intake and selects the treatment.

// ErasureGround is the Art 17(1) limb the request is made under.
//
// The value embeds the article letter so a report cannot cite the wrong one.
// This matters more than it looks: the design document originally attributed
// consent withdrawal to 17(1)(a) and the legal-obligation limb to 17(1)(c), both
// off by one against the Regulation. The concepts were right and the citations
// were wrong, and a wrong citation in a subject-facing compliance artefact is a
// defect in the artefact. The lettering here is pinned by test.
type ErasureGround string

const (
	// GroundNoLongerNecessary — Art 17(1)(a): the data are no longer necessary
	// for the purposes they were collected for.
	GroundNoLongerNecessary ErasureGround = "art17_1_a_no_longer_necessary"

	// GroundConsentWithdrawn — Art 17(1)(b): consent is withdrawn and there is
	// no other legal ground for the processing.
	GroundConsentWithdrawn ErasureGround = "art17_1_b_consent_withdrawn"

	// GroundObjection — Art 17(1)(c): the subject objects under Art 21 and there
	// are no overriding legitimate grounds.
	GroundObjection ErasureGround = "art17_1_c_objection"

	// GroundUnlawfulProcessing — Art 17(1)(d): the data have been processed
	// unlawfully. Discretion-removing: there is no lawful basis on which to keep
	// a redacted remnant of data that should never have been processed.
	GroundUnlawfulProcessing ErasureGround = "art17_1_d_unlawful_processing"

	// GroundLegalObligation — Art 17(1)(e): erasure is required to comply with a
	// legal obligation. Discretion-removing: the obligation is to erase, and
	// redaction is not compliance with it.
	GroundLegalObligation ErasureGround = "art17_1_e_legal_obligation"

	// GroundChildServices — Art 17(1)(f): the data were collected in relation to
	// information-society services offered to a child (Art 8(1)).
	GroundChildServices ErasureGround = "art17_1_f_child_services"
)

// erasureGrounds is the closed set, with the article letter and a plain-language
// label for the report.
var erasureGrounds = map[ErasureGround]struct {
	letter string
	label  string
	// removesDiscretion marks the limbs under which the controller may not
	// retain any part of the data, so the shared-row redaction default is
	// unavailable.
	removesDiscretion bool
}{
	GroundNoLongerNecessary:  {"a", "the data are no longer necessary for the purposes they were collected for", false},
	GroundConsentWithdrawn:   {"b", "consent was withdrawn and no other legal ground applies", false},
	GroundObjection:          {"c", "the subject objected under Art 21 and no overriding legitimate grounds apply", false},
	GroundUnlawfulProcessing: {"d", "the data were processed unlawfully", true},
	GroundLegalObligation:    {"e", "erasure is required to comply with a legal obligation", true},
	GroundChildServices:      {"f", "the data were collected in relation to information-society services offered to a child", false},
}

// Validate reports whether the ground is one of the six Art 17(1) limbs.
//
// An empty or invented ground is rejected rather than defaulted. The ground
// selects the shared-row treatment, so guessing it would mean guessing whether
// another person's data may survive this request.
func (g ErasureGround) Validate() error {
	if _, ok := erasureGrounds[g]; !ok {
		return fmt.Errorf("datasubject: %q is not an Art 17(1) erasure ground — "+
			"the ground is recorded at intake and selects the shared-row treatment, so it cannot be inferred", g)
	}
	return nil
}

// ArticleLetter is the Art 17(1) limb letter, e.g. "d".
func (g ErasureGround) ArticleLetter() string { return erasureGrounds[g].letter }

// Article is the citation for a report, e.g. "Art 17(1)(d)".
func (g ErasureGround) Article() string {
	if l := erasureGrounds[g].letter; l != "" {
		return "Art 17(1)(" + l + ")"
	}
	return "Art 17(1)"
}

// Label is the plain-language reason, for a subject-facing report.
func (g ErasureGround) Label() string { return erasureGrounds[g].label }

// RemovesRetentionDiscretion reports whether this ground leaves the controller no
// discretion to retain any part of the data.
//
// True for Art 17(1)(d) (unlawful processing) and (e) (legal obligation to
// erase). Under those limbs a shared row must be deleted outright rather than
// redacted, even though deletion costs another subject their context — the
// controller has no lawful basis for the remnant, and preferring the other
// subject's convenience would be choosing policy over law.
func (g ErasureGround) RemovesRetentionDiscretion() bool {
	return erasureGrounds[g].removesDiscretion
}

// SharedRowRedactionAvailable reports whether Art 17 shared-row redaction —
// erasure slice 5c — has shipped.
//
// A constant rather than config, because it describes what the BINARY can do,
// not what an operator wants. Features that cannot be defended without it gate
// on this: automatic Workspace ingestion refuses to start while it is false,
// since every ingested meeting record is a shared row and an erasure request
// over them would report almost all as deferred rather than erased.
//
// TRUE since 2026-07-30: slice 5c shipped. A shared row is now rewritten by a model
// to remove the subject, the rewrite is VERIFIED mechanically against the subject's
// identifiers before anything is written, and content, hash, embedding, re-embed
// queue and embedding cache all move in one transaction.
//
// WHAT THIS TRUE DOES AND DOES NOT CLAIM. It claims the capability exists and that a
// subject's known identifiers can be removed from a shared record without destroying
// the other people's data in it. It does NOT claim inferential removal — pronouns,
// role references and Recital 26 quasi-identifiers survive, and the subject-facing
// report says so in those words (report_narrative.go). Nor does it cover shared
// conversation transcripts, which are slice 5d.
//
// It also depends on a rewrite model being configured. With none, every shared row
// DEFERS with a recorded reason rather than being silently skipped — so the honest
// failure mode is a visibly incomplete erasure, not a false completion.
//
// Design: 2026-07-29-art17-redact-and-reembed-design.md
const SharedRowRedactionAvailable = true

// Disposition is what happens to one linked row.
type Disposition string

const (
	// DispositionDelete — remove the row (composing the artifact cascade where
	// the row is an artifact).
	DispositionDelete Disposition = "delete"
	// DispositionRedact — remove this subject's personal data from the row and
	// re-derive anything computed from it (re-embed a memory chunk). Preserves
	// another subject's context at the cost of an LLM pass and imperfect
	// redaction.
	DispositionRedact Disposition = "redact"
)

// Action is one row's decided treatment, with the reason that decided it.
//
// Reason is not decoration: an erasure is irreversible, so "why was this row
// deleted rather than redacted" has to be answerable per row, months later,
// without re-deriving the policy.
type Action struct {
	Table       LinkableTable `json:"table"`
	RowID       string        `json:"row_id"`
	ProjectID   string        `json:"project_id,omitempty"`
	Exclusivity Exclusivity   `json:"exclusivity"`
	Disposition Disposition   `json:"disposition"`
	Reason      string        `json:"reason"`
}

// ErasurePlan is the decided treatment for a whole request, plus the honest
// statement of what erasure does and does not guarantee.
type ErasurePlan struct {
	SubjectID  string        `json:"subject_id"`
	RequestID  string        `json:"request_id"`
	Ground     ErasureGround `json:"ground"`
	GroundCite string        `json:"ground_citation"`
	Actions    []Action      `json:"actions"`

	// MethodsRun names the identification methods that contributed links, so the
	// subject learns HOW the search was performed rather than being handed a
	// bare result.
	MethodsRun []string `json:"identification_methods"`

	// RetainedCategories names data NOT erased, with its legal ground. A subject
	// told "erased" while records remain has been misled.
	RetainedCategories map[string]string `json:"retained_categories"`

	// Limitations is the honest statement of coverage. Erasure over free text is
	// best-effort and the report must say so.
	Limitations []string `json:"limitations"`
}

// DeleteCount is how many rows the plan deletes.
func (p *ErasurePlan) DeleteCount() int { return p.count(DispositionDelete) }

// RedactCount is how many rows the plan redacts.
func (p *ErasurePlan) RedactCount() int { return p.count(DispositionRedact) }

func (p *ErasurePlan) count(d Disposition) int {
	n := 0
	for _, a := range p.Actions {
		if a.Disposition == d {
			n++
		}
	}
	return n
}

// PlanErasure decides the treatment of every linked row for a verified Art 17
// request.
//
// THREE GATES, each of which exists because the failure it prevents is severe:
//
//  1. The identity gate. An unverified request plans nothing. This is the same
//     gate BuildExport applies, for a stronger reason: an export to the wrong
//     person is a disclosure, an erasure for the wrong person destroys somebody's
//     data on a stranger's word, and there is nothing to undo it with.
//
//  2. The ground gate. No recorded ground, no plan. The ground decides whether a
//     shared row may survive redacted, so inferring it would mean inferring
//     whether another person's data is deleted. Defaulting would pick the more
//     retentive option silently, which design §5.3 refuses by name.
//
//  3. The actionable-table gate. A link naming a table no executor handles fails
//     the plan rather than being skipped, because a skipped row plus a success
//     report is the shape of a compliance lie.
//
// An empty link set is NOT an error: "we found nothing identifiable, and here is
// how we looked" is a legitimate and required answer.
func PlanErasure(req Request, items []Item, methodsRun []string) (*ErasurePlan, error) {
	if !req.MayProduceData() {
		return nil, ErrNotVerified
	}
	if req.Kind != RequestErasure {
		return nil, fmt.Errorf("datasubject: PlanErasure handles erasure requests, not %q", req.Kind)
	}
	if err := req.ErasureGround.Validate(); err != nil {
		return nil, err
	}

	ground := req.ErasureGround
	noDiscretion := ground.RemovesRetentionDiscretion()

	plan := &ErasurePlan{
		SubjectID:          req.SubjectID,
		RequestID:          req.ID,
		Ground:             ground,
		GroundCite:         ground.Article(),
		MethodsRun:         append([]string(nil), methodsRun...),
		RetainedCategories: retainedCategories(),
		Limitations:        erasureLimitations(noDiscretion),
	}

	for _, it := range items {
		if err := ValidateTable(string(it.Table)); err != nil {
			return nil, fmt.Errorf("datasubject: cannot plan erasure of %s/%s: %w", it.Table, it.RowID, err)
		}
		plan.Actions = append(plan.Actions, decideAction(it, ground, noDiscretion))
	}
	return plan, nil
}

// decideAction applies the ground-dependent shared-row policy to one row.
func decideAction(it Item, ground ErasureGround, noDiscretion bool) Action {
	a := Action{
		Table: it.Table, RowID: it.RowID, ProjectID: it.ProjectID,
		Exclusivity: it.Exclusivity,
	}
	switch {
	case !it.Exclusivity.TreatAsShared():
		a.Disposition = DispositionDelete
		a.Reason = "row concerns this subject alone, so it is deleted outright"
	case noDiscretion:
		a.Disposition = DispositionDelete
		a.Reason = fmt.Sprintf(
			"row is %s, but %s (%s) leaves no discretion to retain any part — deleted in full despite the cost to other subjects named in it",
			it.Exclusivity, ground.Article(), ground.Label())
	default:
		a.Disposition = DispositionRedact
		a.Reason = fmt.Sprintf(
			"row is %s and %s permits honouring the request without deleting another subject's context — this subject's data is redacted and anything derived from it re-derived",
			it.Exclusivity, ground.Article())
	}
	return a
}

// retainedCategories reports what is NOT erased and why, sourced from the same
// closed map the coverage test polices — so the subject-facing report cannot
// drift from what the code actually does.
func retainedCategories() map[string]string {
	out := make(map[string]string, len(UncoveredTable))
	for table, ground := range UncoveredTable {
		out[table] = ground
	}
	return out
}

// erasureLimitations is the honest-coverage statement (design §5.1). "Erased"
// must not be read as "we are certain nothing remains".
func erasureLimitations(noDiscretion bool) []string {
	lims := []string{
		"Identification is best-effort over free text: a person referred to by a nickname, " +
			"a relationship (\"my sister\"), or a spelling no extractor linked may not appear in the index and " +
			"therefore may not be reached by this erasure.",
		"The guarantee is that everything this deployment can identify as being about the subject was acted on, " +
			"and that the identification methods used are named above — not that no trace remains anywhere.",
		"Data that reached a sub-processor as prompt content cannot be recalled. Art 19 notification to " +
			"recipients is performed by the operator and is not automated.",
		"Backups and replicas are not reached by this erasure. A backup taken before the request and " +
			"restored afterwards reintroduces the data, so the operator must either apply the erasure " +
			"again after any restore or let the affected backups age out under their own retention.",
	}
	if noDiscretion {
		lims = append(lims, "Because the recorded ground leaves no discretion to retain, rows that also concern "+
			"other people were deleted in full rather than redacted. Those subjects lose context they would have kept "+
			"under a discretionary ground.")
	} else {
		lims = append(lims, "Rows that also concern other people were redacted rather than deleted, to preserve "+
			"those subjects' data. Redaction of free text is imperfect by nature.")
	}
	return lims
}
