package memory

// Consumer C of the continuous-learning instinct layer — the memory
// feedback-loop closure (LLD slice 5). It reads retrieval-domain
// instincts mined by the extraction worker and turns them into HINTS
// the existing consolidation / firewall sweepers can fold into their
// scoring:
//
//   - Boost candidates: chunks retrieved into successful steps. The
//     extraction worker keys scope-support on {role, repo_scope}, so the
//     boost signal is a set of repo scopes whose retrievals correlated
//     with `ok` steps; the consolidation sweeper can lift chunks served
//     from those scopes.
//   - Prune candidates: chunks never retrieved in DeadDays. The worker
//     emits one chunk-keyed retrieval instinct per dead chunk; Consumer
//     C lifts the chunk IDs out so the firewall / retention sweeper can
//     surface them to the operator.
//
// NON-NEGOTIABLE invariants (LLD "What does NOT change"):
//
//   - Advisory-only / NEVER auto-delete. Consumer C produces candidate
//     SETS; the existing retention sweeper + operator decide. Nothing
//     here issues a DELETE.
//   - Opt-in. Every behaviour is gated behind instinct.consumers.
//     memory_hygiene (Enabled) AND the master instinct.enabled (the
//     subsystem only wires this when both are true). With the gate off
//     Hints returns an empty result, so the sweepers behave exactly as
//     today.
//   - Firewall-respecting. Candidate chunks are filtered through the
//     policy columns: a firewall-sensitive chunk (confidential /
//     restricted tier, or a credential content-class) is never surfaced
//     as a boost or prune candidate.

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
)

// instinctLister is the narrow read surface Consumer C needs from
// persistence.InstinctRepository. Declared locally so tests can supply
// a fake without a live DB and so the memory package doesn't depend on
// the full repository contract.
type instinctLister interface {
	List(ctx context.Context, filter persistence.InstinctFilter) ([]*persistence.Instinct, error)
}

// chunkPolicyLoader is the narrow surface used to screen candidate
// chunks against the memory firewall. *Repository satisfies it.
type chunkPolicyLoader interface {
	LoadChunkPolicies(ctx context.Context, chunkIDs []string) (map[string]ChunkPolicyRow, error)
}

// RetrievalHints is the advisory output Consumer C feeds to the
// sweepers. Both sets are deterministic and de-duplicated. Empty sets
// (the gate-off / no-signal case) mean "no hint" — the sweeper falls
// back to its existing behaviour.
type RetrievalHints struct {
	// BoostScopes is the set of repo scopes whose retrievals correlated
	// with successful steps (active/promoted scope-support retrieval
	// instincts). A sweeper may lift chunks served from these scopes.
	BoostScopes map[string]struct{}
	// PruneChunkIDs is the set of chunk IDs flagged as never-recently-
	// retrieved prune candidates. ADVISORY ONLY — the sweeper surfaces
	// them; it never deletes.
	PruneChunkIDs map[string]struct{}
}

// IsBoostScope reports whether the scope was flagged as a boost
// candidate. Nil-safe: a nil/empty Hints reports false for everything,
// which is the gate-off behaviour.
func (h *RetrievalHints) IsBoostScope(scope string) bool {
	if h == nil || h.BoostScopes == nil {
		return false
	}
	_, ok := h.BoostScopes[scope]
	return ok
}

// IsPruneCandidate reports whether the chunk was flagged as a prune
// candidate. Nil-safe.
func (h *RetrievalHints) IsPruneCandidate(chunkID string) bool {
	if h == nil || h.PruneChunkIDs == nil {
		return false
	}
	_, ok := h.PruneChunkIDs[chunkID]
	return ok
}

// RetrievalHygiene derives Consumer C hints. It is gated: with Enabled
// false, Hints is a no-op returning empty sets so the sweepers behave
// byte-for-byte as today.
type RetrievalHygiene struct {
	// Enabled mirrors instinct.consumers.memory_hygiene. The subsystem
	// only constructs a RetrievalHygiene with Enabled=true when BOTH the
	// master instinct.enabled and the consumer gate are set; a zero-value
	// RetrievalHygiene (Enabled=false) is the safe default.
	Enabled bool
	// Instincts is the read surface onto the instinct repository. Nil =
	// disabled (Hints returns empty), so a half-wired container degrades
	// to today's behaviour rather than panicking.
	Instincts instinctLister
	// Policies screens candidate chunks against the memory firewall.
	// Nil = no screening (SQLite / tests that don't exercise the firewall
	// path); the boost/prune derivation still runs but skips the
	// sensitivity filter.
	Policies chunkPolicyLoader
	// MaxHints caps the instinct rows pulled per project. 0 → 500.
	MaxHints int
	Logger   zerolog.Logger
}

