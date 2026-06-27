package service

import (
	"testing"

	"vornik.io/vornik/internal/config"
)

func TestResolveWizardModel(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"nil config", nil, ""},
		{"unset → inherit chat default", &config.Config{}, ""},
		{
			"explicit wizard model",
			&config.Config{Chat: config.ChatConfig{Model: "minimax.minimax-m2", WizardModel: "anthropic.claude-sonnet"}},
			"anthropic.claude-sonnet",
		},
		{
			"trimmed",
			&config.Config{Chat: config.ChatConfig{WizardModel: "  google/gemini-3.1-pro  "}},
			"google/gemini-3.1-pro",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveWizardModel(c.cfg); got != c.want {
				t.Errorf("resolveWizardModel = %q, want %q", got, c.want)
			}
		})
	}
}
