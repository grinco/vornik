package apiaccess

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/outputguard"
)

// fakeClient records the requests it receives and returns a canned
// response/error. It implements apigateway.Client only.
type fakeClient struct {
	resp  apigateway.Response
	err   error
	calls []apigateway.Request
}

func (f *fakeClient) Call(_ context.Context, req apigateway.Request) (apigateway.Response, error) {
	f.calls = append(f.calls, req)
	return f.resp, f.err
}

// fakeLister additionally implements apigateway.ProviderLister.
type fakeLister struct {
	fakeClient
	providers []apigateway.ProviderInfo
}

func (f *fakeLister) ListProviders() []apigateway.ProviderInfo { return f.providers }

func staticAllow(providers ...string) func(string) ([]string, error) {
	return func(string) ([]string, error) { return providers, nil }
}

func TestQuery_EmptyProviderRejectedBeforeAllowlist(t *testing.T) {
	fc := &fakeClient{resp: apigateway.Response{Body: "ok"}}
	allowlistConsulted := false
	svc := &Service{
		Client: fc,
		Allowlist: func(string) ([]string, error) {
			allowlistConsulted = true
			return nil, nil
		},
	}
	out := svc.Query(context.Background(), "proj", "", apigateway.Request{Method: "GET", Path: "/x"})
	if out.Refusal == "" || !strings.Contains(out.Refusal, "provider") {
		t.Errorf("empty provider should be refused mentioning provider, got refusal=%q body=%q", out.Refusal, out.Body)
	}
	if len(fc.calls) != 0 {
		t.Error("gateway must not be called when provider is empty")
	}
	if allowlistConsulted {
		t.Error("empty-provider must be rejected BEFORE the allowlist gate")
	}
}

func TestQuery_AllowlistDisallowedProviderRefused(t *testing.T) {
	fc := &fakeClient{resp: apigateway.Response{Body: "ok"}}
	svc := &Service{Client: fc, Allowlist: staticAllow("weather")}
	out := svc.Query(context.Background(), "proj", "", apigateway.Request{Provider: "maps", Method: "GET"})
	want := `provider "maps" is not enabled for project "proj".`
	if out.Refusal != want {
		t.Errorf("refusal = %q, want %q", out.Refusal, want)
	}
	if len(fc.calls) != 0 {
		t.Error("gateway must not be called for a disallowed provider")
	}
}

func TestQuery_EmptyAllowlistAllowsAll(t *testing.T) {
	fc := &fakeClient{resp: apigateway.Response{Body: "ok"}}
	svc := &Service{Client: fc, Allowlist: staticAllow()} // empty ⇒ all
	out := svc.Query(context.Background(), "proj", "", apigateway.Request{Provider: "maps", Method: "GET"})
	if out.Refusal != "" || out.Body != "ok" {
		t.Errorf("empty allowlist should allow all, got refusal=%q body=%q", out.Refusal, out.Body)
	}
}

func TestQuery_NilAllowlistAllowsAll(t *testing.T) {
	fc := &fakeClient{resp: apigateway.Response{Body: "ok"}}
	svc := &Service{Client: fc} // nil Allowlist ⇒ all
	out := svc.Query(context.Background(), "proj", "", apigateway.Request{Provider: "maps", Method: "GET"})
	if out.Refusal != "" || out.Body != "ok" {
		t.Errorf("nil allowlist should allow all, got refusal=%q body=%q", out.Refusal, out.Body)
	}
}

func TestQuery_AllowlistResolverErrorRefuses(t *testing.T) {
	fc := &fakeClient{resp: apigateway.Response{Body: "ok"}}
	svc := &Service{
		Client:    fc,
		Allowlist: func(string) ([]string, error) { return nil, errors.New("registry down") },
	}
	out := svc.Query(context.Background(), "proj", "", apigateway.Request{Provider: "maps", Method: "GET"})
	if out.Refusal == "" {
		t.Fatal("resolver error should refuse the call (fail-closed)")
	}
	if len(fc.calls) != 0 {
		t.Error("gateway must not be called when the allowlist resolver errors")
	}
}

func TestQuery_DefaultsMethodToGET(t *testing.T) {
	fc := &fakeClient{resp: apigateway.Response{Body: "ok"}}
	svc := &Service{Client: fc}
	out := svc.Query(context.Background(), "proj", "", apigateway.Request{Provider: "maps", Path: "/x"})
	if out.Refusal != "" {
		t.Fatalf("unexpected refusal: %q", out.Refusal)
	}
	if len(fc.calls) != 1 || fc.calls[0].Method != "GET" {
		t.Errorf("method should default to GET before Call, got calls=%+v", fc.calls)
	}
}

