package api

import "context"

// checkWebWritesInsecure surfaces web.writes=insecure as a persistent degraded
// signal (LLD 2026-07-21-supervised-web-write-actions §Write authorization I2):
// insecure bypasses the per-project write allowlist for supervised web writes,
// so it must never be left on in production. off/on are healthy; off is the
// safe default. Every other web-write gate (human approval, SSRF, request
// interception, no-evasion) still applies under insecure — only the domain
// allowlist is bypassed — but the mode is a deliberate dev/testing escape hatch
// and the operator must be able to see at a glance that it is active.
func (h *DoctorHandlers) checkWebWritesInsecure(_ context.Context, _ bool) DoctorCheck {
	name := "web_writes_mode"
	switch h.webWritesMode {
	case "insecure":
		return DoctorCheck{
			Name:   name,
			Status: "WARNING",
			Message: "web.writes=insecure — supervised web writes BYPASS the per-project write allowlist " +
				"(any host may be targeted; human approval + SSRF + no-evasion still apply). " +
				"This is a dev/testing escape hatch — set web.writes=on for production.",
		}
	case "on":
		return DoctorCheck{Name: name, Status: "OK", Message: "web.writes=on — supervised web writes gated by each project's write_allowlist (deny-by-default)."}
	default:
		return DoctorCheck{Name: name, Status: "OK", Message: "web.writes=off — supervised web writes are disabled (default)."}
	}
}
