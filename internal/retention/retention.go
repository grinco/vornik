// Package retention prunes historical operational state older than
// configured thresholds. It runs inside the daemon as a background
// goroutine and is also exposed via `vornikctl retention [preview]`.
//
// What gets pruned:
//   - task_llm_usage   — cost history
//   - tool_audit_log   — debug audit entries
//   - terminal tasks + their cascaded executions
//   - terminal executions (stand-alone)
//   - artifacts — both the DB record and the file on disk
//   - orphan worktree directories for long-terminal tasks
//   - task_messages — when TaskMessagesDays > 0 (independent of the
//     parent task's retention; tasks still cascade-delete their
//     messages regardless)
//   - project_memory_chunks — when MemoryChunksDays > 0 (operator
//     opt-in escape hatch on top of per-class TTL)
//
// What is NEVER pruned:
//   - project_memory_chunks unless MemoryChunksDays > 0 (it's the
//     product; per-class TTL handles ordinary retention)
//   - non-terminal tasks or executions (active work)
//   - migrations / schema_version
package retention

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/safepath"

	"github.com/lib/pq"
	"github.com/rs/zerolog"
)

// Defaults for the retention windows, in days. Each per-project field
// inherits from here when zero. Intentionally conservative — expanding
// the retention is a config change; shortening it is a policy decision.
const (
	DefaultTaskLLMUsageDays = 90
	DefaultToolAuditDays    = 30
	DefaultTasksDays        = 60
	DefaultExecutionsDays   = 60
	DefaultArtifactsDays    = 60
	// DefaultTaskMessagesDays is 0 — independent prune disabled by
	// default. Parent-task retention cascades to messages via FK; an
	// explicit setting only matters when operators want messages
	// trimmed faster than terminal tasks.
	DefaultTaskMessagesDays = 0
	// DefaultMemoryChunksDays is 0 — class TTL handles ordinary
	// retention. Operator opt-in escape hatch.
	DefaultMemoryChunksDays = 0
	// DefaultMemoryIngestAuditDays bounds the memory_ingest_audit
	// table (Path A + Path B deposit trail). Always-on at 90 days:
	// without it the table grows forever (mitigation plan §7.3 / §10).
	// 90d matches the cost-history window and is long enough for
	// compliance review while preventing unbounded growth.
	DefaultMemoryIngestAuditDays = 90
	// DefaultMemoryPolicyEvalAllowDays bounds the dense `allow` rows
	// in memory_policy_evaluations. Always-on at 30 days: one row per
	// chunk per recall, so allow rows dominate and accumulate fast.
	// The firewall LLD § Retention specifies 30d for the allow trail.
	// Migration 80's idx_policy_eval_evaluated_at_allow (partial on
	// decision='allow') keeps the sweep query fast.
	DefaultMemoryPolicyEvalAllowDays = 30
	// DefaultMemoryPolicyEvalBlockDays bounds the `block_*` rows — the
	// compliance trail. Always-on at 365 days per the firewall LLD §
	// Retention ("block rows live a year minimum"). Migration 80's
	// idx_policy_eval_decision_recent (partial on decision <> 'allow')
	// supports the sweep.
	DefaultMemoryPolicyEvalBlockDays = 365
	// MinimumFloorDays is the absolute minimum any window can be pinned
	// to regardless of operator config. Protects against typos that
	// would nuke fresh operational data.
	MinimumFloorDays = 1
)

