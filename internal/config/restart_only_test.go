package config

import (
	"strings"
	"testing"
)

// INCIDENT 2026-07-30, customer deployment. Their memory classifier was hammering a
// shared LLM gateway every 30 seconds with calls that each took longer than the interval,
// producing congestion collapse: ~500 timeouts an hour and a stalled ingestion pipeline.
//
// The fix was to raise memory.classifier.auto_backfill_interval_seconds. `vornikctl config
// reload` was run and logged **"config reloaded successfully"** — twice — and the worker
// kept its old 30-second ticker, because the backfill loop creates
// `time.NewTicker(interval)` ONCE at loop start and never re-reads the value. It took a
// daemon restart, and nothing in the product said so.
//
// A reload that reports success on a value it cannot apply is worse than one that fails:
// the operator believes the change is live and looks elsewhere for the problem.
func TestRestartOnlySnapshot_NamesTheFieldsThatNeedARestart(t *testing.T) {
	before := map[string]string{
		"memory.classifier.auto_backfill_interval_seconds": "30",
		"memory.titler.auto_backfill_batch_size":           "25",
		"chat.model":                                       "glm-5",
	}
	after := map[string]string{
		"memory.classifier.auto_backfill_interval_seconds": "600",
		"memory.titler.auto_backfill_batch_size":           "25",
		"chat.model":                                       "glm-5",
	}

	changed := DiffRestartOnly(before, after)
	if len(changed) != 1 {
		t.Fatalf("changed = %v, want exactly the one field that moved", changed)
	}
	c := changed[0]
	if c.Path != "memory.classifier.auto_backfill_interval_seconds" {
		t.Errorf("path = %q", c.Path)
	}
	if c.Was != "30" || c.Now != "600" {
		t.Errorf("values = %q -> %q, want 30 -> 600", c.Was, c.Now)
	}
	// The operator has to be able to act: the message must say what to do.
	msg := RestartOnlyWarning(changed)
	low := strings.ToLower(msg)
	if !strings.Contains(low, "restart") {
		t.Errorf("warning must tell the operator a restart is needed: %q", msg)
	}
	if !strings.Contains(msg, "600") || !strings.Contains(msg, "30") {
		t.Errorf("warning must show old and new so the operator can confirm which is live: %q", msg)
	}
	if !strings.Contains(msg, "memory.classifier.auto_backfill_interval_seconds") {
		t.Errorf("warning must name the field: %q", msg)
	}
}

// No change to a restart-only field means no warning — a reload that genuinely applied
// everything must stay quiet, or the warning becomes noise operators learn to skip.
func TestRestartOnlySnapshot_QuietWhenNothingRestartOnlyChanged(t *testing.T) {
	same := map[string]string{"memory.classifier.auto_backfill_interval_seconds": "600"}
	if changed := DiffRestartOnly(same, same); len(changed) != 0 {
		t.Fatalf("changed = %v, want none", changed)
	}
	if msg := RestartOnlyWarning(nil); msg != "" {
		t.Errorf("warning for no changes = %q, want empty", msg)
	}
}

// A field appearing or disappearing (unset -> set) is still a change the running process
// cannot pick up.
func TestRestartOnlySnapshot_HandlesAppearingAndDisappearingKeys(t *testing.T) {
	changed := DiffRestartOnly(
		map[string]string{"a": "1"},
		map[string]string{"b": "2"},
	)
	if len(changed) != 2 {
		t.Fatalf("changed = %v, want both the removed and the added key", changed)
	}
}

// The snapshot must actually read the fields I verified are restart-only, from a real
// Config — a registry of path strings that does not match the struct is worse than none.
func TestRestartOnlySnapshotOf_ReadsTheVerifiedFields(t *testing.T) {
	cfg := &Config{}
	cfg.Memory.Classifier.AutoBackfillIntervalSeconds = 30
	cfg.Memory.Classifier.AutoBackfillBatchSize = 5
	cfg.Memory.Titler.AutoBackfillIntervalSeconds = 300
	cfg.Memory.Titler.AutoBackfillBatchSize = 25

	snap := RestartOnlySnapshotOf(cfg)
	for path, want := range map[string]string{
		"memory.classifier.auto_backfill_interval_seconds": "30",
		"memory.classifier.auto_backfill_batch_size":       "5",
		"memory.titler.auto_backfill_interval_seconds":     "300",
		"memory.titler.auto_backfill_batch_size":           "25",
	} {
		if got := snap[path]; got != want {
			t.Errorf("snapshot[%q] = %q, want %q", path, got, want)
		}
	}
}

