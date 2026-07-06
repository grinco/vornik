package service

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/storage"
)

// TestSnapshotsToGroundingServers verifies the ServerSnapshot →
// GroundingServer mapping: names + tool names are flattened, and
// unreachable servers are still included (mirrors
// mcpFormRegistryAdapter's behaviour — an operator debugging a down
// server still wants to see it, and the wizard should still ground the
// LLM on the tools it exposed on its last successful refresh).
func TestSnapshotsToGroundingServers(t *testing.T) {
	snap := []mcp.ServerSnapshot{
		{
			Name:      "github",
			Transport: "stdio",
			Reachable: true,
			Tools: []mcp.Tool{
				{Name: "search_issues"},
				{Name: "create_pr"},
			},
		},
		{
			Name:      "down-server",
			Transport: "sse",
			Reachable: false,
			Error:     "connection refused",
			Tools:     nil,
		},
	}

	got := snapshotsToGroundingServers(snap)

	if len(got) != 2 {
		t.Fatalf("expected 2 servers, got %d: %+v", len(got), got)
	}
	if got[0].Name != "github" || len(got[0].Tools) != 2 || got[0].Tools[0] != "search_issues" || got[0].Tools[1] != "create_pr" {
		t.Fatalf("github tools not mapped correctly: %+v", got[0])
	}
	// down-server is unreachable (Tools nil) but must still appear,
	// with an empty (not nil-panicking) tool list.
	if got[1].Name != "down-server" || len(got[1].Tools) != 0 {
		t.Fatalf("unreachable server dropped or has tools: %+v", got[1])
	}
}

// TestSnapshotsToGroundingServers_Empty covers the no-servers-
// configured case degrading to an empty (non-nil) slice.
func TestSnapshotsToGroundingServers_Empty(t *testing.T) {
	got := snapshotsToGroundingServers(nil)
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %+v", got)
	}
}

// TestWizardMCPGroundingAdapter_NilSafe covers a nil adapter/registry
// (MCP not configured on the daemon) degrading to an empty grounding
// section instead of panicking — the path buildProjectWizardOrNil
// takes when c.mcpRegistry is nil.
func TestWizardMCPGroundingAdapter_NilSafe(t *testing.T) {
	var adapter *wizardMCPGroundingAdapter
	if got := adapter.Servers(context.Background()); got != nil {
		t.Fatalf("expected nil from a nil adapter, got %+v", got)
	}
	adapter = &wizardMCPGroundingAdapter{}
	if got := adapter.Servers(context.Background()); got != nil {
		t.Fatalf("expected nil from an adapter with a nil registry, got %+v", got)
	}
}

// TestSnapshotsToKnownMCPSet verifies the known-set mapping: every
// configured server name is present regardless of Reachable — the
// commit-time compose engine validates configuration, not current
// health (appliers.go's mcpServerApplier).
func TestSnapshotsToKnownMCPSet(t *testing.T) {
	snap := []mcp.ServerSnapshot{
		{Name: "github", Reachable: true},
		{Name: "down-server", Reachable: false, Error: "connection refused"},
	}

	got := snapshotsToKnownMCPSet(snap)

	if !got["github"] || !got["down-server"] {
		t.Fatalf("expected both configured servers known regardless of reachability, got %+v", got)
	}
	if got["nonexistent"] {
		t.Fatalf("unconfigured server should not be known")
	}
}

// TestWizardKnownMCPServers_LiveRegistry is a thin integration check
// that wizardKnownMCPServers actually threads a *mcp.Registry's
// Snapshot through snapshotsToKnownMCPSet (as opposed to the pure
// mapping unit test above).
func TestWizardKnownMCPServers_LiveRegistry(t *testing.T) {
	servers := []mcp.ServerConfig{
		{Name: "github"},
		{Name: "down-server"},
	}
	reg := mcp.NewRegistry(servers, 0, zerolog.Nop())

	fn := wizardKnownMCPServers(reg)
	got := fn(context.Background())

	if !got["github"] || !got["down-server"] {
		t.Fatalf("expected both configured servers known regardless of reachability, got %+v", got)
	}
}

// TestWizardKnownMCPServers_NilRegistry covers the accessor factory
// itself: a nil registry (MCP unconfigured) must yield a nil func so
// buildProjectWizardOrNil leaves Wizard.KnownMCP unset rather than
// wiring a closure that always panics on a nil registry.
func TestWizardKnownMCPServers_NilRegistry(t *testing.T) {
	if fn := wizardKnownMCPServers(nil); fn != nil {
		t.Fatalf("expected nil func for a nil registry")
	}
}

// fakeModelListingProvider implements chat.Provider + chat.ModelLister
// with a scripted model list / error, driving templateModelIDs'
// chat.ModelLister branch without a real router.
type fakeModelListingProvider struct {
	models []chat.ModelInfo
	err    error
}

func (f *fakeModelListingProvider) Complete(context.Context, []chat.Message) (*chat.ChatResponse, error) {
	return nil, errors.New("not used")
}
func (f *fakeModelListingProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	return nil, errors.New("not used")
}
func (f *fakeModelListingProvider) CompleteWithToolsStream(context.Context, []chat.Message, []chat.Tool, chat.StreamCallback) (*chat.ChatResponse, error) {
	return nil, errors.New("not used")
}
func (f *fakeModelListingProvider) Model() string            { return "fake" }
func (f *fakeModelListingProvider) SetMetrics(*chat.Metrics) {}
func (f *fakeModelListingProvider) ListModels(context.Context) ([]chat.ModelInfo, error) {
	return f.models, f.err
}

