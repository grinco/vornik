package api

import (
	"context"
	"errors"
	"os"
	"testing"
)

// hostUID returns the daemon's own uid, mirroring what checkAgentImageUID
// compares the agent image's baked uid against.
func hostUID() int {
	return os.Getuid()
}

// Regression: rootless workspace "Permission denied" second guard — a raw
// `podman build` bakes uid 1000; keep-id can't bridge a baked-uid mismatch
// (verified 2026-07-25). See onboarding-hardening-design F3b.
func TestCheckAgentImageUID(t *testing.T) {
	host := hostUID() // helper returns os.Getuid()

	// h builds a DoctorHandlers backed by a real configDir with one
	// role referencing a real (non-noop) agent image, so firstAgentImage
	// resolves unconditionally and the injected bakedUIDFunc/subuidOKFunc
	// seams are what actually determine the outcome.
	h := func(t *testing.T) *DoctorHandlers {
		t.Helper()
		dir := t.TempDir()
		writeSwarmWithImage(t, dir, "vornik-agent:latest")
		return &DoctorHandlers{configDir: dir, usernsMode: "keep-id", subuidOKFunc: func() bool { return true }}
	}

	// baked == host -> OK
	d := h(t)
	d.bakedUIDFunc = func(context.Context, string) (int, error) { return host, nil }
	if got := d.checkAgentImageUID(context.Background()); got.Status != "OK" {
		t.Fatalf("baked==host -> OK, got %q", got.Status)
	}
	// baked != host -> ERROR
	d = h(t)
	d.bakedUIDFunc = func(context.Context, string) (int, error) { return host + 7, nil }
	if got := d.checkAgentImageUID(context.Background()); got.Status != "ERROR" {
		t.Fatalf("baked!=host -> ERROR, got %q", got.Status)
	}
	// podman error -> WARNING
	d = h(t)
	d.bakedUIDFunc = func(context.Context, string) (int, error) { return 0, errors.New("boom") }
	if got := d.checkAgentImageUID(context.Background()); got.Status != "WARNING" {
		t.Fatalf("podman error -> WARNING, got %q", got.Status)
	}
	// keep-id set but subuid missing -> ERROR (preflight)
	d = h(t)
	d.bakedUIDFunc = func(context.Context, string) (int, error) { return host, nil }
	d.subuidOKFunc = func() bool { return false }
	if got := d.checkAgentImageUID(context.Background()); got.Status != "ERROR" {
		t.Fatalf("missing subuid -> ERROR, got %q", got.Status)
	}
}

// No agent image configured (empty configDir, no swarms) and no bakedUIDFunc
// override -> SKIPPED. Proves the image-absent branch is actually reachable
// now that resolution isn't gated behind whether a test seam is injected.
func TestCheckAgentImageUID_NoImageConfigured_Skipped(t *testing.T) {
	dir := t.TempDir() // empty: no swarms/ dir at all
	d := &DoctorHandlers{configDir: dir}
	got := d.checkAgentImageUID(context.Background())
	if got.Status != "SKIPPED" {
		t.Fatalf("no agent image configured -> SKIPPED, got %q (%s)", got.Status, got.Message)
	}
}

// keep-id + missing subuid provisioning must fire even when configDir is
// empty — the preflight is a host-level prerequisite check, not gated on
// any config directory being set.
func TestCheckAgentImageUID_KeepIDPreflight_RunsBeforeConfigDirGuard(t *testing.T) {
	d := &DoctorHandlers{
		configDir:    "",
		usernsMode:   "keep-id",
		subuidOKFunc: func() bool { return false },
	}
	got := d.checkAgentImageUID(context.Background())
	if got.Status != "ERROR" {
		t.Fatalf("keep-id missing subuid with empty configDir -> ERROR, got %q (%s)", got.Status, got.Message)
	}
}
