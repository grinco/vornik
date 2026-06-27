package service

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/contracts"
)

// namedNoOpSubsystem returns a Subsystem whose Name() is the given name and whose
// Build/Start/Stop are all no-ops. Used in provider-registration tests that only
// need to verify the subsystem is registered by name, without requiring the real
// EE subsystem (which would import internal/instinct or internal/blackbox).
func namedNoOpSubsystem(name string) Subsystem { return &namedNoOp{name: name} }

type namedNoOp struct{ name string }

func (n *namedNoOp) Name() string                  { return n.name }
func (n *namedNoOp) Build(_ *BuildDeps) error      { return nil }
func (n *namedNoOp) Start(_ context.Context) error { return nil }
func (n *namedNoOp) Stop(_ context.Context) error  { return nil }

// stubBlackBox lets the test register a sentinel subsystem without the
// enterprise package (which would be an illegal service->enterprise import).
type stubBlackBox struct{ sub Subsystem }

func (s stubBlackBox) BlackBoxSubsystem() Subsystem { return s.sub }

// stubInstinct and stubTrading are sentinel providers for testing Group A
// provider extension without importing internal/enterprise (illegal cycle).
type stubInstinct struct{ sub Subsystem }

func (s stubInstinct) InstinctSubsystem() Subsystem { return s.sub }

type stubTrading struct{ sub Subsystem }

func (s stubTrading) TradingSubsystem() Subsystem { return s.sub }

