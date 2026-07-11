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

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// LeftoverJournal is one composer-commit journal found under
// <liveConfigDir>/.composer-staging/*/ at scan time (design §5.6 step
// 4) — a leftover from a daemon crash/OOM mid-commit (the ONLY way a
// journal can survive stageAndCommitBundle's own cleanup, which runs
// on every in-process return path, success or failure). Consumed by
// both the boot recovery sweep (RecoverComposerCommits) and the
// project-doctor surfacing check (FindLeftoverJournalForProject).
type LeftoverJournal struct {
	SessionID      string
	StagingDir     string
	JournalPath    string
	Targets        []journalTarget
	ProjectRelPath string // "projects/<id>.yaml"; "" if the journal somehow carries no project target
}

// ProjectID derives the composed project's id from the journal's
// project-file target. Empty when ProjectRelPath is empty.
func (j LeftoverJournal) ProjectID() string {
	if j.ProjectRelPath == "" {
		return ""
	}
	base := strings.TrimPrefix(j.ProjectRelPath, "projects/")
	return strings.TrimSuffix(base, ".yaml")
}

// ProjectFileLive reports whether the journal's project file already
// exists in the live tree — the single signal (task 1.2b slice ii's
// load-bearing edge case) that distinguishes a fully-landed commit
// whose only interrupted step was cleanup from a partial commit that
// never activated. A journal with no project target (ProjectRelPath
// == "") can never be considered live — the activating reference was
// never even attempted, so treat it exactly like "absent" (roll it
// back).
func (j LeftoverJournal) ProjectFileLive(liveConfigDir string) bool {
	if j.ProjectRelPath == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(liveConfigDir, filepath.FromSlash(j.ProjectRelPath)))
	return err == nil
}

// findLeftoverJournals scans <liveConfigDir>/.composer-staging/*/ for
// commit journals (design §5.6 step 4). A missing/absent staging root
// is not an error — there is simply nothing to recover. A staging
// directory with no journal file inside it (a crash before the
// journal was even written — stageAndCommitBundle never touched the
// live tree at that point) is left alone: this scan understands only
// journal-listed recovery, and an orphan staging dir with nothing to
// its name never touched anything outside itself. Malformed journal
// JSON is logged and skipped rather than aborting the whole scan, so
// one corrupt journal can't hide every other session's recovery.
func findLeftoverJournals(liveConfigDir string, logger zerolog.Logger) ([]LeftoverJournal, error) {
	root := filepath.Join(liveConfigDir, composerStagingDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan composer staging root: %w", err)
	}

	var out []LeftoverJournal
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionID := entry.Name()
		stageDir := filepath.Join(root, sessionID)
		journalPath := journalPathFor(liveConfigDir, sessionID)
		raw, rErr := os.ReadFile(journalPath)
		if rErr != nil {
			if os.IsNotExist(rErr) {
				continue
			}
			logger.Warn().Err(rErr).Str("staging_dir", stageDir).
				Msg("composer: failed to read a leftover commit journal; skipping")
			continue
		}
		var j commitJournal
		if uErr := json.Unmarshal(raw, &j); uErr != nil {
			logger.Warn().Err(uErr).Str("journal_path", journalPath).
				Msg("composer: leftover commit journal is unreadable; skipping")
			continue
		}
		lj := LeftoverJournal{
			SessionID:   j.SessionID,
			StagingDir:  stageDir,
			JournalPath: journalPath,
			Targets:     j.Targets,
		}
		if lj.SessionID == "" {
			lj.SessionID = sessionID
		}
		for _, t := range j.Targets {
			if strings.HasPrefix(t.RelPath, "projects/") {
				lj.ProjectRelPath = t.RelPath
			}
		}
		out = append(out, lj)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].SessionID < out[k].SessionID })
	return out, nil
}

