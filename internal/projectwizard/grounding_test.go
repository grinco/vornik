package projectwizard

import (
	"context"
	"strings"
	"testing"
)

// fakeMCPGrounding implements MCPGroundingSource for testing
type fakeMCPGrounding []GroundingServer

func (f fakeMCPGrounding) Servers(_ context.Context) []GroundingServer {
	return f
}

// fakeModelLister implements ModelLister for testing
type fakeModelLister []string

func (f fakeModelLister) Models(_ context.Context) []string {
	return f
}

func TestBuildGrounding(t *testing.T) {
	g := BuildGrounding(context.Background(),
		fakeMCPGrounding{{Name: "slack", Tools: []string{"send_message"}}},
		fakeModelLister{"m1", "m2"},
		[]TemplatePrior{{Slug: "report-pipeline", DisplayName: "Report Pipeline"}},
	)
	for _, want := range []string{"slack", "send_message", "m1", "report-pipeline",
		// addon type keywords
		"mcp_server", "schedule", "rag_source", "secret_requirement",
		"role_prompt_append", "chat_tools", "custom-base",
		// addon field-name keywords — asserting these catches drift from the
		// real applier arg structs in appliers.go (the whole point of the block
		// is to state the EXACT JSON each addon takes).
		"interval", "cadence", "task_type", "label", "allowed_tools",
		"role", "text", "name", "source", "goal"} {
		if !strings.Contains(g, want) {
			t.Errorf("grounding missing %q", want)
		}
	}
}

func TestBuildGrounding_NilSourcesNoPanic(t *testing.T) {
	g := BuildGrounding(context.Background(), nil, nil, nil)
	if g == "" {
		t.Fatal("grounding should still describe the addon vocabulary")
	}
}
