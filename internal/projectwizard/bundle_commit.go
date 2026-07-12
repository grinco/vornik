package projectwizard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/safepath"
)

// composerStagingDirName is the subdirectory of LiveConfigDir every
// tier-3 commit stages into, session-scoped so concurrent sessions
// never collide (design §5.6 step 2).
const composerStagingDirName = ".composer-staging"

// ErrBundleCommitFailed wraps every journaled-commit failure. The
// session is left resumable (session.Bundle is never cleared on this
// path — see commitBundleSession) and the wrapped text is safe,
// plain-language operator-facing detail (never a filesystem path).
var ErrBundleCommitFailed = errors.New("projectwizard: bundle commit failed")

// renameFn / removeFn are test seams over os.Rename / os.Remove so
// tests can force a failure at an exact point in the dependency-
// ordered rename sequence (e.g. right before the project file) to
// assert the mid-sequence state and the rollback that follows.
var (
	renameFn = os.Rename
	removeFn = os.Remove
)

// stagingDirFor is the session-scoped staging directory a tier-3
// bundle renders into before anything touches the live tree (design
// §5.6 step 2): "<LiveConfigDir>/.composer-staging/<session_id>/".
func stagingDirFor(liveConfigDir, sessionID string) string {
	return filepath.Join(liveConfigDir, composerStagingDirName, sessionID)
}

// journalPathFor is the commit journal's path, INSIDE the session's
// staging dir (design §5.6 step 3) — so a successful commit's final
// staging-dir removal also removes the journal in the same operation.
func journalPathFor(liveConfigDir, sessionID string) string {
	return filepath.Join(stagingDirFor(liveConfigDir, sessionID), ".composer-commit-"+sessionID+".json")
}

// commitJournal is the durable record of one bundle commit's target
// paths, in the exact dependency order they must land (design §5.6
// step 3): workflows, then the swarm, then the project file LAST —
// the project file is the activating reference, so nothing may
// resolve it until everything it points to already exists.
type commitJournal struct {
	SessionID string          `json:"session_id"`
	CreatedAt time.Time       `json:"created_at"`
	Targets   []journalTarget `json:"targets"`
}

// journalTarget is one file the journal tracks: RelPath is relative to
// LiveConfigDir (the live-tree destination); StagingPath is its
// already-rendered, already-validated staged copy.
type journalTarget struct {
	RelPath     string `json:"rel_path"`
	StagingPath string `json:"staging_path"`
}

// orderedRelPaths sorts a rendered bundle's file set into the commit's
// dependency order (design §5.6 step 3): workflows/*.md first, then
// swarms/*.md, then projects/*.yaml LAST. Anything outside those three
// prefixes (shouldn't occur for a materialized bundle, but never
// silently dropped) sorts first of all, before workflows.
func orderedRelPaths(files map[string]string) []string {
	var other, workflows, swarms, projects []string
	for path := range files {
		switch {
		case strings.HasPrefix(path, "workflows/"):
			workflows = append(workflows, path)
		case strings.HasPrefix(path, "swarms/"):
			swarms = append(swarms, path)
		case strings.HasPrefix(path, "projects/"):
			projects = append(projects, path)
		default:
			other = append(other, path)
		}
	}
	sort.Strings(other)
	sort.Strings(workflows)
	sort.Strings(swarms)
	sort.Strings(projects)
	out := make([]string, 0, len(files))
	out = append(out, other...)
	out = append(out, workflows...)
	out = append(out, swarms...)
	out = append(out, projects...)
	return out
}

// journalTargetsInOrder rebuilds the dependency-ordered target list
// (RelPath only — no StagingPath, since the staging dir is already
// gone by the time this is used) from a rendered file set. Used by
// the post-reload rollback path in commitBundleSession, which runs
// AFTER stageAndCommitBundle has already succeeded and cleaned up its
// own staging dir + journal — so there is no on-disk journal left to
// read the target order back from; it's recomputed from the same
// files map instead.
func journalTargetsInOrder(files map[string]string) []journalTarget {
	order := orderedRelPaths(files)
	out := make([]journalTarget, len(order))
	for i, rel := range order {
		out[i] = journalTarget{RelPath: rel}
	}
	return out
}

// defaultReloadTimeout is design §5.6 step 5's "30 s deadline" for the
// post-commit hot-reload poll.
const defaultReloadTimeout = 30 * time.Second

