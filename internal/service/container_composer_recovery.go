package service

// NL Automation Composer crash-recovery boot wiring (task 1.2b slice
// ii; design https://docs.vornik.io
// §5.6 step 4). Extracted into its own file (rather than inlined in
// Run()) so the wiring is independently testable without driving the
// whole daemon startup sequence.

import (
	"context"

	"vornik.io/vornik/internal/projectwizard"
)

// recoverComposerCommits runs the composer's crash-recovery sweep once
// at daemon startup — BEFORE the scheduler/dispatcher start touching
// the same live config tree, mirroring the executor-recovery-before-
// dispatch ordering Run() already follows for in-flight task
// executions. A leftover staging dir + commit journal
// (<LiveConfigDir>/.composer-staging/<session>/.composer-commit-<session>.json)
// only survives a daemon crash/OOM mid-commit — stageAndCommitBundle's
// own cleanup runs on every in-process return path — so recovering it
// here closes that window before any request can observe a half-
// landed bundle.
//
// Best-effort: a scan/recovery failure is logged, never fatal to
// boot — an unrecovered leftover is retried on the NEXT boot, and is
// separately surfaced in the meantime by the project-doctor
// composer_commit check (internal/projectdoctor), so it is never
// silently invisible.
func (c *Container) recoverComposerCommits(ctx context.Context) {
	if c == nil || c.repos == nil || c.repos.ProjectWizardSessions == nil {
		return
	}
	configDir := resolveRegistryConfigDir(c.ConfigPath)
	if configDir == "" {
		return
	}
	recovered, err := projectwizard.RecoverComposerCommits(ctx, configDir, c.repos.ProjectWizardSessions, c.projectWizardMetrics, c.Logger)
	if err != nil {
		c.Logger.Warn().Err(err).Msg("composer: startup commit-recovery scan failed")
		return
	}
	for _, r := range recovered {
		c.Logger.Info().
			Str("session_id", r.SessionID).
			Str("project_id", r.ProjectID).
			Str("outcome", string(r.Outcome)).
			Msg("composer: recovered a leftover commit journal at startup")
	}
}
