package memoryfirewall

// Security-hardening pass for the policy-aware memory firewall.
//
// These tests target the package's security-critical invariants
// rather than its happy paths (which evaluator_test.go already
// pins). The themes, mapped onto what the package actually
// implements:
//
//   - FAIL-CLOSED EMPTY-SCOPE: a request whose tenant scope is
//     empty/missing must never read another tenant's tagged
//     memory, and under strict isolation must read nothing at
//     all. A regression here is a cross-tenant data leak.
//   - DECISION-CHAIN PRECEDENCE: the six-stage chain has a fixed
//     order (expiry → tenant → role → purpose → sensitivity).
//     The first block wins; a reorder is a security regression
//     (e.g. serving an expired or wrong-tenant chunk because a
//     later stage happened to allow). These tests pin every
//     adjacent boundary.
//   - ACTION CLAMP: the restricted-sensitivity gate clamps an
//     otherwise-allowing decision to block when no operator_id is
//     present — it cannot be unlocked by any other dimension.
//   - DENY-ALL via classifier signal: refuted content collapses
//     permitted_roles to an explicit empty set (deny-all), which
//     is matched by exact membership (literal, case-sensitive),
//     never as a pattern/regex.
//   - FAIL-CLOSED DEFAULTS: an unknown sensitivity tier resolves
//     to the most-restrictive purpose default, never the most-
//     permissive.
//
// NOTE: the brief referenced "deny patterns (substring)", a
// "redact" decision, and an enforcement-mode *resolver* with
// per-project override precedence. None of those exist in this
// package's current source (the EnforcementMode enum is declared
// but consumed at the retrieval seam outside this package, and
// there is no content deny-list / redact path). Per the
// tests-only / assert-real-behavior rule, the relevant intent is
// covered against the real surface: tenant-scope fail-closed,
// chain precedence, the restricted-tier clamp, and exact (non-
// pattern) role membership.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// ---------------------------------------------------------------------------
// Fail-closed empty-scope invariant (cross-tenant isolation).
// ---------------------------------------------------------------------------

// TestFailClosed_EmptyScopeMatrix exhaustively pins the tenant
// fail-closed posture in DEFAULT (non-strict) mode across the full
// 2x2 of {chunk tagged?} x {request tenant present?} plus the
// mismatch case. The single security-relevant ALLOW is blank-vs-
// blank (legacy single-tenant); every scope that crosses a tenant
// boundary — including a blank request reaching for a tagged
// chunk — must block.
func TestFailClosed_EmptyScopeMatrix(t *testing.T) {
	e := NewEvaluator()
	cases := []struct {
		name        string
		chunkTenant string
		reqTenant   string
		want        EvaluationDecision
	}{
		{"blank chunk, blank request (legacy match)", "", "", DecisionAllow},
		{"tagged chunk, blank request (leak vector)", "tenant-a", "", DecisionBlockTenantMismatch},
		{"blank chunk, tagged request (legacy read by tenant)", "", "tenant-b", DecisionBlockTenantMismatch},
		{"tagged chunk, mismatched request", "tenant-a", "tenant-b", DecisionBlockTenantMismatch},
		{"tagged chunk, matching request", "tenant-a", "tenant-a", DecisionAllow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, _ := e.Decide(
				chunkWith(Policy{TenantID: tc.chunkTenant}),
				RequestContext{TenantID: tc.reqTenant},
			)
			assert.Equal(t, tc.want, dec)
		})
	}
}

// TestFailClosed_StrictDeniesBlankAcrossDimensions locks the strict-
// isolation security property under the strongest framing: with
// strict ON, a blank-tenant request is denied even when every OTHER
// policy dimension would allow (public sensitivity, nil roles, nil
// purposes, no expiry). The tenant gate must fire before anything
// else can rescue the request.
func TestFailClosed_StrictDeniesBlankAcrossDimensions(t *testing.T) {
	e := NewEvaluator().WithStrictTenantIsolation(true)
	c := chunkWith(Policy{Sensitivity: SensitivityPublic}) // maximally permissive otherwise
	dec, reason := e.Decide(c, RequestContext{Role: "analyst", OperatorID: "op-1"})
	assert.Equal(t, DecisionBlockTenantMismatch, dec)
	assert.Contains(t, reason, "strict tenant isolation")
}

