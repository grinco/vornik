package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/registry"
)

// fakeListerClient satisfies BOTH apigateway.Client (Call) and the
// optional apigateway.ProviderLister capability, so list_apis tests
// can exercise the type-assertion success path (design §5.2). A
// plain *fakeAPIClient (Call-only, from tool_query_api_test.go)
// covers the assertion-failure path.
type fakeListerClient struct {
	fakeAPIClient
	providers []apigateway.ProviderInfo
}

func (f *fakeListerClient) ListProviders() []apigateway.ProviderInfo {
	return f.providers
}

func stdProviders() []apigateway.ProviderInfo {
	return []apigateway.ProviderInfo{
		{Name: "maps", Description: "Geocoding and directions", AllowedMethods: []string{"GET"}, WritesEnabled: false, Examples: []string{"GET /geocode/json?address={address}"}},
		{Name: "candidates", Description: "Candidate ATS lookup", AllowedMethods: []string{"GET", "POST"}, WritesEnabled: true, Examples: []string{"GET /candidates/{id}"}},
		{Name: "weather", Description: "Weather forecast", AllowedMethods: []string{"GET"}, WritesEnabled: false},
	}
}

// loadListAPIsTestRegistry stages a minimal project + swarm + workflow
// triple on disk (registry.Registry exposes no programmatic project-set
// API — same pattern as loadDispatcherTestRegistry /
// loadAuditEmailRegistry) with an optional api_providers allowlist.
func loadListAPIsTestRegistry(t *testing.T, projectID string, apiProviders []string) *registry.Registry {
	t.Helper()
	configDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(configDir, "swarms", "s1.md"), []byte(`---
swarmId: "s1"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "workflows", "wf.md"), []byte(`---
workflowId: "wf"
entrypoint: "run"
steps:
  run:
    type: "agent"
    prompt: "do work"
    role: "coder"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
---
`), 0o644))

	permYAML := ""
	if len(apiProviders) > 0 {
		var b strings.Builder
		b.WriteString("permissions:\n  api_providers:\n")
		for _, p := range apiProviders {
			b.WriteString("    - \"" + p + "\"\n")
		}
		permYAML = b.String()
	}
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "projects", projectID+".yaml"), []byte(`
projectId: "`+projectID+`"
displayName: "Test Project"
swarmId: "s1"
defaultWorkflowId: "wf"
`+permYAML), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(configDir))
	return reg
}

func TestListAPIs_NotConfigured_NilClient(t *testing.T) {
	te := &ToolExecutor{}
	res := te.listAPIs(context.Background(), `{}`, "proj", []string{"proj"})
	if !strings.Contains(strings.ToLower(res.Content), "not available") {
		t.Errorf("nil client should say discovery not available, got %q", res.Content)
	}
}

func TestListAPIs_NotConfigured_ClientLacksProviderLister(t *testing.T) {
	// fakeAPIClient (from tool_query_api_test.go) only implements Call —
	// exercises the type-assertion failure path distinct from nil.
	te := &ToolExecutor{apiClient: &fakeAPIClient{}}
	res := te.listAPIs(context.Background(), `{}`, "proj", []string{"proj"})
	if !strings.Contains(strings.ToLower(res.Content), "not available") {
		t.Errorf("non-lister client should say discovery not available, got %q", res.Content)
	}
}

func TestListAPIs_RequiresActiveProject(t *testing.T) {
	te := &ToolExecutor{apiClient: &fakeListerClient{providers: stdProviders()}}
	res := te.listAPIs(context.Background(), `{}`, "", nil)
	if !strings.Contains(strings.ToLower(res.Content), "project") {
		t.Errorf("empty activeProject should error, got %q", res.Content)
	}
}

func TestListAPIs_OwnershipGate(t *testing.T) {
	te := &ToolExecutor{apiClient: &fakeListerClient{providers: stdProviders()}}
	res := te.listAPIs(context.Background(), `{}`, "secret", []string{"news"})
	if !strings.Contains(strings.ToLower(res.Content), "not permitted") {
		t.Errorf("disallowed project should be refused, got %q", res.Content)
	}
}

func TestListAPIs_NilRegistry_AllProvidersAndWarns(t *testing.T) {
	var logBuf strings.Builder
	te := &ToolExecutor{
		apiClient: &fakeListerClient{providers: stdProviders()},
		logger:    zerolog.New(&logBuf),
	}
	res := te.listAPIs(context.Background(), `{}`, "proj", []string{"proj"})

	var got []listAPIsProvider
	require.NoError(t, json.Unmarshal([]byte(res.Content), &got))
	if len(got) != 3 {
		t.Errorf("nil registry should surface all providers, got %d: %+v", len(got), got)
	}
	if !strings.Contains(logBuf.String(), "warn") {
		t.Errorf("nil registry should log a warning, log=%q", logBuf.String())
	}
}

func TestListAPIs_MissingProjectInRegistry_AllProvidersAndWarns(t *testing.T) {
	// activeProject is permitted by the session (allowedProjects) but the
	// registry (loaded for a different project) has no record of it.
	reg := loadListAPIsTestRegistry(t, "other", nil)
	var logBuf strings.Builder
	te := &ToolExecutor{
		apiClient: &fakeListerClient{providers: stdProviders()},
		registry:  reg,
		logger:    zerolog.New(&logBuf),
	}
	res := te.listAPIs(context.Background(), `{}`, "ghost", []string{"ghost"})

	var got []listAPIsProvider
	require.NoError(t, json.Unmarshal([]byte(res.Content), &got))
	if len(got) != 3 {
		t.Errorf("missing project should surface all providers, got %d", len(got))
	}
	if !strings.Contains(logBuf.String(), "warn") {
		t.Errorf("missing project should log a warning, log=%q", logBuf.String())
	}
}

func TestListAPIs_EmptyAllowlist_AllProviders(t *testing.T) {
	reg := loadListAPIsTestRegistry(t, "proj", nil)
	te := &ToolExecutor{
		apiClient: &fakeListerClient{providers: stdProviders()},
		registry:  reg,
		logger:    zerolog.Nop(),
	}
	res := te.listAPIs(context.Background(), `{}`, "proj", []string{"proj"})

	var got []listAPIsProvider
	require.NoError(t, json.Unmarshal([]byte(res.Content), &got))
	if len(got) != 3 {
		t.Errorf("empty allowlist should mean all providers, got %d: %+v", len(got), got)
	}
}

func TestListAPIs_SubsetAllowlist_OnlyListed(t *testing.T) {
	reg := loadListAPIsTestRegistry(t, "proj", []string{"maps", "weather"})
	te := &ToolExecutor{
		apiClient: &fakeListerClient{providers: stdProviders()},
		registry:  reg,
		logger:    zerolog.Nop(),
	}
	res := te.listAPIs(context.Background(), `{}`, "proj", []string{"proj"})

	var got []listAPIsProvider
	require.NoError(t, json.Unmarshal([]byte(res.Content), &got))
	names := map[string]bool{}
	for _, p := range got {
		names[p.Name] = true
	}
	if len(got) != 2 || !names["maps"] || !names["weather"] || names["candidates"] {
		t.Errorf("subset allowlist should keep only maps+weather, got %+v", got)
	}
}

func TestListAPIs_AllowlistNamesUnregisteredProvider_Dropped(t *testing.T) {
	reg := loadListAPIsTestRegistry(t, "proj", []string{"maps", "nonexistent"})
	te := &ToolExecutor{
		apiClient: &fakeListerClient{providers: stdProviders()},
		registry:  reg,
		logger:    zerolog.Nop(),
	}
	res := te.listAPIs(context.Background(), `{}`, "proj", []string{"proj"})

	var got []listAPIsProvider
	require.NoError(t, json.Unmarshal([]byte(res.Content), &got))
	if len(got) != 1 || got[0].Name != "maps" {
		t.Errorf("unregistered allowlist entry should be silently dropped, got %+v", got)
	}
}

func TestListAPIs_CaseMismatchedAllowlistEntry_Dropped(t *testing.T) {
	// Allowlist names "Maps" (capitalized); the registered provider is
	// "maps" (lowercase). Match is case-sensitive (design §5.3 step 5,
	// same rule as Registry.Lookup) — this must NOT match.
	reg := loadListAPIsTestRegistry(t, "proj", []string{"Maps"})
	te := &ToolExecutor{
		apiClient: &fakeListerClient{providers: stdProviders()},
		registry:  reg,
		logger:    zerolog.Nop(),
	}
	res := te.listAPIs(context.Background(), `{}`, "proj", []string{"proj"})
	if !strings.Contains(strings.ToLower(res.Content), "no apis are enabled") {
		t.Errorf("case-mismatched allowlist entry should drop the provider, got %q", res.Content)
	}
}

func TestListAPIs_QuerySubstringFilter_CaseInsensitive(t *testing.T) {
	reg := loadListAPIsTestRegistry(t, "proj", nil)
	te := &ToolExecutor{
		apiClient: &fakeListerClient{providers: stdProviders()},
		registry:  reg,
		logger:    zerolog.Nop(),
	}
	res := te.listAPIs(context.Background(), `{"query":"WEATH"}`, "proj", []string{"proj"})

	var got []listAPIsProvider
	require.NoError(t, json.Unmarshal([]byte(res.Content), &got))
	if len(got) != 1 || got[0].Name != "weather" {
		t.Errorf("query filter over name should match weather only, got %+v", got)
	}

	// Also matches on description.
	res2 := te.listAPIs(context.Background(), `{"query":"ats"}`, "proj", []string{"proj"})
	var got2 []listAPIsProvider
	require.NoError(t, json.Unmarshal([]byte(res2.Content), &got2))
	if len(got2) != 1 || got2[0].Name != "candidates" {
		t.Errorf("query filter over description should match candidates only, got %+v", got2)
	}
}

func TestListAPIs_TruncationAtN50(t *testing.T) {
	var many []apigateway.ProviderInfo
	for i := 0; i < 60; i++ {
		many = append(many, apigateway.ProviderInfo{Name: fmt.Sprintf("p%02d", i), Description: "d"})
	}
	te := &ToolExecutor{
		apiClient: &fakeListerClient{providers: many},
		logger:    zerolog.Nop(),
	}
	res := te.listAPIs(context.Background(), `{}`, "proj", []string{"proj"})
	if !strings.Contains(res.Content, "results truncated") {
		t.Errorf("60 providers with no query should truncate with a note, content=%q", res.Content)
	}
	jsonPart := strings.SplitN(res.Content, "\n", 2)[0]
	var got []listAPIsProvider
	require.NoError(t, json.Unmarshal([]byte(jsonPart), &got))
	if len(got) != 50 {
		t.Errorf("truncated result should cap at 50, got %d", len(got))
	}
}

func TestListAPIs_EmptyResult_Message(t *testing.T) {
	reg := loadListAPIsTestRegistry(t, "proj", []string{"nonexistent"})
	te := &ToolExecutor{
		apiClient: &fakeListerClient{providers: stdProviders()},
		registry:  reg,
		logger:    zerolog.Nop(),
	}
	res := te.listAPIs(context.Background(), `{}`, "proj", []string{"proj"})
	if !strings.Contains(res.Content, `no APIs are enabled for project "proj"`) {
		t.Errorf("empty result message mismatch: %q", res.Content)
	}
}

func TestListAPIs_ProvenanceFirstParty_NoInjectionWarningsOnTemplateSyntax(t *testing.T) {
	reg := loadListAPIsTestRegistry(t, "proj", nil)
	te := &ToolExecutor{
		apiClient: &fakeListerClient{providers: stdProviders()},
		registry:  reg,
		logger:    zerolog.Nop(),
	}
	res := te.listAPIs(context.Background(), `{"query":"candidates"}`, "proj", []string{"proj"})
	if res.Provenance != outputguard.ProvenanceFirstParty {
		t.Errorf("Provenance = %v, want ProvenanceFirstParty", res.Provenance)
	}
	if !strings.Contains(res.Content, "/candidates/{id}") {
		t.Fatalf("expected the templated example in content, got %q", res.Content)
	}
	report := outputguard.ScanWithProvenance(res.Content, res.Provenance)
	if report.HasFinding() {
		t.Errorf("expected no output-guard findings on first-party template-syntax content, got %+v", report.Findings)
	}
}

func TestProviderAllowedForProject_NilRegistry_AllowsAll(t *testing.T) {
	if !providerAllowedForProject(nil, "proj", "anything") {
		t.Error("nil registry should allow all providers")
	}
}

func TestProviderAllowedForProject_MissingProject_AllowsAll(t *testing.T) {
	reg := loadListAPIsTestRegistry(t, "other", nil)
	if !providerAllowedForProject(reg, "ghost", "anything") {
		t.Error("project absent from registry should allow all providers")
	}
}

func TestProviderAllowedForProject_EmptyAllowlist_AllowsAll(t *testing.T) {
	reg := loadListAPIsTestRegistry(t, "proj", nil)
	if !providerAllowedForProject(reg, "proj", "maps") {
		t.Error("empty allowlist should allow all providers")
	}
}

func TestProviderAllowedForProject_Subset(t *testing.T) {
	reg := loadListAPIsTestRegistry(t, "proj", []string{"maps"})
	if !providerAllowedForProject(reg, "proj", "maps") {
		t.Error("maps should be allowed")
	}
	if providerAllowedForProject(reg, "proj", "weather") {
		t.Error("weather should NOT be allowed")
	}
}

func TestProviderAllowedForProject_CaseSensitive(t *testing.T) {
	reg := loadListAPIsTestRegistry(t, "proj", []string{"Maps"})
	if providerAllowedForProject(reg, "proj", "maps") {
		t.Error("case-mismatched allowlist entry must NOT match")
	}
}
