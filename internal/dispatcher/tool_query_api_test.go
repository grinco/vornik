package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/outputguard"
)

// TestQueryAPI_ProviderAllowlist covers the per-project api_providers
// gate inserted in queryAPI (design §5.4): it runs after the ownership
// check and before apiClient.Call. loadListAPIsTestRegistry (defined in
// tool_list_apis_test.go, same package) stages the on-disk project
// fixture — Registry.Load is the only way to populate it.
func TestQueryAPI_ProviderAllowlist(t *testing.T) {
	t.Run("provider in allowlist reaches Call", func(t *testing.T) {
		reg := loadListAPIsTestRegistry(t, "proj", []string{"maps"})
		fc := &fakeAPIClient{resp: apigateway.Response{Status: 200, Body: "ok"}}
		te := &ToolExecutor{apiClient: fc, registry: reg}
		res := te.queryAPI(context.Background(), `{"provider":"maps","method":"GET","path":"/x"}`, "proj", []string{"*"})
		if !fc.called {
			t.Errorf("allowlisted provider must reach Call, got called=false content=%q", res.Content)
		}
	})

	t.Run("provider registered but not in allowlist is refused before Call", func(t *testing.T) {
		reg := loadListAPIsTestRegistry(t, "proj", []string{"weather"})
		fc := &fakeAPIClient{}
		te := &ToolExecutor{apiClient: fc, registry: reg}
		res := te.queryAPI(context.Background(), `{"provider":"maps","method":"GET","path":"/x"}`, "proj", []string{"*"})
		want := `query_api: provider "maps" is not enabled for project "proj".`
		if res.Content != want {
			t.Errorf("content = %q, want %q", res.Content, want)
		}
		if fc.called {
			t.Error("gateway must not be called when provider is not in the project's allowlist")
		}
	})

	t.Run("case-mismatched allowlist entry is refused", func(t *testing.T) {
		reg := loadListAPIsTestRegistry(t, "proj", []string{"Maps"})
		fc := &fakeAPIClient{}
		te := &ToolExecutor{apiClient: fc, registry: reg}
		res := te.queryAPI(context.Background(), `{"provider":"maps","method":"GET","path":"/x"}`, "proj", []string{"*"})
		want := `query_api: provider "maps" is not enabled for project "proj".`
		if res.Content != want {
			t.Errorf("content = %q, want %q", res.Content, want)
		}
		if fc.called {
			t.Error("gateway must not be called on a case-mismatched allowlist entry")
		}
	})

	t.Run("empty allowlist allows all providers (regression)", func(t *testing.T) {
		reg := loadListAPIsTestRegistry(t, "proj", nil)
		fc := &fakeAPIClient{resp: apigateway.Response{Status: 200, Body: "ok"}}
		te := &ToolExecutor{apiClient: fc, registry: reg}
		res := te.queryAPI(context.Background(), `{"provider":"maps","method":"GET","path":"/x"}`, "proj", []string{"*"})
		if !fc.called || res.Content != "ok" {
			t.Errorf("empty allowlist should allow all providers through, got called=%v content=%q", fc.called, res.Content)
		}
	})

	t.Run("nil registry allows all providers (regression)", func(t *testing.T) {
		fc := &fakeAPIClient{resp: apigateway.Response{Status: 200, Body: "ok"}}
		te := &ToolExecutor{apiClient: fc} // registry left nil
		res := te.queryAPI(context.Background(), `{"provider":"maps","method":"GET","path":"/x"}`, "proj", []string{"*"})
		if !fc.called || res.Content != "ok" {
			t.Errorf("nil registry should allow all providers through, got called=%v content=%q", fc.called, res.Content)
		}
	})
}

// TestQueryAPI_NonEmptyAllowlistBlocksNonAllowlistedProvider_Regression
// guards the design §7 requirement that the refactor to the apiaccess
// adapter does NOT regress the discovery allowlist: a chat session on a
// project with a NON-EMPTY api_providers must still be blocked from a
// provider that is not on the allowlist, before the gateway is called.
func TestQueryAPI_NonEmptyAllowlistBlocksNonAllowlistedProvider_Regression(t *testing.T) {
	reg := loadListAPIsTestRegistry(t, "proj", []string{"weather"}) // non-empty, excludes maps
	fc := &fakeAPIClient{resp: apigateway.Response{Status: 200, Body: "ok"}}
	te := &ToolExecutor{apiClient: fc, registry: reg}
	res := te.queryAPI(context.Background(), `{"provider":"maps","method":"GET","path":"/x"}`, "proj", []string{"*"})
	want := `query_api: provider "maps" is not enabled for project "proj".`
	if res.Content != want {
		t.Errorf("content = %q, want %q", res.Content, want)
	}
	if fc.called {
		t.Error("gateway must not be called for a provider outside a non-empty allowlist")
	}
}

type fakeAPIClient struct {
	resp   apigateway.Response
	err    error
	called bool
}