// Policy is the effective (resolved) retention window for one project.
type Policy struct {
	ProjectID        string
	TaskLLMUsageDays int
	ToolAuditDays    int
	TasksDays        int
	ExecutionsDays   int
	ArtifactsDays    int
	// TaskMessagesDays prunes task_messages by created_at when > 0.
	// Zero means "no independent prune" — messages still cascade
	// from their parent task.
	TaskMessagesDays int
	// MemoryChunksDays prunes project_memory_chunks by created_at
	// when > 0. Zero means "no operator-level cap" — class TTL is
	// the only mechanism.
	MemoryChunksDays int
	// MemoryIngestAuditDays prunes memory_ingest_audit by ingested_at.
	// Always-on (default 90); the deposit trail otherwise grows
	// unbounded. See mitigation plan §7.3.
	MemoryIngestAuditDays int
	// MemoryPolicyEvalAllowDays prunes the dense `allow` rows in
	// memory_policy_evaluations by evaluated_at. Always-on (default
	// 30). Without it the firewall audit trail grows forever — the
	// gap the 2026-05-29 audit flagged (§8.3). See firewall LLD §
	// Retention.
	MemoryPolicyEvalAllowDays int
	// MemoryPolicyEvalBlockDays prunes the `block_*` (compliance)
	// rows in memory_policy_evaluations. Always-on (default 365).
	MemoryPolicyEvalBlockDays int
	// ArtifactsRoot is the host-side base path for artifact files. Needed
	// for on-disk unlink when pruning the DB record. Empty disables the
	// filesystem unlink step (DB-only prune).
	ArtifactsRoot string
}

// Resolve combines a per-project policy with daemon-wide defaults, filling
// in zeros from the defaults and applying the minimum floor.
func Resolve(projectID string, perProject, defaults Policy) Policy {
	pick := func(a, b, def int) int {
		if a > 0 {
			return a
		}
		if b > 0 {
			return b
		}
		return def
	}
	out := Policy{
		ProjectID:        projectID,
		TaskLLMUsageDays: pick(perProject.TaskLLMUsageDays, defaults.TaskLLMUsageDays, DefaultTaskLLMUsageDays),
		ToolAuditDays:    pick(perProject.ToolAuditDays, defaults.ToolAuditDays, DefaultToolAuditDays),
		TasksDays:        pick(perProject.TasksDays, defaults.TasksDays, DefaultTasksDays),
		ExecutionsDays:   pick(perProject.ExecutionsDays, defaults.ExecutionsDays, DefaultExecutionsDays),
		ArtifactsDays:    pick(perProject.ArtifactsDays, defaults.ArtifactsDays, DefaultArtifactsDays),
		// TaskMessagesDays / MemoryChunksDays default to 0 — opt-in
		// only — so pick() with DefaultX=0 returns the per-project
		// or default value verbatim. A 0-floor field would mistakenly
		// promote a deliberately-zero value to MinimumFloorDays.
		TaskMessagesDays: pickOptIn(perProject.TaskMessagesDays, defaults.TaskMessagesDays),
		MemoryChunksDays: pickOptIn(perProject.MemoryChunksDays, defaults.MemoryChunksDays),
		// Always-on, like the cost-history window.
		MemoryIngestAuditDays: pick(perProject.MemoryIngestAuditDays, defaults.MemoryIngestAuditDays, DefaultMemoryIngestAuditDays),
		// Firewall audit trail — always-on, split allow/block windows.
		MemoryPolicyEvalAllowDays: pick(perProject.MemoryPolicyEvalAllowDays, defaults.MemoryPolicyEvalAllowDays, DefaultMemoryPolicyEvalAllowDays),
		MemoryPolicyEvalBlockDays: pick(perProject.MemoryPolicyEvalBlockDays, defaults.MemoryPolicyEvalBlockDays, DefaultMemoryPolicyEvalBlockDays),
		ArtifactsRoot:             perProject.ArtifactsRoot,
	}
	if out.ArtifactsRoot == "" {
		out.ArtifactsRoot = defaults.ArtifactsRoot
	}
	// Apply the floor — only to the always-on fields. Opt-in fields
	// (TaskMessages, MemoryChunks) stay at 0 = disabled, otherwise
	// at whatever > 0 the operator chose, clamped to floor below.
	if out.TaskLLMUsageDays < MinimumFloorDays {
		out.TaskLLMUsageDays = MinimumFloorDays
	}
	if out.ToolAuditDays < MinimumFloorDays {
		out.ToolAuditDays = MinimumFloorDays
	}
	if out.TasksDays < MinimumFloorDays {
		out.TasksDays = MinimumFloorDays
	}
	if out.ExecutionsDays < MinimumFloorDays {
		out.ExecutionsDays = MinimumFloorDays
	}
	if out.MemoryIngestAuditDays < MinimumFloorDays {
		out.MemoryIngestAuditDays = MinimumFloorDays
	}
	if out.MemoryPolicyEvalAllowDays < MinimumFloorDays {
		out.MemoryPolicyEvalAllowDays = MinimumFloorDays
	}
	if out.MemoryPolicyEvalBlockDays < MinimumFloorDays {
		out.MemoryPolicyEvalBlockDays = MinimumFloorDays
	}
	if out.ArtifactsDays < MinimumFloorDays {
		out.ArtifactsDays = MinimumFloorDays
	}
	// Opt-in fields: only clamp when > 0 (a non-zero typo of "0"
	// stays at 0 = disabled, but "0.5" — not representable as int
	// — never happens; what DOES happen is operator typos like
	// "1" hour intending "1 day"; floor still applies).
	if out.TaskMessagesDays > 0 && out.TaskMessagesDays < MinimumFloorDays {
		out.TaskMessagesDays = MinimumFloorDays
	}
	if out.MemoryChunksDays > 0 && out.MemoryChunksDays < MinimumFloorDays {
		out.MemoryChunksDays = MinimumFloorDays
	}
	return out
}

