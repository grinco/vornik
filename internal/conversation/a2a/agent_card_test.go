package a2a

import (
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

func testWorkflow() *registry.Workflow {
	return &registry.Workflow{
		ID:          "research",
		DisplayName: "Research and Write",
		Description: "Two-step researcher → writer pipeline.",
		Version:     "1.0.0",
		Entrypoint:  "research",
		Steps: map[string]registry.WorkflowStep{
			"research": {Type: "agent", Role: "researcher"},
		},
	}
}

func TestBuildAgentCard_HappyPath(t *testing.T) {
	card, err := BuildAgentCard("https://daemon.example.com", "demo", testWorkflow(), false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if card.ProtocolVersion != AgentCardSpecVersion {
		t.Errorf("protocolVersion: got %q want %q", card.ProtocolVersion, AgentCardSpecVersion)
	}
	if card.Name != "Research and Write" {
		t.Errorf("name: got %q", card.Name)
	}
	if card.URL != "https://daemon.example.com/a2a/v1/agents/demo/research" {
		t.Errorf("URL: got %q", card.URL)
	}
	if !card.Capabilities.Streaming {
		t.Errorf("streaming capability should be true")
	}
	if card.Capabilities.PushNotifications {
		t.Errorf("pushNotifications must be false when pushEnabled=false")
	}
	// With push enabled the capability flips on.
	if pc, _ := BuildAgentCard("https://daemon.example.com", "demo", testWorkflow(), true); !pc.Capabilities.PushNotifications {
		t.Errorf("pushNotifications must be true when pushEnabled=true")
	}
	if len(card.Skills) != 1 || card.Skills[0].ID != "research" {
		t.Errorf("skill list: %#v", card.Skills)
	}
	if card.SecuritySchemes["apiKey"].Name != "X-API-Key" {
		t.Errorf("security scheme: %#v", card.SecuritySchemes)
	}
	if card.Metadata["vornik.projectId"] != "demo" {
		t.Errorf("metadata.vornik.projectId: %v", card.Metadata)
	}
}

func TestBuildAgentCard_NameFallsBackToID(t *testing.T) {
	wf := testWorkflow()
	wf.DisplayName = ""
	card, err := BuildAgentCard("https://example.com", "demo", wf, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if card.Name != "research" {
		t.Errorf("expected name to fall back to workflow id, got %q", card.Name)
	}
}

func TestBuildAgentCard_EmptyBaseSurfacedInMetadata(t *testing.T) {
	card, err := BuildAgentCard("", "demo", testWorkflow(), false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.HasPrefix(card.URL, "/a2a/v1/") {
		t.Errorf("path-only URL when base unset: got %q", card.URL)
	}
	if card.Metadata["vornik.publicBaseUrlUnset"] != true {
		t.Errorf("expected publicBaseUrlUnset metadata flag")
	}
}

func TestBuildAgentCard_RejectsNilWorkflow(t *testing.T) {
	_, err := BuildAgentCard("https://x", "demo", nil, false)
	if err == nil {
		t.Errorf("nil workflow must error")
	}
}

func TestBuildAgentCard_RequiresProject(t *testing.T) {
	_, err := BuildAgentCard("https://x", "", testWorkflow(), false)
	if err == nil {
		t.Errorf("empty projectID must error")
	}
}

func TestBuildAgentCard_IsDeterministic(t *testing.T) {
	// Re-render the same inputs many times; JSON bytes must
	// match byte-for-byte so cached + diffed agent-card files
	// stay stable.
	wf := testWorkflow()
	first, err := BuildAgentCard("https://x", "demo", wf, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	firstBytes, _ := json.Marshal(first)
	for i := 0; i < 10; i++ {
		c, err := BuildAgentCard("https://x", "demo", wf, false)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		b, _ := json.Marshal(c)
		if string(b) != string(firstBytes) {
			t.Errorf("non-deterministic on iter %d\n%s\n%s", i, firstBytes, b)
			break
		}
	}
}

func TestBuildAgentCardIndex_DuplicatesFirstAsTopLevel(t *testing.T) {
	card, _ := BuildAgentCard("https://x", "demo", testWorkflow(), false)
	other := card
	other.Name = "Other"
	other.Skills[0].ID = "other"
	idx := BuildAgentCardIndex([]PublishedAgent{
		{ProjectID: "demo", WorkflowID: "research", Card: card},
		{ProjectID: "demo", WorkflowID: "other", Card: other},
	})
	if idx.Name != "Research and Write" {
		t.Errorf("first agent should populate top level, got %q", idx.Name)
	}
	if len(idx.Agents) != 2 {
		t.Errorf("agents list length: %d", len(idx.Agents))
	}
}

func TestBuildAgentCardIndex_EmptyHasNonNilAgents(t *testing.T) {
	idx := BuildAgentCardIndex(nil)
	if idx.Agents == nil {
		t.Errorf("empty index must have non-nil Agents slice so JSON emits []")
	}
}
