package registry

import (
	"strings"
	"testing"
)

func TestMarshalSwarmMarkdown_RoundTrip(t *testing.T) {
	sw := &Swarm{
		ID:          "assistant-swarm",
		DisplayName: "Assistant",
		LeadRole:    "lead",
		Roles: []SwarmRole{
			{Name: "lead", Description: "Leads.", SystemPrompt: "You are the lead."},
			{Name: "researcher", Description: "Researches.", SystemPrompt: "You research."},
		},
	}
	out, err := MarshalSwarmMarkdown(sw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParseSwarmMarkdown(out, "rt.md")
	if err != nil {
		t.Fatalf("parse: %v\noutput:\n%s", err, out)
	}
	if parsed.ID != sw.ID || parsed.LeadRole != sw.LeadRole {
		t.Errorf("structural mismatch: got %#v", parsed)
	}
	if len(parsed.Roles) != 2 {
		t.Fatalf("role count: got %d want 2", len(parsed.Roles))
	}
	prompts := map[string]string{}
	for _, r := range parsed.Roles {
		prompts[r.Name] = r.SystemPrompt
	}
	if prompts["lead"] != "You are the lead." {
		t.Errorf("lead prompt: got %q", prompts["lead"])
	}
	if prompts["researcher"] != "You research." {
		t.Errorf("researcher prompt: got %q", prompts["researcher"])
	}
}

func TestMarshalSwarmMarkdown_RequiresID(t *testing.T) {
	_, err := MarshalSwarmMarkdown(&Swarm{Roles: []SwarmRole{{Name: "r"}}})
	if err == nil || !strings.Contains(err.Error(), "swarmId") {
		t.Errorf("want swarmId required error, got %v", err)
	}
}

func TestMarshalSwarmMarkdown_NilRejected(t *testing.T) {
	_, err := MarshalSwarmMarkdown(nil)
	if err == nil || !strings.Contains(err.Error(), "nil") {
		t.Errorf("want nil error, got %v", err)
	}
}