// pickOptIn resolves opt-in (default-disabled) fields. Per-project
// non-zero wins; otherwise defaults non-zero wins; otherwise 0
// (disabled). Distinct from `pick` which falls back to the
// compiled-in default day count.
func pickOptIn(perProject, defaults int) int {
	if perProject > 0 {
		return perProject
	}
	if defaults > 0 {
		return defaults
	}
	return 0
}

// Counts reports how many rows were (or would be) pruned in each table.
// Used by both Sweep (actual) and Preview (dry-run).
type Counts struct {
	TaskLLMUsage  int
	ToolAudit     int
	Tasks         int
	Executions    int
	Artifacts     int
	ArtifactFiles int
	// TaskMessages is the count of rows pruned from task_messages
	// by the independent prune. Zero when TaskMessagesDays is 0 or
	// nothing matched.
	TaskMessages int
	// MemoryChunks is the count of rows pruned from
	// project_memory_chunks by the operator-level cap. Zero when
	// MemoryChunksDays is 0 or nothing matched.
	MemoryChunks int
	// MemoryIngestAudit is the count of rows pruned from
	// memory_ingest_audit by the always-on window.
	MemoryIngestAudit int
	// MemoryPolicyEvalAllow / MemoryPolicyEvalBlock count rows pruned
	// from memory_policy_evaluations under the split allow/block
	// windows. The firewall audit trail otherwise grows forever
	// (drift-mitigation §8.3).
	MemoryPolicyEvalAllow int
	MemoryPolicyEvalBlock int
}

// GlobalCounts reports rows pruned from non-project-scoped tables
// (caches keyed on content_hash, model, etc.). Reported separately
// from per-project Counts because the sweep loop runs them
// independently — one global prune per cycle, not once per project.
type GlobalCounts struct {
	// ResponseCache is the count of rows evicted from
	// llm_response_cache whose last_hit_at fell outside the
	// retention window. Zero when ResponseCacheDays is 0 (disabled)
	// or nothing matched.
	ResponseCache int
	// UISessions is the count of expired/revoked browser login
	// sessions hard-deleted from ui_sessions (github-login phase 3).
	// No config knob — a fixed 7-day grace keeps recent rows for
	// audit before they vanish.
	UISessions int
	// APIKeys is the count of expired/revoked api_keys rows
	// hard-deleted from api_keys. No config knob — same fixed 7-day
	// grace as UISessions so per-task agent keys (minted and revoked
	// within seconds) are gone from the table before the next daily
	// sweep. Rows that are still active are never touched.
	APIKeys int
	// LinkCodes is the count of expired/used self-service channel-link
	// codes hard-deleted from link_codes. No config knob — same fixed
	// 7-day grace as UISessions/APIKeys. The table has no writers yet
	// (Phase 4 consumes it); wiring the sweep now means a row can never
	// outlive its grace once Phase 4 starts minting codes.
	LinkCodes int
}

