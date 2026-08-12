// Package ned is the synchronous, shared-scope-only, pre-commit
// named-entity-resolution gate for the chat `remember` tool
// (chat memory-write design slices 4–5, §6). It runs BEFORE any DB
// write on a shared deposit: extract PERSON entities from the raw
// content, resolve them against the project's knowledge graph, and
// decide whether the write may proceed.
//
// WHY IT EXISTS. A shared chat note routinely names a third party —
// "Bob mentioned Alice's deadline is Friday." Alice holds Art 15/17
// rights over that chunk though Bob wrote it. If she cannot be tied
// to a data-subject id, a later erasure request for her would not
// find the chunk. So an unresolved named person BLOCKS the write; a
// resolved one proceeds and is linked (design D4.1).
//
// THE FOUR OUTCOMES (parent §6.2.1 + D6.3):
//   - no PERSON candidate            → proceed
//   - all persons resolve to `match` → proceed, matched entity ids returned
//   - any `new` / `ambiguous`        → BLOCK (named but unresolved)
//   - extract/resolve transport error → BLOCK, fail CLOSED (D6.3) — shared is
//     the high-stakes path; a silent proceed on an NED outage reopens the
//     exact Art 17 hole the gate closes. This is a DISTINCT verdict from an
//     ambiguous decision so the refusal can say "couldn't verify" vs "names
//     someone I don't know".
//
// THE TYPE-LEVEL GUARDRAIL (review I3). A proceed verdict mints a
// SharedWriteAuthorization token whose zero value is unusable and whose
// only field is unexported — so no package outside `ned` can forge a
// granted token. The dispatcher's shared write entry REQUIRES the token
// as a parameter, which makes "a shared write went through NED" a
// compile-time property rather than a review gate: a new shared-scope
// caller cannot compile a write without a token, and the only source of
// a granted token is Gate.Screen returning proceed.
//
// see https://docs.vornik.io §6
package ned

import (
	"context"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/llmspend"
	"vornik.io/vornik/internal/memory/graph"
	"vornik.io/vornik/internal/persistence"
)

// RememberNEDCallSite is the chat call-site label the gate stamps on the
// extract/resolve context (design D6.4). It is DISTINCT from the KG
// worker's "memory.graph" so the gate's own spend is attributable to the
// feature. Declared as a package constant (not a literal) so the
// callsite-accounting guard in internal/chat resolves it and forces a
// registry classification — the mechanism that makes a new spender
// impossible to add silently.
const RememberNEDCallSite = "chat.remember.ned"

// Verdict is the gate's decision on a shared-scope deposit.
type Verdict int

const (
	// VerdictProceed — the write may commit. Carries a granted
	// SharedWriteAuthorization and any matched knowledge_entities ids.
	VerdictProceed Verdict = iota
	// VerdictBlock — a named person could not be resolved (`new` /
	// `ambiguous`). BlockedPersons names them for the refusal.
	VerdictBlock
	// VerdictError — extract/resolve failed; fail CLOSED (D6.3).
	VerdictError
)

// Decision is the outcome of Gate.Screen. Only a VerdictProceed decision
// carries a usable authorization token (Authorization().Granted() == true).
type Decision struct {
	Verdict Verdict
	// BlockedPersons are the PERSON surface forms that did not resolve —
	// named in the refusal so the user can see which name tripped the gate
	// and rephrase (§6.2.1: hiding the name forces the user to guess).
	BlockedPersons []string
	// MatchedEntityIDs are the knowledge_entities ids for resolved persons
	// (Resolution.MatchID). The dispatcher records one data-subject link
	// per (matched entity × chunk) via datasubject.BindKGExtraction (D4.1).
	MatchedEntityIDs []string
	// Err is the underlying transport/model error on VerdictError.
	Err error

	auth SharedWriteAuthorization
}

// Authorization returns the write token. It is granted only on a
// VerdictProceed decision; every other verdict returns the zero (unusable)
// token.
func (d Decision) Authorization() SharedWriteAuthorization { return d.auth }