const defaultRetrievalHygieneMaxHints = 500

// Hints returns the boost/prune candidate sets for one project. It is
// the only behaviour-affecting entry point and is fully gated:
//
//   - Enabled false, or Instincts nil → empty (non-nil) sets. Callers
//     can treat the result uniformly; IsBoostScope / IsPruneCandidate
//     report false for everything.
//   - Otherwise: List the project's retrieval-domain instincts, classify
//     them into boost scopes / prune chunks, then screen prune chunks
//     against the firewall (sensitive chunks are dropped).
func (h *RetrievalHygiene) Hints(ctx context.Context, projectID string) (*RetrievalHints, error) {
	empty := &RetrievalHints{
		BoostScopes:   map[string]struct{}{},
		PruneChunkIDs: map[string]struct{}{},
	}
	if h == nil || !h.Enabled || h.Instincts == nil {
		return empty, nil
	}

	domain := persistence.InstinctDomainRetrieval
	filter := persistence.InstinctFilter{
		Domain:   &domain,
		PageSize: h.maxHints(),
	}
	if projectID != "" {
		filter.ProjectID = &projectID
	}
	instincts, err := h.Instincts.List(ctx, filter)
	if err != nil {
		return empty, err
	}

	hints := DeriveRetrievalHints(instincts)

	// Firewall screen: a sensitive chunk must never be surfaced as a
	// candidate. Boost is scope-keyed (no chunk IDs to screen); prune is
	// chunk-keyed, so we screen those.
	if h.Policies != nil && len(hints.PruneChunkIDs) > 0 {
		hints.PruneChunkIDs = h.screenPrune(ctx, hints.PruneChunkIDs)
	}
	return hints, nil
}

func (h *RetrievalHygiene) maxHints() int {
	if h.MaxHints > 0 {
		return h.MaxHints
	}
	return defaultRetrievalHygieneMaxHints
}

// screenPrune drops firewall-sensitive chunks from the prune candidate
// set. On a policy-load error it FAILS CLOSED — returns the original
// set minus nothing is wrong (the hint is advisory and the operator
// makes the call), but to honour "never surface sensitive chunks" we
// drop the whole set rather than risk surfacing a sensitive chunk we
// couldn't classify.
func (h *RetrievalHygiene) screenPrune(ctx context.Context, in map[string]struct{}) map[string]struct{} {
	ids := make([]string, 0, len(in))
	for id := range in {
		ids = append(ids, id)
	}
	policies, err := h.Policies.LoadChunkPolicies(ctx, ids)
	if err != nil {
		h.Logger.Warn().Err(err).
			Msg("instinct memory-hygiene: chunk policy load failed; suppressing prune candidates")
		return map[string]struct{}{}
	}
	out := make(map[string]struct{}, len(in))
	for id := range in {
		if chunkIsSensitive(policies[id]) {
			continue
		}
		out[id] = struct{}{}
	}
	return out
}

// chunkIsSensitive reports whether a chunk's firewall policy makes it
// off-limits for instinct surfacing. Confidential / restricted tiers
// are sensitive; the classifier bridge bumps credential content to
// restricted, so applying it here catches credential chunks even when
// their persisted tier hasn't been backfilled yet. A zero-value row
// (legacy chunk with no policy columns) resolves via the source default
// and is treated as non-sensitive unless the bridge says otherwise.
func chunkIsSensitive(row ChunkPolicyRow) bool {
	source := memoryfirewall.ProvenanceUnknown
	if row.ProvenanceSource != "" {
		source = memoryfirewall.ProvenanceSource(row.ProvenanceSource)
	}
	tier := memoryfirewall.SensitivityTier(row.SensitivityTier)
	if tier == "" {
		def := memoryfirewall.DefaultPolicyForSource(source, row.ContentClass)
		tier = def.Sensitivity
	}
	// Read-time credential/refuted adjustment.
	adjusted := memoryfirewall.ApplyClassifierSignal(
		memoryfirewall.Policy{Sensitivity: tier},
		row.ContentClass, row.ValidationStatus,
	)
	switch adjusted.Sensitivity {
	case memoryfirewall.SensitivityConfidential, memoryfirewall.SensitivityRestricted:
		return true
	default:
		return false
	}
}