// uiSessionGraceDays is the fixed window kept after a session
// expires or is revoked before the sweeper hard-deletes it. Recent
// dead sessions stay queryable for a short audit window; there is no
// operator knob (the table is small and the grace is conservative).
const uiSessionGraceDays = 7

// apiKeyGraceDays is the fixed window kept after an api_key row
// expires or is revoked before the sweeper hard-deletes it. Matches
// the ui_sessions grace — 7 days provides a short audit window while
// preventing unbounded growth from per-task agent keys (each minted
// and immediately revoked at teardown). Active (non-expired,
// non-revoked) rows are never touched.
const apiKeyGraceDays = 7

// linkCodeGraceDays is the fixed window kept after a channel-link code
// expires or is used before the sweeper hard-deletes it. Matches the
// ui_sessions / api_keys grace — link codes are short-lived (a sha256 of a
// one-time code), so a 7-day audit window then a hard delete keeps the table
// from accumulating once Phase 4 starts minting them.
const linkCodeGraceDays = 7

// Sweeper runs retention prunes against the database and filesystem.
type Sweeper struct {
	db     *sql.DB
	logger zerolog.Logger
}

// New constructs a Sweeper. A nil db returns a no-op; callers may still
// invoke Sweep/Preview on it without a panic.
func New(db *sql.DB, logger zerolog.Logger) *Sweeper {
	return &Sweeper{db: db, logger: logger}
}

// Sweep deletes rows older than the resolved windows for projectID.
// Returns the counts removed; errors surface per-table but don't abort
// the whole sweep — a failure to prune artifacts shouldn't stop the
// tasks/executions cleanup, for example.
func (s *Sweeper) Sweep(ctx context.Context, p Policy) (Counts, error) {
	return s.run(ctx, p, false)
}

// Preview counts what Sweep would prune without actually deleting.
func (s *Sweeper) Preview(ctx context.Context, p Policy) (Counts, error) {
	return s.run(ctx, p, true)
}

// SweepGlobal prunes non-project-scoped tables (caches). Runs once
// per cycle, regardless of the project count. Returns counts +
// best-effort error — a failure in one cache doesn't abort the
// others.
func (s *Sweeper) SweepGlobal(ctx context.Context, responseCacheDays int) (GlobalCounts, error) {
	return s.runGlobal(ctx, responseCacheDays, false)
}

// PreviewGlobal counts what SweepGlobal would prune without
// deleting. Used by the operator-facing preview surface.
func (s *Sweeper) PreviewGlobal(ctx context.Context, responseCacheDays int) (GlobalCounts, error) {
	return s.runGlobal(ctx, responseCacheDays, true)
}

func (s *Sweeper) runGlobal(ctx context.Context, responseCacheDays int, previewOnly bool) (GlobalCounts, error) {
	if s == nil || s.db == nil {
		return GlobalCounts{}, nil
	}
	var counts GlobalCounts
	var firstErr error

	if responseCacheDays > 0 {
		threshold := time.Now().UTC().AddDate(0, 0, -responseCacheDays)
		n, err := s.pruneResponseCache(ctx, threshold, previewOnly)
		if err != nil {
			s.warn("llm_response_cache", err)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			counts.ResponseCache = n
		}
	}

	// ui_sessions cleanup — always runs (no config knob). Fixed
	// 7-day grace after expiry/revocation.
	{
		threshold := time.Now().UTC().AddDate(0, 0, -uiSessionGraceDays)
		n, err := s.pruneUISessions(ctx, threshold, previewOnly)
		if err != nil {
			s.warn("ui_sessions", err)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			counts.UISessions = n
		}
	}

	// api_keys cleanup — always runs (no config knob). Fixed 7-day
	// grace keeps recent dead rows for short-window audit. Per-task
	// agent keys are minted and revoked within seconds; without this
	// sweep the table would grow one row per task indefinitely.
	{
		threshold := time.Now().UTC().AddDate(0, 0, -apiKeyGraceDays)
		n, err := s.pruneAPIKeys(ctx, threshold, previewOnly)
		if err != nil {
			s.warn("api_keys", err)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			counts.APIKeys = n
		}
	}

	// link_codes cleanup — always runs (no config knob). Fixed 7-day grace.
	// The table has no writers yet (Phase 4 consumes it); wiring the sweep
	// now guarantees a code can't outlive its grace once minting begins.
	{
		threshold := time.Now().UTC().AddDate(0, 0, -linkCodeGraceDays)
		n, err := s.pruneLinkCodes(ctx, threshold, previewOnly)
		if err != nil {
			s.warn("link_codes", err)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			counts.LinkCodes = n
		}
	}

	return counts, firstErr
}

