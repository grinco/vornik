//go:build integration

package memory_test

// The lane gate: why this suite is not allowed to skip quietly.
//
// BACKLOG 2026-09-07, "The memory integration suite is in no CI lane".
// Every test in this package funnels through openIngestRecallDB, which
// SKIPS when TEST_DATABASE_URL is unset or the database is unreachable.
// That posture is right for a developer laptop and wrong for a lane: the
// package holds the Art 17 erasure guarantees (no vector derived from
// erased text survives) and the project-wipe cache evictions, and a lane
// that lists the package without supplying the database converts every
// one of those into a SKIP — which reads as a pass. Strictly worse than
// the honest absence it replaced.
//
// So the lane sets VORNIK_REQUIRE_INTEGRATION_DB=1, and under it every
// would-be skip becomes a failure naming what is missing. The gate is
// tested here rather than trusted, because a gate that silently stopped
// firing would restore exactly the condition it exists to prevent — and
// this test is itself unskippable, so a lane that runs this package at
// all proves the gate's behaviour on that run.

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// recorderT captures which testing verb skipOrFail chose.
type recorderT struct {
	skipped bool
	failed  bool
	message string
}

func (r *recorderT) Helper() {}
func (r *recorderT) Skipf(format string, args ...any) {
	r.skipped = true
	r.message = fmt.Sprintf(format, args...)
}
func (r *recorderT) Fatalf(format string, args ...any) {
	r.failed = true
	r.message = fmt.Sprintf(format, args...)
}

func TestSkipOrFail_SkipsWhenTheLaneHasNotClaimedTheSuite(t *testing.T) {
	t.Setenv(requireIntegrationDBEnv, "")
	r := &recorderT{}
	skipOrFail(r, "TEST_DATABASE_URL not set")
	if !r.skipped || r.failed {
		t.Fatalf("outside the lane a missing database must SKIP, got skipped=%v failed=%v", r.skipped, r.failed)
	}
	if !strings.Contains(r.message, "TEST_DATABASE_URL not set") {
		t.Errorf("skip message must carry the reason, got %q", r.message)
	}
}

func TestSkipOrFail_FailsInsideTheLane(t *testing.T) {
	t.Setenv(requireIntegrationDBEnv, "1")
	r := &recorderT{}
	skipOrFail(r, "postgres unreachable: %v", errStub{})
	if !r.failed || r.skipped {
		t.Fatalf("inside the lane a missing database must FAIL, got skipped=%v failed=%v", r.skipped, r.failed)
	}
	if !strings.Contains(r.message, "postgres unreachable: stub") {
		t.Errorf("failure message must carry the underlying reason, got %q", r.message)
	}
	if !strings.Contains(r.message, requireIntegrationDBEnv) {
		t.Errorf("failure must name the variable that turned the skip into a failure, got %q", r.message)
	}
}

type errStub struct{}

func (errStub) Error() string { return "stub" }

// requireIntegrationDBEnv, when set to "1", turns every would-be skip in
// openIngestRecallDB into a failure. The CI integration job and
// `make test-integration` both set it; a developer running the package by
// hand does not, and keeps the clean-skip posture.
const requireIntegrationDBEnv = "VORNIK_REQUIRE_INTEGRATION_DB"

// laneT is the slice of *testing.T skipOrFail needs, so the gate's own
// behaviour can be asserted without provoking a real skip or failure.
type laneT interface {
	Helper()
	Skipf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// skipOrFail reports an absent or unusable integration database. Outside
// the lane that is a skip; inside it — where the caller has declared this
// database must exist — it is a failure, so the lane cannot report a pass
// for tests that never executed.
func skipOrFail(t laneT, format string, args ...any) {
	t.Helper()
	if os.Getenv(requireIntegrationDBEnv) == "1" {
		t.Fatalf(fmt.Sprintf(format, args...)+
			"\n%s=1 — the lane declared this suite must RUN, so this is a FAILURE rather than a skip. "+
			"A skipped Art 17 erasure guarantee reports a pass for a control that never executed. "+
			"Provide TEST_DATABASE_URL for a reachable throwaway Postgres, or unset %s to run the "+
			"package in its clean-skip developer posture.",
			requireIntegrationDBEnv, requireIntegrationDBEnv)
		return
	}
	t.Skipf(format, args...)
}
