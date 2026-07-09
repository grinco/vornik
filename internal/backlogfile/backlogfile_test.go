package backlogfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func tempBacklogPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "BACKLOG.md")
}

func TestAppend_CreatesFileWithHeaderAndItem(t *testing.T) {
	path := tempBacklogPath(t)
	s := NewStore()

	if err := s.Append(path, "proj1", "Do the thing"); err != nil {
		t.Fatalf("Append: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(data)
	if !strings.HasPrefix(got, FormatHeader+"\n") {
		t.Errorf("expected file to start with header, got:\n%s", got)
	}
	if !strings.Contains(got, "- [?] Do the thing\n") {
		t.Errorf("expected proposed item line, got:\n%s", got)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 0600", perm)
	}
}

func TestAppend_PreservesExistingContent(t *testing.T) {
	path := tempBacklogPath(t)
	initial := FormatHeader + "\n\n# Backlog\n\n- [x] already done\n- [ ] pending thing\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("seed WriteFile: %v", err)
	}

	s := NewStore()
	if err := s.Append(path, "proj1", "New proposal"); err != nil {
		t.Fatalf("Append: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "- [x] already done\n") {
		t.Errorf("existing done item lost:\n%s", got)
	}
	if !strings.Contains(got, "- [ ] pending thing\n") {
		t.Errorf("existing pending item lost:\n%s", got)
	}
	if !strings.HasSuffix(got, "- [?] New proposal\n") {
		t.Errorf("new item not appended at end:\n%s", got)
	}
}

func TestAppend_RejectsNewlineInItemText(t *testing.T) {
	path := tempBacklogPath(t)
	s := NewStore()

	err := s.Append(path, "proj1", "line one\nline two")
	if err == nil {
		t.Fatal("expected error for multi-line item text, got nil")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("file should not have been created on rejected Append")
	}
}

func TestItems_ParsesAllFourMarkers(t *testing.T) {
	path := tempBacklogPath(t)
	content := FormatHeader + "\n\n" +
		"- [ ] pending one\n" +
		"- [?] proposed one\n" +
		"- [x] done one\n" +
		"- [!] failed one\n" +
		"prose line, not an item\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed WriteFile: %v", err)
	}

	s := NewStore()
	items, err := s.Items(path, "proj1")
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4: %+v", len(items), items)
	}
	want := []Item{
		{Marker: ' ', Text: "pending one", Line: 2},
		{Marker: '?', Text: "proposed one", Line: 3},
		{Marker: 'x', Text: "done one", Line: 4},
		{Marker: '!', Text: "failed one", Line: 5},
	}
	for i, w := range want {
		if items[i] != w {
			t.Errorf("item[%d] = %+v, want %+v", i, items[i], w)
		}
	}
}

func TestItems_MissingFileReturnsNilNil(t *testing.T) {
	path := tempBacklogPath(t)
	s := NewStore()
	items, err := s.Items(path, "proj1")
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if items != nil {
		t.Errorf("items = %+v, want nil", items)
	}
}

func TestPeekNext_SkipsNonPendingMarkers(t *testing.T) {
	path := tempBacklogPath(t)
	content := "- [?] proposed\n" +
		"- [x] done\n" +
		"- [!] failed\n" +
		"- [ ] take this one\n" +
		"- [ ] and not this one\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed WriteFile: %v", err)
	}

	s := NewStore()
	text, ok, err := s.PeekNext(path, "proj1")
	if err != nil {
		t.Fatalf("PeekNext: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if text != "take this one" {
		t.Errorf("text = %q, want %q", text, "take this one")
	}
}

func TestPeekNext_NoneReturnsFalse(t *testing.T) {
	path := tempBacklogPath(t)
	content := "- [?] proposed\n- [x] done\n- [!] failed\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed WriteFile: %v", err)
	}

	s := NewStore()
	text, ok, err := s.PeekNext(path, "proj1")
	if err != nil {
		t.Fatalf("PeekNext: %v", err)
	}
	if ok {
		t.Errorf("expected ok=false, got text=%q", text)
	}
}

func TestMarkConsumed_ExactTextMatchAppendsTaskAnnotation(t *testing.T) {
	path := tempBacklogPath(t)
	content := "- [ ] first item\n- [ ] second item\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed WriteFile: %v", err)
	}

	s := NewStore()
	ok, err := s.MarkConsumed(path, "proj1", "second item", "task_123")
	if err != nil {
		t.Fatalf("MarkConsumed: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(data)
	want := "- [ ] first item\n- [x] second item (task: task_123)\n"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestMarkConsumed_ReturnsFalseWhenAlreadyFlipped(t *testing.T) {
	path := tempBacklogPath(t)
	// Operator (or a prior call) already marked this line done; the
	// marker is no longer ' ' so a stale in-memory "next item" must
	// not be double-consumed.
	content := "- [x] already flipped\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed WriteFile: %v", err)
	}

	s := NewStore()
	ok, err := s.MarkConsumed(path, "proj1", "already flipped", "task_123")
	if err != nil {
		t.Fatalf("MarkConsumed: %v", err)
	}
	if ok {
		t.Error("expected ok=false for a line whose marker is no longer pending")
	}
}

func TestMarkConsumed_NoMatchReturnsFalse(t *testing.T) {
	path := tempBacklogPath(t)
	content := "- [ ] some item\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed WriteFile: %v", err)
	}

	s := NewStore()
	ok, err := s.MarkConsumed(path, "proj1", "nonexistent item", "task_123")
	if err != nil {
		t.Fatalf("MarkConsumed: %v", err)
	}
	if ok {
		t.Error("expected ok=false when no line matches")
	}
}

