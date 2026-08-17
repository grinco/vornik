package executor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/runtime"
)

func TestObserveAgentImageID_UsesContainerInspectionAndRejectsTags(t *testing.T) {
	rt := &MockRuntime{inspectByID: map[string]*runtime.Container{
		"immutable": {ID: "immutable", Image: "sha256:abcdef"},
		"tag":       {ID: "tag", Image: "localhost/vornik-agent:latest"},
	}}
	e := &Executor{runtime: rt}
	if got := e.observeAgentImageID(context.Background(), "immutable"); got != "sha256:abcdef" {
		t.Fatalf("observed image id = %q", got)
	}
	if got := e.observeAgentImageID(context.Background(), "tag"); got != "" {
		t.Fatalf("mutable tag was accepted as evidence: %q", got)
	}
	if got := e.observeAgentImageID(context.Background(), "missing"); got != "" {
		t.Fatalf("missing inspection invented image evidence: %q", got)
	}
}
