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

	"vornik.io/vornik/internal/graphsweep"
)

// Defaults for the retention windows, in days. Each per-project field
// inherits from here when zero. Intentionally conservative — expanding
// the retention is a config change; shortening it is a policy decision.
const (
	DefaultTaskLLMUsageDays = 90
	DefaultToolAuditDays    = 30
	// DefaultChatAuditDays bounds chat_audit_log. 90, deliberately longer
	// than DefaultTasksDays (60), so a task's origin row outlives by a month
	// any task that could still need it for delivery — the reference guard in
	// pruneChatAuditLog is then belt-and-braces rather than load-bearing.
	//
	// It is also an upgrade horizon, not just a cleanup one: this table has
	// never been swept, so the first sweep after this ships is the only one
	// that will ever remove years at once. See
	// https://docs.vornik.io §4.1.
	DefaultChatAuditDays  = 90
	DefaultTasksDays      = 60
	DefaultExecutionsDays = 60
	DefaultArtifactsDays  = 60
	// DefaultTaskMessagesDays is 0 — independent prune disabled by
	// default. Parent-task retention cascades to messages via FK; an
	// explicit setting only matters when operators want messages
	// trimmed faster than terminal tasks.
	DefaultTaskMessagesDays = 0
	// DefaultMemoryChunksDays is 0 — class TTL handles ordinary
	// retention. Operator opt-in escape hatch.
	DefaultMemoryChunksDays = 0
	// DefaultGraphQuarantineDays is 30 — long enough that a misconfigured TTL
	// noticed within a month is recoverable with UpdateLifecycle, short enough
	// that derived personal data does not outlive the policy that expired its
	// source indefinitely.
	DefaultGraphQuarantineDays = 30
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
	// DefaultMemoryEvictionAuditDays bounds memory_eviction_audit and
	// memory_eviction_runs — the hard-eviction tombstones and their headers.
	//
	// 365, matching MemoryPolicyEvalBlockDays and for the same reason: this is
	// the compliance trail, not a dense operational one. Both tables are
	// registered erasure-exempt under Art 17(3)(b) (erasing them would destroy
	// the evidence that an erasure happened) and NEITHER was swept by anything —
	// they appeared in no allowlist and grew without bound.
	//
	// EXEMPT FROM ERASURE IS NOT EXEMPT FROM RETENTION. That rule is already
	// written down: 2026-07-30-chat-memory-write-design.md §5.3.4 requires an
	// audit table to settle three things — registration as erasure-exempt BY
	// NAME, disclosure in the retained-categories report, and sweeping on its own
	// horizon. The first two were done for both tables; the third was done for
	// neither. Indefinite retention of a table holding an operator identifier and
	// a free-text reason sits badly with Art 5(1)(e) whatever ground justifies
	// keeping it for a while.
	//
	// Priced honestly: production holds 1,240 tombstones across almost three
	// months, and eviction is an operator action rather than a hot path. This is
	// a correctness-of-posture fix, not a capacity one — which is why the horizon
	// is generous rather than tight.
	DefaultMemoryEvictionAuditDays = 365
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
	// GraphQuarantineDays bounds how long knowledge-graph rows parked by
	// this sweeper survive before being hard-deleted. Always-on; zero → the
	// compiled default. Parking bounds visibility, not retention — see
	// graphsweep.PurgeQuarantined.
	GraphQuarantineDays int
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
	// MemoryEvictionAuditDays prunes memory_eviction_audit and
	// memory_eviction_runs by evicted_at. Always-on (default 365).
	// See DefaultMemoryEvictionAuditDays.
	MemoryEvictionAuditDays int
	// ChatAuditDays prunes chat_audit_log by ts. Always-on (default 90).
	// Rows still referenced by a tasks.chat_turn_id are never pruned however
	// old — they are the only record of where a finished task's result gets
	// delivered.
	ChatAuditDays int
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
		MemoryEvictionAuditDays:   pick(perProject.MemoryEvictionAuditDays, defaults.MemoryEvictionAuditDays, DefaultMemoryEvictionAuditDays),
		ChatAuditDays:             pick(perProject.ChatAuditDays, defaults.ChatAuditDays, DefaultChatAuditDays),
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
	if out.ChatAuditDays < MinimumFloorDays {
		out.ChatAuditDays = MinimumFloorDays
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

// addQuarantined accumulates one chunk-prune's graph outcome. Both chunk paths
// feed it, so a project swept by the TTL sweep and the operator ceiling in the
// same cycle reports the sum rather than the last one.
func (c *Counts) addQuarantined(q graphsweep.QuarantineCounts) {
	c.MemoryGraphEdgesQuarantined += q.Edges
	c.MemoryGraphEntitiesQuarantined += q.Entities
}

// Counts reports how many rows were (or would be) pruned in each table.
// Used by both Sweep (actual) and Preview (dry-run).
type Counts struct {
	// MemoryEvictionAudit / MemoryEvictionRuns are the hard-eviction tombstones
	// and their headers, swept together on one horizon. Reported separately
	// because a header surviving its tombstones is the expected steady state
	// within a run's own window, and a reader needs to see which half moved.
	MemoryEvictionAudit int
	MemoryEvictionRuns  int

	TaskLLMUsage int
	ToolAudit    int
	Tasks        int
	Executions   int
	// StepPrompts is the count of step_prompts rows no outcome row referenced
	// any more (pruned after executions, since outcome rows go with them).
	StepPrompts int
	// ChatAuditLog is the count of chat_audit_log rows pruned: past the
	// horizon AND not referenced by any task's chat_turn_id.
	ChatAuditLog int
	// ChatSystemPrompts is the count of chat_system_prompts bodies no
	// chat_audit_log row referenced any more. Like StepPrompts, it has no
	// horizon of its own — a body lives as long as a row points at it.
	ChatSystemPrompts int
	// LLMExchanges is the count of llm_exchanges rows whose execution no
	// longer exists. Like StepPrompts, reference-bound, no horizon of its own.
	LLMExchanges  int
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
	// MemoryExpired is the count of rows hard-deleted from
	// project_memory_chunks by the always-on expires_at sweep — the
	// TTL-verified-to-DELETE step (chat memory-write design §5, parent
	// §6.4). Distinct from MemoryChunks (the created_at operator cap):
	// this honours per-class TTL so a chat_memory chunk past its 90-day
	// horizon is actually deleted, not merely hidden by the retrieval
	// filter. Class-agnostic — any class with a TTL benefits.
	MemoryExpired int
	// MemoryGraphEdgesQuarantined / MemoryGraphEntitiesQuarantined count the
	// knowledge-graph rows a chunk prune left without evidence and PARKED —
	// moved to lifecycle_state 'quarantined', which removes them from every
	// retrieval path while keeping the row auditable and the move reversible.
	//
	// Reported because retention used to delete the chunks and leave these
	// rows published, which is how production accumulated 3,795 entities whose
	// source chunks were already gone. A sweep that silently fixes it would be
	// the same invisibility with the sign flipped.
	MemoryGraphEdgesQuarantined    int
	MemoryGraphEntitiesQuarantined int
	// MemoryGraphPurged counts parked graph rows hard-deleted once their
	// quarantine horizon elapsed — the bound that keeps parking from becoming
	// indefinite retention of derived personal data.
	MemoryGraphPurged int
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
	// EmbeddingCache is the count of rows evicted from embedding_cache
	// whose last_hit_at fell outside the retention window. Zero when
	// EmbeddingCacheDays is 0 (disabled) or nothing matched. Embeddings
	// are deterministic per (content, model), so eviction never changes
	// an answer — a dropped cold entry simply re-embeds on next use.
	EmbeddingCache int
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
func (s *Sweeper) SweepGlobal(ctx context.Context, responseCacheDays, embeddingCacheDays int) (GlobalCounts, error) {
	return s.runGlobal(ctx, responseCacheDays, embeddingCacheDays, false)
}

// PreviewGlobal counts what SweepGlobal would prune without
// deleting. Used by the operator-facing preview surface.
func (s *Sweeper) PreviewGlobal(ctx context.Context, responseCacheDays, embeddingCacheDays int) (GlobalCounts, error) {
	return s.runGlobal(ctx, responseCacheDays, embeddingCacheDays, true)
}

func (s *Sweeper) runGlobal(ctx context.Context, responseCacheDays, embeddingCacheDays int, previewOnly bool) (GlobalCounts, error) {
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

	if embeddingCacheDays > 0 {
		threshold := time.Now().UTC().AddDate(0, 0, -embeddingCacheDays)
		n, err := s.pruneEmbeddingCache(ctx, threshold, previewOnly)
		if err != nil {
			s.warn("embedding_cache", err)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			counts.EmbeddingCache = n
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

// evidenceTables are NEVER prunable, whatever any allowlist says.
//
// EU AI Act Art 50 obliges the provider to disclose that a human is
// interacting with an AI system; Art 99 makes non-compliance enforceable at up
// to €15M or 3% of worldwide turnover from 2 Aug 2026. `channel_disclosure_log`
// is the record that the disclosure WAS served — an obligation met but
// unprovable is worth very little under enforcement, so deleting this trail
// converts a compliant deployment into an indefensible one.
//
// Why a DENYLIST when the two allowlists already omit this table: omission
// protects by accident. Anyone adding a line to `allowedTables` or
// `globalCleanupTables` — reasonably, while wiring some new cleanup — would
// silently start pruning the evidence trail, and nothing would fail. A deny
// checked BEFORE the allowlist protects by assertion instead, and
// TestEvidenceTablesAreNeverPrunable fails the build if this table ever
// appears in an allowlist.
//
// see LLD § https://docs.vornik.io
var evidenceTables = map[string]bool{
	"channel_disclosure_log": true,
}

// globalCleanupTables is the allowlist of global (non-project-scoped) tables
// the threshold cleanups may touch. Closed set — pruneGlobalByThreshold
// rejects anything else so the table name (interpolated into SQL) can never
// be attacker-influenced, mirroring pruneOlderThan's P0 allowlist.
var globalCleanupTables = map[string]bool{
	"ui_sessions":     true,
	"api_keys":        true,
	"link_codes":      true,
	"embedding_cache": true,
}

// pruneGlobalByThreshold hard-deletes rows from a global table whose rows match
// `where` (with $1 = threshold), guarded by to_regclass so a deployment
// missing the table is a no-op rather than a 500. Shared by the ui_sessions /
// api_keys / link_codes cleanups, which differ only in the table and the
// staleness predicate. previewOnly switches DELETE → COUNT.
func (s *Sweeper) pruneGlobalByThreshold(ctx context.Context, table, where string, threshold time.Time, previewOnly bool) (int, error) {
	if evidenceTables[table] {
		return 0, fmt.Errorf("refusing to prune %s: conformity evidence trail (AI Act Art 50 / Art 99) — see evidenceTables", table)
	}
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

// pruneEmbeddingCache evicts cold rows from embedding_cache. Like
// llm_response_cache the table is global (keyed on content_hash +
// model, no project_id), and last_hit_at is the eviction key — a row
// still served on every replay stays warm indefinitely. Eviction is
// always quality-safe: embeddings are deterministic per (content,
// model), so a dropped cold entry simply re-embeds on next use rather
// than returning a stale value. last_hit_at is NOT NULL DEFAULT NOW()
// (migration 41), so `last_hit_at < $1` can't silently skip rows.
func (s *Sweeper) pruneEmbeddingCache(ctx context.Context, threshold time.Time, previewOnly bool) (int, error) {
	return s.pruneGlobalByThreshold(ctx, "embedding_cache", "last_hit_at < $1", threshold, previewOnly)
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
	//
	// TERMINAL-ONLY IS A CONTRACT, not an optimisation: the chat_audit_log
	// prune below protects any row a task still references, and that guard is
	// only sound because a task whose row survives is a task that might still
	// deliver. Widening this filter to non-terminal statuses would silently
	// start collecting the origin records of tasks that can still deliver.
	// See chatAuditLiveTaskWhere.
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

	// 5b. step_prompts — content-addressed parts of each step's first model
	//     request (step-prompt persistence design §6). No horizon of its own:
	//     a part lives exactly as long as an execution_step_outcomes row
	//     references it, and those rows go with their executions above, so
	//     this runs AFTER the execution prune and removes what nothing points
	//     at any more. Project-agnostic by construction — a hash is shared
	//     across projects when the bytes are, and it is unreferenced only when
	//     no project's outcome references it.
	if n, err := s.pruneUnreferencedStepPrompts(ctx, previewOnly); err != nil {
		s.warn("step_prompts", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		counts.StepPrompts = n
	}

	// 5c. chat_audit_log — the per-turn chat record, past its horizon AND
	//     not referenced by a live task (chat-audit retention and redaction
	//     design §4.1). Runs before the prompt-body prune below, which
	//     collects whatever these rows were the last reference to.
	if n, err := s.pruneChatAuditLog(ctx, p.ProjectID,
		now.AddDate(0, 0, -p.ChatAuditDays), previewOnly); err != nil {
		s.warn("chat_audit_log", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		counts.ChatAuditLog = n
	}

	// 5d. chat_system_prompts — content-addressed prompt bodies. Same shape
	//     as step_prompts above: no horizon, project-agnostic, removes what
	//     nothing points at any more.
	if n, err := s.pruneUnreferencedChatPrompts(ctx, previewOnly); err != nil {
		s.warn("chat_system_prompts", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		counts.ChatSystemPrompts = n
	}

	// 5e. llm_exchanges — the recorded model exchanges of agent steps
	//     (llm-exchange record/replay design §3). No horizon of its own: a
	//     row lives exactly as long as its execution, and this sweep — not a
	//     foreign key, which SQLite does not enforce — is what removes the
	//     rows whose execution step 5 (or any other path) deleted. Project-
	//     agnostic: an orphan is an orphan whichever project owned it.
	if n, err := s.pruneOrphanedLLMExchanges(ctx, previewOnly); err != nil {
		s.warn("llm_exchanges", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		counts.LLMExchanges = n
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
		if n, parked, err := s.pruneChunksOlderThan(ctx, p.ProjectID,
			now.AddDate(0, 0, -p.MemoryChunksDays), previewOnly,
		); err != nil {
			s.warn("project_memory_chunks", err)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			counts.MemoryChunks = n
			counts.addQuarantined(parked)
		}
	}

	// 7b. project_memory_chunks — always-on expires_at sweep (chat
	//     memory-write design §5 / parent §6.4). Hard-deletes chunks
	//     whose per-class TTL has elapsed, so a TTL actually DELETES
	//     rather than only hiding the row behind the retrieval filter
	//     (routing.go's `expires_at IS NULL OR expires_at > NOW()`).
	//     Always on (not the opt-in MemoryChunksDays ceiling): a class
	//     TTL that only hides is a compliance gap (Art 17 "without undue
	//     delay"), not a product knob. Runs every retention cycle (6h
	//     cadence, container_autonomy.go initRetention). data_subject_links
	//     do NOT cascade on chunk delete (polymorphic (table_name,row_id),
	//     no FK), so pruneExpiredChunks removes the paired links first.
	if n, parked, err := s.pruneExpiredChunks(ctx, p.ProjectID, now, previewOnly); err != nil {
		s.warn("project_memory_chunks(expires_at)", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		counts.MemoryExpired = n
		counts.addQuarantined(parked)
	}

	// 7c. The parked graph rows, once their grace has run out. Parking removes
	//     a row from retrieval; it does not remove the personal data in it, and
	//     the chunk's TTL already made the storage-limitation decision. Without
	//     this the sweeper would trade one unbounded population (published
	//     stranded rows) for another (parked ones).
	if n, err := s.purgeQuarantinedGraph(ctx, p, now, previewOnly); err != nil {
		s.warn("knowledge_graph(quarantined)", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		counts.MemoryGraphPurged = n
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

	// 10. memory_eviction_audit + memory_eviction_runs — always-on (default
	//     365d). Both are erasure-EXEMPT under Art 17(3)(b) and were swept by
	//     nothing, so they grew without bound. Exempt from erasure is not exempt
	//     from retention (chat memory-write LLD §5.3.4). Ordered last because it
	//     is the newest step and the sweep's order is an observable contract.
	if n, err := s.pruneEvictionEvidence(ctx, p, now, previewOnly); err != nil {
		s.warn("memory_eviction_audit", err)
		if firstErr == nil {
			firstErr = err
		}
	} else {
		counts.MemoryEvictionAudit = n.tombstones
		counts.MemoryEvictionRuns = n.headers
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

// pruneExpiredChunks hard-deletes project_memory_chunks whose
// per-class TTL has elapsed (expires_at < now), for one project. This
// is the mechanism that makes a class TTL actually erase rather than
// only hide the row (chat memory-write design §5, parent §6.4).
//
// The FK cascade is verified, not assumed (review C2). Against
// project_memory_chunks:
//   - entity_mentions.chunk_id, memory_embed_queue.chunk_id and
//     memory_embed_dlq.chunk_id carry ON DELETE CASCADE, so those child
//     rows go automatically with the chunk.
//   - data_subject_links does NOT: it is polymorphic
//     (table_name, row_id) with no foreign key to any one table, so a
//     chunk delete would ORPHAN its links. Erasure's DeleteRow removes
//     the paired links for exactly this reason; the sweep must do the
//     same, in the SAME transaction, deleting the links FIRST so there
//     is never a link pointing at a chunk that is already gone.
//
// Runs even in preview (COUNT only) without touching either table.
func (s *Sweeper) pruneExpiredChunks(
	ctx context.Context, projectID string, now time.Time, previewOnly bool,
) (int, graphsweep.QuarantineCounts, error) {
	return s.pruneChunks(ctx, projectID,
		`expires_at IS NOT NULL AND expires_at < $2`, now, previewOnly)
}

// pruneChunksOlderThan is the operator-level created_at ceiling.
//
// It routes through pruneChunks rather than the generic pruneOlderThan, which
// is what it used to call. That divergence was a bug in two ways: the generic
// helper deletes the chunk rows and nothing else, so this path left orphaned
// data_subject_links (the fix for that landed on the expires_at sweep only,
// review C2) AND left the knowledge graph stranded. One chunk-deletion path
// means one set of consequences.
func (s *Sweeper) pruneChunksOlderThan(
	ctx context.Context, projectID string, threshold time.Time, previewOnly bool,
) (int, graphsweep.QuarantineCounts, error) {
	return s.pruneChunks(ctx, projectID, `created_at < $2`, threshold, previewOnly)
}

// pruneChunks hard-deletes the chunks matching `where` and settles everything
// that pointed at them.
//
// $1 is the project id and $2 the timestamp, so `where` is a constant fragment
// written at the two call sites above — never operator input.
//
// THREE consequences, in an order that is load-bearing:
//
//  1. data_subject_links go first. They are polymorphic ((table_name, row_id))
//     with no foreign key, so nothing cascades them and a deleted chunk would
//     leave a link asserting a person appears in a row that no longer exists.
//  2. What the chunks derived is CAPTURED before they go: entity_mentions
//     cascades with the chunk, so afterwards nothing can say which entities
//     this deletion was responsible for.
//  3. The graph is settled AFTER the delete, so the keep rule is evaluated
//     against the state the deletion produced.
//
// The graph rows are QUARANTINED, not deleted — see graphsweep.Quarantine.
// Retention removed a source because its TTL elapsed; that is not a subject's
// erasure request, so the rows are parked (out of every retrieval path, still
// auditable, still reversible) rather than destroyed.
func (s *Sweeper) pruneChunks(
	ctx context.Context, projectID, where string, threshold time.Time, previewOnly bool,
) (int, graphsweep.QuarantineCounts, error) {
	var parked graphsweep.QuarantineCounts
	if projectID == "" {
		return 0, parked, nil
	}
	if previewOnly {
		var n int
		if err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM project_memory_chunks
			WHERE project_id = $1 AND `+where,
			projectID, threshold).Scan(&n); err != nil {
			return 0, parked, fmt.Errorf("count chunks for retention: %w", err)
		}
		return n, parked, nil
	}

	// BATCHED, one transaction per batch.
	//
	// The graph settlement locks every knowledge_edges row citing a deleted
	// chunk and every candidate entity, and it holds those locks for the whole
	// transaction. Sweeping a project in one go therefore scales the lock set
	// with the project rather than with the work, and ingestion competes for
	// exactly those rows.
	//
	// Measured on production 2026-08-21: only 16 chunks are past their TTL
	// right now, so the always-on sweep is trivial in steady state. The case
	// that is not trivial is a FIRST run — an operator setting
	// MemoryChunksDays on the largest project (8,770 chunks) deletes thousands
	// at once, and so does a TTL sweep after the daemon has been down or a
	// class TTL has been shortened. Batching costs a few more transactions in
	// the common case to bound the uncommon one.
	//
	// Partial progress is real progress and is returned even on failure: each
	// batch commits on its own, so a mid-sweep error leaves the batches that
	// succeeded applied, and the counts must say so.
	total := 0
	for round := 0; ; round++ {
		if round > chunkSweepMaxRounds {
			return total, parked, fmt.Errorf(
				"chunk sweep still finding rows after %d batches of %d for project %q — "+
					"refusing to loop further; %d chunk(s) were deleted, re-run to continue",
				chunkSweepMaxRounds, chunkSweepBatchSize, projectID, total)
		}
		n, got, err := s.pruneChunkBatch(ctx, projectID, where, threshold)
		total += n
		parked.Edges += got.Edges
		parked.Entities += got.Entities
		if err != nil {
			return total, parked, err
		}
		if n == 0 {
			return total, parked, nil
		}
	}
}

const (
	// chunkSweepBatchSize bounds how many chunks one transaction deletes, and
	// with them how many graph rows it locks. Matches the orphaned-entity
	// backfill's batch for the same reason.
	chunkSweepBatchSize = 500
	// chunkSweepMaxRounds backstops a pathological loop — a predicate that
	// keeps matching rows the delete does not remove. 500 × 2000 is far beyond
	// any real project.
	chunkSweepMaxRounds = 2000
)

// pruneChunkBatch deletes up to chunkSweepBatchSize chunks and settles what
// pointed at them, in one transaction.
//
// EVERYTHING IS KEYED BY THE IDS READ AT THE TOP, not by re-running the
// predicate. That is a correctness fix as much as a batching mechanism: the
// unbatched version evaluated the same `where` three times — once for the link
// delete, once for the capture, once for the chunk delete — and under READ
// COMMITTED each statement takes its own snapshot, so a chunk committed by an
// ingest mid-transaction with an already-elapsed TTL could be deleted by the
// third statement without having been captured by the second, stranding its
// graph rows. Pinning the set closes that.
//
// Order within the batch is load-bearing: links first (polymorphic, no FK to
// cascade them), then capture (entity_mentions cascades with the chunk, so
// afterwards nothing can say which entities this deletion was responsible for),
// then the delete, then the graph settlement against the state it produced.
func (s *Sweeper) pruneChunkBatch(
	ctx context.Context, projectID, where string, threshold time.Time,
) (int, graphsweep.QuarantineCounts, error) {
	var parked graphsweep.QuarantineCounts

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, parked, fmt.Errorf("begin chunk sweep: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// ORDER BY id so two sweeps racing on one project take the rows in the
	// same order and queue rather than deadlock.
	chunkIDs, err := scanIDs(ctx, tx, `
		SELECT id FROM project_memory_chunks
		WHERE project_id = $1 AND `+where+`
		ORDER BY id
		LIMIT $3`, projectID, threshold, chunkSweepBatchSize)
	if err != nil {
		return 0, parked, fmt.Errorf("collect chunks for retention: %w", err)
	}
	if len(chunkIDs) == 0 {
		if err := tx.Commit(); err != nil {
			return 0, parked, fmt.Errorf("commit empty chunk sweep: %w", err)
		}
		return 0, parked, nil
	}
	ids := pq.Array(chunkIDs)

	// Links first — no FK cascade backs them (review C2).
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM data_subject_links
		WHERE table_name = 'project_memory_chunks' AND row_id = ANY($1)`,
		ids); err != nil {
		return 0, parked, fmt.Errorf("delete pruned-chunk subject links: %w", err)
	}

	entityIDs, err := graphsweep.CaptureEntities(ctx, tx, chunkIDs)
	if err != nil {
		return 0, parked, err
	}

	// The predicate is RE-CHECKED here, and the delete RETURNS what it actually
	// removed. Both halves matter:
	//
	//   - Re-checking means a chunk that stopped matching between the id read
	//     and now (a TTL extended, a row restored) is not deleted on the
	//     strength of a stale read. The id set can only NARROW.
	//   - Returning the removed ids means the graph settlement runs on exactly
	//     those. Passing the read-time set would prune a still-live chunk's id
	//     out of its edges' source_chunks — destroying provenance for a chunk
	//     that still exists, which is worse than the stranding this fixes.
	// Parameter order is $1 project, $2 threshold, $3 ids — the `where`
	// fragment is written against $2 and is shared with the id read above, so
	// the threshold must keep that position in both statements.
	deleted, err := scanIDs(ctx, tx, `
		DELETE FROM project_memory_chunks
		WHERE project_id = $1 AND id = ANY($3) AND `+where+`
		RETURNING id`, projectID, threshold, ids)
	if err != nil {
		return 0, parked, fmt.Errorf("delete chunks for retention: %w", err)
	}
	if len(deleted) == 0 {
		if err := tx.Commit(); err != nil {
			return 0, parked, fmt.Errorf("commit chunk sweep: %w", err)
		}
		return 0, parked, nil
	}

	if parked, err = graphsweep.Quarantine(ctx, tx, deleted, entityIDs); err != nil {
		return 0, parked, err
	}

	if err := tx.Commit(); err != nil {
		return 0, parked, fmt.Errorf("commit chunk sweep: %w", err)
	}
	return len(deleted), parked, nil
}

// purgeQuarantinedGraph hard-deletes graph rows parked longer than the horizon.
//
// Always-on with a compiled default, like the expires_at chunk sweep and for
// the same reason: a TTL that only hides is a compliance gap rather than a
// product knob, and a parked row that never expires is that gap one level down.
func (s *Sweeper) purgeQuarantinedGraph(
	ctx context.Context, p Policy, now time.Time, previewOnly bool,
) (int, error) {
	// Negative DISABLES the purge outright — the legal-hold escape hatch. An
	// always-on default is the compliance-forward posture, but "preserve
	// everything pending a dispute" is a real instruction and an operator who
	// receives one needs a way to obey it that is not recompiling. Zero still
	// means "use the default", matching every other always-on window here.
	if p.GraphQuarantineDays < 0 {
		return 0, nil
	}
	days := p.GraphQuarantineDays
	if days == 0 {
		days = DefaultGraphQuarantineDays
	}
	before := now.AddDate(0, 0, -days)

	if previewOnly {
		// The SAME predicates the purge applies, including the keep-rule
		// re-check on entities and the evidence-less condition on edges.
		// Counting every parked row past the horizon would overstate: rows
		// that regained evidence while parked are not purged, and a preview
		// that promises a bigger deletion than happens is a small lie about
		// personal data.
		var n int
		if err := s.db.QueryRowContext(ctx, `
			SELECT
			  (SELECT COUNT(*) FROM knowledge_entities ke
			     WHERE ke.project_id = $1 AND ke.lifecycle_state = 'quarantined'
			       AND ke.quarantined_at IS NOT NULL AND ke.quarantined_at < $2
			       AND NOT (`+graphsweep.StillEvidenced+`))
			+ (SELECT COUNT(*) FROM knowledge_edges
			     WHERE project_id = $1 AND lifecycle_state = 'quarantined'
			       AND quarantined_at IS NOT NULL AND quarantined_at < $2
			       AND cardinality(source_chunks) = 0)
		`, p.ProjectID, before).Scan(&n); err != nil {
			return 0, fmt.Errorf("count quarantined graph rows: %w", err)
		}
		return n, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin quarantined-graph purge: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	counts, err := graphsweep.PurgeQuarantined(ctx, tx, p.ProjectID, before)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit quarantined-graph purge: %w", err)
	}
	return counts.Total(), nil
}

// scanIDs reads a single-column id query into a slice.
func scanIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// pruneOlderThan runs a COUNT or DELETE on table rows older than threshold

type evictionSweepCounts struct {
	tombstones int
	headers    int
}

// pruneEvictionEvidence sweeps memory_eviction_audit and memory_eviction_runs on
// one horizon (design: the chat memory-write LLD §5.3.4 rule that an audit table
// exempt from ERASURE is not exempt from RETENTION).
//
// ORDER IS LOAD-BEARING, and the schema enforces it. memory_eviction_audit.run_id
// references memory_eviction_runs ON DELETE RESTRICT, so a sweep that reached the
// headers first would FAIL rather than orphan or cascade — which is the intended
// direction, but only if we never rely on it. Tombstones go first, deliberately.
//
// THE TWO TABLES EXPIRE TOGETHER, on one window rather than two. They are halves
// of one record: a header says what an eviction removed beyond the chunks, and
// the tombstones account for the chunks. Independent windows would leave a
// header whose tombstones are gone (an eviction with no evidence of what it
// evicted) or tombstones whose header is gone (chunks accounted for by no
// operation) — both worse than keeping or dropping the pair.
//
// A HEADER IS ONLY REMOVED ONCE NOTHING REFERENCES IT. Within one window the
// tombstones and their header share an evicted_at, so they age out together; but
// a header written just before the cutoff with tombstones written just after it
// would otherwise hit the RESTRICT and fail the whole sweep. The NOT EXISTS makes
// that case a no-op this run and a clean delete the next, instead of an error
// every run forever.
//
// KNOWN LIMIT, stated rather than left to be discovered. The backlog item that
// prompted this asks for evictions tied to a DATA-SUBJECT REQUEST to expire with
// that request's own record. Nothing links them today — neither table carries a
// request id and no producer writes one — so implementing that half would mean
// inventing a column nothing fills, which is worse than the fixed horizon. The
// horizon here is the operator-accountability one; the request-linked variant
// needs the linkage first.
func (s *Sweeper) pruneEvictionEvidence(ctx context.Context, p Policy, now time.Time, previewOnly bool) (evictionSweepCounts, error) {
	var out evictionSweepCounts
	cutoff := now.AddDate(0, 0, -p.MemoryEvictionAuditDays)

	tomb, err := s.pruneOlderThan(ctx,
		"memory_eviction_audit", "evicted_at",
		"project_id = $2", p.ProjectID,
		cutoff, previewOnly,
	)
	if err != nil {
		return out, err
	}
	out.tombstones = tomb

	// In preview the tombstones above were counted, not deleted, so a header
	// still has referencing rows and the NOT EXISTS would report zero. Count the
	// headers that WOULD become unreferenced once the tombstone sweep lands —
	// otherwise a preview understates its own effect, which is the one thing a
	// preview must not do.
	headerWhere := `project_id = $2 AND NOT EXISTS (
		SELECT 1 FROM memory_eviction_audit a
		 WHERE a.run_id = memory_eviction_runs.id`
	if previewOnly {
		headerWhere += ` AND a.evicted_at >= $1`
	}
	headerWhere += `)`

	heads, err := s.pruneOlderThan(ctx,
		"memory_eviction_runs", "evicted_at",
		headerWhere, p.ProjectID,
		cutoff, previewOnly,
	)
	if err != nil {
		return out, err
	}
	out.headers = heads
	return out, nil
}

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
		"step_prompts":              true,
		"chat_audit_log":            true,
		"project_memory_chunks":     true,
		"memory_ingest_audit":       true,
		"memory_policy_evaluations": true,
		"memory_eviction_audit":     true,
		"memory_eviction_runs":      true,
	}
	allowedCols := map[string]bool{
		"ts":           true,
		"recorded_at":  true,
		"created_at":   true,
		"updated_at":   true,
		"ingested_at":  true,
		"evaluated_at": true,
		"evicted_at":   true,
	}
	if evidenceTables[table] {
		return 0, fmt.Errorf("refusing to prune %s: conformity evidence trail (AI Act Art 50 / Art 99) — see evidenceTables", table)
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

// stepPromptsUnreferencedWhere is the reference check shared by the preview
// count and the delete: a part is unreferenced when no outcome row points at it
// through ANY of the five hash columns — the three prompt parts (migration
// 175) and the two boundary files (migration 178, step-I/O persistence design
// §3). Portable SQL (NOT EXISTS, no dialect functions) so it reads identically
// on Postgres and SQLite; the partial indexes make each probe an index lookup.
// One constant, one predicate: a sixth part is added HERE and in both
// repositories' PruneUnreferenced, and TestStepPromptsPredicateNamesEveryColumn
// fails until it is.
const stepPromptsUnreferencedWhere = `
	NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.prompt_system_hash = step_prompts.hash)
	AND NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.prompt_user_hash = step_prompts.hash)
	AND NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.prompt_tools_hash = step_prompts.hash)
	AND NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.input_hash = step_prompts.hash)
	AND NOT EXISTS (SELECT 1 FROM execution_step_outcomes o WHERE o.result_hash = step_prompts.hash)`

// chatAuditLiveTaskWhere protects a chat_audit_log row that some task still
// points at. A chat-originated task carries the row's PK in
// tasks.chat_turn_id, and chatorigin resolves it to decide where the finished
// result is delivered; nothing backfills it, so a pruned row makes the
// deliverable permanently unsendable.
//
// Keyed on the task ROW existing, not on its status. `tasks` is pruned
// terminal-only (see the tasks step in run()), so a task that could still
// deliver is a task whose row is still there — and once its row goes, there is
// nothing left to deliver from and the origin row protects nothing. The two
// disappear together, in that order.
//
// PERFORMANCE: the correlated NOT EXISTS is only cheap because
// tasks.chat_turn_id is indexed on both drivers — `idx_tasks_chat_turn_id`
// (migration `tasks_chat_turn_id`) and `idx_tasks_chat_turn` (sqlite schema,
// partial on NOT NULL). Without one, this walks tasks once per candidate row.
// Verified 2026-09-04 on review; if either index is ever dropped, this prune
// is where it will be felt.
//
// THE TASKS PRUNE BEING TERMINAL-ONLY IS LOAD-BEARING HERE. Widening it to
// non-terminal rows would silently start collecting origin records for tasks
// that can still deliver. Design §4.1.
const chatAuditLiveTaskWhere = `
	NOT EXISTS (SELECT 1 FROM tasks t WHERE t.chat_turn_id = chat_audit_log.id)`

// pruneChatAuditLog removes (or, in preview, counts) chat_audit_log rows past
// the horizon for one project, except those a task still references.
func (s *Sweeper) pruneChatAuditLog(ctx context.Context, projectID string, threshold time.Time, previewOnly bool) (int, error) {
	return s.pruneOlderThan(ctx,
		"chat_audit_log", "ts",
		"project_id = $2 AND"+chatAuditLiveTaskWhere,
		projectID, threshold, previewOnly)
}

// chatPromptsUnreferencedWhere matches prompt bodies nothing points at.
const chatPromptsUnreferencedWhere = `
	NOT EXISTS (SELECT 1 FROM chat_audit_log c WHERE c.system_prompt_hash = chat_system_prompts.hash)`

// pruneUnreferencedChatPrompts removes (or, in preview, counts) the
// chat_system_prompts rows no chat_audit_log row references any more. The
// sibling of pruneUnreferencedStepPrompts, and project-agnostic for the same
// reason: a hash is shared across projects when the bytes are.
func (s *Sweeper) pruneUnreferencedChatPrompts(ctx context.Context, previewOnly bool) (int, error) {
	if previewOnly {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_system_prompts WHERE`+chatPromptsUnreferencedWhere).Scan(&n); err != nil {
			return 0, fmt.Errorf("count unreferenced chat_system_prompts: %w", err)
		}
		return n, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM chat_system_prompts WHERE`+chatPromptsUnreferencedWhere)
	if err != nil {
		return 0, fmt.Errorf("delete unreferenced chat_system_prompts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected on chat_system_prompts prune: %w", err)
	}
	return int(n), nil
}

// pruneUnreferencedStepPrompts removes (or, in preview, counts) the step_prompts
// rows nothing references. The preview counts exactly what the delete would
// remove — a preview that understates its own effect is the one thing a
// preview must not do.
func (s *Sweeper) pruneUnreferencedStepPrompts(ctx context.Context, previewOnly bool) (int, error) {
	if previewOnly {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM step_prompts WHERE`+stepPromptsUnreferencedWhere).Scan(&n); err != nil {
			return 0, fmt.Errorf("count unreferenced step_prompts: %w", err)
		}
		return n, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM step_prompts WHERE`+stepPromptsUnreferencedWhere)
	if err != nil {
		return 0, fmt.Errorf("delete unreferenced step_prompts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(n), nil
}

// llmExchangesOrphanedWhere selects exchanges whose execution row is gone.
const llmExchangesOrphanedWhere = ` NOT EXISTS (SELECT 1 FROM executions e WHERE e.id = llm_exchanges.execution_id)`

// pruneOrphanedLLMExchanges is retention step 5e: reference-bound, on both
// backends, guarded by to_regclass-free syntax so a database predating
// migration 177 that somehow lacks the table fails loudly rather than
// silently reporting zero.
func (s *Sweeper) pruneOrphanedLLMExchanges(ctx context.Context, previewOnly bool) (int, error) {
	if previewOnly {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM llm_exchanges WHERE`+llmExchangesOrphanedWhere).Scan(&n); err != nil {
			return 0, fmt.Errorf("count orphaned llm_exchanges: %w", err)
		}
		return n, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM llm_exchanges WHERE`+llmExchangesOrphanedWhere)
	if err != nil {
		return 0, fmt.Errorf("delete orphaned llm_exchanges: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(n), nil
}