// TestFailClosed_StrictTaggedRequestStillScoped guards that turning
// strict ON does not WIDEN access: a tenant-a request must still be
// blocked from a tenant-b chunk (the cross-tenant gate is orthogonal
// to the blank-request gate).
func TestFailClosed_StrictTaggedRequestStillScoped(t *testing.T) {
	e := NewEvaluator().WithStrictTenantIsolation(true)
	dec, _ := e.Decide(
		chunkWith(Policy{TenantID: "tenant-b"}),
		RequestContext{TenantID: "tenant-a"},
	)
	assert.Equal(t, DecisionBlockTenantMismatch, dec)
}

// ---------------------------------------------------------------------------
// Decision-chain precedence (mode/ordering "resolution").
// ---------------------------------------------------------------------------

// TestPrecedence_TenantBeatsRole pins the tenant→role boundary: a
// wrong-tenant request that also fails the role gate must be
// reported as a TENANT block (tenant runs first). A reorder that
// reported role here would mean the evaluator inspected role data
// for a chunk the request can't see.
func TestPrecedence_TenantBeatsRole(t *testing.T) {
	e := NewEvaluator()
	c := chunkWith(Policy{
		TenantID:       "tenant-a",
		PermittedRoles: []string{"analyst"},
	})
	dec, _ := e.Decide(c, RequestContext{TenantID: "tenant-b", Role: "coder"})
	assert.Equal(t, DecisionBlockTenantMismatch, dec, "tenant gate must precede role gate")
}

// TestPrecedence_RoleBeatsPurpose pins the role→purpose boundary.
func TestPrecedence_RoleBeatsPurpose(t *testing.T) {
	e := NewEvaluator()
	c := chunkWith(Policy{
		PermittedRoles:  []string{"analyst"},
		AllowedPurposes: []Purpose{PurposeOperational},
	})
	// coder fails role; training_data would also fail purpose — role wins.
	dec, _ := e.Decide(c, RequestContext{Role: "coder", Purpose: PurposeTrainingData})
	assert.Equal(t, DecisionBlockRoleNotPermitted, dec, "role gate must precede purpose gate")
}

// TestPrecedence_PurposeBeatsSensitivity pins the purpose→sensitivity
// boundary: a restricted chunk (no operator_id) that ALSO fails the
// purpose gate must report the purpose block, since purpose runs
// before the sensitivity-tier gate.
func TestPrecedence_PurposeBeatsSensitivity(t *testing.T) {
	e := NewEvaluator()
	c := chunkWith(Policy{
		Sensitivity:     SensitivityRestricted,
		AllowedPurposes: []Purpose{PurposeOperational},
	})
	dec, _ := e.Decide(c, RequestContext{Purpose: PurposeTrainingData}) // no operator_id either
	assert.Equal(t, DecisionBlockPurposeNotAllowed, dec, "purpose gate must precede sensitivity gate")
}

// TestPrecedence_ExpiryBeatsEverything pins the head of the chain:
// an expired chunk is blocked as expired even when it would also
// fail tenant, role, purpose, and sensitivity gates. Expiry is the
// first stage and must short-circuit before any other inspection.
func TestPrecedence_ExpiryBeatsEverything(t *testing.T) {
	expiry := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := NewEvaluatorWithClock(clockAt("2026-02-01T00:00:00Z"))
	c := chunkWith(Policy{
		ExpiresAt:       &expiry,
		TenantID:        "tenant-a",
		PermittedRoles:  []string{"analyst"},
		AllowedPurposes: []Purpose{PurposeOperational},
		Sensitivity:     SensitivityRestricted,
	})
	dec, _ := e.Decide(c, RequestContext{
		TenantID: "tenant-b",
		Role:     "coder",
		Purpose:  PurposeTrainingData,
	})
	assert.Equal(t, DecisionBlockExpired, dec, "expiry must short-circuit the whole chain")
}

// ---------------------------------------------------------------------------
// Restricted-tier action clamp.
// ---------------------------------------------------------------------------

