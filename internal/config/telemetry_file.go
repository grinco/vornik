package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ResolveTelemetryFile reads only the telemetry block. It intentionally does
// not validate the rest of config.yaml: telemetry inspection and best-effort
// emission must not interfere with the operation that just succeeded.
func ResolveTelemetryFile(path, env string) (enabled bool, source string, err error) {
	cfg := struct {
		Telemetry TelemetryConfig `yaml:"telemetry" json:"telemetry"`
	}{Telemetry: TelemetryConfig{Enabled: true}}
	if path != "" {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return false, "config", fmt.Errorf("read telemetry config: %w", readErr)
		}
		if strings.HasSuffix(path, ".json") {
			if parseErr := json.Unmarshal(data, &cfg); parseErr != nil {
				return false, "config", fmt.Errorf("parse telemetry config: %w", parseErr)
			}
		} else if parseErr := yaml.Unmarshal(data, &cfg); parseErr != nil {
			return false, "config", fmt.Errorf("parse telemetry config: %w", parseErr)
		}
	}
	return cfg.Telemetry.Resolve(env)
}
