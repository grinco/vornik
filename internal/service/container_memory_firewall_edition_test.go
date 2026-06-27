package service

// Edition-gate tests for the Memory Firewall AuditWriter block
// (Task 5, feat/ce-ee-phase1b-breadth).
//
// # Design
//
// initScheduler builds the executor, opens DB connections, and has
// many hard infrastructure dependencies — calling it end-to-end in a
// unit test is impractical. Instead we test the gating predicate
// directly via memoryFirewallEditionGatePasses, which is the exact
// two-condition expression (`providers.MemoryFirewall &&
// repos.MemoryPolicyEvaluations != nil`) that gates the AuditWriter
// block. The production code in initScheduler executes the same
// boolean; the predicate is kept in sync by construction (it's a
// named helper, not a copy).
//
// Fidelity limit: we do NOT call initScheduler end-to-end, so we
// cannot assert c.memoryFirewallWriter == nil / non-nil after the
// full scheduler init. What we assert is the gating predicate's
// truth value, which is the load-bearing edition-gate condition.
// An integration test over a stub DB would give stronger coverage;
// this is documented as a known gap.

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
)

// stubMemoryPolicyEvalRepo is a no-op implementation of
// persistence.MemoryPolicyEvaluationRepository for use in tests that
// need a non-nil repo without a real database connection.
type stubMemoryPolicyEvalRepo struct{}

func (stubMemoryPolicyEvalRepo) BatchInsert(_ context.Context, _ []memoryfirewall.EvaluationRow) error {
	return nil
}

func (stubMemoryPolicyEvalRepo) ListRecent(_ context.Context, _, _ string, _ time.Time, _ int) ([]memoryfirewall.EvaluationRow, error) {
	return nil, nil
}

func (stubMemoryPolicyEvalRepo) ListByDigest(_ context.Context, _ string, _ int) ([]memoryfirewall.EvaluationRow, error) {
	return nil, nil
}

func (stubMemoryPolicyEvalRepo) ListByChunk(_ context.Context, _ string, _ int) ([]memoryfirewall.EvaluationRow, error) {
	return nil, nil
}

var _ persistence.MemoryPolicyEvaluationRepository = stubMemoryPolicyEvalRepo{}

// TestMemoryFirewallEditionGate_FalseFlag_PreventsWiring asserts the core
// edition-gate invariant: when providers.MemoryFirewall == false, the
// gating predicate returns false even when the inner pg-repo gate would
// pass (the repo is non-nil). This is the key CE behaviour — the firewall
// MUST NOT wire in Community edition regardless of repo presence.
func TestMemoryFirewallEditionGate_FalseFlag_PreventsWiring(t *testing.T) {
	c := &Container{
		providers: ProviderSet{
			BlackBox:       communityBlackBox{},
			Instinct:       communityInstinct{},
			Trading:        communityTrading{},
			MemoryFirewall: false, // Community: firewall omitted
		},
		repos: &storage.Repositories{
			MemoryPolicyEvaluations: stubMemoryPolicyEvalRepo{}, // inner gate would pass
		},
	}

	if c.memoryFirewallEditionGatePasses() {
		t.Error("memoryFirewallEditionGatePasses() = true with MemoryFirewall=false; " +
			"Community edition must NOT wire the firewall even when the pg-repo is present")
	}
}

// TestMemoryFirewallEditionGate_TrueFlag_WithRepo_Passes asserts the EE
// happy path: when providers.MemoryFirewall == true AND the pg-repo is
// non-nil, the gating predicate returns true (firewall should be wired).
func TestMemoryFirewallEditionGate_TrueFlag_WithRepo_Passes(t *testing.T) {
	c := &Container{
		providers: ProviderSet{
			BlackBox:       communityBlackBox{},
			Instinct:       communityInstinct{},
			Trading:        communityTrading{},
			MemoryFirewall: true, // Enterprise: firewall included
		},
		repos: &storage.Repositories{
			MemoryPolicyEvaluations: stubMemoryPolicyEvalRepo{},
		},
	}

	if !c.memoryFirewallEditionGatePasses() {
		t.Error("memoryFirewallEditionGatePasses() = false with MemoryFirewall=true and repo set; " +
			"Enterprise edition with pg-repo must wire the firewall")
	}
}