// Proceeds reports whether the write may commit.
func (d Decision) Proceeds() bool { return d.Verdict == VerdictProceed }

// SharedWriteAuthorization is the type-level guardrail (design I3/D6.1). Its
// zero value is UNUSABLE (Granted() == false) and its sole field is
// unexported, so no package other than `ned` can construct a granted token.
// The dispatcher's shared write entry takes one as a parameter, making a
// shared write that skips the NED gate a compile error rather than a
// review-time omission — the structural equivalent of the slice-3 SpeakerID
// sentinel and the §5.1 nil-gate default.
type SharedWriteAuthorization struct {
	granted bool
}

// Granted reports whether this token authorizes a shared write. The zero
// token is never granted.
func (a SharedWriteAuthorization) Granted() bool { return a.granted }

// Extractor is the narrow slice of *graph.Extractor the gate needs: raw
// content → candidate entity mentions. Defined here so tests can inject a
// fake without a chat provider.
type Extractor interface {
	Extract(ctx context.Context, content string) ([]graph.Candidate, *graph.ExtractMetrics, error)
}

// Resolver is the narrow slice of *graph.Resolver the gate needs:
// candidates → per-candidate match/new/ambiguous decisions.
type Resolver interface {
	Resolve(ctx context.Context, projectID string, cands []graph.Candidate) ([]graph.Resolution, *graph.ResolveMetrics, error)
}

// UsageRecorder is the narrow interface the gate needs from
// persistence.TaskLLMUsageRepository — only Record. Mirrors graph's own
// local definition so this package doesn't drag the full repo interface in.
type UsageRecorder interface {
	Record(ctx context.Context, u *persistence.TaskLLMUsage) error
}

// PricingTable mirrors *pricing.Table.CostUSD so production wires directly
// with no adapter and tests can supply their own.
type PricingTable interface {
	CostUSD(model string, promptTokens, completionTokens int) float64
}

// Gate runs the pre-commit NED screen. Extractor and Resolver are required;
// Usage and Pricing are optional (nil disables billing, but a call with a
// nil recorder records nothing — the exact silent-unbilled failure class
// this feature was written to avoid, so production MUST wire them).
type Gate struct {
	Extractor Extractor
	Resolver  Resolver
	// Spend records one task_llm_usage row per billed stage. Exported because the
	// Gate is assembled as a struct literal by its containers; a zero value is
	// loud rather than silent, which is the property that matters here.
	Spend llmspend.Recorder
}

// The two task_llm_usage roles this gate bills under. Exported so wiring sites
// name them rather than repeating literals; RoleExtractor is the Recorder's
// default and the resolver stage overrides it per call.
const (
	RoleExtractor = "chat_remember_ned_extractor"
	RoleResolver  = "chat_remember_ned_resolver"
)

