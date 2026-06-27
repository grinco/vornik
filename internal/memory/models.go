package memory

import "time"

// MemoryChunk represents a chunk of text stored in project memory.
type MemoryChunk struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	TaskID      string    `json:"task_id"`
	ArtifactID  string    `json:"artifact_id"`
	SourceName  string    `json:"source_name"`
	ChunkIndex  int       `json:"chunk_index"`
	Content     string    `json:"content"`
	ContentHash string    `json:"content_hash"`
	Embedding   []float32 `json:"embedding,omitempty"`
	CreatedAt   time.Time `json:"created_at"`

	// Document-extraction provenance (added 2026-05-21 alongside the
	// extracted_documents table). When this chunk was produced by the
	// document-ingest path these fields point at the source extracted
	// document + the section within it; downstream retrieval surfaces
	// these in citations ("from Chapter 4 of <title>"). Both NULL for
	// legacy chunks ingested via the markdown-OUTPUT path — that
	// preserves the existing ingest pipeline untouched.
	DerivedFromExtractedDocumentID string `json:"derived_from_extracted_document_id,omitempty"`
	DerivedFromSectionID           string `json:"derived_from_section_id,omitempty"`
}

// SearchResult holds a single result from a memory search query.
type SearchResult struct {
	ChunkID    string  `json:"chunk_id"`
	ProjectID  string  `json:"project_id"`
	TaskID     string  `json:"task_id"`
	SourceName string  `json:"source_name"`
	Content    string  `json:"content"`
	Score      float64 `json:"score"`
	// ContentClass is the chunk's class label (research / spec /
	// decision / commit_msg / diagnostic / external_fetch / summary /
	// unclassified). Populated by the search SQL so role-based
	// boosting can adjust ranking without a second DB hop. Empty when
	// older repositories return chunks without this column.
	ContentClass string `json:"content_class,omitempty"`
	// IsAlive carries the URL liveness verdict for this chunk's
	// referenced URLs. Tri-valued: nil = never checked (legacy
	// chunks or chunk contains no URL); true = at least one HEAD
	// recheck succeeded; false = the URLs in this chunk were dead
	// at last check. Consuming agents can prefer alive hits and warn
	// when only dead ones survive. Populated by
	// URLLivenessChecker; see internal/memory/liveness.go.
	IsAlive *bool `json:"is_alive,omitempty"`
	// LastCheckedAt is the timestamp of the most recent URL liveness
	// recheck. nil = never checked. Useful for surfacing freshness
	// ("alive last week" vs "alive 2 minutes ago") to downstream
	// agents.
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`
	// RepoScope is the chunk's repo-scope token (migration 75).
	// Empty string = uncategorized (DB NULL — kept visible during
	// the migration window unless the caller asked for strict
	// scope filtering). Surface this on the operator UI so a hit
	// under a scope filter can be visually disambiguated as
	// "matched my scope", "cross-cutting (*)", or "uncategorized
	// (NULL leak-through)".
	RepoScope string `json:"repo_scope,omitempty"`
	// PolicyProof carries the firewall's evaluation decision for
	// this chunk. Non-nil only on results returned by
	// RecallWithContext (the firewall-aware retrieval path); the
	// legacy Search / SearchWithOptions surfaces leave it nil so
	// existing callers stay shape-compatible.
	// See https://docs.vornik.io
	// § "PolicyProof".
	PolicyProof *PolicyProofWire `json:"policy_proof,omitempty"`
	// PolicyWarning is populated under EnforcementAdvisory when
	// the evaluator decided to block this chunk but the daemon's
	// mode kept it in the result set. Empty under EnforcementEnforce
	// (blocked chunks don't surface) and EnforcementOff (no
	// warning attached even when the evaluator would have blocked).
	PolicyWarning string `json:"policy_warning,omitempty"`
}

// PolicyProofWire is the JSON-wire shape of the firewall's
// PolicyProof. Decoupled from the firewall package's struct so
// the memory package doesn't carry a hard build-time dep on
// internal/memoryfirewall. The fields are field-compatible with
// memoryfirewall.PolicyProof.
type PolicyProofWire struct {
	ChunkID        string                    `json:"chunk_id"`
	Decision       string                    `json:"decision"`
	EvaluatedAt    time.Time                 `json:"evaluated_at"`
	PolicyDigest   string                    `json:"policy_digest"`
	RequestContext PolicyProofRequestContext `json:"request_context"`
}

// PolicyProofRequestContext is the wire-side view of the
// firewall's RequestContext. Lives here so callers can decode
// the proof without depending on internal/memoryfirewall.
type PolicyProofRequestContext struct {
	TenantID   string `json:"tenant_id,omitempty"`
	OperatorID string `json:"operator_id,omitempty"`
	Role       string `json:"role,omitempty"`
	Purpose    string `json:"purpose,omitempty"`
	TraceID    string `json:"trace_id,omitempty"`
}
