package projectarchive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// passthroughPatcher is a minimal YAMLPatcher seam stand-in: it
// records the patches it was asked to apply + returns a canned
// rewritten document. The lifecycle service treats the patcher as
// opaque, so tests can assert the *intent* (the PatchOps) without
// re-testing the UI package's surgical YAML rewriter.
type passthroughPatcher struct {
	mu      sync.Mutex
	calls   [][]PatchOp
	out     []byte
	err     error
	gotByID map[string][]byte // content the patcher saw, keyed by input
}

func (p *passthroughPatcher) patch(content []byte, patches []PatchOp) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]PatchOp, len(patches))
	copy(cp, patches)
	p.calls = append(p.calls, cp)
	if p.gotByID == nil {
		p.gotByID = map[string][]byte{}
	}
	p.gotByID[string(content)] = content
	if p.err != nil {
		return nil, p.err
	}
	out := p.out
	if out == nil {
		// Default: echo a marker so the atomic write produces
		// something distinct from the input.
		out = []byte("patched: true\n")
	}
	return out, nil
}

// lastPatch returns the most recent PatchOp slice handed to the
// patcher. Fails the test when nothing was recorded.
func (p *passthroughPatcher) lastPatch(t *testing.T) []PatchOp {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		t.Fatalf("patcher was never called")
	}
	return p.calls[len(p.calls)-1]
}

// findPatch returns the Value (and presence) for a dotted lifecycle
// path within a recorded PatchOp slice.
func findPatch(patches []PatchOp, path ...string) (PatchOp, bool) {
	for _, op := range patches {
		if len(op.Path) != len(path) {
			continue
		}
		match := true
		for i := range path {
			if op.Path[i] != path[i] {
				match = false
				break
			}
		}
		if match {
			return op, true
		}
	}
	return PatchOp{}, false
}

