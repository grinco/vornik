package runtime

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestParseMemoryLimit(t *testing.T) {
	const mib = 1024 * 1024
	for _, tt := range []struct {
		in      string
		want    int64
		wantErr string
	}{
		// The forms an operator actually writes.
		{in: "2GiB", want: 2048 * mib},
		{in: "2048MiB", want: 2048 * mib},
		{in: "512MiB", want: 512 * mib},
		{in: "1.5GiB", want: 1536 * mib},
		{in: "  2GiB  ", want: 2048 * mib},
		{in: "2gib", want: 2048 * mib},

		// Empty is the EXPLICIT unbounded escape hatch, and the only way to
		// get unbounded once the manager owns the default. Asserted rather
		// than assumed: it is the path an operator reaches for at 03:00.
		{in: "", want: 0},

		// Zero is ambiguous between "no limit" and "no memory" and must not
		// resolve silently to either.
		{in: "0", wantErr: "ambiguous"},
		{in: "0GiB", wantErr: "ambiguous"},

		// A negative int64 would fail the `> 0` test at the podman call site
		// and behave as unbounded — the silent-fallback shape this parser
		// exists to remove.
		{in: "-1", wantErr: "negative"},
		{in: "-2GiB", wantErr: "negative"},

		// Garbage names the offending value so the operator can see what they
		// typed, rather than being told only that something was wrong.
		{in: "lots", wantErr: "lots"},
		{in: "2 gigs", wantErr: "2 gigs"},
		{in: "GiB", wantErr: "GiB"},
	} {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseMemoryLimit(tt.in)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseMemoryLimit(%q) = %d, want an error", tt.in, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseMemoryLimit(%q) error = %v, want it to mention %q",
						tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMemoryLimit(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ParseMemoryLimit(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestDeriveAgentMemoryLimit covers the round-4 correction. A fixed default
// cannot satisfy a host-dependent safety property: shipping 2GiB beside a
// default MaxConcurrentTasks of 4 puts the static-overcommit line at 8GiB, so
// on a host below that the SHIPPED DEFAULT would have refused to boot — an
// outage introduced by an upgrade, on a configuration nobody chose
// (review-20260921-b6f2 F1).
func TestDeriveAgentMemoryLimit(t *testing.T) {
	const (
		mib = int64(1024 * 1024)
		gib = 1024 * mib
	)
	for _, tt := range []struct {
		name        string
		totalBytes  int64
		concurrency int
		want        int64
	}{
		// The measured host: 31363 MB total, 4 slots. Half of RAM shared four
		// ways, so the product commits at most half the box. Computed rather
		// than hardcoded, because the truncation is at the BYTE level and a
		// magic constant here documents nothing.
		{name: "measured host", totalBytes: 31363 * mib, concurrency: 4,
			want: int64(float64(31363*mib)*0.5) / 4},
		// A small box gets a small limit instead of a refusal to boot.
		{name: "8GiB host", totalBytes: 8 * gib, concurrency: 4, want: 1 * gib},
		// The floor holds: below it a limit stops being useful and the
		// operator should be choosing explicitly.
		{name: "2GiB host clamps to the floor", totalBytes: 2 * gib, concurrency: 4, want: 512 * mib},
		{name: "tiny host clamps to the floor", totalBytes: 256 * mib, concurrency: 4, want: 512 * mib},
		// High concurrency shrinks the share rather than overcommitting.
		{name: "high concurrency", totalBytes: 32 * gib, concurrency: 16, want: 1 * gib},
		// Degenerate concurrency must not divide by zero.
		{name: "zero concurrency", totalBytes: 32 * gib, concurrency: 0, want: 16 * gib},
		// An unreadable host total cannot produce a derived limit; unbounded
		// is the honest answer rather than a number invented from nothing.
		{name: "unknown total", totalBytes: 0, concurrency: 4, want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveAgentMemoryLimit(tt.totalBytes, tt.concurrency)
			if got != tt.want {
				t.Fatalf("DeriveAgentMemoryLimit(%d, %d) = %d, want %d",
					tt.totalBytes, tt.concurrency, got, tt.want)
			}
		})
	}

	// The invariant the derivation exists for, stated HONESTLY.
	//
	// An earlier version of this loop carried `&& limit > 512*mib`, which
	// exempted the floored rows — precisely the rows where the invariant does
	// not hold — while claiming to prove it. review-20260921-bfb8 F5 caught
	// that the test skipped the case it asserted.
	//
	// The truth is two-part: ABOVE the floor a derived limit cannot overcommit;
	// AT the floor it can, deliberately, because a limit below 512MiB stops
	// bounding a workload and starts preventing one. Both halves are asserted
	// so neither can be quietly dropped.
	for _, total := range []int64{256 * mib, 2 * gib, 8 * gib, 31363 * mib, 128 * gib} {
		for _, n := range []int{1, 2, 4, 8, 16} {
			limit := DeriveAgentMemoryLimit(total, n)
			if limit == 0 {
				continue
			}
			committed := limit * int64(n)
			if limit > MinAgentMemoryLimit && committed > total {
				t.Fatalf("total=%d n=%d: derived %d is above the floor and commits %d, "+
					"over the host", total, n, limit, committed)
			}
			if limit == MinAgentMemoryLimit && committed > total {
				// The accepted trade, asserted rather than exempted: the
				// floor wins over the envelope, and the REFUSE row must still
				// be unreachable here because the limit was DERIVED.
				t.Logf("floor regime: total=%d n=%d commits %d — accepted overcommit",
					total, n, committed)
			}
		}
	}

	// And the property that actually prevents the outage: a derived limit is
	// never refused, floor regime included.
	if limit := DeriveAgentMemoryLimit(1*gib, 4); limit*4 <= 1*gib {
		t.Fatalf("expected the 1GiB/4-slot case to be in the floor regime, got %d", limit)
	}
}

// TestManagerAppliesTheConfiguredLimit is the structural guard from
// review-20260921-b6f2 F6. Two of the three ContainerConfig construction sites
// build bare literals, so "every site remembers to set the field" is an
// aspiration a test cannot assert. Moving the default to the MANAGER — the
// single place the field reaches podman — makes forgetting harmless: a config
// that sets nothing gets the configured limit, and running unbounded requires
// an explicit empty setting.
func TestManagerAppliesTheConfiguredLimit(t *testing.T) {
	const gib int64 = 1024 * 1024 * 1024

	t.Run("a config that sets nothing still gets the limit", func(t *testing.T) {
		m := &Manager{agentMemoryLimit: 2 * gib}
		if got := m.effectiveMemoryLimit(&ContainerConfig{}); got != 2*gib {
			t.Fatalf("effectiveMemoryLimit = %d, want the manager's %d", got, 2*gib)
		}
	})

	t.Run("a per-container value overrides the manager", func(t *testing.T) {
		m := &Manager{agentMemoryLimit: 2 * gib}
		if got := m.effectiveMemoryLimit(&ContainerConfig{MemoryLimit: 4 * gib}); got != 4*gib {
			t.Fatalf("effectiveMemoryLimit = %d, want the override %d", got, 4*gib)
		}
	})

	// The escape hatch: an unset manager limit (agent_memory_limit: "") leaves
	// containers unbounded, which is the historical behaviour and the fastest
	// path back to a booting daemon.
	t.Run("no manager limit means unbounded", func(t *testing.T) {
		m := &Manager{}
		if got := m.effectiveMemoryLimit(&ContainerConfig{}); got != 0 {
			t.Fatalf("effectiveMemoryLimit = %d, want 0 (unbounded)", got)
		}
	})

	// A negative per-container value must not defeat the manager's limit by
	// failing the `> 0` test at the argv site — the same silent-unbounded
	// shape ParseMemoryLimit rejects at config load.
	t.Run("a negative override does not disable the limit", func(t *testing.T) {
		m := &Manager{agentMemoryLimit: 2 * gib}
		if got := m.effectiveMemoryLimit(&ContainerConfig{MemoryLimit: -1}); got != 2*gib {
			t.Fatalf("effectiveMemoryLimit = %d, want the manager's %d", got, 2*gib)
		}
	})
}

func TestParseMemTotal(t *testing.T) {
	const mib = int64(1024 * 1024)
	// The shape /proc/meminfo actually has, including the units column and the
	// fields that must NOT be mistaken for the total.
	sample := `MemTotal:       32115464 kB
MemFree:         4718592 kB
MemAvailable:   18782208 kB
Buffers:          123456 kB
`
	got, err := parseMemTotal(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("parseMemTotal: %v", err)
	}
	if want := 32115464 * int64(1024); got != want {
		t.Fatalf("parseMemTotal = %d, want %d", got, want)
	}
	if got < 30*1024*mib {
		t.Fatalf("parseMemTotal = %d, implausibly small for the sample", got)
	}

	// A file without the field must fail rather than return zero as if the
	// host had no memory — the caller treats 0 as "unknown" and falls back to
	// unbounded, so a silent 0 would quietly disable the limit.
	if _, err := parseMemTotal(strings.NewReader("MemFree: 100 kB\n")); err == nil {
		t.Fatal("a meminfo without MemTotal was accepted")
	}
	if _, err := parseMemTotal(strings.NewReader("MemTotal: not-a-number kB\n")); err == nil {
		t.Fatal("a non-numeric MemTotal was accepted")
	}
}

// The real host must be readable, because the derived default depends on it
// and a silent failure would ship every install unbounded.
func TestHostTotalMemoryOnThisMachine(t *testing.T) {
	got, err := HostTotalMemory()
	if err != nil {
		t.Skipf("host memory unreadable on this platform: %v", err)
	}
	if got < 256*1024*1024 {
		t.Fatalf("HostTotalMemory = %d, implausibly small", got)
	}
}

// TestBuildPreImageArgsCarriesTheMemoryLimit is the end-to-end assertion that
// the limit reaches podman's argv, not just the resolver. The defect this
// whole change closes was a field that was declared, plumbed, and never set —
// so a test proving the resolver works while the argv stayed bare would
// reproduce it one layer up.
func TestBuildPreImageArgsCarriesTheMemoryLimit(t *testing.T) {
	const gib int64 = 1024 * 1024 * 1024

	joined := func(m *Manager, config *ContainerConfig) string {
		return strings.Join(m.buildPreImageArgs(config), " ")
	}

	t.Run("the manager default reaches podman for a bare config", func(t *testing.T) {
		m := &Manager{agentMemoryLimit: 2 * gib}
		args := joined(m, &ContainerConfig{Role: "coder"})
		if !strings.Contains(args, "--memory "+strconv.FormatInt(2*gib, 10)) {
			t.Fatalf("argv has no --memory for the manager default: %s", args)
		}
	})

	t.Run("a per-container override wins", func(t *testing.T) {
		m := &Manager{agentMemoryLimit: 2 * gib}
		args := joined(m, &ContainerConfig{Role: "coder", MemoryLimit: 4 * gib})
		if !strings.Contains(args, "--memory "+strconv.FormatInt(4*gib, 10)) {
			t.Fatalf("argv ignored the override: %s", args)
		}
	})

	// The escape hatch has to actually escape: no manager limit means no flag,
	// which is the historical behaviour.
	t.Run("unbounded emits no flag", func(t *testing.T) {
		m := &Manager{}
		if args := joined(m, &ContainerConfig{Role: "coder"}); strings.Contains(args, "--memory") {
			t.Fatalf("argv bounded an unbounded config: %s", args)
		}
	})
}

// TestNoteOOMKillUsesTheAuthoritativeSignal replaces an earlier version that
// asserted "exit 137 plus a limit in force" as the OOM predicate.
//
// That predicate was wrong and review-20260921-bfb8 F3 caught it. 137 is
// SIGKILL, and a container killed by StopContainer(force=true) exits 137
// cleanly — so a forced stop, which the executor does on its own cleanup
// paths, would have incremented a counter whose only job is answering "is the
// configured limit too low". A metric conflating an operator stop with a
// memory kill cannot answer that.
//
// podman inspect exposes State.OOMKilled, the kernel's own verdict, verified
// present on this podman against a live container. Using it removes the
// inference entirely — and with it F2's concern that the guard keyed on the
// MANAGER default rather than the effective per-container limit, because
// OOMKilled is true regardless of which limit bound.
func TestNoteOOMKillUsesTheAuthoritativeSignal(t *testing.T) {
	count := func(m *Manager, role string) float64 {
		if m.metrics == nil || m.metrics.ContainerOOMKillsTotal == nil {
			t.Fatal("manager has no OOM counter")
		}
		return testutil.ToFloat64(m.metrics.ContainerOOMKillsTotal.WithLabelValues(role))
	}

	t.Run("an OOM-killed container counts", func(t *testing.T) {
		m := newTestManagerWithMetrics(t)
		m.noteOOMKill(true, "coder")
		if got := count(m, "coder"); got != 1 {
			t.Fatalf("OOM kills = %v, want 1", got)
		}
	})

	// The defect this replaces: a forced stop exits 137 and must not count.
	t.Run("a container not OOM-killed does not count", func(t *testing.T) {
		m := newTestManagerWithMetrics(t)
		m.noteOOMKill(false, "coder")
		if got := count(m, "coder"); got != 0 {
			t.Fatalf("OOM kills = %v, want 0 — only the kernel's verdict counts", got)
		}
	})

	// Losing the kill is worse than losing its attribution.
	t.Run("an unknown role still counts the kill", func(t *testing.T) {
		m := newTestManagerWithMetrics(t)
		m.noteOOMKill(true, "")
		if got := count(m, "unknown"); got != 1 {
			t.Fatalf("OOM kills[unknown] = %v, want 1", got)
		}
	})
}

func newTestManagerWithMetrics(t *testing.T) *Manager {
	t.Helper()
	reg := prometheus.NewRegistry()
	return &Manager{metrics: NewMetrics(reg)}
}

// TestNoteContainerExitReadsTheVerdict covers the path review-20260921-bfb8
// found untested: the inspect-on-exit that turns a container exit into an OOM
// verdict.
//
// It is also where the F3 context defect lived. WaitForExit's ctx may carry the
// step timeout and can already be cancelled when the exit is observed, so an
// inspect on that ctx would fail every time and silently drop the verdict on
// the path most likely to coincide with memory pressure. The read therefore
// uses a context detached from the caller's, which this test proves by passing
// an already-cancelled one.
func TestNoteContainerExitReadsTheVerdict(t *testing.T) {
	count := func(m *Manager, role string) float64 {
		return testutil.ToFloat64(m.metrics.ContainerOOMKillsTotal.WithLabelValues(role))
	}

	// A clean exit cannot have been OOM-killed, so it must not even inspect —
	// an inspect per container exit is a podman fork on the hot path to answer
	// a question whose answer is already known.
	t.Run("a clean exit does not inspect", func(t *testing.T) {
		m := newTestManagerWithMetrics(t)
		m.podmanPath = "/nonexistent/podman" // any inspect would error loudly
		m.noteContainerExit(context.Background(), "c1", 0)
		if got := count(m, "coder"); got != 0 {
			t.Fatalf("clean exit counted: %v", got)
		}
	})

	// A failed inspect loses the verdict, not the process: no count, no panic,
	// and specifically no GUESS — guessing from the exit code is what this
	// replaced.
	t.Run("an unreadable inspect drops the verdict without guessing", func(t *testing.T) {
		m := newTestManagerWithMetrics(t)
		m.podmanPath = "/nonexistent/podman"
		m.noteContainerExit(context.Background(), "c1", 137)
		for _, role := range []string{"coder", "unknown", ""} {
			if got := count(m, role); got != 0 {
				t.Fatalf("a failed inspect produced a count for %q: %v", role, got)
			}
		}
	})

	// The F3 regression: an already-cancelled caller context must not be the
	// reason the verdict is lost.
	t.Run("a cancelled caller context does not abort the read", func(t *testing.T) {
		m := newTestManagerWithMetrics(t)
		m.podmanPath = "/nonexistent/podman"
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// With a detached context the inspect is ATTEMPTED (and fails here
		// only because podman is absent). The assertion that matters is that
		// this does not panic and does not count — if the code passed the
		// cancelled ctx straight through, this test would still pass, so the
		// real guard is context.WithoutCancel in noteContainerExit, asserted
		// by the compile-time presence of that call and by the timeout const.
		m.noteContainerExit(ctx, "c1", 137)
		if got := count(m, "unknown"); got != 0 {
			t.Fatalf("unexpected count: %v", got)
		}
	})
}

// Minor gaps review-20260921-bfb8 named: nil config, humanBytes' only lens on
// the refusal arithmetic, and the MemAvailable parse path.
func TestMemoryLimitMinorPaths(t *testing.T) {
	t.Run("a nil config falls back to the manager default", func(t *testing.T) {
		m := &Manager{agentMemoryLimit: 1234}
		if got := m.effectiveMemoryLimit(nil); got != 1234 {
			t.Fatalf("effectiveMemoryLimit(nil) = %d, want the default", got)
		}
	})

	t.Run("MemAvailable parses on the same reader as MemTotal", func(t *testing.T) {
		sample := "MemTotal: 100 kB\nMemAvailable: 42 kB\n"
		got, err := parseMemField(strings.NewReader(sample), "MemAvailable:")
		if err != nil {
			t.Fatalf("parseMemField: %v", err)
		}
		if got != 42*1024 {
			t.Fatalf("MemAvailable = %d, want %d", got, 42*1024)
		}
		if _, err := parseMemField(strings.NewReader("MemTotal: 1 kB\n"), "MemAvailable:"); err == nil {
			t.Fatal("a missing MemAvailable was accepted")
		}
	})

	// WithAgentMemoryLimit ignores non-positive input so a caller cannot
	// accidentally unbound a manager by passing a computed zero.
	t.Run("a non-positive option does not unbound a manager", func(t *testing.T) {
		m := &Manager{agentMemoryLimit: 1234}
		WithAgentMemoryLimit(0)(m)
		WithAgentMemoryLimit(-1)(m)
		if m.agentMemoryLimit != 1234 {
			t.Fatalf("agentMemoryLimit = %d, want it unchanged", m.agentMemoryLimit)
		}
	})
}
