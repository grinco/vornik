package memory

import "sort"

// roleClassAffinity records which content classes each consumer role
// most wants to retrieve. Distinct from roleClassMap in class.go,
// which captures what each PRODUCER role tends to write — consumers
// often want a different cross-section. Example: a tester producing
// diagnostics consumes specs and decisions when planning.
//
// Classes carry weights in [0,1]; higher = stronger preference. The
// search-side boost multiplies these against a fixed amplitude
// (DefaultClassBoostAmplitude), so a 1.0 weight on a class doubles
// that class's score in the ranking.
//
// NOTE: this map is GLOBAL — keyed by role name across ALL swarms.
// Changes here affect every project whose swarm declares that role.
// The "writer" entry intentionally sets ClassDecision=1.0 because
// `decision` is the authoritative class for operator-verified facts
// (e.g. a CV promoted via memory_correct). A 0.60-raw decision chunk
// scores 0.60×2.0=1.20 vs a 0.70-raw research chunk 0.70×1.7=1.19,
// so the authoritative resume always outranks a higher-raw non-decision
// result. Acceptable global footprint: `decision` is authoritative
// everywhere — this just makes the writer role reflect that.
var roleClassAffinity = map[string]map[ContentClass]float64{
	// Producers of research consume prior research + specs to avoid
	// re-deriving conclusions.
	"researcher": {ClassResearch: 0.8, ClassSpec: 0.6, ClassDecision: 0.4},
	"scout":      {ClassResearch: 0.8, ClassExternalFetch: 0.6},
	// writer: ClassDecision raised to 1.0 so authoritative decision-class
	// chunks (e.g. the operator-verified resume) outrank higher-raw-score
	// non-decision results — the resume-anchoring invariant (spec B).
	"writer": {ClassResearch: 0.7, ClassSpec: 0.7, ClassDecision: 1.0},

	// Analysts produce specs but read decisions to align on direction.
	"analyst": {ClassDecision: 0.9, ClassSpec: 0.7, ClassResearch: 0.5},

	// Coders read specs + decisions; commit_msgs are weakest because
	// they're noisy and short-lived.
	"coder":       {ClassSpec: 0.9, ClassDecision: 0.7, ClassCommitMsg: 0.3},
	"developer":   {ClassSpec: 0.9, ClassDecision: 0.7, ClassCommitMsg: 0.3},
	"engineer":    {ClassSpec: 0.9, ClassDecision: 0.7, ClassCommitMsg: 0.3},
	"implementer": {ClassSpec: 0.9, ClassDecision: 0.7, ClassCommitMsg: 0.3},

	// Reviewers/architects want decisions + specs to compare against
	// what they're judging.
	"reviewer":  {ClassDecision: 0.9, ClassSpec: 0.7},
	"architect": {ClassDecision: 0.9, ClassSpec: 0.8, ClassResearch: 0.5},

	// Testers/QA pull diagnostics from recent failures + specs to
	// know expected behaviour.
	"tester":   {ClassDiagnostic: 0.9, ClassSpec: 0.7},
	"qa":       {ClassDiagnostic: 0.9, ClassSpec: 0.7},
	"verifier": {ClassDiagnostic: 0.8, ClassSpec: 0.7, ClassDecision: 0.6},

	// Orchestration roles read everything but bias toward decisions
	// + specs so plans stay grounded in approved direction.
	"lead":        {ClassDecision: 0.8, ClassSpec: 0.7, ClassResearch: 0.5},
	"feasibility": {ClassResearch: 0.7, ClassDecision: 0.6, ClassSpec: 0.5},

	// Vision (OCR) doesn't actively read memory in a typical step,
	// but when it does retrieval (e.g. cross-referencing prior OCR
	// runs) it wants prior external_fetch + research chunks.
	"vision": {ClassExternalFetch: 0.8, ClassResearch: 0.5},

	// Trading swarm roles. Strategists need recent fills + decisions
	// to size positions; risk-officers compare proposals against
	// prior rulings; executors just need the latest decision to
	// know whether a trade was approved.
	"strategist":   {ClassDecision: 0.9, ClassSpec: 0.7, ClassCommitMsg: 0.6, ClassDiagnostic: 0.4},
	"risk-officer": {ClassDecision: 0.9, ClassSpec: 0.8},
	"executor":     {ClassDecision: 0.9, ClassCommitMsg: 0.6},
}

// DefaultClassBoostAmplitude is the multiplier applied to a role's
// class-affinity weight before adding to the base score. Tuned so a
// strong affinity (weight=1.0) doubles a chunk's score, which is
// enough to lift class-matching chunks out of ties without
// overwhelming the underlying relevance signal.
const DefaultClassBoostAmplitude = 1.0

// applyRoleClassBoost re-orders results by adding a class-affinity
// bonus to each chunk's score. Pure function: no I/O, deterministic.
// Empty role → input unchanged so callers without RetrievalContext
// pay nothing.
//
// Boost formula:
//
//	score' = score * (1 + amplitude * affinity[class])
//
// Multiplicative rather than additive so it scales with the base
// relevance — a strong topical fit in the wrong class still beats
// a weak fit in the right class.
func applyRoleClassBoost(results []SearchResult, role string, amplitude float64) []SearchResult {
	if role == "" || len(results) < 2 {
		return results
	}
	affinity, ok := roleClassAffinity[role]
	if !ok || len(affinity) == 0 {
		return results
	}
	if amplitude <= 0 {
		amplitude = DefaultClassBoostAmplitude
	}

	boosted := make([]SearchResult, len(results))
	copy(boosted, results)
	for i := range boosted {
		w, ok := affinity[ContentClass(boosted[i].ContentClass)]
		if !ok {
			continue
		}
		boosted[i].Score *= 1 + amplitude*w
	}
	// Stable sort so ties preserve the upstream (RRF) order.
	sort.SliceStable(boosted, func(a, b int) bool {
		return boosted[a].Score > boosted[b].Score
	})
	return boosted
}