// newServiceFixture builds a LifecycleService backed by a temp
// config dir with the named project YAML present. Returns the
// service and the project's YAML path.
func newServiceFixture(t *testing.T, projectID string, patcher *passthroughPatcher) (*LifecycleService, string) {
	t.Helper()
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "configs")
	if err := os.MkdirAll(filepath.Join(configDir, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlPath := filepath.Join(configDir, "projects", projectID+".yaml")
	if err := os.WriteFile(yamlPath, []byte("projectId: "+projectID+"\n"), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	svc := &LifecycleService{
		ConfigDir: configDir,
		Patcher:   patcher.patch,
	}
	return svc, yamlPath
}

// recordingSweeper captures SweepNow invocations so the delete-now
// kick can be asserted. SweepNow runs in a goroutine in the
// service, so the channel makes the assertion deterministic.
type recordingSweeper struct {
	swept chan struct{}
}

func newRecordingSweeper() *recordingSweeper {
	return &recordingSweeper{swept: make(chan struct{}, 1)}
}

func (r *recordingSweeper) SweepNow(_ context.Context) {
	select {
	case r.swept <- struct{}{}:
	default:
	}
}

// --- ParseGraceDuration ---------------------------------------------------

func TestParseGraceDuration(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"empty defaults", "", DefaultGraceDuration, false},
		{"default keyword", "default", DefaultGraceDuration, false},
		{"default with spaces", "  default  ", DefaultGraceDuration, false},
		{"days suffix", "3d", 3 * 24 * time.Hour, false},
		{"zero days", "0d", 0, false},
		{"go duration", "90m", 90 * time.Minute, false},
		{"go duration hours", "2h", 2 * time.Hour, false},
		{"bad days", "xd", 0, true},
		{"negative days", "-2d", 0, true},
		{"negative duration", "-5m", 0, true},
		{"garbage", "not-a-duration", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseGraceDuration(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseGraceDuration(%q): want error, got nil (val=%v)", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGraceDuration(%q): unexpected error %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("ParseGraceDuration(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// --- checkPrereqs (via Archive surface) -----------------------------------

func TestArchive_NilService(t *testing.T) {
	var s *LifecycleService
	_, err := s.Archive(context.Background(), "p", ArchiveInput{})
	if err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Fatalf("nil service: want 'not wired' error, got %v", err)
	}
}

func TestArchive_MissingConfigDir(t *testing.T) {
	s := &LifecycleService{Patcher: (&passthroughPatcher{}).patch}
	_, err := s.Archive(context.Background(), "p", ArchiveInput{})
	if err == nil || !strings.Contains(err.Error(), "config directory not configured") {
		t.Fatalf("missing config dir: got %v", err)
	}
}

func TestArchive_MissingPatcher(t *testing.T) {
	s := &LifecycleService{ConfigDir: t.TempDir()}
	_, err := s.Archive(context.Background(), "p", ArchiveInput{})
	if err == nil || !strings.Contains(err.Error(), "yaml patcher not wired") {
		t.Fatalf("missing patcher: got %v", err)
	}
}

func TestArchive_InvalidProjectID(t *testing.T) {
	patcher := &passthroughPatcher{}
	svc, _ := newServiceFixture(t, "good", patcher)
	for _, id := range []string{"", "a/b", `a\b`} {
		_, err := svc.Archive(context.Background(), id, ArchiveInput{})
		if err == nil || !strings.Contains(err.Error(), "invalid project id") {
			t.Errorf("id %q: want invalid project id error, got %v", id, err)
		}
	}
}

func TestArchive_ProjectNotFound(t *testing.T) {
	patcher := &passthroughPatcher{}
	svc, _ := newServiceFixture(t, "exists", patcher)
	_, err := svc.Archive(context.Background(), "ghost", ArchiveInput{})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing project: want not found, got %v", err)
	}
}

// --- Archive happy path + grace boundaries --------------------------------

func TestArchive_HappyPath_DefaultGrace(t *testing.T) {
	patcher := &passthroughPatcher{}
	svc, yamlPath := newServiceFixture(t, "proj", patcher)

	before := time.Now().UTC()
	snap, err := svc.Archive(context.Background(), "proj", ArchiveInput{
		Reason:    "  stale  ",
		Principal: "  vadim  ",
	})
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if snap.Status != "archived" {
		t.Errorf("status = %q, want archived", snap.Status)
	}
	// Reason / Principal are trimmed.
	if snap.Reason != "stale" {
		t.Errorf("reason = %q, want trimmed 'stale'", snap.Reason)
	}
	if snap.ArchivedBy != "vadim" {
		t.Errorf("archivedBy = %q, want trimmed 'vadim'", snap.ArchivedBy)
	}
	// Default grace applied: ScheduledDeleteAt ~= ArchivedAt + 7d.
	wantDelete := snap.ArchivedAt.Add(DefaultGraceDuration)
	if !snap.ScheduledDeleteAt.Equal(wantDelete) {
		t.Errorf("scheduledDeleteAt = %v, want %v", snap.ScheduledDeleteAt, wantDelete)
	}
	// ArchivedAt sits within the call window.
	if snap.ArchivedAt.Before(before) || snap.ArchivedAt.After(after) {
		t.Errorf("archivedAt %v not within [%v,%v]", snap.ArchivedAt, before, after)
	}

	// YAML on disk was atomically rewritten with the patched bytes.
	got, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	if string(got) != "patched: true\n" {
		t.Errorf("yaml content = %q, want patched marker", string(got))
	}

	// The patch ops carry the expected lifecycle fields.
	patch := patcher.lastPatch(t)
	if op, ok := findPatch(patch, "lifecycle", "status"); !ok || op.Value != "archived" {
		t.Errorf("status patch missing/wrong: %+v ok=%v", op, ok)
	}
	if op, ok := findPatch(patch, "lifecycle", "reason"); !ok || op.Value != "stale" || !op.RemoveIfEmpty {
		t.Errorf("reason patch missing/wrong: %+v ok=%v", op, ok)
	}
}

func TestArchive_PreservesFileMode(t *testing.T) {
	patcher := &passthroughPatcher{}
	svc, yamlPath := newServiceFixture(t, "proj", patcher)
	// Fixture wrote 0o600; confirm atomicWrite preserves it.
	if _, err := svc.Archive(context.Background(), "proj", ArchiveInput{}); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	info, err := os.Stat(yamlPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600 (atomicWrite must not widen perms)", info.Mode().Perm())
	}
}

func TestArchive_GraceBelowMin(t *testing.T) {
	patcher := &passthroughPatcher{}
	svc, _ := newServiceFixture(t, "proj", patcher)
	_, err := svc.Archive(context.Background(), "proj", ArchiveInput{Grace: 10 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "at least") {
		t.Fatalf("below-min grace: want min error, got %v", err)
	}
	// Boundary: exactly MinGraceDuration is allowed.
	if _, err := svc.Archive(context.Background(), "proj", ArchiveInput{Grace: MinGraceDuration}); err != nil {
		t.Fatalf("exactly MinGraceDuration should be allowed: %v", err)
	}
}

func TestArchive_GraceAboveMax(t *testing.T) {
	patcher := &passthroughPatcher{}
	svc, _ := newServiceFixture(t, "proj", patcher)
	_, err := svc.Archive(context.Background(), "proj", ArchiveInput{Grace: MaxGraceDuration + time.Hour})
	if err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("above-max grace: want maximum error, got %v", err)
	}
	// Boundary: exactly MaxGraceDuration is allowed.
	if _, err := svc.Archive(context.Background(), "proj", ArchiveInput{Grace: MaxGraceDuration}); err != nil {
		t.Fatalf("exactly MaxGraceDuration should be allowed: %v", err)
	}
}

func TestArchive_PatcherError(t *testing.T) {
	patcher := &passthroughPatcher{err: errors.New("boom")}
	svc, _ := newServiceFixture(t, "proj", patcher)
	_, err := svc.Archive(context.Background(), "proj", ArchiveInput{})
	if err == nil || !strings.Contains(err.Error(), "archive:") {
		t.Fatalf("patcher error: want wrapped 'archive:' error, got %v", err)
	}
}

func TestArchive_ReloadError(t *testing.T) {
	patcher := &passthroughPatcher{}
	svc, _ := newServiceFixture(t, "proj", patcher)
	svc.Reload = func(_ context.Context) error { return errors.New("reload-failed") }
	_, err := svc.Archive(context.Background(), "proj", ArchiveInput{})
	if err == nil || !strings.Contains(err.Error(), "archive saved but reload failed") {
		t.Fatalf("reload error: got %v", err)
	}
}

func TestArchive_ReloadInvoked(t *testing.T) {
	patcher := &passthroughPatcher{}
	svc, _ := newServiceFixture(t, "proj", patcher)
	called := 0
	svc.Reload = func(_ context.Context) error { called++; return nil }
	if _, err := svc.Archive(context.Background(), "proj", ArchiveInput{}); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if called != 1 {
		t.Errorf("reload called %d times, want 1", called)
	}
}

// --- Unarchive ------------------------------------------------------------

func TestUnarchive_HappyPath(t *testing.T) {
	patcher := &passthroughPatcher{}
	svc, yamlPath := newServiceFixture(t, "proj", patcher)

	if err := svc.Unarchive(context.Background(), "proj"); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	// All five lifecycle keys cleared with RemoveIfEmpty.
	patch := patcher.lastPatch(t)
	for _, key := range []string{"status", "archivedAt", "scheduledDeleteAt", "reason", "archivedBy"} {
		op, ok := findPatch(patch, "lifecycle", key)
		if !ok {
			t.Errorf("unarchive missing patch for %q", key)
			continue
		}
		if op.Value != "" || !op.RemoveIfEmpty {
			t.Errorf("unarchive %q: want empty+RemoveIfEmpty, got %+v", key, op)
		}
	}
	got, _ := os.ReadFile(yamlPath)
	if string(got) != "patched: true\n" {
		t.Errorf("yaml not rewritten: %q", string(got))
	}
}

func TestUnarchive_PrereqError(t *testing.T) {
	svc := &LifecycleService{ConfigDir: t.TempDir()} // no patcher
	if err := svc.Unarchive(context.Background(), "p"); err == nil {
		t.Fatal("expected prereq error")
	}
}

func TestUnarchive_PatcherError(t *testing.T) {
	patcher := &passthroughPatcher{err: errors.New("boom")}
	svc, _ := newServiceFixture(t, "proj", patcher)
	if err := svc.Unarchive(context.Background(), "proj"); err == nil || !strings.Contains(err.Error(), "unarchive:") {
		t.Fatalf("want wrapped 'unarchive:' error, got %v", err)
	}
}

func TestUnarchive_ReloadError(t *testing.T) {
	patcher := &passthroughPatcher{}
	svc, _ := newServiceFixture(t, "proj", patcher)
	svc.Reload = func(_ context.Context) error { return errors.New("x") }
	if err := svc.Unarchive(context.Background(), "proj"); err == nil || !strings.Contains(err.Error(), "unarchive saved but reload failed") {
		t.Fatalf("want reload-failed wrap, got %v", err)
	}
}

// --- ScheduleDeleteNow ----------------------------------------------------

func TestScheduleDeleteNow_RequiresArchived(t *testing.T) {
	patcher := &passthroughPatcher{}
	svc, _ := newServiceFixture(t, "proj", patcher)
	err := svc.ScheduleDeleteNow(context.Background(), "proj", false)
	if err == nil || !strings.Contains(err.Error(), "archived first") {
		t.Fatalf("non-archived: want 'archived first' error, got %v", err)
	}
	// Patcher must not have run when refused.
	if len(patcher.calls) != 0 {
		t.Errorf("patcher should not run on refusal; got %d calls", len(patcher.calls))
	}
}

func TestScheduleDeleteNow_PrereqError(t *testing.T) {
	svc := &LifecycleService{ConfigDir: t.TempDir()} // no patcher
	if err := svc.ScheduleDeleteNow(context.Background(), "p", true); err == nil {
		t.Fatal("expected prereq error")
	}
}

func TestScheduleDeleteNow_HappyPath_KicksSweeper(t *testing.T) {
	patcher := &passthroughPatcher{}
	svc, _ := newServiceFixture(t, "proj", patcher)
	sweeper := newRecordingSweeper()
	svc.Sweeper = sweeper
	reloadCalled := 0
	svc.Reload = func(_ context.Context) error { reloadCalled++; return nil }

	before := time.Now().UTC()
	if err := svc.ScheduleDeleteNow(context.Background(), "proj", true); err != nil {
		t.Fatalf("ScheduleDeleteNow: %v", err)
	}
	after := time.Now().UTC()

	// scheduledDeleteAt patch set to ~now.
	op, ok := findPatch(patcher.lastPatch(t), "lifecycle", "scheduledDeleteAt")
	if !ok {
		t.Fatalf("scheduledDeleteAt patch missing")
	}
	ts, err := time.Parse(time.RFC3339, op.Value.(string))
	if err != nil {
		t.Fatalf("parse scheduledDeleteAt %q: %v", op.Value, err)
	}
	if ts.Before(before.Truncate(time.Second)) || ts.After(after.Add(time.Second)) {
		t.Errorf("scheduledDeleteAt %v not within call window [%v,%v]", ts, before, after)
	}
	if reloadCalled != 1 {
		t.Errorf("reload called %d times, want 1", reloadCalled)
	}

	// Sweeper.SweepNow runs in a goroutine; wait for the kick.
	select {
	case <-sweeper.swept:
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper was not kicked within timeout")
	}
}

func TestScheduleDeleteNow_NilSweeperIsSafe(t *testing.T) {
	patcher := &passthroughPatcher{}
	svc, _ := newServiceFixture(t, "proj", patcher)
	// No sweeper wired — must not panic.
	if err := svc.ScheduleDeleteNow(context.Background(), "proj", true); err != nil {
		t.Fatalf("ScheduleDeleteNow with nil sweeper: %v", err)
	}
}

func TestScheduleDeleteNow_PatcherError(t *testing.T) {
	patcher := &passthroughPatcher{err: errors.New("boom")}
	svc, _ := newServiceFixture(t, "proj", patcher)
	if err := svc.ScheduleDeleteNow(context.Background(), "proj", true); err == nil || !strings.Contains(err.Error(), "delete-now:") {
		t.Fatalf("want wrapped 'delete-now:' error, got %v", err)
	}
}

func TestScheduleDeleteNow_ReloadError(t *testing.T) {
	patcher := &passthroughPatcher{}
	svc, _ := newServiceFixture(t, "proj", patcher)
	svc.Reload = func(_ context.Context) error { return errors.New("x") }
	if err := svc.ScheduleDeleteNow(context.Background(), "proj", true); err == nil || !strings.Contains(err.Error(), "delete-now saved but reload failed") {
		t.Fatalf("want reload-failed wrap, got %v", err)
	}
}

// --- atomicWrite edge cases -----------------------------------------------

func TestAtomicWrite_NewFileDefaultMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh.yaml")
	if err := atomicWrite(path, []byte("hi\n")); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// No pre-existing file => default 0o600.
	if info.Mode().Perm() != 0o600 {
		t.Errorf("new file mode = %o, want 600", info.Mode().Perm())
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hi\n" {
		t.Errorf("content = %q", string(got))
	}
}

func TestAtomicWrite_TempCreateFails(t *testing.T) {
	// A non-existent parent directory makes CreateTemp fail.
	path := filepath.Join(t.TempDir(), "no-such-dir", "x.yaml")
	err := atomicWrite(path, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "create temp") {
		t.Fatalf("want 'create temp' error, got %v", err)
	}
}

func TestApplyPatches_ReadError(t *testing.T) {
	// checkPrereqs passes (file exists) but we delete it before
	// applyPatches reads — exercises the read-error branch via the
	// public Archive surface is awkward, so call applyPatches
	// directly against a missing file.
	patcher := &passthroughPatcher{}
	svc, yamlPath := newServiceFixture(t, "proj", patcher)
	if err := os.Remove(yamlPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := svc.applyPatches("proj", nil); err == nil || !strings.Contains(err.Error(), "read project yaml") {
		t.Fatalf("want 'read project yaml' error, got %v", err)
	}
}
