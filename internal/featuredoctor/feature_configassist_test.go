package featuredoctor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

type stubDurability struct{ rep DurabilityReport }

func (s stubDurability) ProbeDurability(context.Context) DurabilityReport { return s.rep }

type stubPeers struct{ names []string }

func (s stubPeers) A2APeerNames() []string { return s.names }

func prereqNamed(t *testing.T, f Feature, name string) Prereq {
	t.Helper()
	for _, p := range f.Prereqs {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("feature %q has no prereq %q", f.ID, name)
	return Prereq{}
}

func TestConfigAssistantFeature_RegisteredCommunity(t *testing.T) {
	var found bool
	for _, f := range Registry() {
		if f.ID == "config-assistant" {
			found = true
			if f.Edition != "community" {
				t.Fatalf("config-assistant must be Community, got %q", f.Edition)
			}
			if f.Gates[0].Key != "config_assistant.enabled" {
				t.Fatalf("gate must be config_assistant.enabled, got %+v", f.Gates)
			}
		}
	}
	if !found {
		t.Fatal("config-assistant not registered")
	}
}

// Review R6: the durability contract is a REFUSAL, not a warning, and a
// missing prober refuses too (a build that cannot establish the contract
// must not enable the assistant).
func TestConfigAssistantPrereq_Durability(t *testing.T) {
	f := configAssistantFeature()
	p := prereqNamed(t, f, "journal durability contract establishable")
	if r := p.Check(context.Background(), Deps{}); r.OK || r.Fixable {
		t.Fatalf("nil prober must refuse, got %+v", r)
	}
	if r := p.Check(context.Background(), Deps{Durability: stubDurability{DurabilityReport{Driver: "sqlite", OK: false, Detail: "synchronous=NORMAL"}}}); r.OK {
		t.Fatalf("non-durable store must refuse, got %+v", r)
	} else if r.Remediation == "" {
		t.Fatal("refusal must carry a remediation")
	}
	if r := p.Check(context.Background(), Deps{Durability: stubDurability{DurabilityReport{Driver: "postgres", OK: true, Detail: "synchronous_commit=on"}}}); !r.OK {
		t.Fatalf("durable store must pass, got %+v", r)
	}
}

// Design §7.3 / test 37: a same-family pair is refused by the doctor; a
// different-family pair passes; an unclassifiable id fails closed; an
// empty assistant id falls back to chat.model.
func TestConfigAssistantPrereq_JudgeFamily(t *testing.T) {
	f := configAssistantFeature()
	p := prereqNamed(t, f, "assistant and judge models are different families")
	cfg := func(m map[string]any) Deps { return Deps{Config: stubConfig{vals: m}} }

	if r := p.Check(context.Background(), cfg(map[string]any{
		"config_assistant.model": "glm-5.2:cloud", "config_assistant.judge_model": "glm-4.5-air",
	})); r.OK {
		t.Fatalf("same family must be refused, got %+v", r)
	}
	if r := p.Check(context.Background(), cfg(map[string]any{
		"config_assistant.model": "glm-5.2:cloud", "config_assistant.judge_model": "nvidia.nemotron-nano-9b-v2",
	})); !r.OK {
		t.Fatalf("different family must pass, got %+v", r)
	}
	if r := p.Check(context.Background(), cfg(map[string]any{
		"chat.model": "glm-5.2:cloud", "config_assistant.judge_model": "openai.gpt-oss-20b-1:0",
	})); !r.OK {
		t.Fatalf("empty assistant id must fall back to chat.model, got %+v", r)
	}
	if r := p.Check(context.Background(), cfg(map[string]any{
		"config_assistant.model": "glm-5.2:cloud", "config_assistant.judge_model": "mystery-9b",
	})); r.OK {
		t.Fatalf("unclassifiable judge must fail CLOSED, got %+v", r)
	}
	if r := p.Check(context.Background(), cfg(map[string]any{
		"config_assistant.model": "glm-5.2:cloud",
	})); r.OK {
		t.Fatalf("missing judge must refuse, got %+v", r)
	}
	if r := p.Check(context.Background(), Deps{}); r.OK {
		t.Fatalf("nil config must refuse, got %+v", r)
	}
}

func TestConfigAssistantVerify(t *testing.T) {
	f := configAssistantFeature()
	good := Deps{
		Config:     stubConfig{vals: map[string]any{"config_assistant.model": "glm-5.2:cloud", "config_assistant.judge_model": "nvidia.nemotron-nano-9b-v2"}},
		Durability: stubDurability{DurabilityReport{Driver: "sqlite", OK: true, Detail: "synchronous=FULL"}},
	}
	if r := f.Verify(context.Background(), good); !r.OK {
		t.Fatalf("verify must pass, got %+v", r)
	}
	bad := good
	bad.Durability = stubDurability{DurabilityReport{Driver: "sqlite", OK: false, Detail: "synchronous=OFF"}}
	if r := f.Verify(context.Background(), bad); r.OK {
		t.Fatalf("verify must fail on a non-durable store, got %+v", r)
	}
}

func TestArchitectConsultFeature(t *testing.T) {
	var f Feature
	for _, x := range Registry() {
		if x.ID == "architect-consult" {
			f = x
		}
	}
	if f.ID == "" {
		t.Fatal("architect-consult not registered")
	}
	if f.Edition != "community" {
		t.Fatalf("architect-consult must be Community (contract-gated, plan §8), got %q", f.Edition)
	}
	if f.Gates[0].Key != "config_assistant.consult.enabled" {
		t.Fatalf("gate must be config_assistant.consult.enabled, got %+v", f.Gates)
	}

	assistantOn := prereqNamed(t, f, "configuration assistant enabled")
	if r := assistantOn.Check(context.Background(), Deps{Config: stubConfig{vals: map[string]any{"config_assistant.enabled": false}}}); r.OK {
		t.Fatalf("assistant off must block consult, got %+v", r)
	}

	peer := prereqNamed(t, f, "a2a.peers entry for the architect")
	withPeer := Deps{Config: stubConfig{vals: map[string]any{"config_assistant.consult.peer": "vornik_architect"}}}
	if r := peer.Check(context.Background(), withPeer); r.OK {
		t.Fatalf("nil peer lister must refuse, got %+v", r)
	}
	withPeer.A2APeers = stubPeers{names: []string{"other"}}
	if r := peer.Check(context.Background(), withPeer); r.OK {
		t.Fatalf("missing peer must refuse, got %+v", r)
	}
	withPeer.A2APeers = stubPeers{names: []string{"vornik_architect"}}
	if r := peer.Check(context.Background(), withPeer); !r.OK {
		t.Fatalf("configured peer must pass, got %+v", r)
	}
	if r := peer.Check(context.Background(), Deps{Config: stubConfig{vals: map[string]any{}}, A2APeers: stubPeers{}}); r.OK {
		t.Fatalf("empty peer key must refuse, got %+v", r)
	}

	audit := prereqNamed(t, f, "admin audit repository wired")
	if r := audit.Check(context.Background(), Deps{}); r.OK || r.Fixable {
		t.Fatalf("nil audit repo must refuse (R8 fail-closed), got %+v", r)
	}
	if r := audit.Check(context.Background(), Deps{AdminAudit: stubAdminAudit{}}); !r.OK {
		t.Fatalf("wired audit repo must pass, got %+v", r)
	}

	// Verify: peer + audit both required.
	if r := f.Verify(context.Background(), Deps{Config: withPeer.Config, A2APeers: withPeer.A2APeers}); r.OK {
		t.Fatalf("verify must fail without an audit sink, got %+v", r)
	}
	if r := f.Verify(context.Background(), Deps{Config: withPeer.Config, A2APeers: withPeer.A2APeers, AdminAudit: stubAdminAudit{}}); !r.OK {
		t.Fatalf("verify must pass with peer + audit, got %+v", r)
	}
}

type stubAdminAudit struct{}

func (stubAdminAudit) Insert(context.Context, *persistence.AdminAuditEntry) error { return nil }
func (stubAdminAudit) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}
