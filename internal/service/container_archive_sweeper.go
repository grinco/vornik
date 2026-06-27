package service

// Wires the project-archive deletion sweeper into the daemon
// lifecycle. The sweeper itself lives in internal/projectarchive;
// this file is the container glue (constructing it from already-
// wired deps + starting its goroutine inside Run).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/projectarchive"
)

// daemonHolderID returns the per-daemon-instance string every
// leader-election Elector uses as its holder_id. Composed of
// hostname + pid + a boot-time random nonce so a daemon
// restart's holder_id differs from its predecessor's (an old
// crashed leader's row gets taken over after the TTL).
//
// Computed lazily + cached on the container. Multi-elector
// callers share the same value so the doctor check can match
// "this daemon" across worker rows.
func (c *Container) daemonHolderID() string {
	if c.daemonHolderIDValue != "" {
		return c.daemonHolderIDValue
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	var nonce [4]byte
	_, _ = rand.Read(nonce[:])
	c.daemonHolderIDValue = fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(nonce[:]))
	return c.daemonHolderIDValue
}

// initArchiveLifecycle builds the shared archive lifecycle
// service (used by UI handlers + REST API + future vornikctl
// CLI). The YAML patcher is passed in as a closure so this
// file doesn't pull internal/ui (and avoid the import cycle
// that would create). Service Reload calls Registry.Reload
// directly so the in-memory state picks up the YAML change.
//
// Sweeper is wired here too when initArchiveSweeper has already
// run; otherwise the caller's "Delete now" path falls back to
// the next regular tick.
func (c *Container) initArchiveLifecycle(patcher projectarchive.YAMLPatcher) *projectarchive.LifecycleService {
	if c.Registry == nil {
		return nil
	}
	if patcher == nil {
		c.Logger.Warn().Msg("archive-lifecycle: no patcher wired; archive endpoints will return 503")
		return nil
	}
	return &projectarchive.LifecycleService{
		ConfigDir: c.Registry.GetConfigDir(),
		Patcher:   patcher,
		Reload: func(ctx context.Context) error {
			return c.Registry.Reload()
		},
		Sweeper: c.archiveSweeper,
	}
}

// initArchiveSweeper wires the project-archival deletion runner.
// Returns nil when the required deps (registry + DB) aren't
// available — the daemon still boots, just without auto-deletion
// (the UI's archive buttons still write the YAML, and a future
// daemon restart with the deps wired picks up overdue projects).
func (c *Container) initArchiveSweeper() *projectarchive.Sweeper {
	if c.Registry == nil {
		c.Logger.Warn().Msg("archive-sweeper: registry not wired; skipping")
		return nil
	}
	if c.DB == nil {
		// The sweeper is willing to run without a DataDeleter,
		// but the user explicitly asked for "config files + DB
		// rows + artifacts" so refuse to start without a DB.
		// Operators with non-postgres backends won't have the
		// data-cleanup wired yet; the YAML+artifact wipe still
		// fires on the next restart that does have it.
		c.Logger.Warn().Msg("archive-sweeper: DB not wired; skipping")
		return nil
	}
	deleter := postgres.NewProjectDataCleanupRepository(c.DB)
	cfg := projectarchive.Config{
		Registry:    c.Registry,
		DataDeleter: deleter,
		ConfigDir:   c.Registry.GetConfigDir(),
		AuditRepo:   c.repos.AdminAudit,
		Reload: func(ctx context.Context) error {
			return c.Registry.Reload()
		},
		Logger: c.Logger.With().Str("component", "archive-sweeper").Logger(),
	}
	// Prefer the backend-aware wiper so S3-backed deployments
	// actually delete their blobs (the legacy fs-path fallback
	// only works against local disk). The wiper falls back
	// automatically when the store isn't wired.
	if c.artifactStore != nil {
		cfg.ArtifactWiper = c.artifactStore
	} else {
		cfg.ArtifactBasePath = c.Config.Storage.ArtifactsPath
	}
	// Leader election (2026.8.0 prep). Only the elected leader
	// runs the wipe in a multi-replica deployment. Wired only
	// when the repo is available (Postgres branch); SQLite's
	// stub always grants the lock so single-process deployments
	// behave identically.
	if c.repos != nil && c.repos.LeaderLocks != nil {
		c.archiveSweeperElector = leaderelection.New(
			c.repos.LeaderLocks,
			"archive_sweeper",
			c.daemonHolderID(),
			leaderelection.DefaultTTL,
			c.Logger.With().Str("component", "leader-election").Logger(),
		)
		cfg.LeaderGate = c.archiveSweeperElector
	}
	return projectarchive.NewSweeper(cfg)
}
