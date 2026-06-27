package config

import "testing"

func boolp(b bool) *bool { return &b }

func TestResolveNodeProfile(t *testing.T) {
	tests := []struct {
		name string
		in   NodeConfig
		want NodeCapabilities
	}{
		{
			name: "absent block defaults to all",
			in:   NodeConfig{},
			want: NodeCapabilities{ServeUI: true, ServeAPI: true, ServeWebhooks: true, RunWorkers: true, RelayMode: false},
		},
		{
			name: "explicit all",
			in:   NodeConfig{Profile: "all"},
			want: NodeCapabilities{ServeUI: true, ServeAPI: true, ServeWebhooks: true, RunWorkers: true, RelayMode: false},
		},
		{
			name: "ui preset",
			in:   NodeConfig{Profile: "ui"},
			want: NodeCapabilities{ServeUI: true},
		},
		{
			name: "worker preset",
			in:   NodeConfig{Profile: "worker"},
			want: NodeCapabilities{ServeAPI: true, RunWorkers: true},
		},
		{
			name: "webhook preset is relay mode",
			in:   NodeConfig{Profile: "webhook"},
			want: NodeCapabilities{ServeWebhooks: true, RelayMode: true},
		},
		{
			name: "flag override on a preset (ui + api hybrid)",
			in:   NodeConfig{Profile: "ui", ServeAPI: boolp(true)},
			want: NodeCapabilities{ServeUI: true, ServeAPI: true},
		},
		{
			name: "webhook with run_workers override is NOT relay mode",
			in:   NodeConfig{Profile: "webhook", RunWorkers: boolp(true)},
			want: NodeCapabilities{ServeWebhooks: true, RunWorkers: true, RelayMode: false},
		},
		{
			name: "unknown profile treated as all (validation rejects separately)",
			in:   NodeConfig{Profile: "bogus"},
			want: NodeCapabilities{ServeUI: true, ServeAPI: true, ServeWebhooks: true, RunWorkers: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveNodeProfile(tc.in)
			if got != tc.want {
				t.Fatalf("ResolveNodeProfile(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNodeConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		in      NodeConfig
		wantErr bool
	}{
		{name: "absent block ok", in: NodeConfig{}, wantErr: false},
		{name: "worker ok", in: NodeConfig{Profile: "worker"}, wantErr: false},
		{name: "unknown profile rejected", in: NodeConfig{Profile: "bogus"}, wantErr: true},
		{
			name:    "no capability enabled rejected",
			in:      NodeConfig{Profile: "ui", ServeUI: boolp(false)},
			wantErr: true,
		},
		{
			name: "relay config completely absent in relay mode",
			in:   NodeConfig{Profile: "webhook"},
			// all four relay fields (upstream, client_cert, client_key, ca) are required
			wantErr: true,
		},
		{
			name: "relay mode with incomplete relay config rejected",
			in: NodeConfig{Profile: "webhook", Relay: RelayConfig{
				Upstream: "https://worker:8443", ClientCert: "c",
				// ClientKey and CA intentionally absent
			}},
			wantErr: true,
		},
		{
			name:    "relay mode reached via run_workers=false override still requires relay fields",
			in:      NodeConfig{Profile: "all", RunWorkers: boolp(false)},
			wantErr: true,
		},
		{
			name: "webhook relay mode with upstream ok",
			in: NodeConfig{Profile: "webhook", Relay: RelayConfig{
				Upstream: "https://worker:8443", ClientCert: "c", ClientKey: "k", CA: "ca",
			}},
			wantErr: false,
		},
		{
			name: "relay config forbidden on a worker node",
			in: NodeConfig{Profile: "worker", Relay: RelayConfig{
				Upstream: "https://worker:8443",
			}},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate(%+v) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			}
		})
	}
}

// Regression guard for the cluster-roles rollout: a config with no node:
// block MUST resolve to every capability on, so existing single-process
// and uniform-replica deployments are byte-for-byte unchanged.
func TestResolveNodeProfile_DefaultConfigRunsEverything(t *testing.T) {
	caps := ResolveNodeProfile(DefaultConfig().Node)
	if !caps.ServeUI || !caps.ServeAPI || !caps.ServeWebhooks || !caps.RunWorkers {
		t.Fatalf("DefaultConfig must enable every capability, got %+v", caps)
	}
	if caps.RelayMode {
		t.Fatalf("DefaultConfig must not be in relay mode, got %+v", caps)
	}
}

func TestNodeConfig_Validate_RelaySemantics(t *testing.T) {
	tests := []struct {
		name    string
		in      NodeConfig
		wantErr bool
	}{
		{
			name: "relay client timeout must parse",
			in: NodeConfig{Profile: "webhook", Relay: RelayConfig{
				Upstream: "https://w:8443", ClientCert: "c", ClientKey: "k", CA: "ca", Timeout: "not-a-duration",
			}},
			wantErr: true,
		},
		{
			name: "relay client timeout must be positive",
			in: NodeConfig{Profile: "webhook", Relay: RelayConfig{
				Upstream: "https://w:8443", ClientCert: "c", ClientKey: "k", CA: "ca", Timeout: "0s",
			}},
			wantErr: true,
		},
		{
			name: "relay client max_retries must be >= 0",
			in: NodeConfig{Profile: "webhook", Relay: RelayConfig{
				Upstream: "https://w:8443", ClientCert: "c", ClientKey: "k", CA: "ca", MaxRetries: -1,
			}},
			wantErr: true,
		},
		{
			name: "valid relay client timeout + retries ok",
			in: NodeConfig{Profile: "webhook", Relay: RelayConfig{
				Upstream: "https://w:8443", ClientCert: "c", ClientKey: "k", CA: "ca", Timeout: "5s", MaxRetries: 3,
			}},
			wantErr: false,
		},
		{
			name:    "relay_ingress requires all of addr,cert,key,client_ca when any set",
			in:      NodeConfig{Profile: "worker", RelayIngress: RelayIngressConfig{Addr: ":8443"}},
			wantErr: true,
		},
		{
			name: "relay_ingress complete is ok on a worker node",
			in: NodeConfig{Profile: "worker", RelayIngress: RelayIngressConfig{
				Addr: ":8443", ServerCert: "s.crt", ServerKey: "s.key", ClientCA: "ca.crt",
			}},
			wantErr: false,
		},
		{
			name: "relay_ingress forbidden on a non-worker node",
			in: NodeConfig{Profile: "webhook", Relay: RelayConfig{
				Upstream: "https://w:8443", ClientCert: "c", ClientKey: "k", CA: "ca",
			}, RelayIngress: RelayIngressConfig{Addr: ":8443", ServerCert: "s.crt", ServerKey: "s.key", ClientCA: "ca.crt"}},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.in.Validate(); (err != nil) != tc.wantErr {
				t.Fatalf("Validate(%+v) err=%v wantErr=%v", tc.in, err, tc.wantErr)
			}
		})
	}
}