// TestWizardModelLister_Models covers the happy path: model IDs are
// flattened via templateModelIDs, sorted, and returned verbatim.
func TestWizardModelLister_Models(t *testing.T) {
	provider := &fakeModelListingProvider{models: []chat.ModelInfo{{ID: "z-model"}, {ID: "a-model"}}}
	lister := wizardModelLister{provider: provider}

	got := lister.Models(context.Background())
	if len(got) != 2 || got[0] != "a-model" || got[1] != "z-model" {
		t.Fatalf("expected sorted [a-model z-model], got %+v", got)
	}
}

// TestWizardModelLister_ErrorDegradesToEmpty covers ModelLister's
// no-error-return contract: a discovery failure must not panic or
// propagate, it degrades to an empty list so the wizard's grounding
// block falls back to "default model only" instead of failing the
// whole turn.
func TestWizardModelLister_ErrorDegradesToEmpty(t *testing.T) {
	provider := &fakeModelListingProvider{err: errors.New("upstream unreachable")}
	lister := wizardModelLister{provider: provider}

	if got := lister.Models(context.Background()); got != nil {
		t.Fatalf("expected nil/empty on discovery error, got %+v", got)
	}
}

// TestWizardModelLister_NilProvider covers the guard against a nil
// chat.Provider, even though buildProjectWizardOrNil never wires
// wizardModelLister with one (c.ChatClient is checked earlier).
func TestWizardModelLister_NilProvider(t *testing.T) {
	lister := wizardModelLister{}
	if got := lister.Models(context.Background()); got != nil {
		t.Fatalf("expected nil for a nil provider, got %+v", got)
	}
}

// TestBuildProjectWizardOrNil_WiresGroundingDeps is the Task 8
// integration check: given a Container with an MCP registry and a
// chat client wired, buildProjectWizardOrNil must set all four
// grounding fields (MCP, Models, KnownMCP, Resolver) on the
// underlying *projectwizard.Wizard rather than leaving them nil
// (addon-vocab-only grounding).
func TestBuildProjectWizardOrNil_WiresGroundingDeps(t *testing.T) {
	reg := mcp.NewRegistry([]mcp.ServerConfig{{Name: "github"}}, 0, zerolog.Nop())
	c := &Container{
		Logger:      zerolog.Nop(),
		Config:      &config.Config{},
		ChatClient:  &fakeModelListingProvider{models: []chat.ModelInfo{{ID: "m1"}}},
		mcpRegistry: reg,
		repos:       &storage.Repositories{ProjectWizardSessions: newFakeWizardSessionStore()},
	}

	got := buildProjectWizardOrNil(c)
	if got == nil {
		t.Fatal("expected a non-nil wizard adapter")
	}
	adapter, ok := got.(*projectWizardAdapter)
	if !ok {
		t.Fatalf("expected *projectWizardAdapter, got %T", got)
	}
	wiz := adapter.wizard
	if wiz.MCP == nil {
		t.Error("wiz.MCP not wired")
	}
	if wiz.Models == nil {
		t.Error("wiz.Models not wired")
	}
	if wiz.KnownMCP == nil {
		t.Error("wiz.KnownMCP not wired")
	}
	if wiz.Resolver == nil {
		t.Error("wiz.Resolver not wired")
	}

	// Sanity: the wired KnownMCP actually reflects the registry's
	// configured servers, not an empty stand-in.
	if known := wiz.KnownMCP(context.Background()); !known["github"] {
		t.Errorf("expected KnownMCP to report the configured server, got %+v", known)
	}
}

// TestBuildProjectWizardOrNil_NoMCPLeavesGroundingFieldsNil covers the
// degrade path: a deployment without a daemon-level MCP registry still
// builds the wizard (chat + sessions are the only hard requirements),
// but MCP/KnownMCP stay nil rather than wrapping a nil registry —
// wizard.go's BuildGrounding/composeFromEnvelope are nil-safe on both.
func TestBuildProjectWizardOrNil_NoMCPLeavesGroundingFieldsNil(t *testing.T) {
	c := &Container{
		Logger:     zerolog.Nop(),
		Config:     &config.Config{},
		ChatClient: &fakeModelListingProvider{models: []chat.ModelInfo{{ID: "m1"}}},
		repos:      &storage.Repositories{ProjectWizardSessions: newFakeWizardSessionStore()},
	}

	got := buildProjectWizardOrNil(c)
	adapter, ok := got.(*projectWizardAdapter)
	if !ok {
		t.Fatalf("expected *projectWizardAdapter, got %T", got)
	}
	wiz := adapter.wizard
	if wiz.MCP != nil {
		t.Errorf("expected nil MCP without an mcpRegistry, got %+v", wiz.MCP)
	}
	if wiz.KnownMCP != nil {
		t.Error("expected nil KnownMCP without an mcpRegistry")
	}
	// Models still wires — c.ChatClient alone is enough — and Resolver
	// is always non-nil (see buildTemplateOptionsResolver).
	if wiz.Models == nil {
		t.Error("wiz.Models should still be wired from c.ChatClient alone")
	}
	if wiz.Resolver == nil {
		t.Error("wiz.Resolver should still be non-nil (degrades per-source)")
	}
}
