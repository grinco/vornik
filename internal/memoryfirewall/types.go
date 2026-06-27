// Package memoryfirewall implements the Policy-Aware Memory
// Firewall (LLD: https://docs.vornik.io).
//
// The firewall attaches six dimensions of policy metadata to
// every chunk in project_memory_chunks (provenance, sensitivity,
// expiry, tenant_id, permitted_roles, allowed_purposes) and
// makes retrieval return a PolicyProof alongside each chunk.
//
// Design choices flagged for readers:
//
//   - The evaluator is deterministic + in-process. No LLM in
//     the hot path; "is this PII?" classification belongs in
//     internal/memory/class.go which already runs async.
//   - Policy fields are nullable on the chunk row. NULL means
//     "default for this chunk's provenance source" — populated
//     lazily via DefaultPolicyForSource. Zero-config deployments
//     get sensible behaviour without operator intervention.
//   - The audit write is non-blocking: a buffered channel
//     decouples the recall hot path from the DB write. The
//     writer goroutine drains at 100ms intervals or on buffer
//     full and writes in 50-row batches.
//   - Enforcement mode is a daemon-level config switch
//     (off / advisory / enforce). Three-mode rollout keeps
//     operators safe during policy authoring.
package memoryfirewall

import "time"

// SensitivityTier enumerates the four-level sensitivity scale.
// Tiers map to default permitted_roles + allowed_purposes via
// sensitivity.go's DefaultsForTier helper. Per-chunk overrides
// win when set.
type SensitivityTier string

const (
	// SensitivityPublic — safe to surface to any role, any
	// purpose, any third-party model. External-fetch chunks
	// default here.
	SensitivityPublic SensitivityTier = "public"
	// SensitivityInternal — default tier. Operator + workflow
	// roles see it; not safe to ship outside the daemon's
	// trust boundary without an explicit allow.
	SensitivityInternal SensitivityTier = "internal"
	// SensitivityConfidential — operator-eyes-only by default;
	// workflow roles can be added via permitted_roles. Never
	// flows to external embedders without explicit operator
	// opt-in.
	SensitivityConfidential SensitivityTier = "confidential"
	// SensitivityRestricted — strictest tier. Retrieval
	// requires an operator_id on the RequestContext; the
	// evaluator blocks anonymous (autonomy-driven) requests.
	// Credentials, PII, secret recovery codes land here.
	SensitivityRestricted SensitivityTier = "restricted"
)

// ProvenanceSource enumerates how a chunk landed in the store.
// The defaults table maps each source to a baseline policy;
// operators can override per-chunk.
type ProvenanceSource string

const (
	ProvenanceOperatorCorrection ProvenanceSource = "operator_correction"
	ProvenanceWorkflowOutput     ProvenanceSource = "workflow_output"
	ProvenanceChatTurn           ProvenanceSource = "chat_turn"
	ProvenanceIngestedArtifact   ProvenanceSource = "ingested_artifact"
	ProvenanceSelfConsolidated   ProvenanceSource = "self_consolidated"
	ProvenanceExternalFetch      ProvenanceSource = "external_fetch"
	ProvenanceCompanionRemember  ProvenanceSource = "companion_remember"
	// ProvenanceUnknown is the fall-through when a chunk's
	// origin can't be classified. Lands a conservative policy
	// (internal sensitivity, all roles, all purposes) so the
	// chunk stays retrievable while operators investigate.
	ProvenanceUnknown ProvenanceSource = "unknown"
)

// Purpose enumerates retrieval intents. The default is
// PurposeOperational ("the workflow needs this to make a
// decision"); explicit purposes like PurposeTrainingData
// must be supplied by callers that plan to feed retrievals
// into a fine-tune pipeline. Chunks marked with a non-default
// allowed_purposes set block all other purposes — the
// EU AI Act Art 10 data-governance line.
type Purpose string

const (
	PurposeOperational      Purpose = "operational"
	PurposeTrainingData     Purpose = "training_data"
	PurposeAuditReview      Purpose = "audit_review"
	PurposeComplianceExport Purpose = "compliance_export"
)

