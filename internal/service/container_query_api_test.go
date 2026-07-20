package service

import (
	"testing"

	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/config"
)

func TestNewGatewayClient_DisabledReturnsNil(t *testing.T) {
	c, err := newGatewayClient(config.GatewayConfig{Enabled: false})
	if err != nil || c != nil {
		t.Fatalf("disabled → (nil,nil), got (%v,%v)", c, err)
	}
}

func TestNewGatewayClient_MissingAddressReturnsNil(t *testing.T) {
	c, err := newGatewayClient(config.GatewayConfig{Enabled: true, Token: "t"})
	if err != nil || c != nil {
		t.Fatalf("no address → (nil,nil) fail-closed, got (%v,%v)", c, err)
	}
}

func TestNewGatewayClient_MissingTokenReturnsNil(t *testing.T) {
	c, err := newGatewayClient(config.GatewayConfig{Enabled: true, Address: "http://127.0.0.1:8010"})
	if err != nil || c != nil {
		t.Fatalf("no token → (nil,nil) fail-closed, got (%v,%v)", c, err)
	}
}

func TestNewGatewayClient_EnabledBuildsRegistry(t *testing.T) {
	c, err := newGatewayClient(config.GatewayConfig{
		Enabled: true, Address: "http://127.0.0.1:8010", Token: "t",
		Providers: map[string]config.ProviderConfig{
			"maps": {BasePath: "/maps", AllowedMethods: []string{"GET"}},
		},
	})
	if err != nil || c == nil {
		t.Fatalf("enabled → non-nil client, got (%v,%v)", c, err)
	}
}

// TestNewGatewayClient_MapsProviderExamples covers the query_api
// provider-discovery design's Examples round-trip: config.ProviderConfig's
// Examples must survive the config.Providers → apigateway.Registry mapping
// in newGatewayClient, so list_apis can surface them. Verified by
// type-asserting the built client to apigateway.ProviderLister and reading
// back Registry.Describe() — the only way to observe the registry's
// contents from outside the gateway package.
func TestNewGatewayClient_MapsProviderExamples(t *testing.T) {
	c, err := newGatewayClient(config.GatewayConfig{
		Enabled: true, Address: "http://127.0.0.1:8010", Token: "t",
		Providers: map[string]config.ProviderConfig{
			"maps": {
				BasePath:       "/maps",
				AllowedMethods: []string{"GET"},
				Description:    "Google Maps",
				Examples:       []string{"GET /geocode/json?address= — geocode an address"},
			},
		},
	})
	if err != nil || c == nil {
		t.Fatalf("newGatewayClient: (%v, %v)", c, err)
	}

	pl, ok := c.(apigateway.ProviderLister)
	if !ok {
		t.Fatal("gateway client does not satisfy apigateway.ProviderLister")
	}
	infos := pl.ListProviders()
	if len(infos) != 1 || infos[0].Name != "maps" {
		t.Fatalf("infos = %+v, want one entry named maps", infos)
	}
	wantExamples := []string{"GET /geocode/json?address= — geocode an address"}
	if len(infos[0].Examples) != 1 || infos[0].Examples[0] != wantExamples[0] {
		t.Errorf("Examples = %v, want %v", infos[0].Examples, wantExamples)
	}
}
