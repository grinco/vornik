package api

import (
	"testing"

	"vornik.io/vornik/internal/mediakind"
)

// The Ollama-compat /api/show surface decides what the daemon ADVERTISES;
// mediakind decides what the daemon ACTS on. Before they shared a list,
// this proxy held its own open-coded patterns, and the two could disagree
// — the harmful direction being an advertised "vision" capability for a
// model the routing gate treats as blind, which invites an operator to
// send images that are then silently handed off.
//
// This test is the anti-drift assertion that justified extracting
// mediakind: for a shared table of model ids, advertisement and routing
// must agree exactly.
//
// see LLD § https://docs.vornik.io §4.1
func TestOllamaCapabilitiesMatchRoutingGate(t *testing.T) {
	models := []string{
		"claude-opus-4-7",
		"gpt-4o",
		"gpt-5.4",
		"gemini-2.5-pro",
		"llava:13b",
		"qwen2.5-vl-7b",
		"pixtral-large",
		"google.gemma-3-27b-it",
		"gemma4:31b",
		"gemma-2-27b-it",
		"glm-5.2",
		"zai.glm-5",
		"gpt-oss:20b",
		"minimax-m2.7",
		"some-model-nobody-has-heard-of",
		"",
	}

	s := &Server{}
	for _, m := range models {
		advertised := false
		for _, c := range s.capabilitiesForModelID(m) {
			if c == "vision" {
				advertised = true
			}
		}
		acted := mediakind.Capabilities(m, nil).Can(mediakind.ModalityVision)
		if advertised != acted {
			t.Errorf("model %q: /api/show advertises vision=%v but the routing gate says %v", m, advertised, acted)
		}
	}
}

// An explicit declaration must reach the advertisement too. An operator
// who declares a pattern-matching model text-only (because that
// provider's path won't actually accept images) must not still see
// "vision" advertised for it — that is precisely the drift the shared
// list exists to prevent.
func TestOllamaCapabilitiesHonourDeclarations(t *testing.T) {
	s := &Server{modelCapabilities: map[string][]mediakind.Modality{
		"claude-opus-4-7": {mediakind.ModalityText},
	}}
	for _, c := range s.capabilitiesForModelID("claude-opus-4-7") {
		if c == "vision" {
			t.Fatal("an explicit text-only declaration must suppress the advertised vision capability")
		}
	}
	// And the always-on capabilities survive.
	caps := s.capabilitiesForModelID("claude-opus-4-7")
	if len(caps) != 2 || caps[0] != "completion" || caps[1] != "tools" {
		t.Errorf("expected [completion tools], got %v", caps)
	}
}
