package service

import (
	"strings"
	"testing"
)

// TestResolveAgentMemoryLimit covers the round-4 resolution rules
// (review-20260921-b6f2 F1/F2/F3): a derived default that cannot overcommit,
// refusal confined to a limit an operator explicitly SET, and a refusal
// message that names every exit including the one that boots immediately.
func TestResolveAgentMemoryLimit(t *testing.T) {
	const (
		mib = int64(1024 * 1024)
		gib = 1024 * mib
	)

	t.Run("unset derives from the host and never refuses", func(t *testing.T) {
		// An 8GiB host with 4 slots: the fixed-2GiB draft would have put the
		// overcommit line exactly here and refused to boot on a default
		// nobody chose. The derived limit cannot.
		got, warn, err := resolveAgentMemoryLimit("", 4, 8*gib, 6*gib)
		if err != nil {
			t.Fatalf("a derived default refused to boot: %v", err)
		}
		if got != 1*gib {
			t.Fatalf("derived limit = %d, want %d", got, 1*gib)
		}
		if warn != "" {
			t.Fatalf("unexpected warning: %s", warn)
		}
	})

	t.Run("an explicitly set limit over TOTAL refuses, naming all three exits", func(t *testing.T) {
		_, _, err := resolveAgentMemoryLimit("4GiB", 8, 16*gib, 12*gib)
		if err == nil {
			t.Fatal("8 x 4GiB against a 16GiB host was accepted")
		}
		msg := err.Error()
		for _, want := range []string{
			"4GiB",                 // what they set
			"8",                    // the concurrency
			"agent_memory_limit",   // the key to change
			"max_concurrent_tasks", // the other key
			`"none"`,               // the boot-now escape
		} {
			if !strings.Contains(msg, want) {
				t.Fatalf("refusal %q does not name %q", msg, want)
			}
		}
	})

	t.Run("over AVAILABLE only warns", func(t *testing.T) {
		// Fits the host but not what is free right now: could be page cache
		// or a transient sidecar spike, and a dip must not stop a boot.
		got, warn, err := resolveAgentMemoryLimit("3GiB", 4, 32*gib, 8*gib)
		if err != nil {
			t.Fatalf("a transient shortfall refused to boot: %v", err)
		}
		if got != 3*gib {
			t.Fatalf("limit = %d, want the configured %d", got, 3*gib)
		}
		if warn == "" {
			t.Fatal("over-available commitment produced no warning")
		}
		if !strings.Contains(warn, "3GiB") || !strings.Contains(warn, "4") {
			t.Fatalf("warning %q does not name the limit and concurrency", warn)
		}
	})

	// The explicit unbounded escape must survive every check, because it is
	// the fastest path back to a booting daemon — and the refusal recommends
	// it, so the refusal firing on it would be a loop.
	t.Run("none is unbounded and never refuses", func(t *testing.T) {
		for _, spelling := range []string{"none", "unbounded", "NONE"} {
			got, warn, err := resolveAgentMemoryLimit(spelling, 64, 1*gib, 512*mib)
			if err != nil {
				t.Fatalf("%q refused: %v", spelling, err)
			}
			if got != 0 {
				t.Fatalf("%q = %d, want 0 (unbounded)", spelling, got)
			}
			if warn != "" {
				t.Fatalf("%q warned: %s", spelling, warn)
			}
		}
	})

	// ABSENT is not the escape hatch: it derives. In YAML an absent key and
	// an empty string decode identically, so if empty meant unbounded the
	// shipped default would be unreachable.
	t.Run("absent derives rather than going unbounded", func(t *testing.T) {
		got, _, err := resolveAgentMemoryLimit("", 4, 32*gib, 18*gib)
		if err != nil {
			t.Fatalf("absent refused: %v", err)
		}
		if got == 0 {
			t.Fatal("an absent setting went unbounded instead of deriving")
		}
	})

	t.Run("a bad value is fatal, not a silent fallback", func(t *testing.T) {
		if _, _, err := resolveAgentMemoryLimit("0", 4, 32*gib, 18*gib); err == nil {
			t.Fatal(`"0" was accepted`)
		}
		if _, _, err := resolveAgentMemoryLimit("lots", 4, 32*gib, 18*gib); err == nil {
			t.Fatal(`"lots" was accepted`)
		}
	})

	// An unreadable host total must not refuse: the check has no basis, and
	// the honest fallback is the historical unbounded behaviour.
	t.Run("unknown host total does not refuse", func(t *testing.T) {
		if _, _, err := resolveAgentMemoryLimit("4GiB", 8, 0, 0); err != nil {
			t.Fatalf("refused without knowing the host total: %v", err)
		}
	})
}

// TestResolveAgentMemoryLimit_AdversarialInputs covers review-20260921-bfb8 F6
// and F7: the REFUSE guard must not be defeatable by arithmetic, and an
// explicit limit small enough to be useless should say so.
func TestResolveAgentMemoryLimit_AdversarialInputs(t *testing.T) {
	const (
		mib = int64(1024 * 1024)
		gib = 1024 * mib
	)

	// ParseMemoryLimit caps a single value at MaxInt64, but the PRODUCT was
	// unguarded: a limit near the cap times any concurrency above 1 wraps
	// negative, `committed > totalBytes` reads false, and the refusal is
	// defeated by a typo.
	t.Run("a limit near the int64 cap cannot defeat the refusal", func(t *testing.T) {
		_, _, err := resolveAgentMemoryLimit("8000000TiB", 8, 32*gib, 18*gib)
		if err == nil {
			t.Fatal("an absurd explicit limit was accepted")
		}
	})

	t.Run("a limit larger than the host alone refuses before multiplying", func(t *testing.T) {
		_, _, err := resolveAgentMemoryLimit("64GiB", 1, 32*gib, 18*gib)
		if err == nil {
			t.Fatal("a limit exceeding host total at concurrency 1 was accepted")
		}
	})

	// F7: the 512MiB floor applies to DERIVED limits only, so an explicit
	// 1-byte limit parses and produces --memory 1 — a container that OOMs on
	// startup. The operator chose it, so it is not refused, but it must not
	// pass silently.
	t.Run("an explicit limit below the floor warns", func(t *testing.T) {
		got, warn, err := resolveAgentMemoryLimit("1MiB", 4, 32*gib, 18*gib)
		if err != nil {
			t.Fatalf("an explicit small limit was refused: %v", err)
		}
		if got != 1*mib {
			t.Fatalf("limit = %d, want the configured %d", got, 1*mib)
		}
		if warn == "" {
			t.Fatal("a limit below the derived floor produced no warning")
		}
	})
}

// humanBytes is the operator's only lens on the refusal arithmetic: a refusal
// quoting 17179869184 is one nobody can check against their config.
func TestHumanBytes(t *testing.T) {
	const mib = int64(1024 * 1024)
	for _, tt := range []struct {
		in   int64
		want string
	}{
		{in: 16 * 1024 * mib, want: "16.0GiB"},
		{in: 512 * mib, want: "512.0MiB"},
		{in: 1536 * mib, want: "1.5GiB"},
		{in: 512, want: "512B"},
		{in: 0, want: "0B"},
	} {
		if got := humanBytes(tt.in); got != tt.want {
			t.Fatalf("humanBytes(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
