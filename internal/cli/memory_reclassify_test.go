package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubReclassifyRepo records every call to the two methods so tests
// can assert both behaviour (dry-run skips UPDATE) and arguments.
type stubReclassifyRepo struct {
	mu        sync.Mutex
	counts    map[string]int
	countsErr error
	updates   []reclassifyCall
	updateErr error
}

type reclassifyCall struct {
	projectID string
	class     string
	roles     []string
	ttl       time.Duration
}

func (s *stubReclassifyRepo) CountUnclassifiedByRole(_ context.Context, _ string) (map[string]int, error) {
	if s.countsErr != nil {
		return nil, s.countsErr
	}
	return s.counts, nil
}

func (s *stubReclassifyRepo) ReclassifyUnclassifiedByRoles(_ context.Context, projectID, newClass string, roles []string, ttl time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateErr != nil {
		return 0, s.updateErr
	}
	s.updates = append(s.updates, reclassifyCall{
		projectID: projectID,
		class:     newClass,
		roles:     append([]string(nil), roles...),
		ttl:       ttl,
	})
	// Pretend each role contributed its count.
	total := 0
	for _, r := range roles {
		total += s.counts[r]
	}
	return total, nil
}

// captureStdout pipes a temp file in place of os.Stdout for the
// duration of the test so we can assert on the human-readable
// rendering without dragging zerolog/log capture machinery in.
func captureStdout(t *testing.T) (*os.File, func() string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	return w, func() string {
		_ = w.Close()
		b, _ := io.ReadAll(r)
		return string(b)
	}
}

func TestDoMemoryReclassify_DryRunGroupsByClass(t *testing.T) {
	repo := &stubReclassifyRepo{
		counts: map[string]int{
			"researcher": 3,
			"scout":      2,
			"coder":      7,
			"tester":     1,
		},
	}
	w, read := captureStdout(t)
	err := doMemoryReclassify(context.Background(), repo, "assistant", true, false, w)
	if err != nil {
		t.Fatal(err)
	}
	got := read()
	// Dry-run must NOT have called the update.
	if len(repo.updates) != 0 {
		t.Fatalf("dry-run should not write: %+v", repo.updates)
	}
	// Output should mention all four roles' destination classes.
	for _, want := range []string{
		"would reclassify",
		"assistant",
		"research",   // researcher + scout
		"commit_msg", // coder
		"diagnostic", // tester
		"dry-run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestDoMemoryReclassify_WritesGroupedUpdates(t *testing.T) {
	repo := &stubReclassifyRepo{
		counts: map[string]int{
			"researcher": 4,
			"scout":      1,
		},
	}
	w, read := captureStdout(t)
	err := doMemoryReclassify(context.Background(), repo, "p", false, false, w)
	if err != nil {
		t.Fatal(err)
	}
	_ = read()
	// One UPDATE for the research bucket carrying both roles.
	if len(repo.updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(repo.updates))
	}
	upd := repo.updates[0]
	if upd.class != "research" {
		t.Fatalf("class: %q", upd.class)
	}
	sort.Strings(upd.roles)
	if !reflectStringSlice(upd.roles, []string{"researcher", "scout"}) {
		t.Fatalf("roles: %v", upd.roles)
	}
	// research class has a 90-day TTL → non-zero ttl arg.
	if upd.ttl <= 0 {
		t.Fatalf("research TTL should be > 0, got %v", upd.ttl)
	}
}

func TestDoMemoryReclassify_TracksStuck(t *testing.T) {
	repo := &stubReclassifyRepo{
		counts: map[string]int{
			"":           5, // no role
			"alien-role": 3, // unknown → maps to unclassified
			"researcher": 2, // good
		},
	}
	var buf bytes.Buffer
	w, read := captureStdoutBuffered(t, &buf)
	err := doMemoryReclassify(context.Background(), repo, "p", true, false, w)
	if err != nil {
		t.Fatal(err)
	}
	got := read()
	if !strings.Contains(got, "5 chunks with no producer_role") {
		t.Fatalf("missing no-role count: %s", got)
	}
	if !strings.Contains(got, "3 chunks with an unknown role") {
		t.Fatalf("missing unknown-role count: %s", got)
	}
}

func TestDoMemoryReclassify_EmptyProjectIsNoop(t *testing.T) {
	repo := &stubReclassifyRepo{counts: map[string]int{}}
	w, read := captureStdout(t)
	if err := doMemoryReclassify(context.Background(), repo, "p", false, false, w); err != nil {
		t.Fatal(err)
	}
	got := read()
	if !strings.Contains(got, "no unclassified chunks") {
		t.Fatalf("missing empty-case message: %s", got)
	}
	if len(repo.updates) != 0 {
		t.Fatal("empty case must not write")
	}
}

func TestDoMemoryReclassify_CountErrorPropagates(t *testing.T) {
	repo := &stubReclassifyRepo{countsErr: errors.New("db down")}
	w, _ := captureStdout(t)
	if err := doMemoryReclassify(context.Background(), repo, "p", false, false, w); err == nil {
		t.Fatal("want err")
	}
}

func TestDoMemoryReclassify_UpdateErrorPropagates(t *testing.T) {
	repo := &stubReclassifyRepo{
		counts:    map[string]int{"researcher": 1},
		updateErr: errors.New("disk full"),
	}
	w, _ := captureStdout(t)
	if err := doMemoryReclassify(context.Background(), repo, "p", false, false, w); err == nil {
		t.Fatal("want err")
	}
}

