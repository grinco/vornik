package service

import "testing"

func TestCommunityProvidersBlackBoxIsNoOp(t *testing.T) {
	set := CommunityProviders()
	if set.BlackBox == nil {
		t.Fatal("CommunityProviders().BlackBox must not be nil (it is the no-op provider)")
	}
	if sub := set.BlackBox.BlackBoxSubsystem(); sub != nil {
		t.Errorf("community BlackBoxSubsystem() = %v, want nil (no-op)", sub)
	}
}

func TestWithProvidersSetsField(t *testing.T) {
	c := &Container{}
	WithProviders(CommunityProviders())(c)
	if c.providers.BlackBox == nil {
		t.Error("WithProviders did not set c.providers")
	}
}

// TestCommunityProviders_GroupAInstinctIsNoOp asserts that CommunityProviders
// returns a non-nil InstinctProvider whose InstinctSubsystem() returns nil
// (the community no-op contract: provider present, subsystem absent).
func TestCommunityProviders_GroupAInstinctIsNoOp(t *testing.T) {
	set := CommunityProviders()
	if set.Instinct == nil {
		t.Fatal("CommunityProviders().Instinct must not be nil (it is the no-op provider)")
	}
	if sub := set.Instinct.InstinctSubsystem(); sub != nil {
		t.Errorf("community InstinctSubsystem() = %v, want nil (no-op)", sub)
	}
}

// TestCommunityRegistersNoClusterSubsystems is the Phase-2c Group-A seam guard
// for clustering: after the bool→ClusteringProvider type change, the Community
// provider set must leave Clustering nil so the container registers no cluster
// subsystems. Mirrors TestCommunityHasNoIdentityProvider (Phase C).
func TestCommunityRegistersNoClusterSubsystems(t *testing.T) {
	ps := CommunityProviders()
	if ps.Clustering != nil {
		t.Fatal("Community must not provide clustering subsystems")
	}
}

// TestCommunityProviders_GroupATradingIsNoOp asserts that CommunityProviders
// returns a non-nil TradingProvider whose TradingSubsystem() returns nil.
func TestCommunityProviders_GroupATradingIsNoOp(t *testing.T) {
	set := CommunityProviders()
	if set.Trading == nil {
		t.Fatal("CommunityProviders().Trading must not be nil (it is the no-op provider)")
	}
	if sub := set.Trading.TradingSubsystem(); sub != nil {
		t.Errorf("community TradingSubsystem() = %v, want nil (no-op)", sub)
	}
}

// TestCommunityProviders_GroupBFlags asserts the Group B presence flags on
// CommunityProviders(): Admin/OIDC/Logship remain false (Community omits those
// gated capabilities), while MemoryFirewall is TRUE — it was reclassified as a
// Community feature in editions Phase 2c (enforces on a Postgres-backed
// deployment; the inner gate leaves SQLite unfiltered). Guards against
// accidental struct-literal drift in either direction.
func TestCommunityProviders_GroupBFlags(t *testing.T) {
	set := CommunityProviders()
	if set.Admin {
		t.Error("CommunityProviders().Admin must be false")
	}
	if !set.MemoryFirewall {
		t.Error("CommunityProviders().MemoryFirewall must be true (reclassified Community in Phase 2c)")
	}
	if set.OIDC {
		t.Error("CommunityProviders().OIDC must be false")
	}
	if set.Logship {
		t.Error("CommunityProviders().Logship must be false")
	}
}

// TestCommunityHasNoIdentityProvider asserts the Phase-2c identity seam:
// CommunityProviders() must leave the Identity provider nil so a Community
// build wires no SSO/OIDC/RBAC login surface (login disabled — today's CE
// behaviour). The real EE impl is set only by internal/enterprise.Providers().
func TestCommunityHasNoIdentityProvider(t *testing.T) {
	ps := CommunityProviders()
	if ps.Identity != nil {
		t.Fatal("Community must not wire an SSO/OIDC identity provider")
	}
}

// TestCommunityProviders_GroupCContractsAllNil asserts that all Group C
// contract interface fields (Phase-1c) are nil in CommunityProviders().
// Nil is the Community path: CE callers guard on nil and apply the
// no-IP behaviour (fail closed / no-tier / healing skipped).
func TestCommunityProviders_GroupCContractsAllNil(t *testing.T) {
	set := CommunityProviders()
	if set.ReplaySafety != nil {
		t.Error("CommunityProviders().ReplaySafety must be nil (Community omits EE replay-safety classifier)")
	}
	if set.InstinctBudget != nil {
		t.Error("CommunityProviders().InstinctBudget must be nil (Community omits EE budget resolver)")
	}
	if set.Healing != nil {
		t.Error("CommunityProviders().Healing must be nil (Community omits EE healing applier)")
	}
}