func TestMarkFailed_FlipsOnlyTaskLineAndIsIdempotent(t *testing.T) {
	path := tempBacklogPath(t)
	content := "- [x] unrelated done item\n" +
		"- [x] second item (task: task_123)\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed WriteFile: %v", err)
	}

	s := NewStore()
	ok, err := s.MarkFailed(path, "proj1", "task_123")
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(data)
	want := "- [x] unrelated done item\n" +
		"- [!] second item (task: task_123, failed)\n"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}

	// Second call must be a no-op: the line's marker is now '!', not
	// 'x', so the search for "(task: task_123)" on an 'x' line fails.
	ok2, err := s.MarkFailed(path, "proj1", "task_123")
	if err != nil {
		t.Fatalf("MarkFailed (second call): %v", err)
	}
	if ok2 {
		t.Error("expected ok=false on second MarkFailed call (idempotency)")
	}

	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile (second): %v", err)
	}
	if string(data2) != got {
		t.Errorf("file mutated on idempotent second call:\ngot:\n%q\nwant unchanged:\n%q", string(data2), got)
	}
}

func TestMarkFailed_NoMatchingTaskReturnsFalse(t *testing.T) {
	path := tempBacklogPath(t)
	content := "- [x] item (task: other_task)\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed WriteFile: %v", err)
	}

	s := NewStore()
	ok, err := s.MarkFailed(path, "proj1", "task_123")
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if ok {
		t.Error("expected ok=false when no line references the task")
	}
}

// TestConcurrentAppendAndMarkConsumed hammers the same project's
// BACKLOG.md with 50 concurrent Append calls (adding new proposed
// items) interleaved with 50 concurrent MarkConsumed calls (each
// consuming a distinct pre-seeded pending item), and asserts no
// line is lost or corrupted by the interleaving. Run with
// `go test -race` to also catch data races on the per-project mutex
// bookkeeping.
func TestConcurrentAppendAndMarkConsumed(t *testing.T) {
	path := tempBacklogPath(t)
	const n = 50

	// Seed n pending items up front (single-threaded) so the
	// concurrent MarkConsumed calls below each have a distinct,
	// guaranteed-present target — every one of them MUST succeed
	// since no two goroutines target the same line.
	var seedLines []string
	for i := 0; i < n; i++ {
		seedLines = append(seedLines, fmt.Sprintf("- [ ] item %d", i))
	}
	seed := FormatHeader + "\n\n" + strings.Join(seedLines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed WriteFile: %v", err)
	}

	s := NewStore()
	var wg sync.WaitGroup
	wg.Add(n * 2)
	consumedOK := make([]bool, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			extra := fmt.Sprintf("extra %d", i)
			if err := s.Append(path, "proj1", extra); err != nil {
				t.Errorf("Append(%d): %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			text := fmt.Sprintf("item %d", i)
			ok, err := s.MarkConsumed(path, "proj1", text, fmt.Sprintf("task_%d", i))
			if err != nil {
				t.Errorf("MarkConsumed(%d): %v", i, err)
			}
			consumedOK[i] = ok
		}()
	}
	wg.Wait()

	for i, ok := range consumedOK {
		if !ok {
			t.Errorf("MarkConsumed(%d) returned false, want true (line lost under concurrency)", i)
		}
	}

	items, err := s.Items(path, "proj1")
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(items) != n*2 {
		t.Fatalf("got %d items, want %d (lines lost or corrupted)", len(items), n*2)
	}

	seenPending := make(map[string]bool, n)
	seenExtra := make(map[string]bool, n)
	for _, it := range items {
		switch {
		case it.Marker == 'x' && strings.HasPrefix(it.Text, "item "):
			seenPending[it.Text] = true
		case it.Marker == '?' && strings.HasPrefix(it.Text, "extra "):
			seenExtra[it.Text] = true
		default:
			t.Errorf("unexpected item after concurrent run: marker=%q text=%q", it.Marker, it.Text)
		}
	}
	for i := 0; i < n; i++ {
		wantConsumed := fmt.Sprintf("item %d (task: task_%d)", i, i)
		if !seenPending[wantConsumed] {
			t.Errorf("missing consumed annotation for %q", wantConsumed)
		}
		wantExtra := fmt.Sprintf("extra %d", i)
		if !seenExtra[wantExtra] {
			t.Errorf("missing appended item %q", wantExtra)
		}
	}
}

func TestStore_IndependentMutexesPerProject(t *testing.T) {
	// Sanity check that two distinct projects don't share a lock
	// object (would otherwise serialize unrelated projects'
	// writers for no reason). Not a correctness requirement per se,
	// but pins the "per project" contract.
	s := NewStore()
	path1 := tempBacklogPath(t)
	path2 := tempBacklogPath(t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = s.Append(path1, "projA", "a-item")
	}()
	go func() {
		defer wg.Done()
		_ = s.Append(path2, "projB", "b-item")
	}()
	wg.Wait()

	items1, err := s.Items(path1, "projA")
	if err != nil {
		t.Fatalf("Items(projA): %v", err)
	}
	items2, err := s.Items(path2, "projB")
	if err != nil {
		t.Fatalf("Items(projB): %v", err)
	}
	if len(items1) != 1 || len(items2) != 1 {
		t.Fatalf("expected one item per project, got %d and %d", len(items1), len(items2))
	}
}
