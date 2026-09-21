package service

import (
	"fmt"
	"strings"

	"vornik.io/vornik/internal/runtime"
)

// hostMemoryReader reads a host memory figure in bytes.
//
// An injection seam, and the reason it exists is that the startup wiring —
// read the host, resolve, wire the option or fail init — was the single
// integration point for the whole memory-limit change and had no test, which
// is where the round-3 boot-refusal outage would have been caught
// (review-20260921-bfb8, test-coverage finding).
type hostMemoryReader func() (int64, error)

// applyAgentMemoryLimit resolves the agent memory limit and appends it to the
// runtime options, or fails initialisation when the configuration cannot work.
//
// Returns the options rather than mutating, so a caller cannot forget the
// result — and takes the host readers as parameters so all three outcomes
// (derive-and-wire, refuse, unbounded) are reachable in a test without a host
// that happens to have the right amount of memory.
func (c *Container) applyAgentMemoryLimit(
	opts []runtime.ManagerOption,
	readTotal, readAvailable hostMemoryReader,
) ([]runtime.ManagerOption, error) {
	total, err := readTotal()
	if err != nil {
		// Not fatal: an unreadable host total means the derivation has no
		// basis, and the honest fallback is the historical unbounded
		// behaviour rather than a refusal on no evidence.
		c.Logger.Warn().Err(err).
			Msg("host memory unreadable: agent containers will run unbounded unless " +
				"runtime.agent_memory_limit is set")
	}
	available, _ := readAvailable()

	raw := c.Config.Runtime.AgentMemoryLimit
	concurrency := c.Config.Scheduler.MaxConcurrentTasks
	limit, warn, resolveErr := resolveAgentMemoryLimit(raw, concurrency, total, available)
	if resolveErr != nil {
		return nil, fmt.Errorf("agent memory limit: %w", resolveErr)
	}
	if warn != "" {
		c.Logger.Warn().Msg(warn)
	}
	if limit <= 0 {
		c.Logger.Warn().Msg("agent containers run UNBOUNDED: one agent can exhaust host " +
			"memory (runtime.agent_memory_limit is \"none\", or the host total is unreadable)")
		return opts, nil
	}
	c.Logger.Info().
		Int64("bytes", limit).
		Bool("derived", strings.TrimSpace(raw) == "").
		Int("max_concurrent_tasks", concurrency).
		Msg("agent container memory limit")
	return append(opts, runtime.WithAgentMemoryLimit(limit)), nil
}

