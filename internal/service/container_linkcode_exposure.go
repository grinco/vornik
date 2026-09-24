package service

import (
	"vornik.io/vornik/internal/chatauth"
)

// linkCodeExposureGuard returns the ONE scrub-and-burn guard both chat channels
// share (§5.2b).
//
// One instance, for the reason the identity shim is one instance: two would
// count into two counters, and the metric this publishes is the evidence that
// codes are reaching channels at all. A per-channel guard would split that
// population and make a rising number look like two smaller flat ones.
//
// Built even when no accounts service exists. The burn half needs a store; the
// SCRUB half does not, and a deployment without identity wiring should not be
// the one deployment that forwards a credential to an LLM.
func (c *Container) linkCodeExposureGuard() *chatauth.ExposureGuard {
	if c == nil {
		return nil
	}
	c.exposureGuardOnce.Do(func() {
		var burner chatauth.Burner
		if acc := c.accountsService(); acc != nil {
			burner = acc
		}
		c.exposureGuard = chatauth.NewExposureGuard(
			burner,
			// observabilityRegisterer, not observabilityRegistry: this runs from
			// initTelegram, before observability is wired, and a typed-nil
			// Registerer crash-looped startup (2026-09-24).
			chatauth.NewExposureMetrics(c.observabilityRegisterer()),
			c.Logger,
		)
	})
	return c.exposureGuard
}
