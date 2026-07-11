package registry

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestProjectNarratorYAMLRoundTrip pins the config surface added for task
// 2.3 (chat push, https://docs.vornik.io
// §5.7/§9 Q3): narrator.chat_push and narrator.no_narration must parse from
// project YAML using the exact keys the design doc uses.
func TestProjectNarratorYAMLRoundTrip(t *testing.T) {
	raw := `
projectId: "p1"
swarmId: "s1"
defaultWorkflowId: "w1"
narrator:
  chat_push: true
  no_narration: true
`
	var p Project
	if err := yaml.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !p.Narrator.ChatPush {
		t.Error("Narrator.ChatPush = false, want true")
	}
	if !p.Narrator.NoNarration {
		t.Error("Narrator.NoNarration = false, want true")
	}
}

// TestProjectNarratorDefaultsOff pins the "chat push is opt-in, narration
// is opt-out" default: a project YAML with no narrator block at all must
// resolve to ChatPush=false, NoNarration=false (i.e. narration runs, chat
// push doesn't — a dark chat-push feature by default, since it's strictly
// more intrusive than the UI story panel).
func TestProjectNarratorDefaultsOff(t *testing.T) {
	raw := `
projectId: "p1"
swarmId: "s1"
defaultWorkflowId: "w1"
`
	var p Project
	if err := yaml.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Narrator.ChatPush {
		t.Error("Narrator.ChatPush must default to false")
	}
	if p.Narrator.NoNarration {
		t.Error("Narrator.NoNarration must default to false")
	}
}
