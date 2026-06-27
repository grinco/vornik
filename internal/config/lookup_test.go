package config

import (
	"reflect"
	"testing"
)

func TestLookupByPath(t *testing.T) {
	cfg := &Config{}
	cfg.Instinct.Enabled = true
	cfg.Instinct.Consumers.ApplicationFeedback = true
	cfg.API.AuthEnabled = true
	cfg.Instinct.Model = "qwen3.6:35b"

	cases := []struct {
		path  string
		want  any
		found bool
	}{
		{"instinct.enabled", true, true},
		{"instinct.consumers.application_feedback", true, true},
		{"api.auth_enabled", true, true},
		{"instinct.model", "qwen3.6:35b", true},
		// Missing key.
		{"instinct.nope", nil, false},
		// Entirely missing top-level.
		{"nope.key", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got, ok := LookupByPath(cfg, tc.path)
			if ok != tc.found {
				t.Fatalf("LookupByPath(%q) found=%v, want found=%v", tc.path, ok, tc.found)
			}
			if tc.found && got != tc.want {
				t.Fatalf("LookupByPath(%q) = %v (%T), want %v (%T)", tc.path, got, got, tc.want, tc.want)
			}
		})
	}
}

// TestInstinctConsumers_ToolBudgetDefaultOff verifies the new
// instinct.consumers.tool_budget field defaults to false and is reachable via
// LookupByPath (Slice 4 config plumbing — LLD §7).
func TestInstinctConsumers_ToolBudgetDefaultOff(t *testing.T) {
	cfg := &Config{}
	val, ok := LookupByPath(cfg, "instinct.consumers.tool_budget")
	if !ok {
		t.Fatal("instinct.consumers.tool_budget must be resolvable via LookupByPath")
	}
	if val != false {
		t.Fatalf("instinct.consumers.tool_budget default = %v, want false", val)
	}
}

func TestLookupByPath_NilConfig(t *testing.T) {
	v, ok := LookupByPath(nil, "instinct.enabled")
	if ok || v != nil {
		t.Fatalf("nil config must return nil,false; got %v,%v", v, ok)
	}
}

// TestLookupByPath_EmbeddedField exercises walkPath's anonymous/inline field
// traversal. We construct a minimal struct with an embedded (anonymous) inner
// struct and verify that paths through the embedded field resolve correctly.
func TestLookupByPath_EmbeddedField(t *testing.T) {
	type Inner struct {
		Flag bool `yaml:"flag"`
	}
	type Outer struct {
		Inner        // anonymous embed — no yaml tag
		Name  string `yaml:"name"`
	}

	outer := Outer{
		Inner: Inner{Flag: true},
		Name:  "hello",
	}

	v := reflect.ValueOf(outer)

	// Direct field (no embedding involved).
	got, ok := walkPath(v, []string{"name"})
	if !ok || got != "hello" {
		t.Fatalf("walkPath name: got (%v, %v), want (hello, true)", got, ok)
	}

	// Field accessed through anonymous embed.
	got, ok = walkPath(v, []string{"flag"})
	if !ok || got != true {
		t.Fatalf("walkPath flag (via embed): got (%v, %v), want (true, true)", got, ok)
	}

	// Non-existent path must return false.
	_, ok = walkPath(v, []string{"nope"})
	if ok {
		t.Fatal("walkPath nope: want found=false")
	}
}