// resolveAgentMemoryLimit decides the per-container memory limit at config
// load, and says whether the daemon may boot with it.
//
// Three outcomes, and which one applies turns on whether the OPERATOR chose
// the number:
//
//   - `raw` empty: DERIVED from the host envelope. Cannot overcommit by
//     construction, so it never refuses. This is the shipped path.
//   - `raw` set and the product exceeds TOTAL memory: REFUSE. Someone opted
//     into an arithmetically impossible configuration and no workload, timing
//     or transient dip can make it fit.
//   - the product exceeds AVAILABLE memory: WARN and boot. May be page cache
//     or a sidecar spike, and a momentary dip must not stop a daemon starting.
//
// The split exists because an earlier draft shipped a fixed 2GiB default
// beside a default max_concurrent_tasks of 4, putting the refusal line at
// 8GiB: on any smaller host the SHIPPED DEFAULT refused to boot, turning a
// load-dependent OOM risk into a certain outage introduced by an upgrade, on a
// configuration the operator never chose. Deriving the default makes the
// refusal reachable only for a limit someone actually set
// (review-20260921-b6f2 F1).
//
// totalBytes / availableBytes of 0 mean the host figures could not be read.
// Neither check then has a basis, so neither fires — the historical unbounded
// behaviour is the honest fallback rather than a refusal on no evidence.
func resolveAgentMemoryLimit(raw string, concurrency int, totalBytes, availableBytes int64) (int64, string, error) {
	// ABSENT is the shipped path and DERIVES. It is distinct from an explicit
	// "none": in YAML both an absent key and `agent_memory_limit: ""` decode
	// to the zero value, so the escape hatch needs its own spelling or the
	// shipped default becomes unreachable. Found while testing this function,
	// not in review.
	if strings.TrimSpace(raw) == "" {
		return runtime.DeriveAgentMemoryLimit(totalBytes, concurrency), "", nil
	}

	limit, err := runtime.ParseMemoryLimit(raw)
	if err != nil {
		// FATAL, never warn-and-continue: a bad value that fell back to
		// unbounded would reproduce the defect this limit exists to remove.
		return 0, "", err
	}
	if limit == 0 {
		// An explicit "none"/"unbounded". No product to check, and the
		// refusal must never fire on the escape hatch it recommends.
		return 0, "", nil
	}

	if concurrency < 1 {
		concurrency = 1
	}
	// OVERFLOW GUARD. ParseMemoryLimit caps a single value at MaxInt64, but
	// the PRODUCT is unguarded: an explicit limit near the cap with any
	// concurrency above 1 wraps negative, the `committed > totalBytes` test
	// reads false, and the refusal that this whole change is about is
	// arithmetically defeated by a typo (review-20260921-bfb8 F6). Checked
	// before multiplying rather than after, because after is too late.
	if totalBytes > 0 && limit > totalBytes {
		return 0, "", fmt.Errorf(
			"agent_memory_limit %q (%s) is larger than the host's total memory (%s) on its "+
				"own, before concurrency. Lower agent_memory_limit, or set "+
				"agent_memory_limit: \"none\" to boot unbounded now",
			raw, humanBytes(limit), humanBytes(totalBytes))
	}
	committed := limit * int64(concurrency)
	if committed < 0 {
		return 0, "", fmt.Errorf(
			"agent_memory_limit %q x max_concurrent_tasks %d overflows a 64-bit byte count; "+
				"pick a size the host could plausibly have", raw, concurrency)
	}

	if totalBytes > 0 && committed > totalBytes {
		return 0, "", fmt.Errorf(
			"agent_memory_limit %q x max_concurrent_tasks %d commits %s, more than the host's "+
				"total memory (%s): every slot filling would OOM the box. Lower "+
				"agent_memory_limit, lower max_concurrent_tasks, or set "+
				"agent_memory_limit: \"none\" to boot unbounded now",
			raw, concurrency, humanBytes(committed), humanBytes(totalBytes))
	}

	// F7: the 512MiB floor applies to DERIVED limits only. An explicit value
	// below it parses fine and produces a container that OOMs on startup — the
	// operator chose it, so it is not refused, but it is the explicit analogue
	// of the "0" the parser rejects as ambiguous and must not pass silently.
	if limit < runtime.MinAgentMemoryLimit {
		return limit, fmt.Sprintf(
			"agent_memory_limit %q (%s) is below the %s a derived limit would never go under: "+
				"a container this small may be killed before it can run a step. Booting "+
				"because it was set explicitly",
			raw, humanBytes(limit), humanBytes(runtime.MinAgentMemoryLimit)), nil
	}

	if availableBytes > 0 && committed > availableBytes {
		return limit, fmt.Sprintf(
			"agent_memory_limit %q x max_concurrent_tasks %d commits %s, more than is currently "+
				"available (%s). Booting: this may be page cache or a transient sidecar spike, "+
				"but if it persists, lower agent_memory_limit or max_concurrent_tasks",
			raw, concurrency, humanBytes(committed), humanBytes(availableBytes)), nil
	}

	return limit, "", nil
}

// humanBytes renders a byte count the way the operator wrote it, because a
// refusal quoting 17179869184 is a refusal nobody can check against their
// config.
func humanBytes(n int64) string {
	const k = int64(1024)
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.1fGiB", float64(n)/float64(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1fMiB", float64(n)/float64(k*k))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