// triggerPostCommitReload triggers one bounded reload via w.Reloader
// once a tier-3 bundle's project file has landed (design §5.6 step
// 5). Returns nil when there is no Reloader wired at all — a
// documented graceful degradation (CE/minimal wiring): the files stay
// landed and the daemon's own watcher/next reload eventually picks
// them up, same as slice i's behaviour before this field existed.
// Returns a non-nil error on timeout, on a busy/deferred cycle, or on
// a hard validate/activate rejection — every one of those is the
// caller's cue to roll back the just-landed commit.
func (w *Wizard) triggerPostCommitReload() error {
	if w.Reloader == nil {
		return nil
	}
	timeout := w.ReloadTimeout
	if timeout <= 0 {
		timeout = defaultReloadTimeout
	}
	ok, err := w.Reloader.TryReload(timeout)
	if ok {
		return nil
	}
	if err == nil {
		err = errors.New("reload did not apply within the deadline")
	}
	return err
}

// stageAndCommitBundle renders files into a fresh session-scoped
// staging directory, writes the commit journal, then renames each
// staged file into the live tree in dependency order (design §5.6
// steps 2-3). Any failure rolls back every already-landed file (in
// reverse order) and removes the staging dir + journal before
// returning — the caller never has to distinguish "which step failed"
// to know the live tree is back to its pre-commit state. On success
// the staging dir (and the journal inside it) is removed too.
//
// A genuine daemon crash between two renames is the ONLY scenario
// that can leave the journal on disk after this function returns —
// every error this function itself observes is rolled back in-
// process. Crash recovery (task 1.2b slice ii, RecoverComposerCommits)
// is what cleans up that leftover case at the next startup.
func stageAndCommitBundle(liveConfigDir, sessionID string, files map[string]string) (err error) {
	if strings.TrimSpace(liveConfigDir) == "" {
		return errors.New("no live config directory wired for bundle commit")
	}
	if !isSafeProjectID(sessionID) {
		return errors.New("unsafe session id for bundle commit")
	}
	if len(files) == 0 {
		return errors.New("no files to commit")
	}

	stageDir := stagingDirFor(liveConfigDir, sessionID)
	if mkErr := os.MkdirAll(stageDir, 0o700); mkErr != nil {
		return fmt.Errorf("create staging dir: %w", mkErr)
	}
	// Cleanup runs on EVERY return from here on — success removes the
	// staging dir (and, since the journal lives inside it, the journal
	// too) after all renames land; every error path below rolls back
	// first, then falls through to this same removal so no path leaves
	// a staging dir behind (constraint: cleaned up on success AND every
	// error path). A removal failure here is logged, never escalated —
	// same idiom as stageBundleForValidation's cleanup.
	defer func() {
		if rmErr := removeAllFn(stageDir); rmErr != nil {
			log.Warn().Err(rmErr).Str("staging_dir", stageDir).
				Msg("composer: failed to remove commit staging dir")
		}
	}()

	order := orderedRelPaths(files)
	journal := commitJournal{SessionID: sessionID, CreatedAt: time.Now().UTC()}
	for _, rel := range order {
		stagingPath, pathErr := safeComposerPath(stageDir, rel)
		if pathErr != nil {
			return fmt.Errorf("render staged file %s: %w", rel, pathErr)
		}
		if mkErr := os.MkdirAll(filepath.Dir(stagingPath), 0o700); mkErr != nil {
			return fmt.Errorf("render staged file %s: %w", rel, mkErr)
		}
		if wErr := os.WriteFile(stagingPath, []byte(files[rel]), 0o600); wErr != nil {
			return fmt.Errorf("render staged file %s: %w", rel, wErr)
		}
		journal.Targets = append(journal.Targets, journalTarget{RelPath: rel, StagingPath: stagingPath})
	}

	journalBytes, jErr := json.Marshal(journal)
	if jErr != nil {
		return fmt.Errorf("marshal commit journal: %w", jErr)
	}
	if wErr := os.WriteFile(journalPathFor(liveConfigDir, sessionID), journalBytes, 0o600); wErr != nil {
		return fmt.Errorf("write commit journal: %w", wErr)
	}

	var landed []journalTarget
	for _, target := range journal.Targets {
		livePath, pathErr := safeComposerPath(liveConfigDir, target.RelPath)
		if pathErr != nil {
			rollbackLandedTargets(liveConfigDir, landed)
			return fmt.Errorf("prepare live path for %s: %w", target.RelPath, pathErr)
		}
		if mkErr := os.MkdirAll(filepath.Dir(livePath), 0o700); mkErr != nil {
			rollbackLandedTargets(liveConfigDir, landed)
			return fmt.Errorf("prepare live directory for %s: %w", target.RelPath, mkErr)
		}
		if rErr := renameFn(target.StagingPath, livePath); rErr != nil {
			rollbackLandedTargets(liveConfigDir, landed)
			return fmt.Errorf("land %s: %w", target.RelPath, rErr)
		}
		landed = append(landed, target)
	}

	return nil
}