// FindLeftoverJournalForProject scans for a leftover composer-commit
// journal (design §5.6 step 4) whose project-file target names
// projectID. This is the project-doctor "composer_commit" check's
// detection primitive (internal/projectdoctor) — the daemon-startup
// sweep (RecoverComposerCommits) is the primary recovery mechanism;
// this lets an operator see a commit that's stuck BETWEEN two boots
// on the project's own setup page instead of it being silently
// invisible until the next restart.
func FindLeftoverJournalForProject(liveConfigDir, projectID string) (LeftoverJournal, bool, error) {
	leftovers, err := findLeftoverJournals(liveConfigDir, zerolog.Nop())
	if err != nil {
		return LeftoverJournal{}, false, err
	}
	for _, lj := range leftovers {
		if lj.ProjectID() == projectID {
			return lj, true, nil
		}
	}
	return LeftoverJournal{}, false, nil
}

// RecoveryOutcome classifies one journal's boot-recovery action.
// Reuses vornik_composer_commits_total{tier,result} (design §5.8) —
// these are additional "result" label values on the SAME counter the
// in-process commit path already increments, not a new metric.
type RecoveryOutcome string

const (
	// RecoveryOutcomeCommitted — the commit had fully landed (the
	// project file was already live); recovery only finished the
	// interrupted cleanup and stamped the session committed.
	RecoveryOutcomeCommitted RecoveryOutcome = "recovered_committed"
	// RecoveryOutcomeRolledBack — the commit was partial (the project
	// file never landed); recovery rolled back every journal-listed
	// file that did land and marked the session commit-failed-resumable.
	RecoveryOutcomeRolledBack RecoveryOutcome = "recovered_rolledback"
)

// RecoveredCommit is one journal's recovery result — returned so the
// caller (the boot sweep) can log a precise per-session line without
// re-scanning.
type RecoveredCommit struct {
	SessionID string
	ProjectID string
	Outcome   RecoveryOutcome
	Detail    string
}

// RecoverComposerCommits recovers every leftover composer-commit
// journal under liveConfigDir (design §5.6 step 4; task 1.2b slice
// ii). Callers MUST run this before the daemon starts serving traffic
// that touches the same live config tree (see internal/service's
// Container.recoverComposerCommits, called from Run() before the
// scheduler starts).
//
// Per journal, this branches on the ONE signal that distinguishes a
// fully-landed commit from a partial one — whether the project file
// (the activating reference, landed LAST by stageAndCommitBundle) is
// already present in the live tree:
//
//   - PRESENT: the commit fully landed; only the post-success cleanup
//     was interrupted. Recovery FINISHES it — delete the staging dir +
//     journal ONLY, never touch a live file, and (best-effort) stamp
//     the session committed, mirroring commitBundleSession's own
//     success tail.
//   - ABSENT: a partial commit that never activated. Recovery rolls
//     back every journal-listed file that DID land (workflows/swarm —
//     via the SAME rollbackLandedTargets primitive the in-process
//     failure path uses), deletes the staging dir + journal, and marks
//     the session commit-failed-resumable.
//
// Idempotent: recovering a journal removes it, so a second call over
// the same liveConfigDir finds nothing left to do. Touches ONLY
// journal-listed live paths (and only on the ABSENT branch) plus each
// journal's own staging dir — nothing else in the live tree, ever. A
// nil/empty sessions store is tolerated (session-stamping is
// best-effort; the on-disk recovery itself never depends on it).
func RecoverComposerCommits(ctx context.Context, liveConfigDir string, sessions SessionStore, metrics *Metrics, logger zerolog.Logger) ([]RecoveredCommit, error) {
	if strings.TrimSpace(liveConfigDir) == "" {
		return nil, nil
	}
	leftovers, err := findLeftoverJournals(liveConfigDir, logger)
	if err != nil {
		return nil, err
	}
	out := make([]RecoveredCommit, 0, len(leftovers))
	for _, lj := range leftovers {
		out = append(out, recoverOneJournal(ctx, liveConfigDir, sessions, metrics, logger, lj))
	}
	return out, nil
}

func recoverOneJournal(ctx context.Context, liveConfigDir string, sessions SessionStore, metrics *Metrics, logger zerolog.Logger, lj LeftoverJournal) RecoveredCommit {
	projectID := lj.ProjectID()
	if lj.ProjectFileLive(liveConfigDir) {
		return finishInterruptedCleanup(ctx, sessions, metrics, logger, lj, projectID)
	}
	return rollbackPartialCommit(ctx, liveConfigDir, sessions, metrics, logger, lj, projectID)
}

