package configassist

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// IntentFingerprint is the stable digest of what a request ASKED FOR. It is
// stored on the proposal's evidence so a later retry can be told apart from
// a different request that happens to reuse the same idempotency key
// (audit 2026-09-15 CA-16).
//
// Whitespace is normalised so a reformatted-but-identical intent still
// replays; nothing else is. The digest is of the intent only — project and
// actor are compared from their own columns, which no caller can reshape.
func IntentFingerprint(intent string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(intent), " ")))
	return hex.EncodeToString(sum[:])
}

// idempotencyMismatch returns "" when the stored proposal is a replay of req,
// or a short human reason naming the FIRST field that diverged.
//
// An older row that predates the fingerprint (empty IntentHash) is compared
// on project and credential alone: those are recorded columns, so the check
// still binds the key to its tenant and its caller. It does not silently
// widen to "anything matches" — that was the defect.
func idempotencyMismatch(p *persistence.ControlPlaneProposal, req Request) string {
	if p == nil {
		return ""
	}
	if p.ProjectID != req.ProjectID {
		return "it belongs to project " + p.ProjectID
	}
	if p.ActorCredentialID != req.Actor.CredentialID {
		return "it was filed by a different credential"
	}
	if stored := storedIntentHash(p.Evidence); stored != "" && stored != IntentFingerprint(req.Intent) {
		return "it was filed for a different intent"
	}
	return ""
}

// storedIntentHash reads the fingerprint back out of the evidence envelope,
// returning "" for a record that has none or cannot be parsed. It must not
// fail the caller: an unreadable envelope falls back to the column checks
// above, which are stricter than the old key-only behaviour either way.
func storedIntentHash(evidence string) string {
	var ev struct {
		IntentHash string `json:"intent_hash"`
	}
	if err := json.Unmarshal([]byte(evidence), &ev); err != nil {
		return ""
	}
	return ev.IntentHash
}

// hydrateFromProposal rebuilds a Result's presentation fields from a stored
// proposal, so an idempotent REPLAY shows what the original request showed.
//
// A replay used to return the proposal and the Duplicate flag and nothing
// else, leaving class, judge verdict, diff, permission diff and the
// measurement indicator empty — so the retry an operator makes after an
// uncertain response rendered a blank result for a proposal that is perfectly
// well described in the ledger (re-audit 2026-09-15, CA-13). The console's
// whole retry story depends on a replay being both safe and useful.
//
// Every field is read back from the record; none is recomputed, so a replay
// cannot disagree with what was filed.
func hydrateFromProposal(res *Result, p *persistence.ControlPlaneProposal) {
	if res == nil || p == nil {
		return
	}
	res.Diff = p.Diff
	var ev EvidenceRecord
	if err := json.Unmarshal([]byte(p.Evidence), &ev); err != nil {
		return // an unreadable envelope still replays the proposal itself
	}
	res.Class = ev.Class
	res.Touches = ev.Touches
	res.Verdict = ev.Verdict
	res.Permissions = ev.Permissions
	res.HasEvidence = ev.HasMeasurement
	res.Evidence = ev.Measurements
	res.Ignored = ev.Ignored
	if ev.RequestID != "" {
		// The ORIGINAL request's id: a replay is that request's answer.
		res.RequestID = ev.RequestID
	}
	res.AutoApplied = p.Status == persistence.ProposalStatusApplied
}
