package config

import (
	"reflect"
	"sort"
	"testing"
)

// The table replaced 33 hand-written blocks on 2026-09-03. This list is those
// 33, pinned: an override that disappears from the table fails here, and one
// that appears must be added here deliberately.
var pinnedEnvOverrides = []string{
	"VORNIK_SERVER_ADDRESS", "VORNIK_SERVER_UNIX_SOCKET",
	"VORNIK_DATABASE_HOST", "VORNIK_DATABASE_PORT", "VORNIK_DATABASE_NAME",
	"VORNIK_DATABASE_USER", "VORNIK_DATABASE_PASSWORD", "VORNIK_DATABASE_SSLMODE",
	"VORNIK_ARTIFACTS_PATH", "VORNIK_DATA_DIR",
	"VORNIK_STORAGE_BACKEND", "VORNIK_STORAGE_S3_ENDPOINT", "VORNIK_STORAGE_S3_REGION",
	"VORNIK_STORAGE_S3_BUCKET", "VORNIK_STORAGE_S3_PREFIX", "VORNIK_STORAGE_S3_ACCESS_KEY_ID",
	"VORNIK_STORAGE_S3_SECRET_ACCESS_KEY", "VORNIK_STORAGE_S3_USE_PATH_STYLE", "VORNIK_STORAGE_S3_FORCE_SSL",
	"VORNIK_METRICS_ENABLED", "VORNIK_METRICS_ADDR", "VORNIK_TRACING_ENABLED", "VORNIK_TRACING_ENDPOINT",
	"VORNIK_LOG_LEVEL", "VORNIK_RUNTIME_USERNS_MODE", "VORNIK_RUNTIME_RUN_AS_USER",
	"VORNIK_GATEWAY_ENABLED", "VORNIK_GATEWAY_ADDRESS", "VORNIK_GATEWAY_TOKEN", "VORNIK_GATEWAY_AGENT_WRITES",
	"VORNIK_TAINT_LINEAGE_MODE", "VORNIK_WEB_SUBMIT_SECRET",
}

func TestEnvOverrides_TableIsThePinnedSet(t *testing.T) {
	var got []string
	seen := map[string]bool{}
	for _, o := range envOverrides {
		if seen[o.Env] {
			t.Errorf("%s appears twice in the table", o.Env)
		}
		seen[o.Env] = true
		got = append(got, o.Env)
		if len(o.Keys) == 0 || o.Apply == nil {
			t.Errorf("%s has no keys or no Apply", o.Env)
		}
	}
	want := append([]string(nil), pinnedEnvOverrides...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("table env set\n got %v\nwant %v", got, want)
	}
}

// Every Key names a leaf the config walker can find — a typo in a dotted key
// fails here, where a typo in a variable name used to fail nothing.
func TestEnvOverrides_EveryKeyIsAWalkedLeaf(t *testing.T) {
	leaves := map[string]bool{}
	WalkLeaves(reflect.ValueOf(DefaultConfig()).Elem(), func(key string, _ reflect.StructField, _ reflect.Value) {
		leaves[key] = true
	})
	for _, o := range envOverrides {
		for _, k := range o.Keys {
			if !leaves[k] {
				t.Errorf("%s sets %q, which is not a yaml-tagged leaf of config.Config", o.Env, k)
			}
		}
	}
}

// Behaviour of the table equals the hand-written blocks it replaced, on the
// shapes that had logic: mirrored fields, booleans (invalid ignored), the port
// (invalid ignored), the DATA_DIR fallback ordering, and the ForceSSL pointer.
func TestEnvOverrides_PreservesTheHandWrittenSemantics(t *testing.T) {
	// The host running the tests may carry real VORNIK_* variables; blank every
	// override so only the ones this test sets can fire.
	for _, env := range pinnedEnvOverrides {
		t.Setenv(env, "")
	}
	t.Setenv("VORNIK_STORAGE_S3_BUCKET", "b1")
	t.Setenv("VORNIK_STORAGE_S3_USE_PATH_STYLE", "yes")
	t.Setenv("VORNIK_STORAGE_S3_FORCE_SSL", "off")
	t.Setenv("VORNIK_METRICS_ENABLED", "definitely")
	t.Setenv("VORNIK_DATABASE_PORT", "abc")
	t.Setenv("VORNIK_DATA_DIR", "/data")
	t.Setenv("VORNIK_ARTIFACTS_PATH", "")

	cfg := DefaultConfig()
	cfg.Metrics.Enabled = true
	cfg.Database.Port = 5432
	// DATA_DIR applies only when nothing set the artifacts path — and the
	// shipped default sets it, so the fallback fires only for a file that
	// wrote artifacts_path: "" explicitly. Preserved as-is; this test drives
	// that state.
	cfg.Storage.ArtifactsPath = ""
	var recorded []string
	applyEnvOverridesRecording(cfg, func(_ []string, env string, err error) {
		s := env
		if err != nil {
			s += "!"
		}
		recorded = append(recorded, s)
	})

	if cfg.Storage.S3.Bucket != "b1" || cfg.Artifacts.S3.Bucket != "b1" {
		t.Errorf("bucket not mirrored: %q / %q", cfg.Storage.S3.Bucket, cfg.Artifacts.S3.Bucket)
	}
	if !cfg.Storage.S3.UsePathStyle || !cfg.Artifacts.S3.UsePathStyle {
		t.Error("use_path_style 'yes' not applied to both")
	}
	if cfg.Storage.S3.ForceSSL == nil || *cfg.Storage.S3.ForceSSL || cfg.Artifacts.S3.ForceSSL == nil {
		t.Error("force_ssl 'off' not applied as a pointer to false on both")
	}
	if !cfg.Metrics.Enabled {
		t.Error("an invalid boolean must leave the prior value")
	}
	if cfg.Database.Port != 5432 {
		t.Errorf("an invalid port must leave the prior value, got %d", cfg.Database.Port)
	}
	if cfg.Storage.ArtifactsPath != "/data/artifacts" || cfg.Artifacts.ArtifactsPath != "/data/artifacts" {
		t.Errorf("DATA_DIR fallback: %q / %q", cfg.Storage.ArtifactsPath, cfg.Artifacts.ArtifactsPath)
	}
	// The two invalid values were REPORTED, not swallowed.
	want := []string{"VORNIK_DATABASE_PORT!", "VORNIK_DATA_DIR", "VORNIK_STORAGE_S3_BUCKET",
		"VORNIK_STORAGE_S3_USE_PATH_STYLE", "VORNIK_STORAGE_S3_FORCE_SSL", "VORNIK_METRICS_ENABLED!"}
	if !reflect.DeepEqual(recorded, want) {
		t.Errorf("recorded %v, want %v", recorded, want)
	}

	// An explicit artifacts path wins over DATA_DIR, in either order.
	t.Setenv("VORNIK_ARTIFACTS_PATH", "/explicit")
	cfg = DefaultConfig()
	applyEnvOverrides(cfg)
	if cfg.Storage.ArtifactsPath != "/explicit" {
		t.Errorf("ARTIFACTS_PATH must beat DATA_DIR, got %q", cfg.Storage.ArtifactsPath)
	}
	if EnvOverrideFor("storage.s3.bucket") != "VORNIK_STORAGE_S3_BUCKET" || EnvOverrideFor("nope") != "" {
		t.Error("EnvOverrideFor is wrong")
	}
}
