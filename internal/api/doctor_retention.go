package api

import (
	"fmt"
	"sort"
	"strings"
)

// checkRetentionEnabled warns when the retention sweeper is off.
//
// WHY THIS IS A CHECK AND NOT A DEFAULT CHANGE. The sweeper ships disabled on
// purpose: flipping it on during an upgrade would delete data an operator never
// agreed to lose, and silent data loss is worse than a warned-about gap. The
// consequence is that a stock deployment keeps personal data INDEFINITELY, which
// does not satisfy GDPR Art 5(1)(e) — so the default is safe for the data and
// wrong for the compliance posture, and only the operator can resolve that.
//
// The warning is therefore deliberately loud and not dismissible. It names the
// article, says what the consequence is, and points at the profile to copy. A
// dismissible warning would be discovered during a supervisory-authority
// enquiry rather than before one.
//
// It also reports a HALF-CONFIGURED sweeper — enabled with every window unset —
// because that state looks configured in a config diff and prunes nothing.
//
// see LLD § https://docs.vornik.io §4.9
func (h *DoctorHandlers) checkRetentionEnabled() DoctorCheck {
	name := "retention_enabled"

	if !h.retentionKnown {
		return DoctorCheck{Name: name, Status: "SKIPPED",
			Message: "retention configuration not wired into the doctor"}
	}

	if !h.retentionEnabled {
		return DoctorCheck{
			Name:   name,
			Status: "WARNING",
			Message: "retention.enabled=false — this deployment keeps personal data INDEFINITELY, " +
				"which does not satisfy GDPR Art 5(1)(e) (storage limitation). The default is off " +
				"deliberately so an upgrade never deletes data you did not agree to lose, so only you " +
				"can close this. Start from the profile shipped alongside your config " +
				"(<config-dir>/configs/retention-recommended.yaml): copy the block into config.yaml, " +
				"set the windows to YOUR purposes, and set enabled: true.",
		}
	}

	// Enabled but nothing set prunes nothing while looking configured.
	if len(h.retentionWindows) == 0 {
		return DoctorCheck{
			Name:   name,
			Status: "WARNING",
			Message: "retention.enabled=true but every window is unset, so the sweeper runs and prunes " +
				"nothing — the same Art 5(1)(e) exposure as being switched off, with none of the " +
				"visibility. Set windows per data class; see <config-dir>/configs/retention-recommended.yaml.",
		}
	}

	set := make([]string, 0, len(h.retentionWindows))
	for k, days := range h.retentionWindows {
		set = append(set, fmt.Sprintf("%s=%dd", k, days))
	}
	sort.Strings(set)

	msg := fmt.Sprintf("sweeper on, %d window(s) configured (%s)",
		len(set), strings.Join(set, ", "))

	// Memory chunks are the row most operators under-think: an embedding of a
	// sentence about a person is personal data about that person, and this store
	// is designed to outlive its sources. An otherwise-configured sweeper that
	// omits it leaves the longest-lived personal data unbounded.
	if _, ok := h.retentionWindows["memory_chunks_days"]; !ok {
		return DoctorCheck{Name: name, Status: "WARNING",
			Message: msg + " — but memory_chunks_days is UNSET, so RAG memory derived from mail, " +
				"documents and chat is kept indefinitely. That is the longest-lived personal data in " +
				"the system and the one most often missed."}
	}

	return DoctorCheck{Name: name, Status: "OK", Message: msg}
}