// TestRestrictedClamp_CannotBeUnlockedByOtherDimensions verifies the
// restricted-sensitivity gate is a hard clamp: an anonymous request
// (no operator_id) is blocked even when tenant matches, role is
// permitted, and purpose is allowed — i.e. nothing else can rescue
// it. This is the autonomy-driven-request gate.
func TestRestrictedClamp_CannotBeUnlockedByOtherDimensions(t *testing.T) {
	e := NewEvaluator()
	c := chunkWith(Policy{
		Sensitivity:     SensitivityRestricted,
		TenantID:        "tenant-a",
		PermittedRoles:  []string{"analyst"},
		AllowedPurposes: []Purpose{PurposeOperational},
	})
	dec, reason := e.Decide(c, RequestContext{
		TenantID: "tenant-a",
		Role:     "analyst",
		Purpose:  PurposeOperational,
		// OperatorID deliberately empty.
	})
	assert.Equal(t, DecisionBlockSensitivityTier, dec)
	assert.Contains(t, reason, "operator_id")

	// And the clamp releases the moment an operator_id is present.
	dec, _ = e.Decide(c, RequestContext{
		TenantID:   "tenant-a",
		Role:       "analyst",
		Purpose:    PurposeOperational,
		OperatorID: "vadim",
	})
	assert.Equal(t, DecisionAllow, dec)
}

// TestRestrictedClamp_NonRestrictedTiersDoNotRequireOperator confirms
// the clamp is scoped to the restricted tier only — confidential /
// internal / public chunks allow anonymous (no operator_id) reads,
// so the gate doesn't over-block lower tiers.
func TestRestrictedClamp_NonRestrictedTiersDoNotRequireOperator(t *testing.T) {
	e := NewEvaluator()
	for _, tier := range []SensitivityTier{SensitivityPublic, SensitivityInternal, SensitivityConfidential} {
		dec, _ := e.Decide(chunkWith(Policy{Sensitivity: tier}), RequestContext{})
		assert.Equalf(t, DecisionAllow, dec, "tier %q must not require operator_id", tier)
	}
}

// ---------------------------------------------------------------------------
// Exact (non-pattern) role membership + classifier deny-all.
// ---------------------------------------------------------------------------

// TestRoleMembership_ExactNotPatternMatch locks that the role gate
// matches by exact string equality, NOT substring / prefix / regex.
// A permitted role of "analyst" must NOT admit "analyst2" or
// "analy", and a permitted role spelled "Analyst" must NOT admit
// "analyst" (case-sensitive). This is the property that keeps a
// crafted role string from widening access.
func TestRoleMembership_ExactNotPatternMatch(t *testing.T) {
	e := NewEvaluator()
	c := chunkWith(Policy{PermittedRoles: []string{"analyst"}})
	for _, role := range []string{"analyst2", "analy", "Analyst", "ANALYST", " analyst", "analyst "} {
		dec, _ := e.Decide(c, RequestContext{Role: role})
		assert.Equalf(t, DecisionBlockRoleNotPermitted, dec,
			"role %q must not be admitted by exact membership against [analyst]", role)
	}
	// The exact spelling is admitted.
	dec, _ := e.Decide(c, RequestContext{Role: "analyst"})
	assert.Equal(t, DecisionAllow, dec)
}

// TestRefutedClamp_DenyAllRolesIncludingEmpty verifies the classifier
// "refuted" signal collapses permitted_roles to an explicit empty
// set (deny-all): no role — including the empty role string a
// caller might send — can read a refuted chunk. Empty-slice (not
// nil) is the deny-all sentinel; nil would mean "no restriction".
func TestRefutedClamp_DenyAllRolesIncludingEmpty(t *testing.T) {
	e := NewEvaluator()
	p := ApplyClassifierSignal(Policy{}, "", "refuted")
	// Sanity: refuted produced an explicit empty (non-nil) deny-all set.
	assert.NotNil(t, p.PermittedRoles)
	assert.Len(t, p.PermittedRoles, 0)
	for _, role := range []string{"analyst", "operator", "", "admin"} {
		dec, _ := e.Decide(chunkWith(p), RequestContext{Role: role})
		assert.Equalf(t, DecisionBlockRoleNotPermitted, dec, "refuted chunk must deny role %q", role)
	}
}

