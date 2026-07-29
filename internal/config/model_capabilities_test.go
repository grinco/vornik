package config

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/mediakind"
)

func TestChatConfig_DeclaredModalities(t *testing.T) {
	c := ChatConfig{ModelCapabilities: map[string][]string{
		"glm-5.2":               {"text"},
		"google.gemma-3-27b-it": {"text", "vision"},
	}}
	declared, err := c.DeclaredModalities()
	if err != nil {
		t.Fatalf("DeclaredModalities: %v", err)
	}
	if mediakind.Capabilities("glm-5.2", declared).Can(mediakind.ModalityVision) {
		t.Error("glm-5.2 declared text-only must not resolve sighted")
	}
	if !mediakind.Capabilities("google.gemma-3-27b-it", declared).Can(mediakind.ModalityVision) {
		t.Error("gemma-3 declared with vision must resolve sighted")
	}
}

func TestChatConfig_DeclaredModalities_Empty(t *testing.T) {
	declared, err := ChatConfig{}.DeclaredModalities()
	if err != nil {
		t.Fatalf("DeclaredModalities on empty config: %v", err)
	}
	if declared != nil {
		t.Errorf("expected nil map for undeclared config, got %v", declared)
	}
}

// A typo must fail at load, not silently resolve text-only — an operator
// who wrote "visoin" would otherwise see handovers with no explanation.
func TestChatConfig_DeclaredModalities_RejectsUnknown(t *testing.T) {
	_, err := ChatConfig{ModelCapabilities: map[string][]string{
		"some-model": {"visoin"},
	}}.DeclaredModalities()
	if err == nil {
		t.Fatal("expected an error for an unknown modality name")
	}
	if !strings.Contains(err.Error(), "some-model") {
		t.Errorf("error should name the offending model, got: %v", err)
	}
}

// Validate() must surface the same failure so the daemon refuses to boot
// on a malformed declaration rather than running with a blind model the
// operator believes is sighted.
func TestConfigValidate_RejectsBadModelCapabilities(t *testing.T) {
	c := minimalValidConfig()
	c.Chat.ModelCapabilities = map[string][]string{"m": {"telepathy"}}
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() must reject an unknown modality")
	}
	if !strings.Contains(err.Error(), "chat.model_capabilities") {
		t.Errorf("error should name the config key, got: %v", err)
	}
}

func TestConfigValidate_AcceptsGoodModelCapabilities(t *testing.T) {
	c := minimalValidConfig()
	c.Chat.ModelCapabilities = map[string][]string{
		"glm-5.2":    {"text"},
		"gemma4:31b": {"text", "vision"},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() rejected a valid declaration: %v", err)
	}
}

// minimalValidConfig returns the smallest Config that passes Validate, so
// the tests above isolate the model_capabilities behaviour.
func minimalValidConfig() *Config {
	c := &Config{}
	c.Server.Address = ":8080"
	c.Database.Driver = "sqlite"
	c.Database.Path = "/tmp/vornik-test.db"
	return c
}

// Zero means "built-in default" for the inline caps, not unbounded: an
// unbounded amount of base64 on a chat turn is never what an operator wants
// by omission. (Contrast StageMaxBytes, where 0 IS unbounded and preserves
// the behaviour of a deployment that never set it.)
func TestMediaConfig_InlineLimitsDefaults(t *testing.T) {
	perImage, total, count := MediaConfig{}.InlineLimits()
	if perImage != DefaultMediaInlineMaxBytes || total != DefaultMediaInlineMaxBytesTotal || count != DefaultMediaInlineMaxImages {
		t.Errorf("unset config should yield built-in defaults, got %d/%d/%d", perImage, total, count)
	}
}

func TestMediaConfig_InlineLimitsHonourOperatorValues(t *testing.T) {
	perImage, total, count := MediaConfig{
		InlineMaxBytes: 123, InlineMaxBytesTotal: 456, InlineMaxImages: 7,
	}.InlineLimits()
	if perImage != 123 || total != 456 || count != 7 {
		t.Errorf("operator values not honoured, got %d/%d/%d", perImage, total, count)
	}
}

// A negative value is nonsense; treat it as unset rather than as a cap that
// rejects everything.
func TestMediaConfig_InlineLimitsNegativeTreatedAsUnset(t *testing.T) {
	perImage, _, count := MediaConfig{InlineMaxBytes: -1, InlineMaxImages: -3}.InlineLimits()
	if perImage != DefaultMediaInlineMaxBytes || count != DefaultMediaInlineMaxImages {
		t.Errorf("negatives should fall back to defaults, got %d/%d", perImage, count)
	}
}