func registeredNames(c *Container) []string {
	c.registerSubsystems()
	names := make([]string, 0, len(c.subsystems))
	for _, s := range c.subsystems {
		names = append(names, s.Name())
	}
	return names
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestRegisterSubsystems_CommunityOmitsInstinct(t *testing.T) {
	c := &Container{providers: CommunityProviders()}
	if hasName(registeredNames(c), "instinct") {
		t.Error("Community providers must not register instinct")
	}
}

func TestRegisterSubsystems_EnterpriseRegistersInstinct(t *testing.T) {
	// Use a named no-op subsystem instead of service.NewInstinctSubsystem() so that
	// service_test does not import internal/instinct (import-law: CE test → EE import
	// is allowed but unnecessary here — the test only checks registration by name).
	c := &Container{providers: ProviderSet{Instinct: stubInstinct{sub: namedNoOpSubsystem("instinct")}}}
	if !hasName(registeredNames(c), "instinct") {
		t.Error("a real Instinct provider must register instinct")
	}
}

func TestRegisterSubsystems_CommunityOmitsTrading(t *testing.T) {
	c := &Container{providers: CommunityProviders()}
	if hasName(registeredNames(c), "trading") {
		t.Error("Community providers must not register trading")
	}
}

func TestRegisterSubsystems_EnterpriseRegistersTrading(t *testing.T) {
	// Use a named no-op subsystem — same rationale as instinct/blackbox above.
	// The real trading subsystem relocated to internal/enterprise/trading in
	// Phase 2c, so this CE test can no longer import its constructor; the test
	// only checks registration-by-name, which a no-op subsystem covers.
	c := &Container{providers: ProviderSet{Trading: stubTrading{sub: namedNoOpSubsystem("trading")}}}
	if !hasName(registeredNames(c), "trading") {
		t.Error("a real Trading provider must register trading")
	}
}

func TestRegisterSubsystems_CommunityOmitsBlackBox(t *testing.T) {
	c := &Container{providers: CommunityProviders()}
	if hasName(registeredNames(c), "blackbox_detector") {
		t.Error("Community providers must not register blackbox_detector")
	}
}

func TestRegisterSubsystems_EnterpriseRegistersBlackBox(t *testing.T) {
	// Use a named no-op subsystem — same rationale as instinct above.
	c := &Container{providers: ProviderSet{BlackBox: stubBlackBox{sub: namedNoOpSubsystem("blackbox_detector")}}}
	if !hasName(registeredNames(c), "blackbox_detector") {
		t.Error("a real BlackBox provider must register blackbox_detector")
	}
}

// stubClustering is a sentinel ClusteringProvider for the CE-side seam tests.
// It returns the named no-op subsystems handed to it (mirrors stubBlackBox /
// stubInstinct / stubTrading) so the container's registration path can be
// exercised without importing internal/enterprise/clustering (illegal cycle).
// Phase 2c: the real per-node inner gates moved to the EE provider; the
// container only appends whatever ClusterSubsystems returns, which is what
// these tests assert.
type stubClustering struct{ subs []Subsystem }

func (s stubClustering) ClusterSubsystems(_ ClusterDeps) []Subsystem { return s.subs }

func (stubClustering) NewWebhookRelayClient(_ config.RelayConfig) (contracts.WebhookRelayClient, error) {
	return nil, nil
}

// TestRegisterSubsystems_CommunityOmitsClusterSubsystems is the edition-gate
// assertion after the Phase-2c bool→ClusteringProvider change: a Community
// build (nil Clustering provider) must register no cluster subsystems.
func TestRegisterSubsystems_CommunityOmitsClusterSubsystems(t *testing.T) {
	c := &Container{providers: CommunityProviders(), Config: &config.Config{}}
	names := registeredNames(c)
	for _, want := range []string{"cluster_heartbeat", "webhook_heartbeat", "cluster_node_pruner", "cluster_monitor"} {
		if hasName(names, want) {
			t.Errorf("Community (nil Clustering provider): subsystem %q must not be registered (got %v)", want, names)
		}
	}
}

// TestRegisterSubsystems_ClusteringProvider_AppendsReturnedSubsystems asserts
// the container appends exactly what the (non-nil) ClusteringProvider returns —
// the inner per-node gating is the provider's responsibility now (covered by
// the EE clustering package tests), so the container's only job is to splice in
// the returned slice.
func TestRegisterSubsystems_ClusteringProvider_AppendsReturnedSubsystems(t *testing.T) {
	c := &Container{
		providers: ProviderSet{Clustering: stubClustering{subs: []Subsystem{
			namedNoOpSubsystem("cluster_heartbeat"),
			namedNoOpSubsystem("cluster_node_pruner"),
		}}},
		Config: &config.Config{},
	}
	names := registeredNames(c)
	for _, want := range []string{"cluster_heartbeat", "cluster_node_pruner"} {
		if !hasName(names, want) {
			t.Errorf("non-nil Clustering provider: subsystem %q returned by ClusterSubsystems must be registered (got %v)", want, names)
		}
	}
}

// TestApplyOptions_SetsProvidersBeforeAnyInit proves the ordering fix: after
// moving applyOptions to immediately after struct allocation, a WithProviders
// call is reflected in c.providers regardless of when init functions run.
// This is the internal-package assertion for the ordering invariant (the
// stronger runtime assertion — that gated Group B sites run after providers
// are set — is covered by Group B gate tests added in later tasks).
func TestApplyOptions_SetsProvidersBeforeAnyInit(t *testing.T) {
	sentinel := ProviderSet{
		BlackBox:       stubBlackBox{},
		Instinct:       stubInstinct{},
		Trading:        stubTrading{},
		Clustering:     stubClustering{},
		Admin:          true,
		MemoryFirewall: true,
		OIDC:           true,
		Logship:        true,
	}
	c := &Container{}
	c.applyOptions([]ContainerOption{WithProviders(sentinel)})

	// All Group A providers must be the stubs, not community no-ops.
	if _, ok := c.providers.BlackBox.(stubBlackBox); !ok {
		t.Errorf("providers.BlackBox = %T, want stubBlackBox", c.providers.BlackBox)
	}
	if _, ok := c.providers.Instinct.(stubInstinct); !ok {
		t.Errorf("providers.Instinct = %T, want stubInstinct", c.providers.Instinct)
	}
	if _, ok := c.providers.Trading.(stubTrading); !ok {
		t.Errorf("providers.Trading = %T, want stubTrading", c.providers.Trading)
	}

	// Clustering is now a Group-A-style provider (Phase 2c); the sentinel set a
	// non-nil stub, so it must round-trip as non-nil.
	if c.providers.Clustering == nil {
		t.Error("providers.Clustering must be non-nil after WithProviders(sentinel)")
	}
	// All Group B flags must be true (passed sentinel had them all true).
	if !c.providers.Admin {
		t.Error("providers.Admin must be true after WithProviders(sentinel)")
	}
	if !c.providers.MemoryFirewall {
		t.Error("providers.MemoryFirewall must be true after WithProviders(sentinel)")
	}
	if !c.providers.OIDC {
		t.Error("providers.OIDC must be true after WithProviders(sentinel)")
	}
	if !c.providers.Logship {
		t.Error("providers.Logship must be true after WithProviders(sentinel)")
	}
}
