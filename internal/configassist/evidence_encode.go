package configassist

import (
	"encoding/json"
	"fmt"
)

// ErrEvidenceTooLarge is returned when a proposal's load-bearing evidence —
// the class, the read set, the base hash, the actor and the request ids —
// does not fit inside the ledger's per-field limit even with every optional
// field shed. The request is refused rather than filed with a record its
// readers cannot use (audit 2026-09-15 CA-07).
var ErrEvidenceTooLarge = fmt.Errorf("configassist: proposal evidence exceeds the ledger field limit even after shedding optional fields")

// encodeEvidence serializes rec so the result is BOTH under limit bytes and
// valid JSON.
//
// The old code ran the marshalled bytes through a prose truncator, which is
// how a 65,295-byte record of invalid JSON reached the ledger under a 65,536
// limit. Every downstream reader of Evidence parses it, and each one failed
// differently and silently: the apply path read a malformed record as an
// EMPTY read set and skipped the stale-base check entirely, the class-E
// counter could not see the class and stopped counting the row, and the
// workspace applier rejected the proposal outright. Fail-open, fail-quiet
// and fail-closed, from one truncation.
//
// Shedding is ordered least-load-bearing first. Anything dropped is named in
// rec.Shed, so a reader can tell "this proposal had no tool calls" from
// "this proposal's tool calls did not fit".
func encodeEvidence(rec *EvidenceRecord, limit int) ([]byte, error) {
	if out, err := json.Marshal(rec); err == nil && len(out) <= limit {
		return out, nil
	}
	// Each step drops one optional field, records that it did, and retries.
	// Intent goes last of the prose fields because IntentHash preserves the
	// request's identity once it does: the idempotency binding of CA-16
	// keeps working on a record whose intent was shed.
	steps := []struct {
		name string
		drop func(*EvidenceRecord)
	}{
		{"tool_calls", func(r *EvidenceRecord) { r.ToolCalls = nil }},
		{"consult", func(r *EvidenceRecord) { r.Consult = nil }},
		{"measurements", func(r *EvidenceRecord) { r.Measurements = "" }},
		{"permission_diff", func(r *EvidenceRecord) { r.Permissions = nil }},
		{"intent", func(r *EvidenceRecord) { r.Intent = "" }},
		{"verdict", func(r *EvidenceRecord) { r.Verdict = nil }},
		{"ignored_deletions", func(r *EvidenceRecord) { r.Ignored = nil }},
		// Touches carry the per-key reasons. The BUNDLE class in rec.Class
		// is what the ceiling and the counter read, and it survives.
		{"touches", func(r *EvidenceRecord) { r.Touches = nil }},
	}
	for _, step := range steps {
		step.drop(rec)
		rec.Shed = append(rec.Shed, step.name)
		out, err := json.Marshal(rec)
		if err != nil {
			return nil, err
		}
		if len(out) <= limit {
			return out, nil
		}
	}
	// Everything optional is gone and it still does not fit: what remains is
	// the read set (one entry per touched file) and the identifiers. A
	// proposal this wide is refused, not silently weakened.
	return nil, ErrEvidenceTooLarge
}
