package api

import (
	"context"
	"errors"
	"os"
	"strings"
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
	// podman error -> SKIPPED (was WARNING until 2026-09-18).
	//
	// Changed deliberately, not incidentally. The old assertion pinned the
	// original code's behaviour and carried no rationale; it predates both
	// 2026-08-26-doctor-skipped-vs-ok-design.md §E2 — "the check could not run
	// at all → SKIPPED … never WARNING. A driver error is never a verdict" —
	// and CE issue 59, where a probe that could not finish inside its deadline
	// reported "could not read agent image uid: signal: killed" as a WARNING on
	// every run of a HEALTHY deployment. SKIPPED is already excluded from the
	// issue count, which is the behaviour that case needs.
	d = h(t)
	d.bakedUIDFunc = func(context.Context, string) (int, error) { return 0, errors.New("boom") }
	if got := d.checkAgentImageUID(context.Background()); got.Status != "SKIPPED" {
		t.Fatalf("podman error -> SKIPPED, got %q", got.Status)
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

// 2026-09-18: the agent image became uid-agnostic (onboarding-hardening-design
// D4) — /home/vornik, the Go cache tree and the contract mount points are 1777,
// so the image works whatever uid the process runs as. Measured: a uid-1000
// image running as uid 1001 completes a real `go build`.
//
// That falsified this check. It compared baked uid to host uid and, on a
// mismatch, reported ERROR "keep-id cannot bridge this and rootless workspace
// writes will fail. Rebuild with make build-agent" — a failure that no longer
// happens and a remedy the design calls a tax the next `podman pull` undoes.
//
// The image now says so itself with a label, which also removes the container
// start that CE issue 59 showed takes 5.92-12.55s and gets SIGKILLed by the
// probe's own deadline.

// A labelled image is fine whatever the uid: that is the whole point of D4.
func TestCheckAgentImageUID_UIDAgnosticLabelIsOKDespiteMismatch(t *testing.T) {
	dir := t.TempDir()
	writeSwarmWithImage(t, dir, "vornik-agent:latest")
	probed := false
	h := &DoctorHandlers{
		configDir:    dir,
		usernsMode:   "keep-id",
		subuidOKFunc: func() bool { return true },
		imageLabelsFunc: func(context.Context, string) (map[string]string, error) {
			return map[string]string{agentUIDAgnosticLabel: "1"}, nil
		},
		bakedUIDFunc: func(context.Context, string) (int, error) {
			probed = true
			return hostUID() + 1, nil
		},
	}

	got := h.checkAgentImageUID(context.Background())

	if got.Status != "OK" {
		t.Errorf("status = %q (%s), want OK — a uid-agnostic image works at any uid",
			got.Status, got.Message)
	}
	if probed {
		t.Error("the container probe ran even though the label answered — this is the " +
			"start that CE issue 59 reports taking 5.92-12.55s and being SIGKILLed")
	}
}

// An UNLABELLED image with a mismatch is genuinely broken — that is an image
// built before D4 — so the error stays, and it must stay an error.
func TestCheckAgentImageUID_UnlabelledMismatchIsStillAnError(t *testing.T) {
	dir := t.TempDir()
	writeSwarmWithImage(t, dir, "vornik-agent:latest")
	h := &DoctorHandlers{
		configDir:    dir,
		usernsMode:   "keep-id",
		subuidOKFunc: func() bool { return true },
		imageLabelsFunc: func(context.Context, string) (map[string]string, error) {
			return map[string]string{"org.opencontainers.image.version": "2026.9.4"}, nil
		},
		bakedUIDFunc: func(context.Context, string) (int, error) { return hostUID() + 1, nil },
	}

	got := h.checkAgentImageUID(context.Background())

	if got.Status != "ERROR" {
		t.Errorf("status = %q, want ERROR for a pre-D4 image whose uid does not match", got.Status)
	}
}

// A probe that cannot complete must report NOT MEASURED, distinctly from a real
// mismatch. CE issue 59: the check reported "could not read agent image uid:
// signal: killed" as a WARNING on every run, which reads like a finding about
// the image and is actually a finding about the probe.
func TestCheckAgentImageUID_UnreadableProbeSaysNotMeasured(t *testing.T) {
	dir := t.TempDir()
	writeSwarmWithImage(t, dir, "vornik-agent:latest")
	h := &DoctorHandlers{
		configDir:    dir,
		usernsMode:   "keep-id",
		subuidOKFunc: func() bool { return true },
		imageLabelsFunc: func(context.Context, string) (map[string]string, error) {
			return nil, errors.New("no such image")
		},
		bakedUIDFunc: func(context.Context, string) (int, error) {
			return 0, errors.New("signal: killed")
		},
	}

	got := h.checkAgentImageUID(context.Background())

	// SKIPPED is the established vocabulary for "the check could not run at
	// all" — 2026-08-26-doctor-skipped-vs-ok-design.md §E2 — and it is already
	// excluded from the issue count, which WARNING is not.
	if got.Status != "SKIPPED" {
		t.Errorf("status = %q (%s), want SKIPPED — a probe that could not complete "+
			"is a finding about the probe, not about the image", got.Status, got.Message)
	}
	if !strings.Contains(strings.ToLower(got.Message), "not measured") {
		t.Errorf("message = %q, want it to say the uid was NOT MEASURED", got.Message)
	}
}