// finishInterruptedCleanup is the PRESENT branch: the commit fully
// landed (the project file is live), so the ONLY thing left to do is
// finish the cleanup the crash interrupted — remove the staging dir
// (and, since it lives inside it, the journal) and stamp the session
// committed. It NEVER removes a live file: rollbackLandedTargets is
// simply never called on this branch.
func finishInterruptedCleanup(ctx context.Context, sessions SessionStore, metrics *Metrics, logger zerolog.Logger, lj LeftoverJournal, projectID string) RecoveredCommit {
	if rmErr := removeAllFn(lj.StagingDir); rmErr != nil {
		logger.Warn().Err(rmErr).Str("staging_dir", lj.StagingDir).
			Msg("composer: recovery could not remove the staging dir of a fully-landed commit; will retry next boot")
	}
	detail := "commit had fully landed; finished the interrupted cleanup"
	if sessions != nil && lj.SessionID != "" {
		if cErr := sessions.CommitTo(ctx, lj.SessionID, projectID); cErr != nil &&
			!errors.Is(cErr, persistence.ErrInvalidTransition) && !errors.Is(cErr, persistence.ErrNotFound) {
			logger.Warn().Err(cErr).Str("session_id", lj.SessionID).
				Msg("composer: recovery could not stamp the session committed (the files are live regardless)")
			detail += "; session stamp failed: " + cErr.Error()
		}
	}
	metrics.recordComposerCommit(composerCommitTier3, string(RecoveryOutcomeCommitted))
	logger.Info().Str("session_id", lj.SessionID).Str("project_id", projectID).
		Msg("composer: recovered a fully-landed commit whose cleanup was interrupted")
	return RecoveredCommit{SessionID: lj.SessionID, ProjectID: projectID, Outcome: RecoveryOutcomeCommitted, Detail: detail}
}

// rollbackPartialCommit is the ABSENT branch: the project file never
// landed, so the commit never activated. Recovery rolls back every
// journal-listed target (a no-op for any that never landed —
// rollbackLandedTargets already tolerates "already gone"), removes
// the staging dir + journal, and marks the session commit-failed-
// resumable the same way failBundleCommit does in-process.
func rollbackPartialCommit(ctx context.Context, liveConfigDir string, sessions SessionStore, metrics *Metrics, logger zerolog.Logger, lj LeftoverJournal, projectID string) RecoveredCommit {
	rollbackLandedTargets(liveConfigDir, lj.Targets)
	if rmErr := removeAllFn(lj.StagingDir); rmErr != nil {
		logger.Warn().Err(rmErr).Str("staging_dir", lj.StagingDir).
			Msg("composer: recovery could not remove the staging dir after rollback; will retry next boot")
	}
	reason := "the daemon restarted mid-commit; the build was rolled back and is safe to retry"
	if sessions != nil && lj.SessionID != "" {
		if s, gErr := sessions.Get(ctx, lj.SessionID); gErr == nil && s != nil {
			now := time.Now().UTC()
			s.BundleCommitFailedAt = &now
			s.BundleCommitError = reason
			if uErr := sessions.Update(ctx, s); uErr != nil {
				logger.Warn().Err(uErr).Str("session_id", lj.SessionID).
					Msg("composer: recovery could not persist the commit-failed-resumable marker")
			}
		} else if gErr != nil && !errors.Is(gErr, persistence.ErrNotFound) {
			logger.Warn().Err(gErr).Str("session_id", lj.SessionID).
				Msg("composer: recovery could not load the session to mark it resumable")
		}
	}
	metrics.recordComposerCommit(composerCommitTier3, string(RecoveryOutcomeRolledBack))
	logger.Warn().Str("session_id", lj.SessionID).Str("project_id", projectID).
		Msg("composer: rolled back a partial commit found at startup")
	return RecoveredCommit{SessionID: lj.SessionID, ProjectID: projectID, Outcome: RecoveryOutcomeRolledBack, Detail: reason}
}
