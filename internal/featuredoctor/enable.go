package featuredoctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"

	"vornik.io/vornik/internal/config"
)

// applyMu serializes ApplyEnable's backup→write→reload→verify transaction
// so two concurrent feature-enable calls can't interleave and have one's
// rollback clobber the other's committed gate change. Process-wide: vornik
// is a single deployable unit, so there is no second writer to guard
// against beyond this process.
var applyMu sync.Mutex

// EnablePlan is the result of a dry-run PlanEnable call: the set of gate
// changes required to enable a feature plus the apply mechanism.
type EnablePlan struct {
	Changes []GateChange
	Apply   ApplyMechanism
}

// GateChange is one gate key transition.
type GateChange struct {
	Key  string
	From any
	To   any
}

// ConfigWriter is the write surface for transactionally applying gate changes.
// Backup and Restore implement the rollback safety net; Read/Write carry the
// comment-preserving YAML content.  Validate re-parses the written file (by
// path) so RestartRequired applies can confirm the edit is valid before
// returning to the caller.
type ConfigWriter interface {
	Read() ([]byte, error)
	Write([]byte) error
	Backup() (string, error)
	Restore(backup string) error
	// Validate re-reads the file at its current path and calls the config
	// loader's parse+validate pipeline.  Must be callable immediately after
	// Write without any global state mutation (no flag.Parse).
	Validate() error
}

// Reloader triggers a daemon config reload and waits for the result.
type Reloader interface {
	Reload(ctx context.Context) error
}

// PlanEnable runs all prereqs and computes the gate diff needed to enable f.
//
// Hard stops (returns nil, non-nil error):
//   - Any unfixable prereq is unmet.
//   - f.Apply == RestartRequired and there are active tasks.
//
// On success the returned plan carries the list of gate changes (may be
// empty if all gates are already at EnableTo) and the apply mechanism.
func PlanEnable(ctx context.Context, f Feature, deps Deps) (*EnablePlan, error) {
	// Run all prereqs and look for hard stops.
	for _, p := range f.Prereqs {
		r := p.Check(ctx, deps)
		if !r.OK && !r.Fixable {
			return nil, fmt.Errorf("feature %q: prereq %q unmet (unfixable): %s — %s",
				f.ID, p.Name, r.Detail, r.Remediation)
		}
	}

	// Guard: RestartRequired + busy system.
	if f.Apply == RestartRequired && deps.Tasks != nil {
		active, err := deps.Tasks.HasActiveTasks(ctx)
		if err != nil {
			return nil, fmt.Errorf("feature %q: cannot check active tasks: %w", f.ID, err)
		}
		if active {
			return nil, fmt.Errorf("feature %q: daemon restart required but there are active tasks; wait for an idle window", f.ID)
		}
	}

	// Compute the gate diff.
	var changes []GateChange
	for _, g := range f.Gates {
		var current any
		if deps.Config != nil {
			current, _ = deps.Config.GateValue(g.Key)
		}
		if !reflect.DeepEqual(current, g.EnableTo) {
			changes = append(changes, GateChange{Key: g.Key, From: current, To: g.EnableTo})
		}
	}

	return &EnablePlan{Changes: changes, Apply: f.Apply}, nil
}