// InstinctChunkScreener adapts the memory-firewall policy loader to the
// instinct extraction worker's ChunkScreener: it reports which chunk IDs
// are firewall-sensitive so the worker never LEARNS (persists) a sensitive
// chunk identifier into the instincts table — enforcing the firewall
// invariant at mine time, upstream of Consumer C's read-time screen. It
// reuses the same chunkIsSensitive classification Consumer C applies.
type InstinctChunkScreener struct {
	Policies chunkPolicyLoader
}

// SensitiveChunks returns the subset of chunkIDs that are firewall-
// sensitive. A propagated policy-load error is returned so the worker can
// fail closed (it suppresses the chunk-keyed extractions rather than risk
// persisting an unclassified chunk). A nil loader screens nothing.
func (s *InstinctChunkScreener) SensitiveChunks(ctx context.Context, chunkIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(chunkIDs))
	if s == nil || s.Policies == nil || len(chunkIDs) == 0 {
		return out, nil
	}
	policies, err := s.Policies.LoadChunkPolicies(ctx, chunkIDs)
	if err != nil {
		return nil, err
	}
	for _, id := range chunkIDs {
		if chunkIsSensitive(policies[id]) {
			out[id] = true
		}
	}
	return out, nil
}

// pruneCandidatePrefix is the canonical action prefix the extraction
// worker emits for a never-recently-retrieved chunk
// ("chunk <id> has not been retrieved in N days (prune candidate)").
// DeriveRetrievalHints matches on it to separate prune instincts from
// scope-support instincts.
const pruneCandidateActionMarker = "(prune candidate)"

// DeriveRetrievalHints classifies a slice of retrieval-domain instincts
// into boost scopes and prune chunk IDs. PURE over its input — no I/O,
// no firewall screen — so the boost/prune logic is unit-testable in
// isolation. The firewall screen is applied by Hints on top of this.
//
// Classification rules (mirror the extraction worker's emission shape in
// internal/instinct/extract.go ExtractRetrievalDomain):
//
//   - Prune candidate: a retrieval instinct whose Action carries the
//     "(prune candidate)" marker and whose Trigger.StepID holds the
//     chunk ID. Only candidate/active/promoted rows count (a retired
//     prune candidate has been resolved). Status is the worker's signal
//     that the chunk is still dead.
//   - Boost scope: an ACTIVE or PROMOTED scope-support retrieval
//     instinct — Trigger.RepoScope set, support-weighted confidence
//     crossed the active threshold. Candidate rows are too weak to act
//     on; retired rows have decayed out.
//
// Non-retrieval-domain rows, retired rows (except as noted), and rows
// with empty triggers are ignored.
func DeriveRetrievalHints(instincts []*persistence.Instinct) *RetrievalHints {
	hints := &RetrievalHints{
		BoostScopes:   map[string]struct{}{},
		PruneChunkIDs: map[string]struct{}{},
	}
	for _, in := range instincts {
		if in == nil || in.Domain != persistence.InstinctDomainRetrieval {
			continue
		}
		tr := parseRetrievalTrigger(in.Trigger)
		isPrune := strings.Contains(in.Action, pruneCandidateActionMarker)
		switch {
		case isPrune:
			// Prune candidate. Chunk ID lives in the step_id trigger
			// field (the worker reuses it as the chunk key). A retired
			// prune instinct is no longer a candidate.
			if in.Status == persistence.InstinctStatusRetired {
				continue
			}
			if tr.StepID != "" {
				hints.PruneChunkIDs[tr.StepID] = struct{}{}
			}
		case tr.RepoScope != "":
			// Scope-support boost. Only act once the instinct has
			// corroborated enough to be active/promoted; candidate /
			// retired rows are not acted on.
			if in.Status == persistence.InstinctStatusActive ||
				in.Status == persistence.InstinctStatusPromoted {
				hints.BoostScopes[tr.RepoScope] = struct{}{}
			}
		}
	}
	return hints
}

// retrievalTrigger is the subset of the instinct trigger Consumer C
// reads. Mirrors instinct.Trigger's JSON tags; redeclared here to avoid
// a memory→instinct package dependency.
type retrievalTrigger struct {
	Role      string `json:"role,omitempty"`
	StepID    string `json:"step_id,omitempty"`
	RepoScope string `json:"repo_scope,omitempty"`
}

func parseRetrievalTrigger(raw json.RawMessage) retrievalTrigger {
	var tr retrievalTrigger
	if len(raw) == 0 {
		return tr
	}
	_ = json.Unmarshal(raw, &tr)
	return tr
}
