package api

import (
	"fmt"
	"net/http"
	"time"

	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
)

// verifyTradingHMAC enforces the trading-channel HMAC signature when
// the feature is wired (s.tradingAuthVerifier != nil). It returns true
// when the request was REJECTED — in which case it has already written
// the 401 and recorded the metric, and the caller MUST return without
// further processing. Returns false to mean "proceed" (either the
// feature is off, or the signature verified).
//
// Fail-closed: with the verifier wired, a missing, malformed, forged,
// stale, or replayed signature is a 401. Backward-compatible: with no
// verifier the function is a no-op so an unsigned rollout keeps
// working.
//
// body MUST be the exact bytes read off the wire (the same bytes the
// broker signed); for the bodyless GET state-replay path, pass the
// request body as read (empty), which the broker signs as an empty
// payload.
func (s *Server) verifyTradingHMAC(w http.ResponseWriter, r *http.Request, body []byte, endpoint string) bool {
	if s == nil || s.tradingAuthVerifier == nil {
		return false
	}
	if err := s.tradingAuthVerifier.Verify(r, body, time.Now()); err != nil {
		s.recordTradingIngestError(endpoint, "auth")
		// Don't echo the verifier's reason into the response: a
		// distinguishable "expired" vs "mismatch" vs "replay" message
		// is a small oracle. The reason is in the metric label + the
		// debug log (dropped below debug level) for operators.
		s.logger.Debug().Err(err).Str("endpoint", endpoint).Msg("trading auth rejected")
		respondUnauthorized(w, "trading request authentication failed")
		return true
	}
	return false
}

// enforceTradingRateLimit applies the per-project trading-order cap.
// Returns true when the request was REJECTED (429 written, caller must
// return). No-op (returns false) when the limiter or registry is not
// wired, the project is unknown, or its caps are zero. The caller
// invokes recordTradingOrderRate after the order is accepted so the
// window only counts admitted orders.
func (s *Server) enforceTradingRateLimit(w http.ResponseWriter, projectID string) bool {
	if s == nil || s.tradingRateLimiter == nil || s.projectRegistry == nil || projectID == "" {
		return false
	}
	proj := s.projectRegistry.GetProject(projectID)
	if proj == nil {
		return false
	}
	caps := tradingCaps(proj)
	if caps.PerMinute == 0 && caps.PerHour == 0 {
		return false
	}
	d := s.tradingRateLimiter.CheckKey(projectID, time.Now(), caps)
	if !d.Blocked {
		return false
	}
	s.recordTradingIngestError("order", "rate_limited")
	retrySecs := int(d.RetryAfter.Seconds())
	if d.RetryAfter%time.Second != 0 {
		retrySecs++
	}
	if retrySecs < 1 {
		retrySecs = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", retrySecs))
	respondError(w, http.StatusTooManyRequests, "RATE_LIMITED", d.Reason)
	return true
}

// recordTradingOrderRate marks one accepted order toward the
// project's trading window. No-op when the limiter isn't wired.
func (s *Server) recordTradingOrderRate(projectID string) {
	if s == nil || s.tradingRateLimiter == nil || projectID == "" {
		return
	}
	s.tradingRateLimiter.RecordKey(projectID, time.Now())
}

// tradingCaps lifts a project's trading rate-limit config into the
// ratelimit primitive's generic cap shape.
func tradingCaps(p *registry.Project) ratelimit.RateLimit {
	return ratelimit.RateLimit{
		PerMinute: p.TradingRateLimit.OrdersPerMinute,
		PerHour:   p.TradingRateLimit.OrdersPerHour,
	}
}
