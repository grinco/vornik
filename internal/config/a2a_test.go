package config

import (
	"testing"
	"time"
)

func TestA2AConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     A2AConfig
		wantErr bool
	}{
		{
			name: "https peer ok",
			cfg:  A2AConfig{Peers: map[string]A2APeer{"vornik_expert": {URL: "https://host/a2a/v1/agents/p/w"}}},
		},
		{
			name:    "http peer without insecure_http rejected",
			cfg:     A2AConfig{Peers: map[string]A2APeer{"lan_expert": {URL: "http://192.0.2.10/a2a/v1/agents/p/w"}}},
			wantErr: true,
		},
		{
			name: "http peer with insecure_http ok",
			cfg:  A2AConfig{Peers: map[string]A2APeer{"lan_expert": {URL: "http://192.0.2.10/a2a/v1/agents/p/w", InsecureHTTP: true}}},
		},
		{
			name:    "bad scheme rejected",
			cfg:     A2AConfig{Peers: map[string]A2APeer{"x": {URL: "ftp://host/a"}}},
			wantErr: true,
		},
		{
			name:    "invalid peer key rejected",
			cfg:     A2AConfig{Peers: map[string]A2APeer{"Vornik Expert": {URL: "https://host/a"}}},
			wantErr: true,
		},
		{
			name:    "missing url rejected",
			cfg:     A2AConfig{Peers: map[string]A2APeer{"x": {URL: "  "}}},
			wantErr: true,
		},
		{
			name:    "negative max_hops rejected",
			cfg:     A2AConfig{Consult: A2AConsultConfig{MaxHops: -1}},
			wantErr: true,
		},
		{
			name: "empty config is valid (feature off)",
			cfg:  A2AConfig{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestA2AConsultConfig_Defaults(t *testing.T) {
	var c A2AConsultConfig
	if got := c.EffectiveTimeout(); got != DefaultConsultTimeout {
		t.Errorf("timeout default = %v, want %v", got, DefaultConsultTimeout)
	}
	if got := c.EffectiveMaxCallsPerTask(); got != DefaultConsultMaxCallsPerTask {
		t.Errorf("max-calls default = %d, want %d", got, DefaultConsultMaxCallsPerTask)
	}
	if got := c.EffectiveMaxHops(); got != DefaultConsultMaxHops {
		t.Errorf("max-hops default = %d, want %d", got, DefaultConsultMaxHops)
	}
	set := A2AConsultConfig{Timeout: "5s", MaxCallsPerTask: 3, MaxHops: 4}
	if set.EffectiveTimeout() != 5*time.Second || set.EffectiveMaxCallsPerTask() != 3 || set.EffectiveMaxHops() != 4 {
		t.Error("explicit values should override defaults")
	}
}

// The peer api_key must be ${VAR}-expanded from the environment like every
// other vornik secret, which requires the config env-walker to recurse into
// the map[string]A2APeer struct values.
func TestA2APeer_APIKeyEnvExpansion(t *testing.T) {
	t.Setenv("TEST_A2A_PEER_KEY", "sk-vornik-secret")
	cfg := &Config{
		A2A: A2AConfig{Peers: map[string]A2APeer{
			"vornik_expert": {URL: "https://host/a2a/v1/agents/p/w", APIKey: "${TEST_A2A_PEER_KEY}"},
		}},
	}
	expandEnvPlaceholders(cfg)
	if got := cfg.A2A.Peers["vornik_expert"].APIKey; got != "sk-vornik-secret" {
		t.Fatalf("api_key = %q, want expanded secret", got)
	}
}