// Screen runs extract → filter PERSON → resolve and maps the result to a
// Decision (design D6.2). It records the extract + resolve spend as
// task_llm_usage rows under TaskLLMUsageSourceChatRememberNED (D6.4) so the
// gate does not repeat the reranker/distiller unbilled-call-site mistake.
//
// It NEVER writes to memory or the data-subject store — the caller does that
// only on a proceed verdict, using the returned token. A block or error
// verdict therefore leaves ZERO rows written (C1: the orphan case is designed
// out, not compensated).
func (g *Gate) Screen(ctx context.Context, projectID, content string) Decision {
	if g == nil || g.Extractor == nil || g.Resolver == nil {
		// Cannot verify who this names → fail closed (treated as error).
		return Decision{Verdict: VerdictError, Err: errors.New("ned: gate not configured")}
	}

	// Stamp the feature's call-site label. The graph package overwrites this
	// with "memory.graph" for the actual provider call, but the label must
	// exist in code so the callsite-accounting guard classifies the gate as a
	// spender (and forces the billing below to be considered).
	ctx = chat.WithCallSite(ctx, RememberNEDCallSite)

	cands, exMetrics, err := g.Extractor.Extract(ctx, content)
	g.recordExtract(ctx, projectID, exMetrics)
	if err != nil {
		return Decision{Verdict: VerdictError, Err: fmt.Errorf("ned: extract: %w", err)}
	}

	persons := make([]graph.Candidate, 0, len(cands))
	for _, c := range cands {
		if c.Type == persistence.EntityTypePerson {
			persons = append(persons, c)
		}
	}
	if len(persons) == 0 {
		// Nobody named — nothing for Art 17 to miss. Proceed.
		return Decision{Verdict: VerdictProceed, auth: granted()}
	}

	resns, resMetrics, err := g.Resolver.Resolve(ctx, projectID, persons)
	g.recordResolve(ctx, projectID, resMetrics)
	if err != nil {
		return Decision{Verdict: VerdictError, Err: fmt.Errorf("ned: resolve: %w", err)}
	}

	var matched, blocked []string
	for i, p := range persons {
		var r graph.Resolution
		if i < len(resns) {
			r = resns[i]
		}
		if r.Decision == "match" && r.MatchID != "" {
			matched = append(matched, r.MatchID)
			continue
		}
		// `new`, `ambiguous`, a match with no id, or a missing decision — all
		// mean "named but not tied to a data-subject id". Block.
		blocked = append(blocked, p.Name)
	}

	if len(blocked) > 0 {
		return Decision{Verdict: VerdictBlock, BlockedPersons: blocked}
	}
	return Decision{Verdict: VerdictProceed, MatchedEntityIDs: matched, auth: granted()}
}

// granted mints a usable token. This is the ONLY place a granted
// SharedWriteAuthorization is constructed; keeping it in one unexported
// helper is what makes the guarantee auditable.
func granted() SharedWriteAuthorization { return SharedWriteAuthorization{granted: true} }

// Outcome is the fine-grained §6.2.1 resolver decision for a single piece of
// content, as measured by the calibration harness (§7). It is a SUPERSET of the
// gate's Verdict: Screen collapses New/Ambiguous into VerdictBlock (both mean
// "named but unresolved → block"), but calibration must tell them apart because
// the two cross DIFFERENT ship/re-scope thresholds (new > 30% → don't ship
// shared as-is; ambiguous > 20% → tune the resolver).
type Outcome int

const (
	// OutcomeNone — the extractor found no PERSON candidate. Nobody named,
	// nothing for Art 17 to miss; the gate would proceed.
	OutcomeNone Outcome = iota
	// OutcomeMatch — at least one PERSON and EVERY person resolved to an
	// existing entity (Decision=="match" with a MatchID). The gate proceeds
	// and links each subject.
	OutcomeMatch
	// OutcomeNew — at least one person resolved as `new` (not in the graph).
	// Dominant outcome > 30% means shared scope refuses the normal case.
	OutcomeNew
	// OutcomeAmbiguous — at least one person is ambiguous (or a match with no
	// id / a decision the resolver omitted) and NONE is `new`. > 20% means the
	// resolver needs tuning.
	OutcomeAmbiguous
)

// String renders the outcome as the stable label used in the calibration report
// and its JSON (lower-case, matching the §6.2.1 axis names).
func (o Outcome) String() string {
	switch o {
	case OutcomeNone:
		return "none"
	case OutcomeMatch:
		return "match"
	case OutcomeNew:
		return "new"
	case OutcomeAmbiguous:
		return "ambiguous"
	default:
		return "unknown"
	}
}

// filterPersons returns the subset of candidates the gate cares about — PERSON
// entities, the only type that carries Art 15/17 data-subject weight.
func filterPersons(cands []graph.Candidate) []graph.Candidate {
	persons := make([]graph.Candidate, 0, len(cands))
	for _, c := range cands {
		if c.Type == persistence.EntityTypePerson {
			persons = append(persons, c)
		}
	}
	return persons
}

