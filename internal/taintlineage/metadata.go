package taintlineage

import "encoding/json"

// CheckpointDecisionKind is the discriminator stamped on an untrusted-review
// checkpoint's metadata (decision.kind == "untrusted_review"), so the answer
// handlers (API + UI) branch onto the admin-class allow/cancel path (§4.5).
// Sibling of the budget checkpoint's "budget" kind.
const CheckpointDecisionKind = "untrusted_review"

// LatchMarkerKind is the discriminator on the task_message the admin `allow`
// answer records to latch a reviewed source set (D7). It is NOT a checkpoint —
// it is a durable marker the gate reads on a later run to suppress the
// content-driven re-park when the lineage source-set hash is unchanged.
const LatchMarkerKind = "taint_latch"

// checkpointMeta is the decision envelope shared by the budget/taint
// checkpoints (see task_budget_gate's parkForBudget for the sibling shape).
type checkpointMeta struct {
	Decision struct {
		Kind          string `json:"kind"`
		SourceSetHash string `json:"source_set_hash"`
	} `json:"decision"`
}

// IsTaintReviewCheckpointMeta reports whether a checkpoint message's metadata
// is an untrusted-review decision (decision.kind == "untrusted_review").
func IsTaintReviewCheckpointMeta(meta []byte) bool {
	if len(meta) == 0 {
		return false
	}
	var m checkpointMeta
	if err := json.Unmarshal(meta, &m); err != nil {
		return false
	}
	return m.Decision.Kind == CheckpointDecisionKind
}

// CheckpointSourceSetHash pulls the reviewed source-set hash out of an
// untrusted-review checkpoint's metadata, so the answer handler can record it
// as the D7 latch. Returns "" when absent.
func CheckpointSourceSetHash(meta []byte) string {
	if len(meta) == 0 {
		return ""
	}
	var m checkpointMeta
	if err := json.Unmarshal(meta, &m); err != nil {
		return ""
	}
	if m.Decision.Kind != CheckpointDecisionKind {
		return ""
	}
	return m.Decision.SourceSetHash
}

// LatchMarkerMetadata builds the metadata blob for a latch marker task_message
// recording a reviewed source-set hash (D7).
func LatchMarkerMetadata(hash string) []byte {
	b, _ := json.Marshal(map[string]any{
		"kind":            LatchMarkerKind,
		"source_set_hash": hash,
	})
	return b
}

// ParseLatchHash extracts the source-set hash from a latch-marker task_message's
// metadata. Returns (hash, true) only when the metadata is a taint_latch marker
// with a non-empty hash.
func ParseLatchHash(meta []byte) (string, bool) {
	if len(meta) == 0 {
		return "", false
	}
	var m struct {
		Kind          string `json:"kind"`
		SourceSetHash string `json:"source_set_hash"`
	}
	if err := json.Unmarshal(meta, &m); err != nil {
		return "", false
	}
	if m.Kind != LatchMarkerKind || m.SourceSetHash == "" {
		return "", false
	}
	return m.SourceSetHash, true
}