func TestDoMemoryReclassify_JSONOutput(t *testing.T) {
	repo := &stubReclassifyRepo{
		counts: map[string]int{
			"researcher": 2,
			"reviewer":   1,
			"":           1,
		},
	}
	w, read := captureStdout(t)
	if err := doMemoryReclassify(context.Background(), repo, "p", true, true, w); err != nil {
		t.Fatal(err)
	}
	raw := read()
	var got PlanReport
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, raw)
	}
	if got.Project != "p" || !got.DryRun {
		t.Fatalf("bad envelope: %+v", got)
	}
	// Researcher + reviewer = 2 + 1 = 3 reclassified across 2 classes.
	if got.TotalReclassed != 3 {
		t.Fatalf("total: %d", got.TotalReclassed)
	}
	if got.StuckNoRole != 1 {
		t.Fatalf("no-role: %d", got.StuckNoRole)
	}
	if len(got.PerClass) != 2 {
		t.Fatalf("class count: %d", len(got.PerClass))
	}
	// Output is alphabetically ordered.
	if got.PerClass[0].Class > got.PerClass[1].Class {
		t.Fatalf("classes not sorted: %+v", got.PerClass)
	}
}

// TestRunReclassifyFlow_BranchingMatrix exercises every cell of
// the use-llm × llm-only truth table to lock in the
// flag-driven branching. The deterministic pass is "called" iff
// the repo's Count method ran (recorded by stubReclassifyRepo);
// the LLM pass is "called" iff the stub runner saw an invocation.
// --llm-only must skip deterministic regardless of --use-llm.
func TestRunReclassifyFlow_BranchingMatrix(t *testing.T) {
	cases := []struct {
		name          string
		useLLM        bool
		llmOnly       bool
		wantDetCalled bool
		wantLLMCalled bool
	}{
		{name: "neither flag", useLLM: false, llmOnly: false, wantDetCalled: true, wantLLMCalled: false},
		{name: "use-llm only", useLLM: true, llmOnly: false, wantDetCalled: true, wantLLMCalled: true},
		{name: "llm-only", useLLM: false, llmOnly: true, wantDetCalled: false, wantLLMCalled: true},
		{name: "both flags", useLLM: true, llmOnly: true, wantDetCalled: false, wantLLMCalled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &countingReclassifyRepo{counts: map[string]int{"researcher": 1}}
			llmCalled := false
			stubRunner := func(_ string, _, _ bool, _ int, _ *os.File) error {
				llmCalled = true
				return nil
			}
			w, _ := captureStdout(t)
			err := runReclassifyFlow(context.Background(), repo, "p",
				false /*dryRun*/, false /*asJSON*/, tc.useLLM, tc.llmOnly, 10, w, stubRunner)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if repo.countCalls > 0 != tc.wantDetCalled {
				t.Errorf("deterministic pass: got called=%v, want %v", repo.countCalls > 0, tc.wantDetCalled)
			}
			if llmCalled != tc.wantLLMCalled {
				t.Errorf("LLM runner: got called=%v, want %v", llmCalled, tc.wantLLMCalled)
			}
		})
	}
}

// TestRunReclassifyFlow_LLMOnlyAnnouncesSkip — the --llm-only path
// prints an explicit "skipping deterministic pass" line so the
// operator knows their flag was honoured (vs. silently doing
// nothing when no chunks need classifying). Plain English check;
// the exact wording is allowed to evolve.
func TestRunReclassifyFlow_LLMOnlyAnnouncesSkip(t *testing.T) {
	repo := &countingReclassifyRepo{counts: map[string]int{}}
	stubRunner := func(_ string, _, _ bool, _ int, _ *os.File) error { return nil }
	w, read := captureStdout(t)
	err := runReclassifyFlow(context.Background(), repo, "p",
		false, false, false /*useLLM*/, true /*llmOnly*/, 10, w, stubRunner)
	if err != nil {
		t.Fatal(err)
	}
	got := read()
	if !strings.Contains(got, "skipping deterministic pass") {
		t.Fatalf("expected skip announcement in output, got:\n%s", got)
	}
}

// countingReclassifyRepo tracks whether CountUnclassifiedByRole
// was invoked. Distinct from stubReclassifyRepo (which records
// UPDATE arguments) because the flag-branching tests only need
// to know "did the deterministic pass run."
type countingReclassifyRepo struct {
	counts     map[string]int
	countCalls int
}

func (r *countingReclassifyRepo) CountUnclassifiedByRole(_ context.Context, _ string) (map[string]int, error) {
	r.countCalls++
	return r.counts, nil
}

func (r *countingReclassifyRepo) ReclassifyUnclassifiedByRoles(_ context.Context, _, _ string, roles []string, _ time.Duration) (int, error) {
	total := 0
	for _, role := range roles {
		total += r.counts[role]
	}
	return total, nil
}

func reflectStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// captureStdoutBuffered is a variant that lets the test compose
// additional assertions across multiple writes by sharing a backing
// buffer. The non-buffered helper above is sufficient for single-
// shot tests.
func captureStdoutBuffered(t *testing.T, _ *bytes.Buffer) (*os.File, func() string) {
	t.Helper()
	return captureStdout(t)
}