func (f *fakeAPIClient) Call(_ context.Context, _ apigateway.Request) (apigateway.Response, error) {
	f.called = true
	return f.resp, f.err
}

func TestQueryAPI_NotConfigured(t *testing.T) {
	te := &ToolExecutor{} // apiClient nil
	res := te.queryAPI(context.Background(), `{"provider":"maps","method":"GET","path":"/x"}`, "proj", []string{"proj"})
	if !strings.Contains(strings.ToLower(res.Content), "not configured") {
		t.Errorf("nil client should say not configured, got %q", res.Content)
	}
}

func TestQueryAPI_RequiresActiveProject(t *testing.T) {
	te := &ToolExecutor{apiClient: &fakeAPIClient{}}
	res := te.queryAPI(context.Background(), `{"provider":"maps","method":"GET","path":"/x"}`, "", nil)
	if !strings.Contains(strings.ToLower(res.Content), "project") {
		t.Errorf("empty activeProject should error, got %q", res.Content)
	}
}

func TestQueryAPI_OwnershipGate(t *testing.T) {
	fc := &fakeAPIClient{}
	te := &ToolExecutor{apiClient: fc}
	res := te.queryAPI(context.Background(), `{"provider":"maps","method":"GET","path":"/x"}`, "secret", []string{"news"})
	if !strings.Contains(strings.ToLower(res.Content), "not permitted") {
		t.Errorf("disallowed project should be refused, got %q", res.Content)
	}
	if fc.called {
		t.Error("gateway must not be called when ownership gate fails")
	}
}

func TestQueryAPI_InvalidJSON(t *testing.T) {
	fc := &fakeAPIClient{}
	te := &ToolExecutor{apiClient: fc}
	res := te.queryAPI(context.Background(), `{not-json`, "proj", []string{"*"})
	if !strings.Contains(strings.ToLower(res.Content), "invalid arguments") {
		t.Errorf("malformed args should report invalid arguments, got %q", res.Content)
	}
	if fc.called {
		t.Error("gateway must not be called when args fail to parse")
	}
}

func TestQueryAPI_ProviderRequired(t *testing.T) {
	fc := &fakeAPIClient{}
	te := &ToolExecutor{apiClient: fc}
	res := te.queryAPI(context.Background(), `{"method":"GET","path":"/x"}`, "proj", []string{"*"})
	if !strings.Contains(strings.ToLower(res.Content), "provider") {
		t.Errorf("missing provider should be reported, got %q", res.Content)
	}
	if fc.called {
		t.Error("gateway must not be called without a provider")
	}
}

func TestQueryAPI_DefaultsMethodToGET(t *testing.T) {
	// No method supplied → the gate defaults to GET and the call proceeds
	// (success path), proving read-only-by-default reaches the client.
	fc := &fakeAPIClient{resp: apigateway.Response{Status: 200, Body: `ok`}}
	te := &ToolExecutor{apiClient: fc}
	res := te.queryAPI(context.Background(), `{"provider":"maps","path":"/x"}`, "proj", []string{"*"})
	if !fc.called || res.Content != "ok" {
		t.Errorf("defaulted-method call should reach client, got called=%v content=%q", fc.called, res.Content)
	}
}

func TestQueryAPI_SuccessTaggedThirdParty(t *testing.T) {
	fc := &fakeAPIClient{resp: apigateway.Response{Status: 200, Body: `{"status":"OK"}`}}
	te := &ToolExecutor{apiClient: fc}
	res := te.queryAPI(context.Background(), `{"provider":"maps","method":"GET","path":"/geocode/json"}`, "proj", []string{"*"})
	if res.Provenance != outputguard.ProvenanceThirdParty {
		t.Errorf("provenance = %v, want ThirdParty", res.Provenance)
	}
	if !strings.Contains(res.Content, `"status":"OK"`) {
		t.Errorf("content = %q", res.Content)
	}
}

func TestQueryAPI_ErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{apigateway.ErrUnknownProvider, "unknown provider"},
		{apigateway.ErrMethodNotAllowed, "does not support"},
		{apigateway.ErrGatewayAuth, "authentication failed"},
		{apigateway.ErrUpstreamMethod, "does not support"},
		// A non-sentinel error falls through to the default branch and is
		// surfaced verbatim (still prefixed, still human-readable Content).
		{errors.New("boom"), "boom"},
	}
	for _, tc := range cases {
		fc := &fakeAPIClient{err: tc.err}
		te := &ToolExecutor{apiClient: fc}
		res := te.queryAPI(context.Background(), `{"provider":"maps","method":"POST","path":"/x"}`, "proj", []string{"*"})
		if !strings.Contains(strings.ToLower(res.Content), tc.want) {
			t.Errorf("err %v → %q, want substring %q", tc.err, res.Content, tc.want)
		}
		_ = errors.Is // keep import
	}
}
