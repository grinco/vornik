package service

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/registry"
)

// BACKLOG 2026-07-30: the slash command was compared literally to "/vornik", so a
// differently-branded bot on another instance (Holly, in the same workspace) had to
// answer /vornik regardless of its own name. The name belongs to the deployment.
func TestResolveSlackConfig_SlashCommandNameIsPerProject(t *testing.T) {
	t.Setenv("SLACK_SIGNING", "secret")

	for _, tc := range []struct {
		name       string
		configured string
		want       string
	}{
		{"unset falls back to the product default", "", "/vornik"},
		{"a custom name is honoured", "/holly", "/holly"},
		{"a bare name gains the leading slash", "holly", "/holly"},
		{"surrounding whitespace is trimmed", "  /holly  ", "/holly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := resolveSlackConfig(registry.ProjectSlack{
				TeamID:           "T123",
				SigningSecretEnv: "SLACK_SIGNING",
				SlashCommand:     tc.configured,
			}, "project-x")
			if err != nil {
				t.Fatalf("resolveSlackConfig: %v", err)
			}
			if cfg.SlashCommand != tc.want {
				t.Errorf("SlashCommand = %q, want %q", cfg.SlashCommand, tc.want)
			}
		})
	}
}

// The progress placeholder is on by default and opt-out-able, because it costs an extra
// message per slow turn and an operator may prefer silence. Zero means "take the
// default" and must not read as "disabled", or the feature would never fire.
func TestResolveSlackConfig_ProgressSignalOptOut(t *testing.T) {
	t.Setenv("SLACK_SIGNING", "secret")

	cfg, err := resolveSlackConfig(registry.ProjectSlack{
		TeamID:           "T123",
		SigningSecretEnv: "SLACK_SIGNING",
	}, "project-x")
	if err != nil {
		t.Fatalf("resolveSlackConfig: %v", err)
	}
	if cfg.ProgressSignalDelay != 0 {
		t.Errorf("ProgressSignalDelay = %v, want 0 so the channel applies its default",
			cfg.ProgressSignalDelay)
	}

	disabled := false
	cfg, err = resolveSlackConfig(registry.ProjectSlack{
		TeamID:           "T123",
		SigningSecretEnv: "SLACK_SIGNING",
		ProgressSignal:   &disabled,
	}, "project-x")
	if err != nil {
		t.Fatalf("resolveSlackConfig: %v", err)
	}
	if cfg.ProgressSignalDelay >= 0 {
		t.Errorf("ProgressSignalDelay = %v, want negative to disable the signal",
			cfg.ProgressSignalDelay)
	}

	enabled := true
	cfg, err = resolveSlackConfig(registry.ProjectSlack{
		TeamID:            "T123",
		SigningSecretEnv:  "SLACK_SIGNING",
		ProgressSignal:    &enabled,
		ProgressSignalGap: "5s",
	}, "project-x")
	if err != nil {
		t.Fatalf("resolveSlackConfig: %v", err)
	}
	if cfg.ProgressSignalDelay != 5*time.Second {
		t.Errorf("ProgressSignalDelay = %v, want 5s", cfg.ProgressSignalDelay)
	}
}

// A malformed duration must fail at boot rather than silently disabling the signal —
// the operator asked for a specific gap and got neither it nor a warning.
func TestResolveSlackConfig_RejectsUnparseableProgressGap(t *testing.T) {
	t.Setenv("SLACK_SIGNING", "secret")
	if _, err := resolveSlackConfig(registry.ProjectSlack{
		TeamID:            "T123",
		SigningSecretEnv:  "SLACK_SIGNING",
		ProgressSignalGap: "soon",
	}, "project-x"); err == nil {
		t.Fatal("expected an error for an unparseable progress_signal_gap")
	}
}
