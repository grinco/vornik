package config

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// TelemetryConfig controls anonymous install and project-creation telemetry.
// Enabled defaults true while explicit records whether the operator actually
// supplied telemetry.enabled; environment values may only decide an absent key.
type TelemetryConfig struct {
	Enabled  bool `yaml:"enabled" json:"enabled"`
	explicit bool
}

// UnmarshalYAML preserves key presence, which ordinary default-then-unmarshal
// bool handling loses.
func (c *TelemetryConfig) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		Enabled *bool `yaml:"enabled"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	if raw.Enabled == nil {
		c.Enabled = true
		c.explicit = false
		return nil
	}
	c.Enabled = *raw.Enabled
	c.explicit = true
	return nil
}

// UnmarshalJSON provides the same presence semantics for JSON configs.
func (c *TelemetryConfig) UnmarshalJSON(data []byte) error {
	var raw struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Enabled == nil {
		c.Enabled = true
		c.explicit = false
		return nil
	}
	c.Enabled = *raw.Enabled
	c.explicit = true
	return nil
}

// Explicit reports whether telemetry.enabled was supplied in config.
func (c TelemetryConfig) Explicit() bool { return c.explicit }

// IsZero keeps the implicit enabled default out of generated/shipped YAML.
func (c TelemetryConfig) IsZero() bool { return !c.explicit }

// Resolve applies the documented presence-aware environment fallback.
func (c TelemetryConfig) Resolve(env string) (enabled bool, source string, err error) {
	if c.explicit {
		return c.Enabled, "config", nil
	}
	v := strings.ToLower(strings.TrimSpace(env))
	if v == "" {
		return c.Enabled, "default", nil
	}
	switch v {
	case "1", "true", "yes", "on":
		return true, "environment", nil
	case "0", "false", "no", "off":
		return false, "environment", nil
	default:
		return false, "environment", fmt.Errorf("invalid VORNIK_TELEMETRY value; telemetry disabled")
	}
}
