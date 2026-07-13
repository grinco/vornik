package backlogfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBacklog writes content to a temp BACKLOG.md and returns its path.
func writeBacklog(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "BACKLOG.md")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func readBacklog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

// TestMarkInFlight_StampsTildeNotDone — dispatch stamps the in-flight marker
// `[~]` (NOT `[x]`), so a task that fails before the next reconcile is never
// misread as done (LLD §5). The task annotation is appended like MarkConsumed.
func TestMarkInFlight_StampsTildeNotDone(t *testing.T) {
	s := NewStore()
	p := writeBacklog(t, "- [ ] implement the thing\n")
	ok, err := s.MarkInFlight(p, "proj", "implement the thing", "task_123")
	if err != nil || !ok {
		t.Fatalf("MarkInFlight: ok=%v err=%v", ok, err)
	}
	got := readBacklog(t, p)
	if !strings.Contains(got, "- [~] implement the thing (task: task_123)") {
		t.Fatalf("expected [~] in-flight stamp, got:\n%s", got)
	}
	if strings.Contains(got, "[x]") {
		t.Fatalf("dispatch must NOT stamp [x], got:\n%s", got)
	}
}

// TestMarkInFlight_NoMatchReturnsFalse — no matching pending line → (false,nil).
func TestMarkInFlight_NoMatchReturnsFalse(t *testing.T) {
	s := NewStore()
	p := writeBacklog(t, "- [ ] some other item\n")
	ok, err := s.MarkInFlight(p, "proj", "nonexistent", "task_1")
	if err != nil || ok {
		t.Fatalf("expected (false,nil), got ok=%v err=%v", ok, err)
	}
}

// TestMarkDone_FlipsInFlightToDone — the reconciler flips `[~]` → `[x]` when
// the task reached a successful terminal (LLD §5). Idempotent: a second call
// no longer matches (marker is 'x').
func TestMarkDone_FlipsInFlightToDone(t *testing.T) {
	s := NewStore()
	p := writeBacklog(t, "- [~] ship it (task: task_9)\n")
	ok, err := s.MarkDone(p, "proj", "task_9")
	if err != nil || !ok {
		t.Fatalf("MarkDone: ok=%v err=%v", ok, err)
	}
	got := readBacklog(t, p)
	if !strings.Contains(got, "- [x] ship it (task: task_9)") {
		t.Fatalf("expected [x] done, got:\n%s", got)
	}
	// Idempotent — second call is a no-op.
	ok2, _ := s.MarkDone(p, "proj", "task_9")
	if ok2 {
		t.Fatalf("MarkDone should be idempotent; second call matched")
	}
}

// TestMarkDone_WrongTaskIsNoOp — MarkDone only touches the line carrying the
// given task annotation.
func TestMarkDone_WrongTaskIsNoOp(t *testing.T) {
	s := NewStore()
	p := writeBacklog(t, "- [~] a (task: task_A)\n- [~] b (task: task_B)\n")
	if ok, _ := s.MarkDone(p, "proj", "task_B"); !ok {
		t.Fatal("expected task_B to flip")
	}
	got := readBacklog(t, p)
	if !strings.Contains(got, "- [~] a (task: task_A)") {
		t.Fatalf("task_A must be untouched, got:\n%s", got)
	}
	if !strings.Contains(got, "- [x] b (task: task_B)") {
		t.Fatalf("task_B must be done, got:\n%s", got)
	}
}

// TestMarkFailed_MatchesBothInFlightAndDone — MarkFailed flips BOTH `[~]` (new
// in-flight path) and `[x]` (pre-deploy stranded optimistic stamp) to `[!]`
// (LLD §5 back-compat). Idempotent.
func TestMarkFailed_MatchesBothInFlightAndDone(t *testing.T) {
	for _, marker := range []string{"~", "x"} {
		s := NewStore()
		p := writeBacklog(t, "- ["+marker+"] task text (task: task_7)\n")
		ok, err := s.MarkFailed(p, "proj", "task_7")
		if err != nil || !ok {
			t.Fatalf("marker %s: MarkFailed ok=%v err=%v", marker, ok, err)
		}
		got := readBacklog(t, p)
		if !strings.Contains(got, "- [!] task text (task: task_7, failed)") {
			t.Fatalf("marker %s: expected [!] failed, got:\n%s", marker, got)
		}
		// Idempotent.
		if ok2, _ := s.MarkFailed(p, "proj", "task_7"); ok2 {
			t.Fatalf("marker %s: MarkFailed should be idempotent", marker)
		}
	}
}

// TestItems_RecognisesInFlightMarker — the parser recognises `[~]` as a
// distinct marker (itemRE widened to `[ ?x!~]`).
func TestItems_RecognisesInFlightMarker(t *testing.T) {
	s := NewStore()
	p := writeBacklog(t, "- [~] running (task: t1)\n- [ ] pending\n")
	items, err := s.Items(p, "proj")
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	var sawTilde bool
	for _, it := range items {
		if it.Marker == '~' {
			sawTilde = true
		}
	}
	if !sawTilde {
		t.Fatalf("parser must recognise [~], got items: %+v", items)
	}
}

// TestPeekNext_SkipsInFlight — an in-flight `[~]` item is NOT consumable;
// PeekNext skips it and returns the next `[ ]` (the file-level interlock that
// survives daemon restart — LLD §5 I5).
func TestPeekNext_SkipsInFlight(t *testing.T) {
	s := NewStore()
	p := writeBacklog(t, "- [~] running (task: t1)\n- [ ] next up\n")
	text, ok, err := s.PeekNext(p, "proj")
	if err != nil || !ok {
		t.Fatalf("PeekNext: ok=%v err=%v", ok, err)
	}
	if text != "next up" {
		t.Fatalf("PeekNext must skip [~] and return the pending item, got %q", text)
	}
}

// TestFormatHeader_DocumentsInFlight — the self-documenting header lists the
// new `[~]` marker.
func TestFormatHeader_DocumentsInFlight(t *testing.T) {
	if !strings.Contains(FormatHeader, "[~]") {
		t.Fatalf("FormatHeader must document the [~] in-flight marker: %q", FormatHeader)
	}
}