// globalCleanupTables is the allowlist of global (non-project-scoped) tables
// the threshold cleanups may touch. Closed set — pruneGlobalByThreshold
// rejects anything else so the table name (interpolated into SQL) can never
// be attacker-influenced, mirroring pruneOlderThan's P0 allowlist.
var globalCleanupTables = map[string]bool{
	"ui_sessions": true,
	"api_keys":    true,
	"link_codes":  true,
}

// pruneGlobalByThreshold hard-deletes rows from a global table whose rows match
// `where` (with $1 = threshold), guarded by to_regclass so a deployment
// missing the table is a no-op rather than a 500. Shared by the ui_sessions /
// api_keys / link_codes cleanups, which differ only in the table and the
// staleness predicate. previewOnly switches DELETE → COUNT.
func (s *Sweeper) pruneGlobalByThreshold(ctx context.Context, table, where string, threshold time.Time, previewOnly bool) (int, error) {
	if !globalCleanupTables[table] {
		return 0, fmt.Errorf("forbidden global cleanup table: %s", table)
	}
	var present bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT to_regclass('public.`+table+`') IS NOT NULL`).Scan(&present); err != nil {
		return 0, fmt.Errorf("probe %s: %w", table, err)
	}
	if !present {
		return 0, nil
	}
	if previewOnly {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE `+where, threshold).Scan(&n); err != nil {
			return 0, fmt.Errorf("count %s: %w", table, err)
		}
		return n, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+where, threshold)
	if err != nil {
		return 0, fmt.Errorf("delete %s: %w", table, err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected for %s: %w", table, err)
	}
	return int(aff), nil
}

// pruneUISessions hard-deletes browser login sessions expired or revoked
// before the grace cutoff. A deployment without the table is a no-op.
func (s *Sweeper) pruneUISessions(ctx context.Context, threshold time.Time, previewOnly bool) (int, error) {
	return s.pruneGlobalByThreshold(ctx, "ui_sessions",
		`expires_at < $1 OR (revoked_at IS NOT NULL AND revoked_at < $1)`, threshold, previewOnly)
}

// pruneAPIKeys hard-deletes api_keys rows expired or revoked before the grace
// cutoff. Active (non-expired, non-revoked) rows are never touched.
func (s *Sweeper) pruneAPIKeys(ctx context.Context, threshold time.Time, previewOnly bool) (int, error) {
	return s.pruneGlobalByThreshold(ctx, "api_keys",
		`(expires_at IS NOT NULL AND expires_at < $1) OR (revoked_at IS NOT NULL AND revoked_at < $1)`, threshold, previewOnly)
}

// pruneLinkCodes hard-deletes self-service channel-link codes expired or used
// before the grace cutoff. No writers yet (Phase 4), so this is a no-op today
// — wired now so codes can't accumulate once minting starts.
func (s *Sweeper) pruneLinkCodes(ctx context.Context, threshold time.Time, previewOnly bool) (int, error) {
	return s.pruneGlobalByThreshold(ctx, "link_codes",
		`expires_at < $1 OR (used_at IS NOT NULL AND used_at < $1)`, threshold, previewOnly)
}