// EvaluationDecision is the per-chunk outcome from the
// evaluator. Decisions are exhaustive — every blocked
// retrieval picks one specific reason so the UI can group
// blocks by class.
type EvaluationDecision string

const (
	DecisionAllow                  EvaluationDecision = "allow"
	DecisionBlockExpired           EvaluationDecision = "block_expired"
	DecisionBlockTenantMismatch    EvaluationDecision = "block_tenant_mismatch"
	DecisionBlockRoleNotPermitted  EvaluationDecision = "block_role_not_permitted"
	DecisionBlockPurposeNotAllowed EvaluationDecision = "block_purpose_not_allowed"
	DecisionBlockSensitivityTier   EvaluationDecision = "block_sensitivity_tier"
)

// EnforcementMode controls what the firewall does with blocked
// chunks at the retrieval seam. Three modes for safe rollout.
type EnforcementMode string

const (
	// EnforcementOff — evaluator runs, audit is written, but
	// blocked chunks STILL surface in the recall result.
	// Initial-rollout mode for collecting baseline data.
	EnforcementOff EnforcementMode = "off"
	// EnforcementAdvisory — evaluator runs, blocked chunks
	// surface with a PolicyWarning on each result.
	// Operator-visible but workflows keep running.
	EnforcementAdvisory EnforcementMode = "advisory"
	// EnforcementEnforce — blocked chunks do NOT surface in
	// the recall result. Production-grade gate.
	EnforcementEnforce EnforcementMode = "enforce"
)

// Provenance captures deposit-time origin metadata for one
// chunk. The audit trail consumes this; the evaluator uses
// only TrustLevel as an input dimension.
type Provenance struct {
	Source     ProvenanceSource `json:"source"`
	ProducerID string           `json:"producer_id,omitempty"`
	TrustLevel int              `json:"trust_level"`
	SourceURL  string           `json:"source_url,omitempty"`
}

// Policy is the per-chunk metadata attached at ingest. Six
// independent dimensions; the evaluator AND-combines them.
type Policy struct {
	Provenance      Provenance      `json:"provenance"`
	Sensitivity     SensitivityTier `json:"sensitivity"`
	ExpiresAt       *time.Time      `json:"expires_at,omitempty"`
	TenantID        string          `json:"tenant_id,omitempty"`
	PermittedRoles  []string        `json:"permitted_roles,omitempty"`
	AllowedPurposes []Purpose       `json:"allowed_purposes,omitempty"`
}

// RequestContext is the retrieval-side input. Constructed by
// the caller (dispatcher tool / executor / admin API) and
// passed through memory.Searcher.RecallWithContext. Backwards-
// compatible: callers omitting fields get evaluator defaults.
type RequestContext struct {
	TenantID   string  `json:"tenant_id,omitempty"`
	OperatorID string  `json:"operator_id,omitempty"`
	Role       string  `json:"role,omitempty"`
	Purpose    Purpose `json:"purpose,omitempty"`
	TraceID    string  `json:"trace_id,omitempty"`
}

// PolicyProof is returned alongside every allowed chunk in a
// recall response. Carries the evaluator's decision trace so a
// workflow that later cites the chunk can serve the proof as
// part of its output.
type PolicyProof struct {
	ChunkID        string             `json:"chunk_id"`
	Decision       EvaluationDecision `json:"decision"`
	EvaluatedAt    time.Time          `json:"evaluated_at"`
	PolicyDigest   string             `json:"policy_digest"`
	RequestContext RequestContext     `json:"request_context"`
}

// Chunk is the firewall's view of one row from
// project_memory_chunks. Carries enough state for the evaluator
// to decide without a DB round-trip. Sourced by the memory
// Repository's read methods (which load the policy columns).
//
// Kept as a flat struct (not interface) because the evaluator
// runs per-chunk in the recall hot path; allocation cost
// dominates over abstraction cleanliness here.
type Chunk struct {
	ID     string
	Policy Policy
	// Digest is the canonical sha256 of Policy. Recomputed
	// whenever Policy is mutated. The audit row stores this
	// alongside the decision so external verifiers can prove
	// the policy revision the decision was made under.
	Digest string
}
