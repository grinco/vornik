package reminders

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestNew_DefaultTaskTypeFromEnv: with no explicit Config.DefaultTaskType,
// New reads VORNIK_REMINDERS_TASK_TYPE so an operator can tune the task
// type the reminders runner hands the creator without a code change.
// (Backlog: "wire the task caps to env vars".)
func TestNew_DefaultTaskTypeFromEnv(t *testing.T) {
	t.Setenv(taskTypeEnvVar, "digest")
	r := New(Config{Repo: &stubRepo{}, Logger: zerolog.Nop()})
	if r.cfg.DefaultTaskType != "digest" {
		t.Fatalf("DefaultTaskType = %q, want %q (from env)", r.cfg.DefaultTaskType, "digest")
	}
}

// TestNew_DefaultTaskTypeExplicitBeatsEnv: an explicit Config value wins
// over the env override (the env is only a fallback when unset).
func TestNew_DefaultTaskTypeExplicitBeatsEnv(t *testing.T) {
	t.Setenv(taskTypeEnvVar, "digest")
	r := New(Config{Repo: &stubRepo{}, DefaultTaskType: "research", Logger: zerolog.Nop()})
	if r.cfg.DefaultTaskType != "research" {
		t.Fatalf("DefaultTaskType = %q, want explicit %q", r.cfg.DefaultTaskType, "research")
	}
}

// TestNew_DefaultTaskTypeFallback: neither Config nor env set falls back
// to the "research" default (unchanged behavior).
func TestNew_DefaultTaskTypeFallback(t *testing.T) {
	t.Setenv(taskTypeEnvVar, "")
	r := New(Config{Repo: &stubRepo{}, Logger: zerolog.Nop()})
	if r.cfg.DefaultTaskType != "research" {
		t.Fatalf("DefaultTaskType = %q, want default %q", r.cfg.DefaultTaskType, "research")
	}
}

// TestNew_FiringGraceVornikEnv: the firing-grace override is read from
// the VORNIK_ name.
func TestNew_FiringGraceVornikEnv(t *testing.T) {
	t.Setenv(firingGraceEnvVar, "5m")
	r := New(Config{Repo: &stubRepo{}, Logger: zerolog.Nop()})
	if r.cfg.FiringGrace != 5*time.Minute {
		t.Fatalf("FiringGrace = %v, want 5m (from %s)", r.cfg.FiringGrace, firingGraceEnvVar)
	}
}

// TestNew_FiringGraceLegacyEnvFallback: a deployment still setting the
// pre-rename SWARMD_ name keeps working (VORNIK_ unset).
func TestNew_FiringGraceLegacyEnvFallback(t *testing.T) {
	t.Setenv(firingGraceEnvVar, "")
	t.Setenv(legacyFiringGraceEnvVar, "7m")
	r := New(Config{Repo: &stubRepo{}, Logger: zerolog.Nop()})
	if r.cfg.FiringGrace != 7*time.Minute {
		t.Fatalf("FiringGrace = %v, want 7m (from legacy %s)", r.cfg.FiringGrace, legacyFiringGraceEnvVar)
	}
}

// TestNew_FiringGraceVornikBeatsLegacy: when both are set the VORNIK_
// name wins.
func TestNew_FiringGraceVornikBeatsLegacy(t *testing.T) {
	t.Setenv(firingGraceEnvVar, "5m")
	t.Setenv(legacyFiringGraceEnvVar, "7m")
	r := New(Config{Repo: &stubRepo{}, Logger: zerolog.Nop()})
	if r.cfg.FiringGrace != 5*time.Minute {
		t.Fatalf("FiringGrace = %v, want VORNIK_ value 5m to win", r.cfg.FiringGrace)
	}
}