// rollbackLandedTargets removes every file that made it into the live
// tree before the failure, in reverse landing order — the journal-
// based rollback (design §5.6 step 6). Touches ONLY the paths passed
// in (already-landed journal targets, resolved against liveConfigDir),
// never anything else in the live tree. A remove failure (already
// gone, permission issue) is logged, not escalated — the caller's own
// error is what's returned.
func rollbackLandedTargets(liveConfigDir string, landed []journalTarget) {
	for i := len(landed) - 1; i >= 0; i-- {
		path, pathErr := safeComposerPath(liveConfigDir, landed[i].RelPath)
		if pathErr != nil {
			log.Warn().Err(pathErr).Str("rel_path", landed[i].RelPath).
				Msg("composer: rollback skipped unsafe journal target")
			continue
		}
		if rmErr := removeFn(path); rmErr != nil && !os.IsNotExist(rmErr) {
			log.Warn().Err(rmErr).Str("path", path).
				Msg("composer: rollback failed to remove a landed file")
		}
	}
}

func safeComposerPath(root, rel string) (string, error) {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" || filepath.IsAbs(rel) || strings.Contains(rel, "..") || strings.Contains(rel, "\\") ||
		filepath.Clean(rel) != filepath.FromSlash(rel) {
		return "", fmt.Errorf("unsafe composer path %q", rel)
	}
	switch {
	case strings.HasPrefix(rel, "projects/") && strings.HasSuffix(rel, ".yaml"):
	case strings.HasPrefix(rel, "swarms/") && strings.HasSuffix(rel, ".md"):
	case strings.HasPrefix(rel, "workflows/") && strings.HasSuffix(rel, ".md"):
	default:
		return "", fmt.Errorf("unexpected composer path %q", rel)
	}
	return safepath.JoinUnder(root, filepath.FromSlash(rel))
}

