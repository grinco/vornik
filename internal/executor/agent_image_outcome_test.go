package executor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/runtime"
)

func TestObserveAgentImageID_UsesContainerInspectionAndRejectsTags(t *testing.T) {
	// A REAL image id: 64 hex characters. The original fixture was
	// "sha256:abcdef" — six characters, a shape nothing emits — which is why
	// this test passed for months while the function rejected every real id.
	const realID = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	rt := &MockRuntime{inspectByID: map[string]*runtime.Container{
		"immutable": {ID: "immutable", Image: "sha256:" + realID},
		"tag":       {ID: "tag", Image: "ghcr.io/grinco/vornik-agent:latest"},
	}}
	e := &Executor{runtime: rt}
	if got := e.observeAgentImageID(context.Background(), "immutable"); got != "sha256:"+realID {
		t.Fatalf("observed image id = %q", got)
	}
	if got := e.observeAgentImageID(context.Background(), "tag"); got != "" {
		t.Fatalf("mutable tag was accepted as evidence: %q", got)
	}
	if got := e.observeAgentImageID(context.Background(), "missing"); got != "" {
		t.Fatalf("missing inspection invented image evidence: %q", got)
	}
}

// THE SHAPE PODMAN ACTUALLY RETURNS.
//
// `podman inspect --format '{{.Image}}'` returns a BARE 64-character hex id
// with no "sha256:" prefix — verified on podman 5.8.4, 2026-08-29:
//
//	$ podman inspect <cid> --format '{{.Image}}'
//	6d95a86b6c872b466641770b4d39fc1d802603d357435c5d7d4808b7c71f3c38
//
// The original implementation required the prefix and returned "" otherwise,
// so it discarded a good image id on EVERY container. Measured consequence: 162
// execution_step_outcomes rows from a full benchmark arm carried no image id,
// every arm key was PARTIAL, and `bench agent rollup` refused to merge a
// completed 30-task run — the agent-image provenance axis had never worked.
//
// The test above did not catch it because its fixture was "sha256:abcdef" — a
// form nothing emits, and only six hex characters. A fixture that cannot occur
// in production tests the path and not the behaviour.
func TestObserveAgentImageID_AcceptsBarePodmanImageID(t *testing.T) {
	const bare = "6d95a86b6c872b466641770b4d39fc1d802603d357435c5d7d4808b7c71f3c38"
	rt := &MockRuntime{inspectByID: map[string]*runtime.Container{
		"bare":     {ID: "bare", Image: bare},
		"prefixed": {ID: "prefixed", Image: "sha256:" + bare},
		"tagref":   {ID: "tagref", Image: "ghcr.io/grinco/vornik-agent:bench"},
		"short":    {ID: "short", Image: "abc123"},
	}}
	e := &Executor{runtime: rt}

	if got := e.observeAgentImageID(context.Background(), "bare"); got != "sha256:"+bare {
		t.Errorf("bare podman id rejected or not normalised: %q", got)
	}
	// Already-prefixed must still work and must not be double-prefixed.
	if got := e.observeAgentImageID(context.Background(), "prefixed"); got != "sha256:"+bare {
		t.Errorf("prefixed id mangled: %q", got)
	}
	// A mutable tag is still not provenance.
	if got := e.observeAgentImageID(context.Background(), "tagref"); got != "" {
		t.Errorf("mutable tag accepted as evidence: %q", got)
	}
	// A short hex string is not an image id and must not be padded into one.
	if got := e.observeAgentImageID(context.Background(), "short"); got != "" {
		t.Errorf("short value accepted as an image id: %q", got)
	}
}
