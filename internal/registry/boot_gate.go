package registry

import (
	"fmt"
	"strings"
)

// BootRejectedError reports a daemon that refused to start because the deployed
// tree contained files it could not load — loader-validator agreement design
// §13.1.
//
// It carries the per-file diagnosis rather than a count, for the reason
// ReloadRejectedError does: an operator told "refused to start" and nothing else
// has to go and find out which file and why, which is the gap §11 closed and
// this must not reopen.
type BootRejectedError struct {
	Rejected []RejectedFile
}

func (e *BootRejectedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "daemon start REFUSED: %d file(s) in the deployed tree could not be loaded, "+
		"so starting would serve a configuration missing what they define.\n", len(e.Rejected))
	for _, r := range e.Rejected {
		fmt.Fprintf(&b, "\n  %s %s:\n%s", r.Kind, r.Path, indentLines(r.Error, "    "))
	}
	// Name the key. An operator meeting this at 03:00 needs the way out in the
	// message, not in a design document.
	b.WriteString("\nThis daemon is configured with registry.refuse_start_on_rejected_project: true. " +
		"Fix the file(s) above, or set that key to false to start with the affected project(s) " +
		"dark — `vornikctl doctor` reports them at ERROR either way " +
		"(loader-validator agreement design §13.1).")
	return b.String()
}

// BootRejectionError decides whether rejected files stop the daemon.
//
// THE DEFAULT IS TO START. A degraded boot is loud — the loader prints §11.1's
// diagnosis and project_config_skew reports it at ERROR — and refusing by
// default would turn one dark project into a dark deployment, which is a larger
// outage than the one it prevents.
//
// It is configurable because that judgement is not universal: a regulated
// deployment may prefer a daemon that does not start over one serving a
// configuration it cannot fully load, and that is exactly the kind of thing such
// a customer states in advance.
//
// Note the asymmetry with the RELOAD refusal (§12), which is unconditional:
// there, refusing can never leave the operator worse off because the running
// configuration keeps serving. Boot has nothing to keep. Same defect, different
// available fallback, different default.
func BootRejectionError(rejected []RejectedFile, refuseStart bool) error {
	if len(rejected) == 0 || !refuseStart {
		return nil
	}
	return &BootRejectedError{Rejected: append([]RejectedFile(nil), rejected...)}
}

// Rejections returns the files the ACTIVE tree refused, or nil. The boot path
// reads this after the initial load; stagedRejections is its reload-time twin.
func (r *Registry) Rejections() []RejectedFile {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.active == nil || r.active.index == nil {
		return nil
	}
	return append([]RejectedFile(nil), r.active.index.Rejected...)
}
