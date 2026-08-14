package membench

import (
	"fmt"
	"strings"

	"vornik.io/vornik/internal/swarmclass"
)

// Scope guards for a benchmark run
// (https://docs.vornik.io §5.2).
//
// CheckDestructiveTarget answers "may this run write that database". These
// answer the two questions the agent-quality benchmark added, because it does
// not merely write memory — it submits real tasks to a real daemon, which can
// reach anything the daemon can:
//
//   - is this the project the benchmark is allowed to run in, and
//   - is this swarm on a path automation must never touch.
//
// They live here rather than in internal/agentbench because §6.1 shares exactly
// two things between the harnesses, the guard and the comparability key, on the
// grounds that a safety check with two implementations has one that is wrong.

// RunScope is everything a run must be authorised against.
type RunScope struct {
	// Database and Confirmation are CheckDestructiveTarget's inputs.
	Database     string
	Confirmation string
	// ProjectID is the project this run will submit tasks to.
	ProjectID string
	// BenchmarkProject is the project the deployment permits benchmarking in.
	BenchmarkProject string
	// SwarmID is the swarm whose roles will execute the run's tasks.
	SwarmID string
}

// CheckRunScope authorises a run, destructive target first.
//
// ORDER IS THE POINT, not a detail. The destructive-target check runs before
// anything else so an operator who is shown an error and fixes it is not walked
// toward the wipe by a sequence of lesser complaints — each fixed, each
// revealing the next, with the actual danger arriving last. A test supplies a
// run invalid in four ways and requires this error specifically.
func CheckRunScope(s RunScope) error {
	if err := CheckDestructiveTarget(s.Database, s.Confirmation); err != nil {
		return err
	}
	if err := CheckBenchmarkProject(s.ProjectID, s.BenchmarkProject); err != nil {
		return err
	}
	return CheckExcludedRoles(s.SwarmID)
}

// CheckBenchmarkProject refuses any project but the configured benchmark one.
//
// FAILS CLOSED ON AN EMPTY CONFIGURED VALUE. An unconfigured deployment must not
// read as "any project is acceptable": that is the reading which turns this into
// decoration on exactly the fresh deployment least likely to be watching. The
// operator configures the benchmark project or the harness does not run.
func CheckBenchmarkProject(projectID, benchmarkProject string) error {
	target := strings.TrimSpace(projectID)
	permitted := strings.TrimSpace(benchmarkProject)

	if permitted == "" {
		return fmt.Errorf("refusing to run: no benchmark project is configured, so there is "+
			"nothing to check %q against. Configure one — an unset value is not permission",
			target)
	}
	if target == "" {
		return fmt.Errorf("refusing to run: no target project named. An empty project usually "+
			"means a default was substituted downstream, and the default is not %q", permitted)
	}
	if !strings.EqualFold(target, permitted) {
		return fmt.Errorf("refusing to run against project %q: this deployment permits "+
			"benchmarking only in %q. A benchmark run submits real tasks and clears state; "+
			"pointing it at a working project would do so there", target, permitted)
	}
	return nil
}

// CheckExcludedRoles refuses a swarm on the trading path.
//
// Uses the shared classifier rather than its own list. The cost/quality detector
// and the proposal applier already share it precisely so the two "can never
// diverge" on what trading means (2026-07-24-applier-trading-refusal D3); a
// third copy here would re-create that divergence while still reading like
// protection.
func CheckExcludedRoles(swarmID string) error {
	swarm := strings.TrimSpace(swarmID)
	if swarm == "" {
		return fmt.Errorf("refusing to run: no swarm named, so it cannot be checked against " +
			"the trading exclusion")
	}
	if swarmclass.IsTrading(swarm) {
		return fmt.Errorf("refusing to run against swarm %q: it is on the trading path, which "+
			"automation must never drive. A benchmark run submits tasks in bulk and repeats "+
			"them across arms. Nothing overrides this", swarm)
	}
	return nil
}