func TestQuery_AgentWriteRefusedWhenNil(t *testing.T) {
	fc := &fakeClient{resp: apigateway.Response{Body: "ok"}}
	svc := &Service{Client: fc} // AgentWrites nil ⇒ read-only
	out := svc.Query(context.Background(), "proj", "researcher", apigateway.Request{Provider: "maps", Method: "POST"})
	if out.Refusal == "" || !strings.Contains(strings.ToLower(out.Refusal), "read-only") {
		t.Errorf("nil AgentWrites should refuse a write, got refusal=%q", out.Refusal)
	}
	if len(fc.calls) != 0 {
		t.Error("gateway must not be called for a refused write")
	}
}

func TestQuery_AgentWriteRefusedWhenFalse(t *testing.T) {
	fc := &fakeClient{resp: apigateway.Response{Body: "ok"}}
	svc := &Service{Client: fc, AgentWrites: func(string, string) bool { return false }}
	out := svc.Query(context.Background(), "proj", "researcher", apigateway.Request{Provider: "maps", Method: "DELETE"})
	if out.Refusal == "" {
		t.Error("AgentWrites returning false should refuse a write")
	}
	if len(fc.calls) != 0 {
		t.Error("gateway must not be called for a refused write")
	}
}

func TestQuery_AgentWriteAllowedWhenTrue(t *testing.T) {
	fc := &fakeClient{resp: apigateway.Response{Body: "ok"}}
	svc := &Service{Client: fc, AgentWrites: func(string, string) bool { return true }}
	out := svc.Query(context.Background(), "proj", "", apigateway.Request{Provider: "candidates", Method: "POST"})
	if out.Refusal != "" {
		t.Fatalf("AgentWrites=true should allow the write through, got refusal=%q", out.Refusal)
	}
	if len(fc.calls) != 1 || fc.calls[0].Method != "POST" {
		t.Errorf("write should reach Call unchanged, got calls=%+v", fc.calls)
	}
}

func TestQuery_GatewaySentinelMapped(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"unknown", apigateway.ErrUnknownProvider, "unknown provider"},
		{"method", apigateway.ErrMethodNotAllowed, "does not support"},
		{"upstream", apigateway.ErrUpstreamMethod, "does not support"},
		{"auth", apigateway.ErrGatewayAuth, "authentication failed"},
		{"other", errors.New("transport failed for https://gateway/x?token=LEAK-ME"), "gateway request failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeClient{err: tc.err}
			svc := &Service{Client: fc, AgentWrites: func(string, string) bool { return true }}
			out := svc.Query(context.Background(), "proj", "", apigateway.Request{Provider: "maps", Method: "POST", Path: "/x"})
			if out.Refusal == "" || !strings.Contains(strings.ToLower(out.Refusal), tc.want) {
				t.Errorf("err %v → refusal %q, want substring %q", tc.err, out.Refusal, tc.want)
			}
			// A mapped refusal is human-readable, never a bare Go error prefix.
			if out.Body != "" {
				t.Errorf("refusal should carry no body, got %q", out.Body)
			}
			if strings.Contains(out.Refusal, "LEAK-ME") || strings.Contains(out.Refusal, "token=") {
				t.Errorf("gateway refusal leaked request data: %q", out.Refusal)
			}
		})
	}
}

func TestQuery_SuccessTagsThirdPartyAndDoesNotAlterBody(t *testing.T) {
	// A body that outputguard WOULD redact (secret-looking) plus a large
	// payload: apiaccess must return it verbatim (no redact, no cap).
	secretish := `{"api_key":"sk-live-ABCDEF1234567890","note":"` + strings.Repeat("x", 100_000) + `"}`
	fc := &fakeClient{resp: apigateway.Response{Status: 200, Body: secretish}}
	svc := &Service{Client: fc}
	out := svc.Query(context.Background(), "proj", "", apigateway.Request{Provider: "maps", Method: "GET", Path: "/x"})
	if out.Refusal != "" {
		t.Fatalf("unexpected refusal: %q", out.Refusal)
	}
	if out.Provenance != outputguard.ProvenanceThirdParty {
		t.Errorf("provenance = %v, want ThirdParty", out.Provenance)
	}
	if out.Body != secretish {
		t.Errorf("body must be returned verbatim (no redact/cap); len got=%d want=%d", len(out.Body), len(secretish))
	}
}

