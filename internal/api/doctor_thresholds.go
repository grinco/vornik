package api

import "vornik.io/vornik/internal/config"

// doctorThresholds returns the resolved bounds every check judges against.
//
// A handler constructed without SetServerConfig — a test, an early-boot
// surface — gets the COMPILED DEFAULTS rather than a zero value. Zero is a real
// threshold (0% warns on anything, a zero sample floor judges on one call), so
// an unwired handler falling through to zero would silently invert several
// checks. Nothing here reports "configured", because nothing was.
func (h *DoctorHandlers) doctorThresholds() config.ResolvedDoctorThresholds {
	if h == nil || !h.thresholdsKnown {
		return config.DoctorThresholds{}.Resolve()
	}
	return h.thresholds
}
