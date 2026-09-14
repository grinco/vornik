package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestAssistantConfig_Defaults(t *testing.T) {
	cfg := DefaultConfig()
	ca := cfg.ConfigAssistant
	if ca.Enabled || ca.Paused || ca.Consult.Enabled || ca.ChatDoor {
		t.Fatalf("assistant, consult and chat door must default OFF: %+v", ca)
	}
	if ca.MaxOutputBytes != ConfigAssistantDefaultMaxOutputBytes || ca.MaxToolTurns != ConfigAssistantDefaultMaxToolTurns {
		t.Fatalf("caps not defaulted: %+v", ca)
	}
	if ca.EffectiveRequestTimeout() != 5*time.Minute || ca.Consult.EffectiveTimeout() != 2*time.Minute {
		t.Fatalf("timeouts not defaulted: %s / %s", ca.EffectiveRequestTimeout(), ca.Consult.EffectiveTimeout())
	}
	if ca.ClassEDailyCap != ConfigAssistantDefaultClassEDailyCap || ca.MaxConcurrentPerProject != 1 {
		t.Fatalf("budgets not defaulted: %+v", ca)
	}
	for _, k := range []string{"config_assistant.enabled", "config_assistant.consult.enabled", "config_assistant.model", "config_assistant.judge_model", "config_assistant.consult.peer"} {
		if _, ok := LookupByPath(cfg, k); !ok {
			t.Fatalf("%s must resolve via LookupByPath (doctor gate / prereq key)", k)
		}
	}
}

// Design test 19 (schema half): there exists NO config key that enables a
// class C, D or E auto-apply. The only opt-in vocabulary is A, B1, B2 and a
// C/D/E value is a validation error, not a silently-ignored one.
func TestAssistantConfig_NoClassCDEAutoApplyKey(t *testing.T) {
	for _, cl := range []string{"C", "D", "E", "c", "e"} {
		c := AssistantConfig{AutoApply: map[string]AssistantAutoApply{"assistant": {Classes: []string{cl}}}}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "can never auto-apply") {
			t.Fatalf("class %s auto-apply must be refused at validation, got %v", cl, err)
		}
	}
	ok := AssistantConfig{AutoApply: map[string]AssistantAutoApply{"assistant": {Classes: []string{"A", "b2"}}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("A/B2 opt-in must validate: %v", err)
	}
	if !ok.AutoApplyAllows("assistant", "B2") || ok.AutoApplyAllows("assistant", "C") || ok.AutoApplyAllows("other", "A") {
		t.Fatal("AutoApplyAllows must be per-project and per-class")
	}
}

func TestAssistantConfig_Validate(t *testing.T) {
	cases := []struct {
		name string
		c    AssistantConfig
		want string
	}{
		{"bad disabled class", AssistantConfig{DisabledClasses: []string{"F"}}, "disabled_classes"},
		{"bad timeout", AssistantConfig{RequestTimeout: "later"}, "request_timeout"},
		{"consult on without peer", AssistantConfig{Consult: AssistantConsult{Enabled: true}}, "consult.peer"},
		{"bad consult timeout", AssistantConfig{Consult: AssistantConsult{Timeout: "x"}}, "consult.timeout"},
		{"negative cap", AssistantConfig{ClassEDailyCap: -1}, "numeric limits"},
	}
	for _, tc := range cases {
		err := tc.c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
	good := AssistantConfig{DisabledClasses: []string{"c", "E"}, Consult: AssistantConsult{Enabled: true, Peer: "vornik_architect"}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good config must validate: %v", err)
	}
	if !good.ClassDisabled("C") || good.ClassDisabled("A") {
		t.Fatal("ClassDisabled must be case-insensitive and exact")
	}
}

func TestAssistantConfig_YAML(t *testing.T) {
	src := "api:\n  auth_enabled: false\nconfig_assistant:\n  enabled: true\n  model: glm-5.2:cloud\n  judge_model: nvidia.nemotron-nano-9b-v2\n  paused: true\n  disabled_classes: [C]\n  auto_apply:\n    assistant:\n      classes: [A]\n  consult:\n    enabled: true\n    peer: vornik_architect\n"
	if err := ValidateBytes([]byte(src)); err != nil {
		t.Fatalf("ValidateBytes: %v", err)
	}
	cfg := DefaultConfig()
	if err := yaml.Unmarshal([]byte(src), cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.ConfigAssistant.Enabled || cfg.ConfigAssistant.JudgeModel != "nvidia.nemotron-nano-9b-v2" || !cfg.ConfigAssistant.Paused {
		t.Fatalf("yaml did not land: %+v", cfg.ConfigAssistant)
	}
}
