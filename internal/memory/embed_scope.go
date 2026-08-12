package memory

import "fmt"

// EmbedScope is who an embedding call is billed to.
//
// It is a REQUIRED argument to Embed / EmbedQuery rather than a context value,
// and that is deliberate. The obvious alternative — mirroring
// chat.WithCallSite and stamping attribution onto the context — was rejected
// during design review on evidence from this codebase: graph.completeWithRetry
// already overwrites the ctx call-site with its own label, so a context value
// can be silently rewritten by a layer between the caller and the provider.
// Attribution that a lower layer can rewrite is not attribution.
//
// Making it a parameter also means the compiler, not a reviewer's memory,
// enforces that every caller states what it bills — including the next caller
// somebody adds. See
// https://docs.vornik.io §4.1.
type EmbedScope struct {
	// ProjectID is the project the spend belongs to. It may be empty ONLY for
	// EmbedCallSiteInfraProbe — daemon-level work that genuinely has no
	// project. Any other empty ProjectID is a programming error, because
	// unattributed project spend is the defect this design removes.
	ProjectID string
	// CallSite names the path doing the spending ("memory.ingest",
	// "skill.preflight", …). Always required: a project alone cannot say which
	// path spent the money, which is the question an operator asks first.
	CallSite string
}

// RoleEmbedder is the task_llm_usage.role for embedding spend. Exported so the
// wiring site names the component's own identity rather than a string literal.
const RoleEmbedder = "memory_embedder"

// Embed call sites. Declared as constants so a future audit of call-site
// labels can resolve them from the AST — the callsite-accounting guard learned
// this the hard way when `narrator.line`, declared as a const, was invisible to
// a literal-only grep and dropped out of a hand audit.
const (
	// EmbedCallSiteIngest is the memory ingest worker embedding chunk content.
	EmbedCallSiteIngest = "memory.ingest"
	// EmbedCallSiteSearchQuery is query-time embedding on the retrieval path.
	EmbedCallSiteSearchQuery = "memory.search_query"
	// EmbedCallSiteSkillPreflight is the propose-time near-duplicate check.
	EmbedCallSiteSkillPreflight = "skill.preflight"
	// EmbedCallSiteKGResolve is knowledge-graph entity resolution: the
	// resolver's name-vector shortlist, the KG searcher's entity lookup, and
	// the extraction pipeline's candidate embedding. Project work, so it is
	// never billed to infrastructure — graph.EmbedFn carries a projectID for
	// exactly this reason.
	EmbedCallSiteKGResolve = "memory.kg_resolve"
	// EmbedCallSiteInfraProbe is a reachability probe: a real, billable call
	// with no project to charge. It is recorded rather than skipped — "too
	// small to bill" is the judgement that left 1.29M tokens invisible — and
	// surfaces as infrastructure spend, not against a customer's project.
	EmbedCallSiteInfraProbe = "infra.probe"
)

// Validate reports whether the scope can be billed.
//
// Returns an error rather than falling back to a default: Embed treats an
// invalid scope as a programming error, distinct from the (nil, nil) degrade it
// returns for a flaky endpoint. Collapsing the two would let an unattributed
// caller ship looking exactly like a network blip.
func (s EmbedScope) Validate() error {
	if s.CallSite == "" {
		return fmt.Errorf("embed scope: CallSite is required (project %q)", s.ProjectID)
	}
	if s.ProjectID == "" && s.CallSite != EmbedCallSiteInfraProbe {
		return fmt.Errorf("embed scope: ProjectID is required for call site %q", s.CallSite)
	}
	return nil
}
