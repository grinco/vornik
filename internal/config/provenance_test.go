package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// One fixture, every origin in the design's table (resolved-config provenance
// design §3): default, file, alias, placeholder, env, env_invalid, derived,
// secret_file, unset — and the distinctions that carry the design: an explicit
// value equal to its default is `file`; an explicitly empty collection is
// `file`; a secret-file source is the basename; a failed load returns no
// provenance at all.
func TestLoadFromPathWithProvenance_EveryOrigin(t *testing.T) {
	for _, env := range pinnedEnvOverrides {
		t.Setenv(env, "")
	}
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "gateway.token")
	if err := os.WriteFile(tokenFile, []byte("tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROV_TEST_HOST", "db.internal")
	t.Setenv("VORNIK_DATABASE_PORT", "abc") // env_invalid: prior value stands
	t.Setenv("VORNIK_LOG_LEVEL", "debug")   // env
	cfgPath := filepath.Join(dir, "config.yaml")
	yamlDoc := `
server:
  address: ":8080"        # equals the default: file by presence, not value
database:
  host: "${PROV_TEST_HOST}"
  port: 5433
artifacts:
  artifacts_path: /tmp/prov-artifacts
gateway:
  token_file: ` + tokenFile + `
api:
  auth_enabled: false     # the shipped default (true) needs keys; off keeps the fixture small
  api_keys: []
`
	if err := os.WriteFile(cfgPath, []byte(yamlDoc), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, prov, err := LoadFromPathWithProvenance(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if prov.Path != cfgPath || prov.LoadedAt.IsZero() {
		t.Errorf("provenance header: %+v", prov.Path)
	}
	want := map[string]ValueOrigin{
		"server.address":           {Origin: OriginFile, Source: "config.yaml"},
		"database.host":            {Origin: OriginPlaceholder, Source: "${PROV_TEST_HOST}"},
		"database.port":            {Origin: OriginEnvInvalid, Source: `VORNIK_DATABASE_PORT ("abc" is not an integer)`},
		"logging.level":            {Origin: OriginEnv, Source: "VORNIK_LOG_LEVEL"},
		"storage.artifacts_path":   {Origin: OriginAlias, Source: "artifacts.artifacts_path"},
		"artifacts.artifacts_path": {Origin: OriginFile, Source: "config.yaml"},
		"gateway.token":            {Origin: OriginSecretFile, Source: "gateway.token"},
		"gateway.token_file":       {Origin: OriginFile, Source: "config.yaml"},
		"api.api_keys":             {Origin: OriginFile, Source: "config.yaml"}, // explicitly empty: present
		"api.auth_enabled":         {Origin: OriginFile, Source: "config.yaml"},
		"auth.session.lifetime":    {Origin: OriginDerived, Source: "applyAuthDefaults"},
		"database.name":            {Origin: OriginDefault},
		"gateway.address":          {Origin: OriginUnset},
	}
	for key, w := range want {
		got, ok := prov.Values[key]
		if !ok {
			t.Errorf("%s: no provenance recorded", key)
			continue
		}
		if got != w {
			t.Errorf("%s: got %+v, want %+v", key, got, w)
		}
	}
	// The values the origins describe.
	if cfg.Database.Host != "db.internal" || cfg.Database.Port != 5433 || cfg.Logging.Level != "debug" {
		t.Errorf("values: host=%q port=%d level=%q", cfg.Database.Host, cfg.Database.Port, cfg.Logging.Level)
	}
	if cfg.Gateway.Token != "tok-123" || cfg.Gateway.TokenFile != "" {
		t.Errorf("secret file not resolved: %q / %q", cfg.Gateway.Token, cfg.Gateway.TokenFile)
	}
	// Every walked leaf has an origin, and nothing else does.
	n := 0
	WalkLeaves(reflect.ValueOf(cfg).Elem(), func(string, reflect.StructField, reflect.Value) { n++ })
	if n != len(prov.Values) {
		t.Errorf("%d walked leaves, %d provenance entries", n, len(prov.Values))
	}
	for _, o := range prov.Values {
		if o.Origin == "" {
			t.Error("an entry with no origin")
		}
	}
}

// LoadFromPath is the wrapper: same config, same errors, provenance dropped.
func TestLoadFromPath_IsTheWrapper(t *testing.T) {
	for _, env := range pinnedEnvOverrides {
		t.Setenv(env, "")
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("api:\n  auth_enabled: false\nlogging:\n  level: warn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := LoadFromPath(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := LoadFromPathWithProvenance(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if a.Logging.Level != "warn" || b.Logging.Level != "warn" {
		t.Errorf("levels %q / %q", a.Logging.Level, b.Logging.Level)
	}
}

// A load that fails returns no provenance — never a partial map for a config
// that is not running.
func TestLoadFromPathWithProvenance_FailedLoadReturnsNothing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("gateway:\n  token_file: /nonexistent/token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, prov, err := LoadFromPathWithProvenance(cfgPath)
	if err == nil || cfg != nil || prov != nil {
		t.Fatalf("want an error and nothing else, got cfg=%v prov=%v err=%v", cfg != nil, prov != nil, err)
	}
	if !strings.Contains(err.Error(), "token_file") {
		t.Errorf("error does not name the cause: %v", err)
	}
}
