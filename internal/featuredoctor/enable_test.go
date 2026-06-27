package featuredoctor

import (
	"context"
	"errors"
	"os"
	"testing"
)

// stubTasks implements TaskLister for tests.
type stubTasks struct {
	active bool
	err    error
}

func (s stubTasks) HasActiveTasks(_ context.Context) (bool, error) { return s.active, s.err }

// fakeConfigWriter records calls and optionally injects errors.
type fakeConfigWriter struct {
	content    []byte
	backupPath string

	readErr     error
	writeErr    error
	backupErr   error
	restoreErr  error
	validateErr error

	writeCalled    bool
	backupCalled   bool
	restoreCalled  bool
	validateCalled bool
	restoreTarget  string
}

func (f *fakeConfigWriter) Read() ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.content, nil
}

func (f *fakeConfigWriter) Write(data []byte) error {
	f.writeCalled = true
	if f.writeErr != nil {
		return f.writeErr
	}
	f.content = data
	return nil
}

func (f *fakeConfigWriter) Backup() (string, error) {
	f.backupCalled = true
	if f.backupErr != nil {
		return "", f.backupErr
	}
	return f.backupPath, nil
}

func (f *fakeConfigWriter) Restore(backup string) error {
	f.restoreCalled = true
	f.restoreTarget = backup
	return f.restoreErr
}

func (f *fakeConfigWriter) Validate() error {
	f.validateCalled = true
	return f.validateErr
}

// fakeReloader records calls and optionally injects an error.
type fakeReloader struct {
	err       error
	called    bool
	callCount int
}

func (r *fakeReloader) Reload(_ context.Context) error {
	r.called = true
	r.callCount++
	return r.err
}

// --- PlanEnable tests ---

func TestPlanEnable_StopsOnUnfixablePrereq(t *testing.T) {
	// authFeature prereq: admin-key file absent (unfixable, because the operator
	// must create it manually). Pass a temp dir with no admin-key.txt.
	f := authFeature()
	deps := Deps{
		Config:     stubConfig{vals: map[string]any{"api.auth_enabled": false}},
		SecretsDir: t.TempDir(),
	}
	plan, err := PlanEnable(context.Background(), f, deps)
	if err == nil {
		t.Fatal("expected hard stop: admin key missing (unfixable)")
	}
	if plan != nil {
		t.Fatal("no plan when an unfixable prereq is unmet")
	}
}

func TestPlanEnable_RestartRequiredAndBusyRefuses(t *testing.T) {
	f := Feature{ID: "x", Apply: RestartRequired, Gates: []Gate{{Key: "k", EnableTo: true}}}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": false}}, Tasks: stubTasks{active: true}}
	_, err := PlanEnable(context.Background(), f, deps)
	if err == nil {
		t.Fatal("restart-required + active tasks must refuse")
	}
}

func TestPlanEnable_RestartRequiredTaskErrorRefuses(t *testing.T) {
	// If HasActiveTasks returns an error we must also refuse.
	f := Feature{ID: "x", Apply: RestartRequired, Gates: []Gate{{Key: "k", EnableTo: true}}}
	deps := Deps{
		Config: stubConfig{vals: map[string]any{"k": false}},
		Tasks:  stubTasks{active: false, err: errors.New("db down")},
	}
	_, err := PlanEnable(context.Background(), f, deps)
	if err == nil {
		t.Fatal("HasActiveTasks error must refuse")
	}
}

func TestPlanEnable_RestartRequiredIdleOK(t *testing.T) {
	// RestartRequired but no active tasks → should produce a plan.
	f := Feature{ID: "x", Apply: RestartRequired, Gates: []Gate{{Key: "k", EnableTo: true}}}
	deps := Deps{
		Config: stubConfig{vals: map[string]any{"k": false}},
		Tasks:  stubTasks{active: false},
	}
	plan, err := PlanEnable(context.Background(), f, deps)
	if err != nil {
		t.Fatalf("idle system must be allowed: %v", err)
	}
	if len(plan.Changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(plan.Changes))
	}
}

func TestPlanEnable_ProducesGateDiff(t *testing.T) {
	f := instinctFeature()
	// Instinct prereqs: "distill model reachable" and "instinct.enabled set"
	// stubModels.reachable=true satisfies the model prereq.
	// "instinct.enabled set" is fixable (it's a gate), so PlanEnable should proceed.
	deps := Deps{
		Config: stubConfig{vals: map[string]any{
			"instinct.enabled":                          false,
			"instinct.model":                            "",
			"instinct.consumers.application_feedback":   false,
			"instinct.consumers.execution_step_outcome": false,
			"instinct.consumers.workflow_heal":          false,
		}},
		Models: stubModels{reachable: true},
	}
	plan, err := PlanEnable(context.Background(), f, deps)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected gate changes in plan")
	}
}

