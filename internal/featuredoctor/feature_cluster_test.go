package featuredoctor

import (
	"context"
	"testing"
)

// findPrereq returns a pointer to the named prereq within a Feature, or nil.
func findClusterPrereq(f Feature, name string) *Prereq {
	for i := range f.Prereqs {
		if f.Prereqs[i].Name == name {
			return &f.Prereqs[i]
		}
	}
	return nil
}

func TestClusterFeature_ValidWorkerProfile(t *testing.T) {
	// A node with profile=worker and no relay keys set — both prereqs must be OK.
	f := clusterFeature()
	deps := Deps{
		Config: stubConfig{vals: map[string]any{
			"node.profile": "worker",
		}},
	}

	profilePrereq := findClusterPrereq(f, "node profile valid")
	if profilePrereq == nil {
		t.Fatal("missing 'node profile valid' prereq")
	}
	res := profilePrereq.Check(context.Background(), deps)
	if !res.OK {
		t.Fatalf("valid profile 'worker' must be OK, got %+v", res)
	}

	relayPrereq := findClusterPrereq(f, "relay config coherent")
	if relayPrereq == nil {
		t.Fatal("missing 'relay config coherent' prereq")
	}
	res = relayPrereq.Check(context.Background(), deps)
	if !res.OK {
		t.Fatalf("worker profile with no relay keys must be relay-coherent, got %+v", res)
	}
}

func TestClusterFeature_InvalidProfile(t *testing.T) {
	// An unrecognised profile must make 'node profile valid' unmet and unfixable.
	f := clusterFeature()
	deps := Deps{
		Config: stubConfig{vals: map[string]any{
			"node.profile": "bogus",
		}},
	}

	profilePrereq := findClusterPrereq(f, "node profile valid")
	if profilePrereq == nil {
		t.Fatal("missing 'node profile valid' prereq")
	}
	res := profilePrereq.Check(context.Background(), deps)
	if res.OK {
		t.Fatal("invalid profile must be unmet")
	}
	if res.Fixable {
		t.Fatal("invalid profile is not auto-fixable")
	}
	if res.Detail == "" {
		t.Fatal("unmet prereq must carry a detail message")
	}
}

func TestClusterFeature_RelayUpstreamWithNonRelayProfile(t *testing.T) {
	// node.relay.upstream set but profile is 'worker' (run_workers=true, relay not valid).
	// 'relay config coherent' must be unmet.
	f := clusterFeature()
	deps := Deps{
		Config: stubConfig{vals: map[string]any{
			"node.profile":        "worker",
			"node.relay.upstream": "https://job-tier:8443",
		}},
	}

	relayPrereq := findClusterPrereq(f, "relay config coherent")
	if relayPrereq == nil {
		t.Fatal("missing 'relay config coherent' prereq")
	}
	res := relayPrereq.Check(context.Background(), deps)
	if res.OK {
		t.Fatalf("relay.upstream set on non-relay profile must be unmet, got %+v", res)
	}
	if res.Fixable {
		t.Fatal("relay misconfiguration is not auto-fixable")
	}
}

func TestClusterFeature_CoherentRelayProfile(t *testing.T) {
	// profile=webhook → serve_webhooks=true, run_workers=false by preset.
	// node.relay.upstream set → relay node. Both prereqs must be OK.
	f := clusterFeature()
	deps := Deps{
		Config: stubConfig{vals: map[string]any{
			"node.profile":        "webhook",
			"node.relay.upstream": "https://job-tier:8443",
		}},
	}

	profilePrereq := findClusterPrereq(f, "node profile valid")
	if profilePrereq == nil {
		t.Fatal("missing 'node profile valid' prereq")
	}
	res := profilePrereq.Check(context.Background(), deps)
	if !res.OK {
		t.Fatalf("profile 'webhook' must be valid, got %+v", res)
	}

	relayPrereq := findClusterPrereq(f, "relay config coherent")
	if relayPrereq == nil {
		t.Fatal("missing 'relay config coherent' prereq")
	}
	res = relayPrereq.Check(context.Background(), deps)
	if !res.OK {
		t.Fatalf("webhook profile + relay.upstream set must be relay-coherent, got %+v", res)
	}
}