// TestMemoryFirewallEditionGate_TrueFlag_NilRepo_DoesNotPass asserts that
// the inner pg-repo nil-check is preserved: MemoryFirewall=true with a nil
// repo (SQLite deployment) should NOT pass the gate — the inner gate is
// the second condition, not replaced.
func TestMemoryFirewallEditionGate_TrueFlag_NilRepo_DoesNotPass(t *testing.T) {
	c := &Container{
		providers: ProviderSet{
			BlackBox:       communityBlackBox{},
			Instinct:       communityInstinct{},
			Trading:        communityTrading{},
			MemoryFirewall: true,
		},
		repos: &storage.Repositories{
			MemoryPolicyEvaluations: nil, // SQLite: no pg-repo
		},
	}

	if c.memoryFirewallEditionGatePasses() {
		t.Error("memoryFirewallEditionGatePasses() = true with nil repo; " +
			"inner pg-repo gate must still block even when the edition flag is set")
	}
}

// TestMemoryFirewallEditionGate_NilRepos_DoesNotPass asserts the nil-repos
// guard: a container with repos == nil does not pass the gate.
func TestMemoryFirewallEditionGate_NilRepos_DoesNotPass(t *testing.T) {
	c := &Container{
		providers: ProviderSet{
			BlackBox:       communityBlackBox{},
			Instinct:       communityInstinct{},
			Trading:        communityTrading{},
			MemoryFirewall: true,
		},
		repos: nil,
	}

	if c.memoryFirewallEditionGatePasses() {
		t.Error("memoryFirewallEditionGatePasses() = true with nil repos; " +
			"must return false when repos is nil")
	}
}

// TestMemoryFirewallEditionGate_NilContainer_DoesNotPass is a nil-safety
// guard: a nil Container must not panic and must return false.
func TestMemoryFirewallEditionGate_NilContainer_DoesNotPass(t *testing.T) {
	if (*Container)(nil).memoryFirewallEditionGatePasses() {
		t.Error("nil container must return false, not true")
	}
}

// TestCommunityProviders_MemoryFirewall_EnabledWhenPostgresRepo asserts the
// Phase 2c reclassification: Memory Firewall is now a Community feature, so
// CommunityProviders() sets MemoryFirewall=true and a CE deployment backed by
// a Postgres eval repo enforces policy. (Previously this asserted the
// Community default left the firewall OFF; that posture is reversed.)
func TestCommunityProviders_MemoryFirewall_EnabledWhenPostgresRepo(t *testing.T) {
	c := &Container{
		providers: CommunityProviders(), // now MemoryFirewall: true
		repos: &storage.Repositories{
			MemoryPolicyEvaluations: stubMemoryPolicyEvalRepo{}, // pg repo present
		},
	}
	if !c.memoryFirewallEditionGatePasses() {
		t.Fatal("CE-on-Postgres must enforce the memory firewall after reclassification")
	}
}

// TestCommunityProviders_MemoryFirewall_SilentWhenSQLite asserts that the
// inner pg-repo gate still governs runtime behaviour: a CE deployment without
// a Postgres eval repo (SQLite / single-process) runs unfiltered — no
// firewall, no error — even though the edition flag is now on.
func TestCommunityProviders_MemoryFirewall_SilentWhenSQLite(t *testing.T) {
	c := &Container{
		providers: CommunityProviders(),
		repos: &storage.Repositories{
			MemoryPolicyEvaluations: nil, // SQLite / no pg eval repo
		},
	}
	if c.memoryFirewallEditionGatePasses() {
		t.Fatal("CE without a Postgres eval repo must run unfiltered (no firewall, no error)")
	}
}