// A nil config must not panic — the probe is called from the reload path and a partially
// built daemon should degrade to "no snapshot", not crash the reload.
func TestRestartOnlySnapshotOf_NilIsSafe(t *testing.T) {
	if snap := RestartOnlySnapshotOf(nil); snap != nil {
		t.Errorf("snapshot of nil = %v, want nil", snap)
	}
}

// The reloader must record the warning through the surface that already exists for
// "succeeded but did not activate everything", so `config reload-status` shows it.
func TestConfigReloader_WarnsWhenARestartOnlyFieldChanged(t *testing.T) {
	r := &ConfigReloader{}
	live := map[string]string{"memory.classifier.auto_backfill_interval_seconds": "30"}
	r.SetRestartOnlyProbe(func() map[string]string { return live })

	// The baseline is taken when the probe is wired (process start), not lazily.
	if got := r.Status().Warnings; len(got) != 0 {
		t.Fatalf("warnings before any edit = %v, want none", got)
	}

	// Operator edits the file and reloads.
	live = map[string]string{"memory.classifier.auto_backfill_interval_seconds": "600"}
	r.checkRestartOnlyDrift()

	st := r.Status()
	if !st.HasWarnings {
		t.Fatal("a changed restart-only field must raise a reload warning")
	}
	joined := strings.ToLower(strings.Join(st.Warnings, " | "))
	if !strings.Contains(joined, "restart") {
		t.Errorf("warning does not mention a restart: %q", joined)
	}
}

// No probe wired is not a failure — it just means the daemon cannot assess drift.
func TestConfigReloader_NoProbeIsSafe(t *testing.T) {
	r := &ConfigReloader{}
	r.checkRestartOnlyDrift()
	if got := r.Status().Warnings; len(got) != 0 {
		t.Fatalf("warnings = %v, want none without a probe", got)
	}
}

// REGRESSION on my own first design. The baseline was captured lazily on the first drift
// check, which runs on the first RELOAD — by which time the file already contains the
// operator's edit. The baseline would equal the new value and the very first change, the
// exact case this feature exists for, would warn about nothing.
func TestConfigReloader_BaselineIsTakenWhenTheProbeIsWiredNotOnFirstReload(t *testing.T) {
	r := &ConfigReloader{}
	live := map[string]string{"memory.classifier.auto_backfill_interval_seconds": "30"}
	r.SetRestartOnlyProbe(func() map[string]string { return live })

	// Operator's FIRST action after boot is the edit + reload.
	live = map[string]string{"memory.classifier.auto_backfill_interval_seconds": "600"}
	r.checkRestartOnlyDrift()

	if !r.Status().HasWarnings {
		t.Fatal("the first edit after boot must warn — a lazily-captured baseline would " +
			"have swallowed it")
	}
}

// The warning must PERSIST across later reloads: the old value keeps running until a
// restart, so a second reload that silently dropped the warning would tell the operator
// the problem had resolved itself.
func TestConfigReloader_WarningPersistsUntilRestart(t *testing.T) {
	r := &ConfigReloader{}
	live := map[string]string{"memory.titler.auto_backfill_batch_size": "25"}
	r.SetRestartOnlyProbe(func() map[string]string { return live })

	live = map[string]string{"memory.titler.auto_backfill_batch_size": "10"}
	r.checkRestartOnlyDrift()
	if !r.Status().HasWarnings {
		t.Fatal("first reload must warn")
	}

	// A second reload with no further edit: still not applied, still must warn.
	r.mu.Lock()
	r.reloadWarnings = nil // runCycle clears warnings at the start of each cycle
	r.mu.Unlock()
	r.checkRestartOnlyDrift()
	if !r.Status().HasWarnings {
		t.Fatal("the warning must persist while the old value is still running")
	}
}
