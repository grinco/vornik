package leaderelection

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// isLeaderOnlyGate implements only IsLeader() — it deliberately does
// NOT implement EpochVerifier, mirroring the narrow leader gates the
// autonomy manager and telegram bot expose. DangerousWriteAllowed must
// treat it as pre-fence (always proceed).
type isLeaderOnlyGate struct {
	leader bool
}

func (g isLeaderOnlyGate) IsLeader() bool { return g.leader }

// verifierGate implements EpochVerifier so DangerousWriteAllowed exercises
// the fencing branch.
type verifierGate struct {
	ok      bool
	current int64
	err     error
}

func (g verifierGate) VerifyEpoch(context.Context) (bool, int64, error) {
	return g.ok, g.current, g.err
}

func TestDangerousWriteAllowed_NilGate(t *testing.T) {
	proceed, reason := DangerousWriteAllowed(context.Background(), nil)
	if !proceed {
		t.Fatalf("nil gate: want proceed=true, got false")
	}
	if reason != "" {
		t.Fatalf("nil gate: want empty reason, got %q", reason)
	}
}

func TestDangerousWriteAllowed_IsLeaderOnlyGate(t *testing.T) {
	// A plain IsLeader-only gate is NOT an EpochVerifier; pre-fence
	// behaviour is preserved (proceed, no reason).
	proceed, reason := DangerousWriteAllowed(context.Background(), isLeaderOnlyGate{leader: true})
	if !proceed {
		t.Fatalf("IsLeader-only gate: want proceed=true, got false")
	}
	if reason != "" {
		t.Fatalf("IsLeader-only gate: want empty reason, got %q", reason)
	}
}

func TestDangerousWriteAllowed_VerifierOK(t *testing.T) {
	proceed, reason := DangerousWriteAllowed(context.Background(), verifierGate{ok: true, current: 7})
	if !proceed {
		t.Fatalf("verifier ok=true: want proceed=true, got false")
	}
	if reason != "" {
		t.Fatalf("verifier ok=true: want empty reason, got %q", reason)
	}
}

func TestDangerousWriteAllowed_VerifierSuperseded(t *testing.T) {
	proceed, reason := DangerousWriteAllowed(context.Background(), verifierGate{ok: false, current: 9})
	if proceed {
		t.Fatalf("verifier ok=false: want proceed=false, got true")
	}
	if !strings.Contains(reason, "superseded") {
		t.Fatalf("verifier ok=false: want reason containing %q, got %q", "superseded", reason)
	}
}

func TestDangerousWriteAllowed_VerifierReadError(t *testing.T) {
	proceed, reason := DangerousWriteAllowed(context.Background(), verifierGate{err: errors.New("db down")})
	if proceed {
		t.Fatalf("verifier err!=nil: want proceed=false, got true")
	}
	if !strings.Contains(reason, "read failed") {
		t.Fatalf("verifier err!=nil: want reason containing %q, got %q", "read failed", reason)
	}
}

func TestLeaderFenceRejected(t *testing.T) {
	// Restore the package-level counter after the test so we don't leak
	// wiring state into sibling tests.
	saved := fenceRejections
	t.Cleanup(func() { fenceRejections = saved })

	// Wire against a fresh registry — the production path registers against
	// the registry actually served at /metrics, so assert through that
	// registry rather than the default registerer.
	reg := prometheus.NewRegistry()
	RegisterFenceMetrics(reg)

	const worker = "fence_metric_test_worker"
	LeaderFenceRejected(worker)
	LeaderFenceRejected(worker)

	got := testutil.ToFloat64(fenceRejections.WithLabelValues(worker))
	if got != 2 {
		t.Fatalf("LeaderFenceRejected: counter = %v, want 2", got)
	}

	// The series must be present on the injected registry (visible on /metrics).
	const want = `
# HELP vornik_leader_fence_rejections_total Dangerous leader-gated writes refused by the epoch fence, by worker.
# TYPE vornik_leader_fence_rejections_total counter
vornik_leader_fence_rejections_total{worker="fence_metric_test_worker"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "vornik_leader_fence_rejections_total"); err != nil {
		t.Fatalf("registry did not expose the fence counter: %v", err)
	}
}

func TestLeaderFenceRejected_NoopBeforeRegister(t *testing.T) {
	// Simulate the single-process / test path where RegisterFenceMetrics has
	// not been called: the counter is nil and LeaderFenceRejected must not panic.
	saved := fenceRejections
	t.Cleanup(func() { fenceRejections = saved })

	fenceRejections = nil
	// Must not panic.
	LeaderFenceRejected("worker_before_register")
}