// ApplyEnable transactionally applies the gate changes in plan and — depending
// on the apply mechanism — either reloads the daemon or signals that a restart
// is required.  Any failure after the backup step triggers an automatic restore.
//
// RestartRequired sequence: Backup → patch YAML → Write → Validate → return
// "config written; restart vornik to apply".  Reload and Verify are NOT called
// because the gates are not live until the daemon restarts; calling them would
// produce a false "verified" signal.
//
// ReloadHot sequence: Backup → patch YAML → Write → Reload → Verify.
// On any step-failure: Restore(backup) then return the error.
func ApplyEnable(ctx context.Context, f Feature, deps Deps, plan *EnablePlan, w ConfigWriter, r Reloader) (PrereqResult, error) {
	// Serialize the whole backup→write→reload→verify transaction. The
	// individual Write is atomic (temp+rename), but the backup/restore
	// WINDOW is not: two concurrent enables could each snapshot the same
	// pre-state and then one's rollback would revert the other's committed
	// gate change. A single in-process mutex is sufficient — vornik is one
	// deployable unit (no second writer process) and enables are rare,
	// operator-driven mutations. Held across Reload/Verify so a hot-apply
	// can't race a second enable's write either.
	applyMu.Lock()
	defer applyMu.Unlock()

	if len(plan.Changes) == 0 {
		// Nothing to write — skip backup/write/reload and go straight to verify.
		// For RestartRequired with no changes the gates are already at their
		// target values; no restart is needed either.
		return runVerify(ctx, f, deps)
	}

	// Step 1: backup.
	backup, err := w.Backup()
	if err != nil {
		return PrereqResult{}, fmt.Errorf("feature %q: backup failed: %w", f.ID, err)
	}

	rollbackNoReload := func(cause error) error {
		if rerr := w.Restore(backup); rerr != nil {
			return fmt.Errorf("feature %q: %w (also restore failed: %v)", f.ID, cause, rerr)
		}
		return fmt.Errorf("feature %q: %w (config restored from backup)", f.ID, cause)
	}

	// Step 2: read current content and apply all changes in memory.
	content, err := w.Read()
	if err != nil {
		return PrereqResult{}, rollbackNoReload(fmt.Errorf("read failed: %w", err))
	}

	for _, ch := range plan.Changes {
		// created is intentionally ignored here: a feature gate key with a
		// default is legitimately absent from a hand-written config and gets
		// created on first enable, so a "created" warning would be noise. The
		// registry's keys are validated against *config.Config by the
		// keys_integration_test resolve check, and RestartRequired applies
		// re-Validate the written file below.
		content, _, err = config.SetYAMLKey(content, ch.Key, ch.To)
		if err != nil {
			return PrereqResult{}, rollbackNoReload(fmt.Errorf("set key %q: %w", ch.Key, err))
		}
	}

	// Step 3: write.
	if err := w.Write(content); err != nil {
		return PrereqResult{}, rollbackNoReload(fmt.Errorf("write failed: %w", err))
	}

	// Branch on apply mechanism.
	switch plan.Apply {
	case RestartRequired:
		// Step 4 (RestartRequired): validate the written file parses cleanly.
		// Reload and Verify are intentionally skipped: config.yaml is only
		// re-read on daemon startup, so the gates are not live yet.  Claiming
		// "verified" here would be misleading.
		if err := w.Validate(); err != nil {
			return PrereqResult{}, rollbackNoReload(fmt.Errorf("written config invalid: %w", err))
		}
		return PrereqResult{
			OK:     true,
			Detail: "config written; restart vornik to apply (config.yaml changes are not hot-reloaded)",
		}, nil

	default: // ReloadHot
		// reloadAttempted tracks whether Reload was called so rollback can decide
		// whether a post-restore re-reload is needed to re-sync the daemon.
		reloadAttempted := false

		rollback := func(cause error) error {
			if rerr := w.Restore(backup); rerr != nil {
				return fmt.Errorf("feature %q: %w (also restore failed: %v)", f.ID, cause, rerr)
			}
			// Best-effort re-reload so the daemon re-syncs to the restored config.
			// Only necessary when a reload was already attempted (daemon may have
			// loaded a partially-applied or post-write config). A second reload
			// error must NOT mask the original cause.
			if reloadAttempted && r != nil {
				if rerr := r.Reload(ctx); rerr != nil {
					deps.Logger.Warn().Err(rerr).Str("feature", f.ID).
						Msg("re-reload after rollback failed (ignored)")
				}
			}
			return fmt.Errorf("feature %q: %w (config restored from backup)", f.ID, cause)
		}

		// Step 4: reload.
		if r != nil {
			reloadAttempted = true
			if err := r.Reload(ctx); err != nil {
				return PrereqResult{}, rollback(fmt.Errorf("reload failed: %w", err))
			}
		}

		// Step 5: verify.
		result, err := runVerify(ctx, f, deps)
		if err != nil {
			return PrereqResult{}, rollback(err)
		}
		if !result.OK {
			return PrereqResult{}, rollback(fmt.Errorf("verify failed: %s", result.Detail))
		}
		return result, nil
	}
}

// runVerify calls f.Verify if it exists; returns OK when Verify is nil.
func runVerify(ctx context.Context, f Feature, deps Deps) (PrereqResult, error) {
	if f.Verify == nil {
		return PrereqResult{OK: true, Detail: "no verify func defined"}, nil
	}
	r := f.Verify(ctx, deps)
	if !r.OK {
		return r, fmt.Errorf("verify: %s", r.Detail)
	}
	return r, nil
}

// FileConfigWriter is the production ConfigWriter that operates on the
// daemon's config.yaml path.
type FileConfigWriter struct {
	Path string
}

// Read returns the raw bytes of the config file.
func (w *FileConfigWriter) Read() ([]byte, error) {
	return os.ReadFile(w.Path)
}

// renameFunc indirects os.Rename so tests can force the EBUSY fallback
// without needing a real bind mount.
var renameFunc = os.Rename

// Write atomically replaces the config file with data: it writes to a
// temporary file in the same directory, fsyncs, then renames over the target.
// A crash mid-write therefore never leaves a truncated config.yaml — the old
// file survives until the rename commits the new one in a single step. (A
// plain os.WriteFile truncates in place, so a crash between truncate and the
// final write would corrupt the live config.)
//
// Fallback: when w.Path is a single-file bind mount (podman/docker
// `-v host.yaml:/etc/vornik/config.yaml`), rename(2) over the mountpoint
// returns EBUSY — the kernel won't replace a mountpoint inode. EXDEV is the
// sibling case (temp on a different filesystem). In both, atomic rename is
// impossible, so Write rewrites the file in place. That is not crash-atomic,
// but every onboarding caller takes a Backup() before Write, so a rare
// crash-mid-write is recoverable; a hard failure here would otherwise leave
// the operator unable to save config at all on these (common) deployments.
func (w *FileConfigWriter) Write(data []byte) error {
	dir := filepath.Dir(w.Path)
	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename commits.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := renameFunc(tmpName, w.Path); err != nil {
		if errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.EXDEV) {
			return w.writeInPlace(data)
		}
		return fmt.Errorf("rename temp config into place: %w", err)
	}
	return nil
}

// writeInPlace truncates w.Path and rewrites it directly, preserving the
// inode so a single-file bind mount stays intact. Used only when atomic
// rename is impossible (see Write). The caller's Backup() is the safety net
// against a crash mid-write.
func (w *FileConfigWriter) writeInPlace(data []byte) error {
	f, err := os.OpenFile(w.Path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open config for in-place write: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("in-place write config: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync config: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	return nil
}

// Backup creates a timestamped copy of the config file and returns its path.
func (w *FileConfigWriter) Backup() (string, error) {
	return backupConfig(w.Path)
}

// Restore copies backup back to w.Path, undoing any writes made since Backup.
func (w *FileConfigWriter) Restore(backup string) error {
	return restoreConfig(backup, w.Path)
}

// Validate re-reads the file at w.Path and runs the config parse+validate
// pipeline without touching any global state (no flag.Parse).
func (w *FileConfigWriter) Validate() error {
	return config.ValidateFile(w.Path)
}
