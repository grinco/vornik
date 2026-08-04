package report

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// OPERATOR INSTRUCTION 2026-08-03: "make sure the appropriate logs are included".
// The journal tail was CLI-only (offlineCheckJournal), so a report filed from a
// chat channel — against a LIVE daemon — carried no log lines at all. One
// implementation, used by both paths.
func TestJournalTail_SurfacesErrorLines(t *testing.T) {
	orig := journalRun
	journalRun = func(context.Context, int) ([]byte, error) {
		return []byte(strings.Join([]string{
			`{"level":"info","msg":"started"}`,
			`{"level":"error","msg":"dial tcp: refused"}`,
			`panic: nil map write`,
			`{"level":"info","msg":"still fine"}`,
			`fatal: cannot open config`,
		}, "\n")), nil
	}
	t.Cleanup(func() { journalRun = orig })

	c := JournalTail(context.Background())

	if c.Name != "journal" {
		t.Errorf("Name = %q, want journal", c.Name)
	}
	if c.Status != "warn" {
		t.Errorf("Status = %q, want warn when error lines exist", c.Status)
	}
	if len(c.Items) != 3 {
		t.Fatalf("Items = %v, want the 3 error/panic/fatal lines", c.Items)
	}
	joined := strings.Join(c.Items, "\n")
	for _, want := range []string{"dial tcp: refused", "panic: nil map write", "fatal: cannot open config"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Items missing %q: %v", want, c.Items)
		}
	}
	if strings.Contains(joined, "still fine") {
		t.Errorf("info lines must not be collected: %v", c.Items)
	}
}

// A clean journal is an OK check with no lines — not a missing section.
func TestJournalTail_CleanJournalIsOK(t *testing.T) {
	orig := journalRun
	journalRun = func(context.Context, int) ([]byte, error) {
		return []byte(`{"level":"info","msg":"started"}`), nil
	}
	t.Cleanup(func() { journalRun = orig })

	c := JournalTail(context.Background())
	if c.Status != "ok" || len(c.Items) != 0 {
		t.Errorf("clean journal = %+v, want ok with no items", c)
	}
}

// No journalctl (a container, or a non-systemd host) is a WARN, not a failure —
// the daemon inside a container is the normal CE quickstart deployment.
func TestJournalTail_UnavailableIsAWarnNotAFailure(t *testing.T) {
	orig := journalRun
	journalRun = func(context.Context, int) ([]byte, error) {
		return nil, errors.New("exec: \"journalctl\": executable file not found in $PATH")
	}
	t.Cleanup(func() { journalRun = orig })

	c := JournalTail(context.Background())
	if c.Status != "warn" {
		t.Errorf("Status = %q, want warn", c.Status)
	}
	if !strings.Contains(c.Message, "unavailable") {
		t.Errorf("Message = %q, want it to say the tail was skipped", c.Message)
	}
	if len(c.Items) != 0 {
		t.Errorf("Items = %v, want none", c.Items)
	}
}

// The tail is bounded at collection time too, so a screaming daemon cannot hand
// the renderer thousands of lines.
func TestJournalTail_CapsCollectedLines(t *testing.T) {
	orig := journalRun
	journalRun = func(context.Context, int) ([]byte, error) {
		var b strings.Builder
		for i := 0; i < 200; i++ {
			b.WriteString(`{"level":"error","msg":"boom ` + strings.Repeat("x", 400) + `"}` + "\n")
		}
		return []byte(b.String()), nil
	}
	t.Cleanup(func() { journalRun = orig })

	c := JournalTail(context.Background())
	if len(c.Items) > maxCheckItems {
		t.Errorf("collected %d lines, cap is %d", len(c.Items), maxCheckItems)
	}
	for _, it := range c.Items {
		if len(it) > maxCheckItemBytes+len("…") {
			t.Errorf("line not truncated at collection: %d bytes", len(it))
		}
	}
	if !strings.Contains(c.Message, "200") {
		t.Errorf("Message %q should state how many lines were seen, not just the kept ones", c.Message)
	}
}

// SECURITY INVARIANT (operator question 2026-08-03: "isn't it dangerous to give
// chat access to OS commands?"). The journal tail is reachable from a chat-triggered
// report, so its argument vector must be a CONSTANT: no shell, and no value that a
// reporter — or any caller — can influence. JournalTail takes no such parameter
// today; this test is the guard that keeps it that way, because the dangerous
// version of this function is the one that grows a `unit` or `filter` argument.
func TestJournalArgs_AreConstantAndShellFree(t *testing.T) {
	got := journalArgs(journalScanLines)
	want := []string{"--user", "-u", "vornik", "-n", "200", "--no-pager"}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %v, want %v", got, want)
		}
	}
	// No shell in the picture: nothing that would re-parse metacharacters.
	for _, a := range got {
		for _, bad := range []string{"-c", "sh", "bash", ";", "|", "&&", "$("} {
			if a == bad || strings.Contains(a, "$(") {
				t.Errorf("argv element %q looks shell-ish: %v", a, got)
			}
		}
	}
}

// review-20260803-6eef CRITICAL follow-up: this package must not acquire a
// daemon-side caller. The boundary is architectural (only vornikctl may spawn
// processes), so it is asserted here as prose-in-code plus the caller audit in
// the file header — and by this test, which fails if the collector ever grows a
// caller-steerable parameter, the shape that would make a mistaken daemon-side
// call exploitable rather than merely wrong.
func TestJournalTail_TakesNoCallerSteerableInput(t *testing.T) {
	// JournalTail's only parameter is a context. If this stops compiling because a
	// unit/filter/path argument was added, STOP: see the file header.
	var f func(context.Context) Check = JournalTail
	_ = f
}
