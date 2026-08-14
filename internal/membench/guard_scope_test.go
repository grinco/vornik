package membench

import (
	"strings"
	"testing"
)

// Scope guards for the agent-quality benchmark
// (https://docs.vornik.io §5.2).
//
// These live beside the destructive-run guard rather than in internal/agentbench
// because §6.1 shares exactly two things between the two harnesses — the guard
// and the comparability key — on the grounds that a safety check with two
// implementations has one that is wrong.

func TestCheckBenchmarkProject_RefusesAnyOtherProject(t *testing.T) {
	err := CheckBenchmarkProject("vornik-trading", "bench")
	if err == nil {
		t.Fatal("ran against a project that is not the benchmark project")
	}
	if !strings.Contains(err.Error(), "vornik-trading") || !strings.Contains(err.Error(), "bench") {
		t.Errorf("error must name both the attempted and the configured project, got: %v", err)
	}
}

func TestCheckBenchmarkProject_AllowsTheConfiguredProject(t *testing.T) {
	if err := CheckBenchmarkProject("bench", "bench"); err != nil {
		t.Fatalf("refused the configured benchmark project: %v", err)
	}
}

// Fails closed on an unconfigured benchmark project. An empty configured value
// must not mean "any project is fine" — that is the reading that turns a guard
// into decoration on a fresh deployment.
func TestCheckBenchmarkProject_FailsClosedWhenUnconfigured(t *testing.T) {
	if err := CheckBenchmarkProject("anything", ""); err == nil {
		t.Fatal("an unconfigured benchmark project allowed the run — empty must not mean 'any'")
	}
	if err := CheckBenchmarkProject("", "bench"); err == nil {
		t.Fatal("an empty target project allowed the run")
	}
}

func TestCheckExcludedRoles_RefusesTradingAndBrokerSwarms(t *testing.T) {
	for _, swarm := range []string{"ibkr-trader", "trading-research", "broker-ops", "IBKR-Trader"} {
		if err := CheckExcludedRoles(swarm); err == nil {
			t.Errorf("swarm %q was allowed — the benchmark bulk-runs tasks and must never "+
				"be pointed at the path that moves real money", swarm)
		}
	}
}

func TestCheckExcludedRoles_AllowsOrdinarySwarms(t *testing.T) {
	for _, swarm := range []string{"bench", "companion", "deep-research"} {
		if err := CheckExcludedRoles(swarm); err != nil {
			t.Errorf("swarm %q refused: %v", swarm, err)
		}
	}
}

// The exclusion must use the SAME classifier the cost/quality detector and the
// applier use (2026-07-24-applier-trading-refusal D3). A second copy of the rule
// would drift, and both copies would still read like protection.
func TestCheckExcludedRoles_UsesTheSharedClassifier(t *testing.T) {
	// "trading" appears only via swarmclass's marker set; if this guard grew its
	// own list, a marker added there would stop being honoured here.
	if err := CheckExcludedRoles("some-trading-thing"); err == nil {
		t.Fatal("a swarm matching a shared marker was allowed — this guard has its own list")
	}
}

// Guard-ordering law (§5.2): the guard is the FIRST statement in the run path.
// The run below is invalid in three other ways — no confirmation, an empty
// project, and a trading swarm — and the destructive-target error is the one
// that must surface, because an operator who fixes the error they are shown
// must not be walked toward the wipe by a sequence of lesser complaints.
func TestRunGuards_DestructiveTargetIsCheckedFirst(t *testing.T) {
	err := CheckRunScope(RunScope{
		// A production-shaped name from the SHIPPED denylist. A deployment's OWN
		// database names must never appear in source — that is exactly why
		// guard.go keeps them in VORNIK_BENCH_DENY_DATABASES instead, and why the
		// CE export's operator-token scan fails the build when one slips in.
		Database:         "production",
		Confirmation:     "",
		ProjectID:        "",
		BenchmarkProject: "bench",
		SwarmID:          "ibkr-trader",
	})
	if err == nil {
		t.Fatal("a run invalid in four ways was authorised")
	}
	if !strings.Contains(err.Error(), "--i-know-this-wipes") {
		t.Errorf("the destructive-target guard must fire first; got: %v", err)
	}
}

func TestRunGuards_AuthorisesAFullyValidRun(t *testing.T) {
	err := CheckRunScope(RunScope{
		Database:         "agentbench_local",
		Confirmation:     "agentbench_local",
		ProjectID:        "bench",
		BenchmarkProject: "bench",
		SwarmID:          "bench",
	})
	if err != nil {
		t.Fatalf("a valid run was refused: %v", err)
	}
}