func TestPlanEnable_NoChangesWhenGatesAlreadyOn(t *testing.T) {
	// A feature whose single gate is already at EnableTo → zero changes.
	f := Feature{
		ID:    "already-on",
		Apply: ReloadHot,
		Gates: []Gate{{Key: "k", EnableTo: true}},
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": true}}}
	plan, err := PlanEnable(context.Background(), f, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("want 0 changes when gates already on, got %d", len(plan.Changes))
	}
}

func TestPlanEnable_ReloadHotNilTasksOK(t *testing.T) {
	// ReloadHot features do not consult TaskLister even when nil.
	f := Feature{
		ID:    "r",
		Apply: ReloadHot,
		Gates: []Gate{{Key: "k", EnableTo: true}},
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": false}}, Tasks: nil}
	plan, err := PlanEnable(context.Background(), f, deps)
	if err != nil {
		t.Fatalf("ReloadHot with nil tasks must succeed: %v", err)
	}
	if len(plan.Changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(plan.Changes))
	}
}

// --- ApplyEnable tests ---

// minimalYAML is a valid YAML document with the key "k".
const minimalYAML = "k: false\n"

func TestApplyEnable_RollbackOnReloaderError(t *testing.T) {
	// writer.Read returns a valid YAML, writer.Write succeeds,
	// but reloader.Reload returns an error → Restore must be called.
	f := Feature{
		ID:    "r",
		Apply: ReloadHot,
		Gates: []Gate{{Key: "k", EnableTo: true}},
		Verify: func(_ context.Context, _ Deps) PrereqResult {
			return PrereqResult{OK: true}
		},
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": false}}}

	plan := &EnablePlan{
		Changes: []GateChange{{Key: "k", From: false, To: true}},
		Apply:   ReloadHot,
	}

	writer := &fakeConfigWriter{
		content:    []byte(minimalYAML),
		backupPath: "config.yaml.20240101T000000Z.bak",
	}
	reloader := &fakeReloader{err: errors.New("reload failed")}

	_, err := ApplyEnable(context.Background(), f, deps, plan, writer, reloader)
	if err == nil {
		t.Fatal("expected error from failed reload")
	}
	if !writer.restoreCalled {
		t.Fatal("Restore must be called when reload fails")
	}
	if writer.restoreTarget != writer.backupPath {
		t.Errorf("Restore called with %q, want %q", writer.restoreTarget, writer.backupPath)
	}
	// fix(featuredoctor): rollback must re-reload — Reload is called once on the
	// initial attempt (which fails here) and once more (best-effort) after Restore
	// succeeds to re-sync the daemon to the restored config file.
	if reloader.callCount != 2 {
		t.Errorf("want Reload called 2 times (initial + post-restore re-sync), got %d", reloader.callCount)
	}
}

func TestApplyEnable_RollbackOnWriteError(t *testing.T) {
	f := Feature{
		ID:    "r",
		Apply: ReloadHot,
		Gates: []Gate{{Key: "k", EnableTo: true}},
		Verify: func(_ context.Context, _ Deps) PrereqResult {
			return PrereqResult{OK: true}
		},
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": false}}}

	plan := &EnablePlan{
		Changes: []GateChange{{Key: "k", From: false, To: true}},
		Apply:   ReloadHot,
	}

	writer := &fakeConfigWriter{
		content:    []byte(minimalYAML),
		backupPath: "config.yaml.bak",
		writeErr:   errors.New("disk full"),
	}
	reloader := &fakeReloader{}

	_, err := ApplyEnable(context.Background(), f, deps, plan, writer, reloader)
	if err == nil {
		t.Fatal("expected error from failed write")
	}
	if !writer.restoreCalled {
		t.Fatal("Restore must be called when write fails")
	}
	if reloader.called {
		t.Fatal("reloader must not be called when write fails")
	}
}

func TestApplyEnable_RollbackOnVerifyFail(t *testing.T) {
	f := Feature{
		ID:    "r",
		Apply: ReloadHot,
		Gates: []Gate{{Key: "k", EnableTo: true}},
		Verify: func(_ context.Context, _ Deps) PrereqResult {
			return PrereqResult{OK: false, Detail: "still broken"}
		},
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": false}}}

	plan := &EnablePlan{
		Changes: []GateChange{{Key: "k", From: false, To: true}},
		Apply:   ReloadHot,
	}

	writer := &fakeConfigWriter{
		content:    []byte(minimalYAML),
		backupPath: "config.yaml.bak",
	}
	reloader := &fakeReloader{}

	_, err := ApplyEnable(context.Background(), f, deps, plan, writer, reloader)
	if err == nil {
		t.Fatal("expected error when verify fails")
	}
	if !writer.restoreCalled {
		t.Fatal("Restore must be called when verify fails")
	}
}

func TestApplyEnable_NoChanges_SkipsWriteAndReload(t *testing.T) {
	f := Feature{
		ID:    "r",
		Apply: ReloadHot,
		Gates: []Gate{{Key: "k", EnableTo: true}},
		Verify: func(_ context.Context, _ Deps) PrereqResult {
			return PrereqResult{OK: true}
		},
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": true}}}

	plan := &EnablePlan{Changes: nil, Apply: ReloadHot}

	writer := &fakeConfigWriter{
		content:    []byte(minimalYAML),
		backupPath: "config.yaml.bak",
	}
	reloader := &fakeReloader{}

	result, err := ApplyEnable(context.Background(), f, deps, plan, writer, reloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.OK {
		t.Fatalf("verify must pass, got %+v", result)
	}
	if writer.writeCalled {
		t.Error("Write must not be called when there are no changes")
	}
	if reloader.called {
		t.Error("Reload must not be called when there are no changes")
	}
}

func TestApplyEnable_HappyPath(t *testing.T) {
	f := Feature{
		ID:    "r",
		Apply: ReloadHot,
		Gates: []Gate{{Key: "k", EnableTo: true}},
		Verify: func(_ context.Context, _ Deps) PrereqResult {
			return PrereqResult{OK: true, Detail: "all good"}
		},
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": false}}}

	plan := &EnablePlan{
		Changes: []GateChange{{Key: "k", From: false, To: true}},
		Apply:   ReloadHot,
	}

	writer := &fakeConfigWriter{
		content:    []byte(minimalYAML),
		backupPath: "config.yaml.bak",
	}
	reloader := &fakeReloader{}

	result, err := ApplyEnable(context.Background(), f, deps, plan, writer, reloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.OK {
		t.Fatalf("verify must pass, got %+v", result)
	}
	if !writer.writeCalled {
		t.Error("Write must be called")
	}
	if !reloader.called {
		t.Error("Reload must be called")
	}
	if writer.restoreCalled {
		t.Error("Restore must NOT be called on success")
	}
}

func TestApplyEnable_NilVerify_Success(t *testing.T) {
	// Feature with no Verify func → ApplyEnable returns OK result after write+reload.
	f := Feature{
		ID:     "r",
		Apply:  ReloadHot,
		Gates:  []Gate{{Key: "k", EnableTo: true}},
		Verify: nil,
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": false}}}

	plan := &EnablePlan{
		Changes: []GateChange{{Key: "k", From: false, To: true}},
		Apply:   ReloadHot,
	}

	writer := &fakeConfigWriter{
		content:    []byte(minimalYAML),
		backupPath: "bak",
	}
	reloader := &fakeReloader{}

	result, err := ApplyEnable(context.Background(), f, deps, plan, writer, reloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.OK {
		t.Fatal("nil Verify must yield OK result")
	}
}

// TestApplyEnable_BackupError ensures that a backup failure immediately
// returns an error and nothing else is attempted.
func TestApplyEnable_BackupError(t *testing.T) {
	f := Feature{
		ID:    "r",
		Apply: ReloadHot,
		Gates: []Gate{{Key: "k", EnableTo: true}},
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": false}}}

	plan := &EnablePlan{
		Changes: []GateChange{{Key: "k", From: false, To: true}},
		Apply:   ReloadHot,
	}

	writer := &fakeConfigWriter{
		content:   []byte(minimalYAML),
		backupErr: errors.New("no space"),
	}
	reloader := &fakeReloader{}

	_, err := ApplyEnable(context.Background(), f, deps, plan, writer, reloader)
	if err == nil {
		t.Fatal("expected error when backup fails")
	}
	if writer.writeCalled {
		t.Error("Write must not be called after backup failure")
	}
	if reloader.called {
		t.Error("Reload must not be called after backup failure")
	}
}

func TestApplyEnable_RollbackRestoreAlsoFails(t *testing.T) {
	// writer.Restore also fails → error message must mention both failures.
	f := Feature{
		ID:    "r",
		Apply: ReloadHot,
		Gates: []Gate{{Key: "k", EnableTo: true}},
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": false}}}

	plan := &EnablePlan{
		Changes: []GateChange{{Key: "k", From: false, To: true}},
		Apply:   ReloadHot,
	}

	writer := &fakeConfigWriter{
		content:    []byte(minimalYAML),
		backupPath: "config.yaml.bak",
		writeErr:   errors.New("disk full"),
		restoreErr: errors.New("restore also failed"),
	}
	reloader := &fakeReloader{}

	_, err := ApplyEnable(context.Background(), f, deps, plan, writer, reloader)
	if err == nil {
		t.Fatal("expected error")
	}
	// The error should mention both failures.
	msg := err.Error()
	if !contains(msg, "disk full") || !contains(msg, "restore also failed") {
		t.Errorf("error should mention both failures, got: %s", msg)
	}
}

func TestApplyEnable_ReadErrorAfterBackup(t *testing.T) {
	f := Feature{
		ID:    "r",
		Apply: ReloadHot,
		Gates: []Gate{{Key: "k", EnableTo: true}},
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": false}}}

	plan := &EnablePlan{
		Changes: []GateChange{{Key: "k", From: false, To: true}},
		Apply:   ReloadHot,
	}

	// Backup succeeds; Read fails.
	writer := &fakeConfigWriter{
		backupPath: "config.yaml.bak",
		readErr:    errors.New("read error"),
	}
	reloader := &fakeReloader{}

	_, err := ApplyEnable(context.Background(), f, deps, plan, writer, reloader)
	if err == nil {
		t.Fatal("expected error from failed read")
	}
	if !writer.restoreCalled {
		t.Fatal("Restore must be called when read fails after backup")
	}
}

func TestApplyEnable_SetYAMLKeyError(t *testing.T) {
	// SetYAMLKey now creates missing keys, so a genuine error needs a path
	// that descends into a non-mapping: gate key "k.nested" over content
	// where "k" is a scalar → cannot descend → SetYAMLKey errors.
	f := Feature{
		ID:    "r",
		Apply: ReloadHot,
		Gates: []Gate{{Key: "k.nested", EnableTo: true}},
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k.nested": false}}}

	plan := &EnablePlan{
		Changes: []GateChange{{Key: "k.nested", From: false, To: true}},
		Apply:   ReloadHot,
	}

	// "k" is a scalar; "k.nested" can't descend into it → SetYAMLKey errors.
	writer := &fakeConfigWriter{
		content:    []byte("k: scalar\n"),
		backupPath: "bak",
	}
	reloader := &fakeReloader{}

	_, err := ApplyEnable(context.Background(), f, deps, plan, writer, reloader)
	if err == nil {
		t.Fatal("expected SetYAMLKey error")
	}
	if !writer.restoreCalled {
		t.Fatal("Restore must be called when SetYAMLKey fails")
	}
}

func TestApplyEnable_NilReloader_StillWritesAndVerifies(t *testing.T) {
	f := Feature{
		ID:    "r",
		Apply: ReloadHot,
		Gates: []Gate{{Key: "k", EnableTo: true}},
		Verify: func(_ context.Context, _ Deps) PrereqResult {
			return PrereqResult{OK: true}
		},
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": false}}}

	plan := &EnablePlan{
		Changes: []GateChange{{Key: "k", From: false, To: true}},
		Apply:   ReloadHot,
	}

	writer := &fakeConfigWriter{
		content:    []byte(minimalYAML),
		backupPath: "bak",
	}

	result, err := ApplyEnable(context.Background(), f, deps, plan, writer, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.OK {
		t.Fatal("expected OK result with nil reloader")
	}
	if !writer.writeCalled {
		t.Error("Write must still be called with nil reloader")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

// --- RestartRequired ApplyEnable tests ---

// restartYAML is a minimal YAML with the key used by RestartRequired tests.
const restartYAML = "k: false\n"

// TestApplyEnable_RestartRequired_WriteValidate verifies that for a
// RestartRequired feature ApplyEnable writes the config, calls Validate, does
// NOT call Reload, and returns a restart-pending OK result.
func TestApplyEnable_RestartRequired_WriteValidate(t *testing.T) {
	f := Feature{
		ID:    "rr",
		Apply: RestartRequired,
		Gates: []Gate{{Key: "k", EnableTo: true}},
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": false}}}

	plan := &EnablePlan{
		Changes: []GateChange{{Key: "k", From: false, To: true}},
		Apply:   RestartRequired,
	}

	writer := &fakeConfigWriter{
		content:    []byte(restartYAML),
		backupPath: "config.yaml.bak",
	}
	reloader := &fakeReloader{}

	result, err := ApplyEnable(context.Background(), f, deps, plan, writer, reloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.OK {
		t.Fatalf("expected OK result, got %+v", result)
	}
	if !contains(result.Detail, "restart vornik to apply") {
		t.Errorf("detail should mention restart, got: %q", result.Detail)
	}
	if !writer.writeCalled {
		t.Error("Write must be called for RestartRequired")
	}
	if !writer.validateCalled {
		t.Error("Validate must be called for RestartRequired")
	}
	if reloader.called {
		t.Error("Reload must NOT be called for RestartRequired (gates not live until restart)")
	}
	if writer.restoreCalled {
		t.Error("Restore must NOT be called on success")
	}
}

// TestApplyEnable_RestartRequired_ValidateFail verifies that a validation
// failure after Write causes Restore to be called and returns an error.
// The Reloader must still not be called.
func TestApplyEnable_RestartRequired_ValidateFail(t *testing.T) {
	f := Feature{
		ID:    "rr",
		Apply: RestartRequired,
		Gates: []Gate{{Key: "k", EnableTo: true}},
	}
	deps := Deps{Config: stubConfig{vals: map[string]any{"k": false}}}

	plan := &EnablePlan{
		Changes: []GateChange{{Key: "k", From: false, To: true}},
		Apply:   RestartRequired,
	}

	writer := &fakeConfigWriter{
		content:     []byte(restartYAML),
		backupPath:  "config.yaml.bak",
		validateErr: errors.New("server address is required"),
	}
	reloader := &fakeReloader{}

	_, err := ApplyEnable(context.Background(), f, deps, plan, writer, reloader)
	if err == nil {
		t.Fatal("expected error when validate fails")
	}
	if !contains(err.Error(), "server address is required") {
		t.Errorf("error should mention validation failure, got: %s", err.Error())
	}
	if !writer.restoreCalled {
		t.Error("Restore must be called when validate fails")
	}
	if writer.restoreTarget != writer.backupPath {
		t.Errorf("Restore called with %q, want %q", writer.restoreTarget, writer.backupPath)
	}
	if reloader.called {
		t.Error("Reload must NOT be called even on validate failure for RestartRequired")
	}
}

// TestFileConfigWriter_Validate verifies that FileConfigWriter.Validate calls
// the config parser on the file it manages. An arbitrary YAML file that fails
// the vornik config validator (missing server address etc.) returns a non-nil
// error; a nonexistent path returns a read error.
func TestFileConfigWriter_Validate(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"

	// File does not exist yet → read error.
	w := &FileConfigWriter{Path: path}
	if err := w.Validate(); err == nil {
		t.Error("Validate on missing file must return error")
	}

	// File exists but is minimal YAML → config validator returns error (missing
	// required fields like server.address). We only verify it returns non-nil.
	if err := os.WriteFile(path, []byte("k: v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Validate(); err == nil {
		t.Error("minimal YAML must fail config validation (missing required fields)")
	}
}

// TestFileConfigWriter_RoundTrip exercises the real file-backed writer.
func TestFileConfigWriter_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	if err := os.WriteFile(path, []byte("k: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := &FileConfigWriter{Path: path}

	data, err := w.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "k: false\n" {
		t.Fatalf("unexpected content: %q", data)
	}

	if err := w.Write([]byte("k: true\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	bak, err := w.Backup()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Overwrite the file then restore.
	if err := os.WriteFile(path, []byte("k: broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Restore(bak); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	data2, _ := os.ReadFile(path)
	if string(data2) != "k: true\n" {
		t.Fatalf("restore failed, got %q", data2)
	}
}

// TestFileConfigWriter_WriteIsAtomic verifies Write replaces the file via
// rename rather than truncating in place. A hard link to the original inode
// must still see the OLD content after Write — which only holds if Write
// creates a new file and renames it over the path. A plain os.WriteFile
// (truncate-in-place) mutates the shared inode and fails this test.
// Regression: the 2026-06-11 docs-publishing-session review found
// FileConfigWriter.Write was documented "atomic" but used os.WriteFile, so a
// crash mid-write could leave a truncated config.yaml.
func TestFileConfigWriter_WriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	if err := os.WriteFile(path, []byte("k: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := dir + "/config.yaml.hardlink"
	if err := os.Link(path, link); err != nil {
		t.Skipf("hard links unsupported on this platform: %v", err)
	}

	w := &FileConfigWriter{Path: path}
	if err := w.Write([]byte("k: true\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// The path now carries the new content.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "k: true\n" {
		t.Fatalf("path content = %q, want new content", got)
	}
	// The original inode (reachable via the hard link) is untouched — proving
	// rename-replace, not in-place truncate.
	linkContent, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if string(linkContent) != "k: false\n" {
		t.Fatalf("hard link content = %q; Write truncated in place instead of renaming (not atomic)", linkContent)
	}
	// Mode is preserved on the replaced file.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}