// Classify runs the SAME billed extract → filter PERSON → resolve pipeline as
// Screen (design D6.2/D6.4) but maps the result to the fine-grained calibration
// Outcome instead of the coarse proceed/block Verdict. It exists ONLY for the
// §7 calibration harness: the harness needs to distinguish `new` from
// `ambiguous` (Screen deliberately does not, since both block).
//
// It records the extract + resolve spend as task_llm_usage rows under
// TaskLLMUsageSourceChatRememberNED (chat_remember_ned) via the exact same
// recordExtract/recordResolve helpers Screen uses — so the measurement's own LLM
// spend is billed and attributable to the feature, not silently unbilled (the
// reranker/distiller failure class D6.4 was written to avoid). A transport/model
// error is returned to the caller (the harness tallies it as a separate "error"
// bucket, never folded into the four rates).
//
// Like Screen it NEVER writes to memory or the data-subject store, and it never
// returns or logs the content — only the decision.
func (g *Gate) Classify(ctx context.Context, projectID, content string) (Outcome, error) {
	if g == nil || g.Extractor == nil || g.Resolver == nil {
		return OutcomeNone, errors.New("ned: gate not configured")
	}
	ctx = chat.WithCallSite(ctx, RememberNEDCallSite)

	cands, exMetrics, err := g.Extractor.Extract(ctx, content)
	g.recordExtract(ctx, projectID, exMetrics)
	if err != nil {
		return OutcomeNone, fmt.Errorf("ned: extract: %w", err)
	}

	persons := filterPersons(cands)
	if len(persons) == 0 {
		return OutcomeNone, nil
	}

	resns, resMetrics, err := g.Resolver.Resolve(ctx, projectID, persons)
	g.recordResolve(ctx, projectID, resMetrics)
	if err != nil {
		return OutcomeNone, fmt.Errorf("ned: resolve: %w", err)
	}

	var sawNew, sawUnresolved bool
	for i := range persons {
		var r graph.Resolution
		if i < len(resns) {
			r = resns[i]
		}
		switch {
		case r.Decision == "match" && r.MatchID != "":
			// resolved — contributes to "all match" only if nothing else trips
		case r.Decision == "new":
			sawNew = true
		default:
			// ambiguous, a match with no id, or a missing decision — "named but
			// not tied to a data-subject id", same as Screen's block bucket.
			sawUnresolved = true
		}
	}

	// Precedence (§6.2.1): `new` dominates (it's the ship-blocking signal),
	// then ambiguous, else every person matched.
	switch {
	case sawNew:
		return OutcomeNew, nil
	case sawUnresolved:
		return OutcomeAmbiguous, nil
	default:
		return OutcomeMatch, nil
	}
}

func (g *Gate) recordExtract(ctx context.Context, projectID string, m *graph.ExtractMetrics) {
	if m == nil {
		return
	}
	g.record(ctx, projectID, RoleExtractor, m.Model, m.PromptTokens, m.CompletionTokens)
}

func (g *Gate) recordResolve(ctx context.Context, projectID string, m *graph.ResolveMetrics) {
	if m == nil {
		return
	}
	g.record(ctx, projectID, RoleResolver, m.Model, m.PromptTokens, m.CompletionTokens)
}

// record writes one task_llm_usage row for a billed stage. Mirrors
// graph.Pipeline.recordStageUsage and LLMReranker.recordUsage: a
// zero-token call (the resolver short-circuits ~70% without an LLM call)
// records nothing rather than polluting the dashboard with empty rows;
// errors are swallowed because failing to bill is a dashboard-fidelity
// issue, never a reason to fail the gate.
func (g *Gate) record(ctx context.Context, projectID, role, model string, prompt, completion int) {
	if projectID == "" {
		return
	}
	// RoleOverride: the gate bills under two roles (extractor, resolver) from one
	// component. TaskID stays nil — a chat deposit is tied to no task.
	g.Spend.Record(ctx, llmspend.Input{
		ProjectID:        projectID,
		Model:            model,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		RoleOverride:     role,
	})
}
