package service

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
)

func boolp(b bool) *bool { return &b }

func TestResolveChatHealthConfig_DefaultsOn(t *testing.T) {
	cfg, ok := resolveChatHealthConfig(config.ChatHealthConfig{})
	if !ok {
		t.Fatal("absent block must default to enabled")
	}
	if cfg.Window != time.Minute || cfg.MinSamples != 5 || cfg.FailureRate != 0.5 || cfg.OpenCooldown != 30*time.Second {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestResolveChatHealthConfig_ExplicitDisable(t *testing.T) {
	if _, ok := resolveChatHealthConfig(config.ChatHealthConfig{Enabled: boolp(false)}); ok {
		t.Fatal("enabled:false must disable")
	}
	// Explicit enable stays on.
	if _, ok := resolveChatHealthConfig(config.ChatHealthConfig{Enabled: boolp(true)}); !ok {
		t.Fatal("enabled:true must enable")
	}
}

func TestResolveChatHealthConfig_Overrides(t *testing.T) {
	cfg, ok := resolveChatHealthConfig(config.ChatHealthConfig{
		Window: "90s", MinSamples: 10, FailureRate: 0.7, OpenCooldown: "45s",
	})
	if !ok {
		t.Fatal("should be enabled")
	}
	if cfg.Window != 90*time.Second || cfg.MinSamples != 10 || cfg.FailureRate != 0.7 || cfg.OpenCooldown != 45*time.Second {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	// A bad duration falls back to the default, doesn't error.
	cfg2, _ := resolveChatHealthConfig(config.ChatHealthConfig{Window: "garbage"})
	if cfg2.Window != time.Minute {
		t.Fatalf("bad window should fall back to default, got %v", cfg2.Window)
	}
}
