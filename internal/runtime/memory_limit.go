package runtime

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// MinAgentMemoryLimit is the floor a DERIVED limit clamps to.
//
// Below this a limit stops bounding a workload and starts preventing one: an
// agent container that cannot hold a few hundred megabytes cannot run a step
// at all, so a smaller derived value would trade a rare OOM for a certain
// failure. An operator whose host genuinely cannot afford this must choose the
// number themselves, which is exactly the case where an explicit setting — and
// the refusal that guards it — is the right mechanism.
const MinAgentMemoryLimit int64 = 512 * 1024 * 1024

// agentMemoryHeadroomShare is the fraction of host memory a derived limit may
// commit across ALL concurrent agent containers.
//
// A POLICY CONSTANT, not a measurement, and stated as one. It leaves half the
// host for the daemon, Postgres, the sidecars (measured at ~2.4 GB on the
// reference host, kong the largest at 1.53 GB) and the page cache — and page
// cache pressure is what actually collapsed on 2026-09-20, not the sum of RSS.
//
// It is the one guess left in the derivation. Per-container peak recording
// exists to replace it with a measured figure; until then it errs toward
// leaving memory unused, which fails safe.
const agentMemoryHeadroomShare = 0.5

// ParseMemoryLimit converts an operator-written memory limit into bytes.
//
// Accepts the byte-suffix forms an operator reasons about ("2GiB", "512MiB",
// "1.5GiB") because 2147483648 is not a number anyone types correctly twice.
//
// "none" / "unbounded" return 0, the EXPLICIT unbounded escape hatch. It needs
// its own spelling because in YAML an ABSENT `agent_memory_limit` and an empty
// `agent_memory_limit: ""` both decode to the zero value, and those two must
// mean different things: absent derives a limit from the host, explicit means
// run without one. A shared spelling would make the shipped default
// unreachable or the escape hatch invisible.
//
// Empty also returns 0 here, and the CALLER distinguishes it: this function
// answers "what did this string ask for", and resolveAgentMemoryLimit decides
// what an unset field means.
//
// Rejects, rather than resolving, two values that would silently run unbounded:
//   - "0", ambiguous between "no limit" and "no memory";
//   - any negative value, which would fail the `> 0` test at the podman call
//     site and behave exactly as if nothing had been configured.
func ParseMemoryLimit(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	switch strings.ToLower(s) {
	case "none", "unbounded":
		return 0, nil
	}
	if strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("agent memory limit %q is negative; a negative limit would "+
			"behave as UNBOUNDED at the container boundary. Use \"\" to mean unbounded "+
			"deliberately, or a positive size such as \"2GiB\"", raw)
	}

	value, unit := splitMemoryUnit(s)
	if value == "" {
		return 0, fmt.Errorf("agent memory limit %q has no numeric part; write a size such "+
			"as \"2GiB\", \"512MiB\", or \"\" for unbounded", raw)
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("agent memory limit %q is not a size; write a size such as "+
			"\"2GiB\", \"512MiB\", or \"\" for unbounded", raw)
	}
	multiplier, ok := memoryUnitMultiplier(unit)
	if !ok {
		return 0, fmt.Errorf("agent memory limit %q has an unrecognised unit %q; use B, "+
			"KiB, MiB, GiB or TiB", raw, unit)
	}
	if n == 0 {
		return 0, fmt.Errorf("agent memory limit %q is ambiguous: 0 could mean no limit or "+
			"no memory. Use \"\" for unbounded, or a positive size", raw)
	}
	bytes := n * float64(multiplier)
	if bytes > math.MaxInt64 {
		return 0, fmt.Errorf("agent memory limit %q overflows", raw)
	}
	return int64(bytes), nil
}

// splitMemoryUnit divides "1.5GiB" into "1.5" and "GiB". A missing unit is
// treated as bytes, so a bare integer still parses.
func splitMemoryUnit(s string) (value, unit string) {
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	return s[:i], strings.TrimSpace(s[i:])
}

func memoryUnitMultiplier(unit string) (int64, bool) {
	const k = int64(1024)
	switch strings.ToLower(unit) {
	case "", "b":
		return 1, true
	case "k", "kb", "kib":
		return k, true
	case "m", "mb", "mib":
		return k * k, true
	case "g", "gb", "gib":
		return k * k * k, true
	case "t", "tb", "tib":
		return k * k * k * k, true
	default:
		return 0, false
	}
}