func TestListProviders_NonListerReturnsEmpty(t *testing.T) {
	svc := &Service{Client: &fakeClient{}} // Call-only, not a lister
	got, truncated, err := svc.ListProviders(context.Background(), "proj", "")
	if err != nil || got != nil || truncated {
		t.Errorf("non-lister client should return (nil, false, nil), got %+v truncated=%v err=%v", got, truncated, err)
	}
}

func TestListProviders_NilClientReturnsEmpty(t *testing.T) {
	svc := &Service{}
	got, truncated, err := svc.ListProviders(context.Background(), "proj", "")
	if err != nil || got != nil || truncated {
		t.Errorf("nil client should return (nil, false, nil), got %+v truncated=%v err=%v", got, truncated, err)
	}
}

func stdProviders() []apigateway.ProviderInfo {
	return []apigateway.ProviderInfo{
		{Name: "maps", Description: "Geocoding and directions"},
		{Name: "candidates", Description: "Candidate ATS lookup"},
		{Name: "weather", Description: "Weather forecast"},
	}
}

func TestListProviders_EmptyAllowlistReturnsAll(t *testing.T) {
	svc := &Service{Client: &fakeLister{providers: stdProviders()}, Allowlist: staticAllow()}
	got, truncated, err := svc.ListProviders(context.Background(), "proj", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || truncated {
		t.Errorf("empty allowlist ⇒ all (untruncated), got %d truncated=%v", len(got), truncated)
	}
}

func TestListProviders_SubsetAllowlistFilters(t *testing.T) {
	svc := &Service{Client: &fakeLister{providers: stdProviders()}, Allowlist: staticAllow("maps", "weather")}
	got, _, err := svc.ListProviders(context.Background(), "proj", "")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, p := range got {
		names[p.Name] = true
	}
	if len(got) != 2 || !names["maps"] || !names["weather"] || names["candidates"] {
		t.Errorf("subset allowlist should keep only maps+weather, got %+v", got)
	}
}

func TestListProviders_QueryFilterCaseInsensitive(t *testing.T) {
	svc := &Service{Client: &fakeLister{providers: stdProviders()}}
	got, _, err := svc.ListProviders(context.Background(), "proj", "WEATH")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "weather" {
		t.Errorf("query filter should match weather only, got %+v", got)
	}
	// Match on description too.
	got2, _, _ := svc.ListProviders(context.Background(), "proj", "ats")
	if len(got2) != 1 || got2[0].Name != "candidates" {
		t.Errorf("description match should return candidates only, got %+v", got2)
	}
}

func TestListProviders_CapAt50(t *testing.T) {
	var many []apigateway.ProviderInfo
	for i := 0; i < 60; i++ {
		many = append(many, apigateway.ProviderInfo{Name: string(rune('a'+i%26)) + strings.Repeat("z", i)})
	}
	svc := &Service{Client: &fakeLister{providers: many}}
	got, truncated, err := svc.ListProviders(context.Background(), "proj", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxListProviders {
		t.Errorf("cap should bound at %d, got %d", maxListProviders, len(got))
	}
	if !truncated {
		t.Error("60 providers should report truncated=true (entries were dropped)")
	}
}

// F6: exactly maxListProviders is NOT truncated — nothing was dropped. The
// dispatcher's old len(kept)==cap heuristic false-positived here.
func TestListProviders_ExactlyCapNotTruncated(t *testing.T) {
	var many []apigateway.ProviderInfo
	for i := 0; i < maxListProviders; i++ {
		many = append(many, apigateway.ProviderInfo{Name: fmt.Sprintf("p%03d", i)})
	}
	svc := &Service{Client: &fakeLister{providers: many}}
	got, truncated, err := svc.ListProviders(context.Background(), "proj", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxListProviders {
		t.Errorf("expected all %d providers, got %d", maxListProviders, len(got))
	}
	if truncated {
		t.Error("exactly the cap size must NOT report truncated (nothing dropped)")
	}
}

func TestListProviders_ResolverErrorPropagates(t *testing.T) {
	svc := &Service{
		Client:    &fakeLister{providers: stdProviders()},
		Allowlist: func(string) ([]string, error) { return nil, errors.New("boom") },
	}
	_, _, err := svc.ListProviders(context.Background(), "proj", "")
	if err == nil {
		t.Error("resolver error should propagate")
	}
}