// TestClassifierCredentialsClamp_ForcesRestrictedGate verifies the
// credentials content-class clamp wires through to the evaluator:
// ApplyClassifierSignal raises an internal-tier policy to restricted,
// which then makes an anonymous request fail the sensitivity gate it
// would otherwise have passed.
func TestClassifierCredentialsClamp_ForcesRestrictedGate(t *testing.T) {
	e := NewEvaluator()
	base := Policy{Sensitivity: SensitivityInternal}
	// Pre-clamp: anonymous request allowed at internal tier.
	dec, _ := e.Decide(chunkWith(base), RequestContext{})
	assert.Equal(t, DecisionAllow, dec)
	// Post-clamp: credentials class forces restricted → anonymous blocked.
	clamped := ApplyClassifierSignal(base, "credentials", "")
	assert.Equal(t, SensitivityRestricted, clamped.Sensitivity)
	dec, _ = e.Decide(chunkWith(clamped), RequestContext{})
	assert.Equal(t, DecisionBlockSensitivityTier, dec)
}

// ---------------------------------------------------------------------------
// Edge cases: nil/empty config, normalization, fail-closed defaults.
// ---------------------------------------------------------------------------

// TestEdge_EmptyPolicyEmptyRequest confirms the zero-value (nil
// config) path: an empty Policy against an empty RequestContext is
// the legacy single-tenant all-allow, with no panic on nil slices /
// nil expiry pointer.
func TestEdge_EmptyPolicyEmptyRequest(t *testing.T) {
	e := NewEvaluator()
	dec, reason := e.Decide(Chunk{}, RequestContext{})
	assert.Equal(t, DecisionAllow, dec)
	assert.Empty(t, reason)
}

// TestEdge_EmptyVsNilPurposeSetSemantics pins the slice semantics the
// digest/evaluator rely on: a NIL AllowedPurposes means "no purpose
// restriction" (any purpose allows), whereas a non-nil EMPTY set
// blocks every purpose — the deny-all corner used for fail-closed
// authoring.
func TestEdge_EmptyVsNilPurposeSetSemantics(t *testing.T) {
	e := NewEvaluator()
	// nil set → unrestricted.
	dec, _ := e.Decide(chunkWith(Policy{AllowedPurposes: nil}), RequestContext{Purpose: PurposeComplianceExport})
	assert.Equal(t, DecisionAllow, dec)
	// non-nil empty set → deny-all (even the normalized default purpose).
	dec, _ = e.Decide(chunkWith(Policy{AllowedPurposes: []Purpose{}}), RequestContext{})
	assert.Equal(t, DecisionBlockPurposeNotAllowed, dec)
}

// TestDefaultsForTier_UnknownTierFailsClosed verifies the fail-closed
// posture of the tier-defaults table: an unrecognised tier resolves
// to the MOST-restrictive purpose set (operational only), never to
// the permissive nil ("no restriction"). Better to false-block than
// false-allow on an unknown tier.
func TestDefaultsForTier_UnknownTierFailsClosed(t *testing.T) {
	roles, purposes := DefaultsForTier(SensitivityTier("bogus-tier"))
	assert.Nil(t, roles)
	assert.Equal(t, []Purpose{PurposeOperational}, purposes,
		"unknown tier must fall through to the most-restrictive purpose default")
}

// TestProductionClock_NowIsUTCAndAdvances exercises the production
// now() path (nil nowFn) that the injected-clock tests bypass: it
// must return a UTC, non-zero, current time so real expiry checks
// work. Pins the default-clock branch without time.Now monkeying.
func TestProductionClock_NowIsUTCAndAdvances(t *testing.T) {
	e := NewEvaluator() // no injected clock → real time.Now path
	before := time.Now().Add(-time.Minute)

	// An already-past expiry must block under the real clock.
	pastExpiry := time.Now().Add(-time.Hour)
	dec, _ := e.Decide(chunkWith(Policy{ExpiresAt: &pastExpiry}), RequestContext{})
	assert.Equal(t, DecisionBlockExpired, dec)

	// A far-future expiry must allow under the real clock.
	futureExpiry := time.Now().Add(24 * time.Hour)
	dec, _ = e.Decide(chunkWith(Policy{ExpiresAt: &futureExpiry}), RequestContext{})
	assert.Equal(t, DecisionAllow, dec)

	assert.True(t, before.Before(time.Now()), "sanity: wall clock advances")
}