// DeriveAgentMemoryLimit computes the per-container limit for a host when the
// operator has set none.
//
// Why derived and not a shipped constant. The safety property is a PRODUCT —
// `concurrency x limit <= headroom` — and both inputs are properties of the
// host, known at config load. An earlier draft shipped a fixed 2GiB beside a
// default MaxConcurrentTasks of 4, which put the static-overcommit line at
// 8GiB: on any host below that, the SHIPPED DEFAULT tripped the refusal and the
// daemon would not boot. An upgrade that stops a daemon starting, on a
// configuration the operator never chose, is worse than the unbounded
// behaviour the box was already surviving (review-20260921-b6f2 F1).
//
// A derived limit cannot overcommit the host ABOVE THE 512MiB FLOOR, which is
// what makes the refusal reachable only for a limit an operator explicitly
// set. The absolute "cannot overcommit by construction" was wrong and is
// corrected here: in the floor regime — a host small enough that
// total x share / concurrency falls under 512MiB — the floor wins and the
// product CAN exceed the host. That is the accepted trade, because a limit
// below the floor stops bounding a workload and starts preventing one, and the
// consequence is a runtime OOM that the counter observes rather than a boot
// refusal (review-20260921-bfb8 F5).
//
// totalBytes of 0 means the host total could not be read, and returns 0
// (unbounded) rather than inventing a number from nothing.
func DeriveAgentMemoryLimit(totalBytes int64, concurrency int) int64 {
	if totalBytes <= 0 {
		return 0
	}
	if concurrency < 1 {
		// A degenerate concurrency must not divide by zero. One slot is the
		// smallest configuration that can run anything.
		concurrency = 1
	}
	share := int64(float64(totalBytes)*agentMemoryHeadroomShare) / int64(concurrency)
	if share < MinAgentMemoryLimit {
		return MinAgentMemoryLimit
	}
	return share
}

// HostTotalMemory reports the host's total physical memory in bytes.
//
// Used only to DERIVE a default limit, so an unreadable value is not fatal:
// the caller treats 0 as "unknown" and leaves containers unbounded rather than
// inventing a number. That is the historical behaviour, so a platform where
// this fails is no worse off than before the limit existed.
func HostTotalMemory() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("read host memory: %w", err)
	}
	defer func() { _ = f.Close() }()
	return parseMemTotal(f)
}

// HostAvailableMemory reports MemAvailable in bytes — what the kernel
// estimates is obtainable without swapping.
//
// Feeds only the WARNING path, never the refusal: a low reading may be page
// cache or a transient sidecar spike, and a momentary dip must not stop a
// daemon from booting. Returns 0 when unreadable, which suppresses the warning
// rather than firing it on no evidence.
func HostAvailableMemory() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("read host available memory: %w", err)
	}
	defer func() { _ = f.Close() }()
	return parseMemField(f, "MemAvailable:")
}

func parseMemTotal(r io.Reader) (int64, error) {
	return parseMemField(r, "MemTotal:")
}

// parseMemField pulls one kB-denominated field out of /proc/meminfo content.
//
// Split from the two exported readers so the parsing is testable without a
// host that happens to have the file — these numbers feed a safety default,
// and a parser verified only against the machine it was written on is verified
// against one sample.
func parseMemField(r io.Reader, prefix string) (int64, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		// "<prefix>", "<value>", "kB"
		if len(fields) < 2 {
			return 0, fmt.Errorf("host memory: %s line has no value: %q", prefix, line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("host memory: %s %q is not a number", prefix, fields[1])
		}
		// meminfo reports kB (kibibytes, despite the spelling).
		return kb * 1024, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("host memory: %w", err)
	}
	// NOT zero-with-nil-error: the caller reads 0 as "unknown", so returning
	// it without an error would quietly disable the limit on a malformed file.
	return 0, fmt.Errorf("host memory: no %s field", prefix)
}

// noteOOMKill records the closed-loop signal for the memory limit: a container
// the KERNEL killed for exceeding it.
//
// It takes the verdict rather than inferring it. An earlier version keyed on
// "exit 137 and a limit in force", which review-20260921-bfb8 F3 showed was
// wrong twice over: 137 is SIGKILL, so a container stopped with
// StopContainer(force=true) — which the executor does on its own cleanup paths
// — exits 137 cleanly and would have been counted as a memory kill; and the
// guard tested the MANAGER's default rather than the limit that actually bound
// the container, so a future per-container override under a global "none"
// would have dropped the count.
//
// podman inspect reports State.OOMKilled, which is the kernel's own answer and
// is true regardless of which limit bound. Both defects dissolve rather than
// being patched.
//
// Why a counter at all. The limit's default is DERIVED from host memory and
// concurrency, and its headroom share is a policy constant rather than a
// measurement, so "the default is too low" must be observable rather than
// something an operator infers from a task that quietly stopped working.
//
// An empty role still counts, labelled "unknown": losing the kill is worse
// than losing its attribution.
func (m *Manager) noteOOMKill(oomKilled bool, role string) {
	if !oomKilled {
		return
	}
	m.logger.Warn().
		Str("role", role).
		Int64("manager_memory_limit_bytes", m.agentMemoryLimit).
		Msg("container was OOM-killed for exceeding its memory limit: if this recurs, raise " +
			"runtime.agent_memory_limit or lower scheduler.max_concurrent_tasks")
	m.metrics.RecordContainerOOMKill(role)
}