// pruneResponseCache evicts cold rows from llm_response_cache. The
// table is global (no project_id) so this isn't routed through
// pruneOlderThan's allowlist. last_hit_at is the eviction key — a
// row that's still being served on every replay stays warm
// indefinitely.
func (s *Sweeper) pruneResponseCache(ctx context.Context, threshold time.Time, previewOnly bool) (int, error) {
	// to_regclass guards against "older deployment without
	// migration 47" — the sweeper shouldn't 500 the whole retention
	// loop because the optional Phase E table isn't there.
	var present bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT to_regclass('public.llm_response_cache') IS NOT NULL`).Scan(&present); err != nil {
		return 0, fmt.Errorf("probe llm_response_cache: %w", err)
	}
	if !present {
		return 0, nil
	}
	if previewOnly {
		var n int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM llm_response_cache WHERE last_hit_at < $1`,
			threshold).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("count llm_response_cache: %w", err)
		}
		return n, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM llm_response_cache WHERE last_hit_at < $1`,
		threshold)
	if err != nil {
		return 0, fmt.Errorf("delete llm_response_cache: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected for llm_response_cache: %w", err)
	}
	return int(aff), nil
}

func (s *Sweeper) run(ctx context.Context, p Policy, previewOnly bool) (Counts, error) {
	if s == nil || s.db == nil {
		return Counts{}, nil
	}
	var counts Counts
	var firstErr error

	now := time.Now().UTC()

	// 1. task_llm_usage — cost history.
	if n, err := s.pruneOlderThan(ctx,
		"task_llm_usage", "recorded_at",
		"project_id = $2", p.ProjectID,
		now.AddDate(0, 0, -p.TaskLLMUsageDays),
		previewOnly,
	); err != nil {
		s.warn("task_llm_usage", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		counts.TaskLLMUsage = n
	}

	// 2. tool_audit_log — debug entries.
	if n, err := s.pruneOlderThan(ctx,
		"tool_audit_log", "created_at",
		"project_id = $2", p.ProjectID,
		now.AddDate(0, 0, -p.ToolAuditDays),
		previewOnly,
	); err != nil {
		s.warn("tool_audit_log", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		counts.ToolAudit = n
	}

	// 3. Artifacts — DB record + file on disk. Do this BEFORE tasks so
	//    the cascaded delete from tasks → artifacts via FK doesn't orphan
	//    files. We read rows, unlink files, then delete DB records.
	artThreshold := now.AddDate(0, 0, -p.ArtifactsDays)
	artFiles, n, err := s.pruneArtifacts(ctx, p.ProjectID, artThreshold, p.ArtifactsRoot, previewOnly)
	if err != nil {
		s.warn("artifacts", err)
		if firstErr == nil {
			firstErr = err
		}
	}
	counts.Artifacts = n
	counts.ArtifactFiles = artFiles

	// 4. Tasks — terminal only. Cascades to executions via FK.
	if n, err := s.pruneOlderThan(ctx,
		"tasks", "updated_at",
		"project_id = $2 AND status IN ('COMPLETED','FAILED','CANCELLED')",
		p.ProjectID,
		now.AddDate(0, 0, -p.TasksDays),
		previewOnly,
	); err != nil {
		s.warn("tasks", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		counts.Tasks = n
	}

	// 5. Executions — stand-alone terminal ones whose task already got
	//    pruned or that outlived the tasks window on their own.
	if n, err := s.pruneOlderThan(ctx,
		"executions", "created_at",
		"project_id = $2 AND status IN ('COMPLETED','FAILED','CANCELLED')",
		p.ProjectID,
		now.AddDate(0, 0, -p.ExecutionsDays),
		previewOnly,
	); err != nil {
		s.warn("executions", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		counts.Executions = n
	}

	// 6. task_messages — independent prune, only when explicitly
	//    enabled (TaskMessagesDays > 0). task_messages has no direct
	//    project_id column — we join via tasks. Cascade from
	//    parent-task retention (step 4) already handles the default
	//    "messages live as long as their task" case.
	if p.TaskMessagesDays > 0 {
		if n, err := s.pruneTaskMessages(ctx, p.ProjectID,
			now.AddDate(0, 0, -p.TaskMessagesDays), previewOnly,
		); err != nil {
			s.warn("task_messages", err)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			counts.TaskMessages = n
		}
	}

	// 7. project_memory_chunks — operator escape hatch (default off).
	//    The class taxonomy's per-class TTL is the primary retention
	//    mechanism; this lets operators apply a hard ceiling on top
	//    when their chunk table grows unbounded.
	if p.MemoryChunksDays > 0 {
		if n, err := s.pruneOlderThan(ctx,
			"project_memory_chunks", "created_at",
			"project_id = $2", p.ProjectID,
			now.AddDate(0, 0, -p.MemoryChunksDays),
			previewOnly,
		); err != nil {
			s.warn("project_memory_chunks", err)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			counts.MemoryChunks = n
		}
	}

	// 8. memory_ingest_audit — always-on (default 90d). Both ingest
	//    paths (companion + agent) write here; without a sweep the
	//    deposit trail grows forever (mitigation plan §7.3 / §8.3).
	if n, err := s.pruneOlderThan(ctx,
		"memory_ingest_audit", "ingested_at",
		"project_id = $2", p.ProjectID,
		now.AddDate(0, 0, -p.MemoryIngestAuditDays),
		previewOnly,
	); err != nil {
		s.warn("memory_ingest_audit", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		counts.MemoryIngestAudit = n
	}

	// 9. memory_policy_evaluations — always-on, split windows. Allow
	//    rows are dense (one per chunk per recall) and swept
	//    aggressively (30d default); block rows are the compliance
	//    trail and live a year (365d default). Without this the
	//    firewall audit table grows forever (drift-mitigation §8.3).
	//    Migration 80's partial indexes (allow-only on evaluated_at,
	//    non-allow on (decision, evaluated_at)) keep both queries fast.
	if n, err := s.pruneOlderThan(ctx,
		"memory_policy_evaluations", "evaluated_at",
		"project_id = $2 AND decision = 'allow'", p.ProjectID,
		now.AddDate(0, 0, -p.MemoryPolicyEvalAllowDays),
		previewOnly,
	); err != nil {
		s.warn("memory_policy_evaluations(allow)", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		counts.MemoryPolicyEvalAllow = n
	}
	if n, err := s.pruneOlderThan(ctx,
		"memory_policy_evaluations", "evaluated_at",
		"project_id = $2 AND decision <> 'allow'", p.ProjectID,
		now.AddDate(0, 0, -p.MemoryPolicyEvalBlockDays),
		previewOnly,
	); err != nil {
		s.warn("memory_policy_evaluations(block)", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		counts.MemoryPolicyEvalBlock = n
	}

	return counts, firstErr
}

// pruneTaskMessages handles the special case for the task_messages
// table — it lacks a project_id column, so we constrain via the
// parent task's project_id. This is a separate function from
// pruneOlderThan because the SQL shape differs (JOIN / IN
// subquery), and the table allowlist in pruneOlderThan would have
// to relax to admit it.
func (s *Sweeper) pruneTaskMessages(ctx context.Context, projectID string, threshold time.Time, previewOnly bool) (int, error) {
	if previewOnly {
		var n int
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM task_messages tm
			JOIN tasks t ON t.id = tm.task_id
			WHERE t.project_id = $1 AND tm.created_at < $2
		`, projectID, threshold).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("count task_messages: %w", err)
		}
		return n, nil
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM task_messages
		WHERE id IN (
			SELECT tm.id FROM task_messages tm
			JOIN tasks t ON t.id = tm.task_id
			WHERE t.project_id = $1 AND tm.created_at < $2
		)
	`, projectID, threshold)
	if err != nil {
		return 0, fmt.Errorf("delete task_messages: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected for task_messages: %w", err)
	}
	return int(aff), nil
}

// pruneOlderThan runs a COUNT or DELETE on table rows older than threshold
// matching extraWhere. $1 is the timestamp threshold; $2+ are bound to
// extraWhereArgs. previewOnly switches DELETE → COUNT.
func (s *Sweeper) pruneOlderThan(ctx context.Context, table, tsCol, extraWhere string, extraWhereArg string, threshold time.Time, previewOnly bool) (int, error) {
	// P0: Strict allowlist for table and column names to prevent SQL injection.
	// These are internal constants in this package today, but we guard them
	// defensively.
	allowedTables := map[string]bool{
		"task_llm_usage":            true,
		"tool_audit_log":            true,
		"tasks":                     true,
		"executions":                true,
		"project_memory_chunks":     true,
		"memory_ingest_audit":       true,
		"memory_policy_evaluations": true,
	}
	allowedCols := map[string]bool{
		"recorded_at":  true,
		"created_at":   true,
		"updated_at":   true,
		"ingested_at":  true,
		"evaluated_at": true,
	}
	if !allowedTables[table] {
		return 0, fmt.Errorf("forbidden table name: %s", table)
	}
	if !allowedCols[tsCol] {
		return 0, fmt.Errorf("forbidden timestamp column: %s", tsCol)
	}

	args := []any{threshold, extraWhereArg}
	if previewOnly {
		q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s < $1 AND %s", table, tsCol, extraWhere)
		var n int
		if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			return 0, fmt.Errorf("count %s: %w", table, err)
		}
		return n, nil
	}
	q := fmt.Sprintf("DELETE FROM %s WHERE %s < $1 AND %s", table, tsCol, extraWhere)
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("delete %s: %w", table, err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected for %s: %w", table, err)
	}
	return int(aff), nil
}

// pruneArtifacts needs special handling because we have to unlink files on
// disk, not just rows. We read matching paths first, delete the file (or
// count it in preview mode), then delete the DB record.
func (s *Sweeper) pruneArtifacts(ctx context.Context, projectID string, threshold time.Time, root string, previewOnly bool) (filesRemoved int, rowsRemoved int, err error) {
	rows, qErr := s.db.QueryContext(ctx,
		`SELECT id, storage_path FROM artifacts WHERE project_id = $1 AND created_at < $2`,
		projectID, threshold,
	)
	if qErr != nil {
		return 0, 0, fmt.Errorf("query artifacts: %w", qErr)
	}
	defer func() { _ = rows.Close() }()

	var toDelete []string
	for rows.Next() {
		var id, storagePath string
		if err := rows.Scan(&id, &storagePath); err != nil {
			return filesRemoved, rowsRemoved, err
		}
		toDelete = append(toDelete, id)
		// File cleanup. Only touch paths under the configured root —
		// never chase symlinks or absolute paths outside. This is
		// belt-and-braces even though storage_path is operator-owned.
		if storagePath != "" && pathWithinRoot(storagePath, root) {
			if previewOnly {
				if _, statErr := os.Stat(storagePath); statErr == nil {
					filesRemoved++
				}
			} else {
				if rmErr := os.Remove(storagePath); rmErr == nil {
					filesRemoved++
				} else if !os.IsNotExist(rmErr) {
					s.logger.Warn().Err(rmErr).Str("path", storagePath).Msg("retention: failed to unlink artifact file")
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return filesRemoved, rowsRemoved, err
	}

	if previewOnly {
		return filesRemoved, len(toDelete), nil
	}
	if len(toDelete) == 0 {
		return 0, 0, nil
	}

	// Batch DELETE by ID. One round-trip, no N+1.
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM artifacts WHERE id = ANY($1)`, pq.Array(toDelete),
	)
	if err != nil {
		return filesRemoved, 0, fmt.Errorf("delete artifacts: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return filesRemoved, 0, fmt.Errorf("rows affected for artifacts: %w", err)
	}
	return filesRemoved, int(aff), nil
}

// pathWithinRoot returns true when path lives under root (cleaned form).
// Empty root means "no filesystem check configured" and we skip the unlink.
// It uses safepath.JoinUnder to evaluate symlinks and prevent escape.
func pathWithinRoot(path, root string) bool {
	if root == "" {
		return false
	}
	cleanRoot := filepath.Clean(root)
	cleanPath := filepath.Clean(path)
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == "." {
		return false
	}
	_, err = safepath.JoinUnder(cleanRoot, rel)
	return err == nil
}

func (s *Sweeper) warn(table string, err error) {
	if s == nil {
		return
	}
	s.logger.Warn().Err(err).Str("table", table).Msg("retention sweep failed on table")
}
