package memoryfirewall

// Evaluator runs the per-chunk policy check on the retrieval
// hot path. Single Decide() entry point; six-stage chain that
// short-circuits on the first block.
//
// The evaluator is in-process Go with zero DB round-trips —
// policy columns are loaded by the Repository's query and
// passed in as a flat Chunk struct. Median execution time
// target: < 50µs per chunk.

import (
	"fmt"
	"time"
)

// Evaluator is the firewall's decision function. Stateless +
// safe for concurrent use; one instance per daemon is enough.
type Evaluator struct {
	// nowFn lets tests inject a fixed clock without touching
	// time.Now globally. Production wiring leaves this nil
	// and the evaluator falls back to time.Now.
	nowFn func() time.Time

	// strictTenant turns on fail-closed tenant isolation: a request
	// with an EMPTY tenant_id matches nothing (rather than matching
	// untagged/legacy chunks). OFF by default so single-tenant
	// deployments — where every chunk is untagged and every request
	// arrives blank — keep working byte-for-byte. A multi-tenant
	// deployment flips this ON once its recall paths thread the real
	// tenant_id onto RequestContext, closing the "blank request reads
	// all untagged memory" gap. Tagged-chunk isolation is enforced
	// regardless of this flag.
	strictTenant bool
}

// NewEvaluator returns a production-configured evaluator (strict
// tenant isolation OFF — see Evaluator.strictTenant).
func NewEvaluator() *Evaluator { return &Evaluator{} }

// NewEvaluatorWithClock returns an evaluator with a custom
// clock. Test-only constructor.
func NewEvaluatorWithClock(nowFn func() time.Time) *Evaluator {
	return &Evaluator{nowFn: nowFn}
}

// WithStrictTenantIsolation returns the evaluator with fail-closed
// tenant isolation enabled/disabled. Chainable on any constructor:
//
//	e := NewEvaluator().WithStrictTenantIsolation(cfg.MultiTenant)
//
// Enable only after every recall path populates RequestContext.TenantID,
// or blank-context callers (e.g. the legacy Search path) will read
// nothing.
func (e *Evaluator) WithStrictTenantIsolation(on bool) *Evaluator {
	e.strictTenant = on
	return e
}

func (e *Evaluator) now() time.Time {
	if e.nowFn != nil {
		return e.nowFn()
	}
	return time.Now().UTC()
}

// Decide returns the decision for one (chunk, request) pair.
// The reason string is empty on allow; on block it carries
// operator-readable detail ("role 'coder' not in permitted set
// [analyst,executor]") suitable for the audit row's
// reason_detail column.
func (e *Evaluator) Decide(chunk Chunk, req RequestContext) (EvaluationDecision, string) {
	// 1. Expiry check. Past the chunk's firewall expiry?
	if chunk.Policy.ExpiresAt != nil && e.now().After(*chunk.Policy.ExpiresAt) {
		return DecisionBlockExpired, fmt.Sprintf("chunk expired at %s", chunk.Policy.ExpiresAt.Format(time.RFC3339))
	}

	// 2. Tenant check. A tenant-TAGGED chunk is served only to a
	// request carrying the SAME tenant — a blank or mismatched
	// request tenant is blocked (fail-closed against cross-tenant
	// leaks). An untagged chunk matched by a blank request is the
	// legacy single-tenant case: allowed by default, but blocked
	// under strict tenant isolation (see below).
	if chunk.Policy.TenantID != req.TenantID {
		// One side might be empty (legacy chunk in a multi-
		// tenant deployment, or vice versa). Treat empty-vs-
		// empty as match; any other combo is a block.
		if chunk.Policy.TenantID != "" || req.TenantID != "" {
			return DecisionBlockTenantMismatch, fmt.Sprintf(
				"chunk tenant=%q does not match request tenant=%q",
				chunk.Policy.TenantID, req.TenantID,
			)
		}
	}
	// Strict (multi-tenant) isolation: a request with no tenant_id
	// must not match untagged chunks — it would otherwise read every
	// legacy/untagged chunk in the store. Tagged chunks are already
	// blocked above, so this only newly closes the blank-request /
	// untagged-chunk path. OFF by default (single-tenant deployments
	// rely on the blank-vs-blank match).
	if e.strictTenant && req.TenantID == "" {
		return DecisionBlockTenantMismatch,
			"strict tenant isolation: request has no tenant_id"
	}

	// 3. Role check. Non-nil PermittedRoles requires
	// req.Role to be in the set.
	if chunk.Policy.PermittedRoles != nil {
		if !containsString(chunk.Policy.PermittedRoles, req.Role) {
			return DecisionBlockRoleNotPermitted, fmt.Sprintf(
				"role %q not in permitted set", req.Role,
			)
		}
	}

	// 4. Purpose check. Non-nil AllowedPurposes requires
	// req.Purpose to be in the set. Empty request purpose
	// is normalised to PurposeOperational.
	purpose := req.Purpose
	if purpose == "" {
		purpose = PurposeOperational
	}
	if chunk.Policy.AllowedPurposes != nil {
		if !containsPurpose(chunk.Policy.AllowedPurposes, purpose) {
			return DecisionBlockPurposeNotAllowed, fmt.Sprintf(
				"purpose %q not allowed for this chunk", purpose,
			)
		}
	}

	// 5. Sensitivity tier gate. Restricted requires an
	// operator_id on the request — autonomy-driven requests
	// (empty operator_id) are blocked even if everything
	// else allows.
	if chunk.Policy.Sensitivity == SensitivityRestricted && req.OperatorID == "" {
		return DecisionBlockSensitivityTier, "restricted-tier chunk requires operator_id on request"
	}

	// 6. Allow.
	return DecisionAllow, ""
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func containsPurpose(s []Purpose, v Purpose) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
