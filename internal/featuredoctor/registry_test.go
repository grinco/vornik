package featuredoctor

import (
	"context"
	"testing"
)

func TestRegistry_HasSeedFeatures(t *testing.T) {
	ids := map[string]bool{}
	for _, f := range Registry() {
		ids[f.ID] = true
	}
	for _, want := range []string{"instinct", "auth", "memory-rag", "cluster"} {
		if !ids[want] {
			t.Errorf("registry missing seed feature %q", want)
		}
	}
}

func TestDiagnose_DisabledWhenGatesOff(t *testing.T) {
	f := authFeature()
	deps := Deps{
		Config:     stubConfig{vals: map[string]any{"api.auth_enabled": false}},
		SecretsDir: t.TempDir(), // no admin key -> unfixable prereq unmet
	}
	d := Diagnose(context.Background(), f, deps)
	if d.Status != StatusBlocked {
		t.Fatalf("auth off + no admin key => blocked, got %q", d.Status)
	}
}

func TestDiagnose_NilConfigIsUnknown(t *testing.T) {
	// A zero-value Deps has a nil Config; Diagnose must not panic and must
	// return StatusUnknown because gates/prereqs cannot be evaluated.
	f := authFeature()
	d := Diagnose(context.Background(), f, Deps{})
	if d.Status != StatusUnknown {
		t.Fatalf("nil Config must yield StatusUnknown, got %q", d.Status)
	}
	if d.Feature.ID != f.ID {
		t.Fatalf("Diagnosis.Feature must be preserved, got %q", d.Feature.ID)
	}
}
