package registry

import "testing"

func TestRegisterTransient_ResolvableViaGetWorkflow(t *testing.T) {
	r := New()
	id := "trading-candidate-abc123"
	if err := r.RegisterTransient(id, &Workflow{ID: "trading", Entrypoint: "strategize"}); err != nil {
		t.Fatalf("RegisterTransient: %v", err)
	}
	got := r.GetWorkflow(id)
	if got == nil {
		t.Fatalf("GetWorkflow(%q) = nil; want the transient workflow", id)
	}
	// RegisterTransient rewrites the workflow's own ID to the
	// transient id for self-consistency.
	if got.ID != id {
		t.Errorf("transient workflow ID = %q; want %q", got.ID, id)
	}
}

func TestDeregisterTransient_Removes(t *testing.T) {
	r := New()
	id := "trading-candidate-def456"
	_ = r.RegisterTransient(id, &Workflow{ID: "trading"})
	r.DeregisterTransient(id)
	if got := r.GetWorkflow(id); got != nil {
		t.Errorf("GetWorkflow after deregister = %+v; want nil", got)
	}
	// Deregister of an absent id is a no-op (safe to defer).
	r.DeregisterTransient("never-registered")
}

func TestRegisterTransient_DoesNotShadowLoadedWorkflow(t *testing.T) {
	r := New()
	// Seed a loaded-config workflow.
	r.workflows["trading"] = &Workflow{ID: "trading", Entrypoint: "real"}
	// A transient under the SAME id must not win over loaded config.
	_ = r.RegisterTransient("trading", &Workflow{ID: "trading", Entrypoint: "transient"})
	got := r.GetWorkflow("trading")
	if got == nil || got.Entrypoint != "real" {
		t.Errorf("loaded config workflow must win over transient; got %+v", got)
	}
}

func TestRegisterTransient_Validation(t *testing.T) {
	r := New()
	if err := r.RegisterTransient("", &Workflow{}); err == nil {
		t.Error("empty id should error")
	}
	if err := r.RegisterTransient("id", nil); err == nil {
		t.Error("nil workflow should error")
	}
}