// commitBundleSession activates a tier-3 session.Bundle: re-runs the
// full defense-in-depth pipeline (materialize → guardrail → schedule-
// confirm → render → staged registry validation, design §5.6 step 1 —
// the SAME gates a fresh tier-3 turn runs, since the registry may have
// drifted since the last /converse turn), then lands the result via
// the journaled stager. Any failure at any step marks the session
// commit-failed-resumable (session.Bundle is left intact) and returns
// ErrBundleCommitFailed; nothing reaches disk on a failure before
// stageAndCommitBundle, and stageAndCommitBundle itself only ever
// leaves the live tree in a state where the project file's presence
// exactly signals full success (it lands last).
func (w *Wizard) commitBundleSession(ctx context.Context, session *persistence.ProjectWizardSession) (*CommitResult, error) {
	var bundle ComposedBundle
	if err := json.Unmarshal(session.Bundle, &bundle); err != nil {
		return nil, w.failBundleCommit(ctx, session, "the saved build is no longer readable — please start a fresh session")
	}

	// Re-run the live-collision check (design §5.2 step 1 / §7's
	// "registry drift between last turn and commit" row): the registry
	// may have gained a project/swarm/workflow with one of this
	// bundle's IDs since the turn that produced it (a concurrent
	// creation elsewhere). LoadFromPaths' later-path-wins semantics
	// (registry_layered_test.go) mean stageBundleForValidation below
	// would NOT itself catch this — a same-ID rename would silently
	// clobber the live file. Catching it here, before anything renders
	// to staging, is what makes the bundle's commit refuse exactly the
	// way a fresh tier-3 turn already refuses.
	ids, shapeErrs := shapeCheckBundle(&bundle)
	live, err := liveEntityIDsFromConfigDir(w.LiveConfigDir)
	if err != nil {
		return nil, w.failBundleCommit(ctx, session, "could not check the live registry for id collisions: "+err.Error())
	}
	shapeErrs = append(shapeErrs, collisionCheckBundle(ids, live)...)
	if len(shapeErrs) > 0 {
		return nil, w.failBundleCommit(ctx, session, strings.Join(shapeErrs, "; "))
	}

	mb, toolViolations, err := materializeBundle(&bundle, w.RoleLibrary)
	if err != nil {
		return nil, w.failBundleCommit(ctx, session, "this build no longer resolves against the current role library: "+err.Error())
	}

	gr := applyGuardrails(mb, bundle.Plan, toolViolations, w.Composer.DefaultBudget, session.ScheduleConfirmedCron)
	for _, v := range gr.Violations {
		w.Metrics.recordGuardrailHit(v.Rule)
	}
	if len(gr.Violations) > 0 {
		msgs := make([]string, len(gr.Violations))
		for i, v := range gr.Violations {
			msgs[i] = v.Message
		}
		return nil, w.failBundleCommit(ctx, session, "a guardrail check failed at commit time: "+strings.Join(msgs, "; "))
	}

	if mb.Project.Autonomy.Enabled && !scheduleConfirmed(session, mb) {
		return nil, w.failBundleCommit(ctx, session, "the autonomy schedule is not confirmed (or no longer matches what was confirmed) — re-confirm the schedule before committing")
	}

	files, err := renderMaterializedBundle(mb)
	if err != nil {
		return nil, w.failBundleCommit(ctx, session, "rendering the build failed: "+err.Error())
	}

	staged, err := stageBundleForValidation(w.LiveConfigDir, files)
	if err != nil {
		return nil, w.failBundleCommit(ctx, session, "validating the build failed: "+err.Error())
	}
	if !staged.OK {
		w.Metrics.recordBundleValidated(bundleValidationResultInvalid)
		return nil, w.failBundleCommit(ctx, session, "the build no longer validates against the current registry: "+strings.Join(staged.Errors, "; "))
	}
	w.Metrics.recordBundleValidated(bundleValidationResultValid)

	projectID := mb.Project.ID
	if projectID == "" || !isSafeProjectID(projectID) {
		return nil, w.failBundleCommit(ctx, session, "the build's project id is invalid")
	}

	if err := stageAndCommitBundle(w.LiveConfigDir, session.ID, files); err != nil {
		log.Error().Err(err).Str("session_id", session.ID).Msg("composer: journaled bundle commit failed")
		return nil, w.failBundleCommit(ctx, session, "writing the build to disk failed; nothing was activated")
	}

	// Hot-reload (design §5.6 step 5): the project file has now landed
	// in the live tree, so trigger ONE bounded reload and poll it. The
	// staging dir + journal are already gone at this point (stageAndCommitBundle
	// removes them on its own success path), so a reload failure here
	// rolls back directly against the live tree via the SAME
	// rollbackLandedTargets primitive the in-process staging failure
	// path uses — reverse dependency order removes the project file
	// FIRST (deactivating the reference before its dependencies
	// disappear), then the swarm, then the workflows.
	if err := w.triggerPostCommitReload(); err != nil {
		rollbackLandedTargets(w.LiveConfigDir, journalTargetsInOrder(files))
		log.Error().Err(err).Str("session_id", session.ID).Msg("composer: post-commit reload failed; rolled back the journaled commit")
		return nil, w.failBundleCommit(ctx, session, "the config reload after committing failed ("+err.Error()+"); the build was rolled back and is safe to retry")
	}

	url := composerDoctorURL(projectID)
	if err := w.Sessions.CommitTo(ctx, session.ID, projectID); err != nil {
		// Files already landed — don't unwind a successful on-disk
		// commit just because the session-stamp update failed. Mirrors
		// the legacy/composition path's identical non-unwind choice
		// (commit.go's own CommitTo handling).
		w.Metrics.recordComposerCommit(composerCommitTier3, composerCommitResultCreated)
		return &CommitResult{SessionID: session.ID, ProjectID: projectID, URL: url},
			fmt.Errorf("projectwizard: stamp session: %w (commit landed)", err)
	}

	w.Metrics.recordComposerCommit(composerCommitTier3, composerCommitResultCreated)
	return &CommitResult{SessionID: session.ID, ProjectID: projectID, URL: url}, nil
}

// composerDoctorURL is the tier-3 commit's redirect target — the
// project doctor / setup page (design §5.6 step 7), same convention
// the legacy and composition commit paths already redirect to.
func composerDoctorURL(projectID string) string {
	return "/ui/projects/" + projectID + "/setup"
}

// failBundleCommit marks session commit-failed-resumable (migration
// 124's bundle_commit_failed_at/bundle_commit_error columns) WITHOUT
// touching session.Bundle — the whole point is that a retry re-runs
// commitBundleSession against the exact same reviewed build. Persist
// failure here is best-effort and logged, never escalated: the caller
// already has a real error to return, and session.Bundle was never
// touched regardless of whether this Update succeeds.
func (w *Wizard) failBundleCommit(ctx context.Context, session *persistence.ProjectWizardSession, reason string) error {
	w.Metrics.recordComposerCommit(composerCommitTier3, composerCommitResultFailed)
	now := time.Now().UTC()
	session.BundleCommitFailedAt = &now
	session.BundleCommitError = reason
	updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if uerr := w.Sessions.Update(updateCtx, session); uerr != nil {
		log.Warn().Err(uerr).Str("session_id", session.ID).
			Msg("composer: failed to persist commit-failure marker (session.Bundle is unaffected either way)")
	}
	return fmt.Errorf("%w: %s", ErrBundleCommitFailed, reason)
}
