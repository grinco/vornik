// Package autonomy implements the autonomous task creation loop.
// For each project with autonomy enabled, a lead agent periodically
// evaluates the project goal against current state and schedules
// the next logical tasks.
package autonomy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/budget"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/safepath"
	"vornik.io/vornik/internal/untrusted"
)

// backlogPendingRE matches a markdown checklist line whose box is
// unchecked. Capture group 1 is the bullet prefix (indent + `-`/`*`
// + space + `[ ]` + space). Capture group 2 is the prompt text — the
// inline description the operator wrote next to the checkbox.
var backlogPendingRE = regexp.MustCompile(`^(\s*[-*]\s+\[\s\]\s+)(.*)$`)

// isNoActionSentinel returns true when the planner's response is
// the NO_ACTION no-op signal. The hard rule baked into autonomy
// prompts (see janka's PROJECT_CONTEXT §5: "respond NO_ACTION
// instead of inventing slugs") tells the LLM to emit this literal
// when no work is appropriate. Centralising the check fixes the
// 2026-05-12 bug where a JSON-wrapped {"prompt": "NO_ACTION"}
// bypassed the text-only check at the tool-call-absent branch
// and got created as a real task (task_20260512212548) which
// spent a worker container iteration on a no-op prompt.
//
// Case-insensitive Contains is intentional — the planner may emit
// "no_action", "NoAction", or wrap it in surrounding prose like
// "I think NO_ACTION is appropriate here". All count.
func isNoActionSentinel(s string) bool {
	return strings.Contains(strings.ToUpper(strings.TrimSpace(s)), "NO_ACTION")
}

// Manager runs the autonomous task creation loop for all eligible projects.
type Manager struct {
	client         chat.Provider
	registry       *registry.Registry
	taskRepo       persistence.TaskRepository
	execRepo       persistence.ExecutionRepository
	auditRepo      persistence.ToolAuditRepository
	llmUsageRepo   persistence.TaskLLMUsageRepository
	reservRepo     persistence.BudgetReservationRepository
	steering       steeringNotifier
	evalRepo       persistence.AutonomyEvaluationRepository
	rateLimiter    ratelimit.ProjectLimiter
	budgetNotifier budget.Notifier
	pricing        *pricing.Table
	defaultModel   string
	logger         zerolog.Logger
	metrics        *Metrics
	workspacePath  string
	defaultTimeout time.Duration
	// leaderGate, when non-nil, is consulted at the top of every
	// per-project tick. IsLeader()=false → skip the evaluate call.
	// 2026.8.0 horizontal-scaling prep: only the elected leader
	// schedules autonomy tasks. Without this gate, two daemons
	// running autonomy would double-schedule via create_task.
	// nil leaves the gate open (single-process default).
	leaderGate LeaderGate

	mu         sync.Mutex
	taskCounts map[string]int                // projectID -> tasks created this hour
	hourReset  map[string]time.Time          // projectID -> when the hour window started
	cancelFns  map[string]context.CancelFunc // projectID -> per-project loop cancel function

	wg sync.WaitGroup
}

// LeaderGate is the narrow contract Manager uses to skip ticks
// on non-leader daemons. Satisfied by *leaderelection.Elector.
// The cheap IsLeader() bit gates the top of each tick; the epoch
// fence in createAutonomousTask additionally type-asserts the gate
// to leaderelection.EpochVerifier for the fail-closed B1 check, so
// a gate that only exposes IsLeader() stays on pre-fence behaviour.
type LeaderGate interface {
	IsLeader() bool
}

// Option configures the Manager.
type Option func(*Manager)

// WithLogger sets the logger.
func WithLogger(l zerolog.Logger) Option {
	return func(m *Manager) { m.logger = l }
}

// WithMetrics sets the Prometheus metrics.
func WithMetrics(metrics *Metrics) Option {
	return func(m *Manager) { m.metrics = metrics }
}

// WithLeaderGate gates every per-project tick on the supplied
// election Elector. Non-leader daemons skip create_task entirely
// — required for multi-replica deployments so two daemons don't
// double-schedule autonomous work. nil leaves the gate open
// (single-process default).
func WithLeaderGate(g LeaderGate) Option {
	return func(m *Manager) { m.leaderGate = g }
}

// WithToolAuditRepository enables tool audit logging for autonomous task creation.
func WithToolAuditRepository(repo persistence.ToolAuditRepository) Option {
	return func(m *Manager) { m.auditRepo = repo }
}

// WithLLMUsageRepository wires the per-step LLM usage repo. When set, each
// autonomy tick checks the project's daily and monthly spend against its
// configured budget and skips evaluation if the hard cap is exceeded.
func WithLLMUsageRepository(repo persistence.TaskLLMUsageRepository) Option {
	return func(m *Manager) { m.llmUsageRepo = repo }
}

// WithBudgetReservationRepository wires the reservation ledger so an
// autonomy-created task atomically reserves hard-cap headroom before it's
// inserted (trading-hardening §1). Optional — nil keeps the read-only
// budget gate at tick time.
func WithBudgetReservationRepository(repo persistence.BudgetReservationRepository) Option {
	return func(m *Manager) { m.reservRepo = repo }
}

// steeringNotifier is the narrow contract the manager needs to push a
// steering prompt when an autonomy task is parked for approval. Satisfied by
// *steering.Notifier. (Today autonomy tasks have no originating chat, so this
// no-ops; wired for completeness + a future chat-originated approval path —
// see https://docs.vornik.io)
type steeringNotifier interface {
	NotifySteeringRequired(ctx context.Context, task *persistence.Task, state string)
}

// WithSteeringNotifier wires the steering-notification sink. Optional; nil
// disables it.
func WithSteeringNotifier(n steeringNotifier) Option {
	return func(m *Manager) { m.steering = n }
}

// WithEvaluationRepository wires the durable autonomy audit trail. When
// set, every evaluation tick writes one row recording its outcome and
// the reason — silent rejections stop being invisible to operators.
func WithEvaluationRepository(repo persistence.AutonomyEvaluationRepository) Option {
	return func(m *Manager) { m.evalRepo = repo }
}

// WithBudgetNotifier wires a sink that receives soft- and hard-cap alerts
// from each autonomy tick's budget.Check. Optional — nil keeps the
// log-only behaviour.
func WithBudgetNotifier(n budget.Notifier) Option {
	return func(m *Manager) { m.budgetNotifier = n }
}

// WithRateLimiter wires the shared task-creation rate limiter so the
// autonomy loop respects per-project per-minute/per-hour caps alongside
// the dispatcher and API gates.
func WithRateLimiter(l ratelimit.ProjectLimiter) Option {
	return func(m *Manager) { m.rateLimiter = l }
}

// WithPricing wires the model pricing table used by the cost
// forecast's cold-start fallback. Optional — without it, cold-start
// steps contribute zero to the forecast and history-only mode
// applies.
func WithPricing(t *pricing.Table) Option {
	return func(m *Manager) { m.pricing = t }
}

// WithDefaultModel pins the daemon's VORNIK_LLM_MODEL fallback so
// the cost forecast can resolve roles whose swarm config doesn't
// override the model. Optional — empty disables the
// pricing-fallback path for those steps.
func WithDefaultModel(model string) Option {
	return func(m *Manager) { m.defaultModel = model }
}

// WithWorkspacePath sets the host-side base path for per-project workspaces.
// When set, buildStateContext checks whether PROJECT_CONTEXT.md exists in the
// project workspace and reports its presence to the LLM.
func WithWorkspacePath(p string) Option {
	return func(m *Manager) { m.workspacePath = p }
}

// WithDefaultEvaluateTimeout overrides the compiled-in 5m evaluation timeout.
func WithDefaultEvaluateTimeout(d time.Duration) Option {
	return func(m *Manager) { m.defaultTimeout = d }
}

// evalRecord carries the shape passed to recordEvaluation. Constructed by
// the evaluate path at every exit point so the audit trail always reflects
// what happened, not just what succeeded.
type evalRecord struct {
	projectID  string
	outcome    string
	reason     string
	taskID     string
	taskType   string
	workflowID string
	prompt     string // raw — hashed in recordEvaluation, never stored in cleartext
	start      time.Time
}

// recordEvaluation persists one autonomy audit row. Non-fatal: a DB error
// logs but does not propagate, so the autonomy loop keeps running even if
// the audit table is temporarily unreachable. The prompt is hashed with
// SHA-256 before storage so operators can correlate evaluations by prompt
// without leaking task content through the audit log.
func (m *Manager) recordEvaluation(ctx context.Context, r evalRecord) {
	if m == nil || m.evalRepo == nil {
		return
	}
	// Use a detached, short-bounded context: the tick context is often
	// cancelled by the time we reach the audit write (LLM error, tick
	// timeout) and we still want the row to land.
	writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ctx // reserved for tracing/attribution if we ever wire it

	var durationMs int64
	if !r.start.IsZero() {
		durationMs = time.Since(r.start).Milliseconds()
	}
	var taskIDPtr *string
	if r.taskID != "" {
		id := r.taskID
		taskIDPtr = &id
	}
	entry := &persistence.AutonomyEvaluation{
		ID:         persistence.GenerateID("eval"),
		ProjectID:  r.projectID,
		Outcome:    r.outcome,
		Reason:     truncate(r.reason, 500),
		TaskID:     taskIDPtr,
		TaskType:   r.taskType,
		WorkflowID: r.workflowID,
		PromptHash: hashPrompt(r.prompt),
		DurationMs: durationMs,
		CreatedAt:  time.Now().UTC(),
	}
	if err := m.evalRepo.Record(writeCtx, entry); err != nil {
		m.logger.Warn().
			Err(err).
			Str("project", r.projectID).
			Str("outcome", r.outcome).
			Msg("autonomy: failed to write evaluation audit row")
	}
}

// hashPrompt returns a short hex prefix of the prompt's SHA-256 so rows
// are correlatable without storing cleartext. Empty input yields empty
// output (not a zero-hash) so "no prompt at all" isn't confused with "I
// forgot to set it".
func hashPrompt(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return fmt.Sprintf("%x", sum[:8])
}

// New creates a new autonomy manager.
func New(client chat.Provider, reg *registry.Registry, taskRepo persistence.TaskRepository, execRepo persistence.ExecutionRepository, opts ...Option) *Manager {
	m := &Manager{
		client:         client,
		registry:       reg,
		taskRepo:       taskRepo,
		execRepo:       execRepo,
		logger:         zerolog.Nop(),
		defaultTimeout: 5 * time.Minute, // default if no option provided
		taskCounts:     make(map[string]int),
		hourReset:      make(map[string]time.Time),
		cancelFns:      make(map[string]context.CancelFunc),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Start begins the autonomous evaluation loops for all eligible projects.
// Projects with autonomy.enabled=true in YAML start automatically; projects
// explicitly enabled via EnableProject also start even when YAML has enabled=false.
func (m *Manager) Start() {
	if m.registry == nil {
		return
	}
	projects := m.registry.ListProjects()
	started := 0
	for _, p := range projects {
		if !p.Autonomy.Enabled {
			continue
		}
		// Goal is required for llm and cron modes (it's either
		// the evaluator's compass or the per-tick prompt). For
		// backlog mode the prompt comes from BACKLOG.md, so Goal
		// can be empty.
		if p.Autonomy.Goal == "" && p.NormalizedAutonomyMode() != registry.AutonomyModeBacklog {
			continue
		}
		m.mu.Lock()
		_, alreadyRunning := m.cancelFns[p.ID]
		var ctx context.Context
		var cancel context.CancelFunc
		if !alreadyRunning {
			ctx, cancel = context.WithCancel(context.Background())
			m.cancelFns[p.ID] = cancel
		}
		m.mu.Unlock()

		if cancel != nil {
			m.wg.Add(1)
			go m.projectLoop(ctx, p)
			started++
		}
	}
	if started > 0 {
		m.logger.Info().Int("projects", started).Msg("autonomous manager started")
	} else {
		m.logger.Debug().Msg("autonomous manager: no projects with autonomy enabled")
	}
}

// SetMetrics updates the Prometheus metrics on a running manager.
// Used when observability is initialized after the manager is created.
func (m *Manager) SetMetrics(metrics *Metrics) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.metrics = metrics
}

// SetWorkspacePath updates the workspace path on a running manager.
func (m *Manager) SetWorkspacePath(p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workspacePath = p
}

// stopGracePeriod caps how long Stop()/Reload() wait for in-flight
// ticks to exit after their contexts are cancelled. In the healthy
// path all ticks return within milliseconds of cancel (HTTP clients
// respect the context). This grace period is a safety net so a buggy
// code path that ignores context cannot deadlock the daemon's shutdown
// — if we hit the timeout, systemd SIGKILLs us anyway, so logging and
// returning is strictly better than waiting forever.
const stopGracePeriod = 30 * time.Second

// Stop shuts down all loops. Cancels each project's context first so
// in-flight evaluation ticks (which derive their LLM-call context
// from the project context) abort promptly, then waits for goroutines
// to exit with a grace period.
func (m *Manager) Stop() {
	m.mu.Lock()
	for _, cancel := range m.cancelFns {
		cancel()
	}
	m.cancelFns = make(map[string]context.CancelFunc)
	m.mu.Unlock()

	m.waitWithGrace("autonomous manager stopped")
}

// waitWithGrace waits for the manager's wg with a bounded timeout.
// Returns when either all goroutines have exited or the grace period
// elapses. The caller-supplied successMsg is logged on clean exit;
// a timeout is logged at WARN level with a hint about what to check.
func (m *Manager) waitWithGrace(successMsg string) {
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		m.logger.Info().Msg(successMsg)
	case <-time.After(stopGracePeriod):
		m.logger.Warn().
			Dur("grace", stopGracePeriod).
			Msg("autonomous manager shutdown timed out; some ticks did not exit after context cancellation — investigate blocking LLM/DB calls that ignore ctx.Done()")
	}
}

// Reload stops all project loops and restarts them using the current
// registry state. Call this after config hot-reload to pick up new
// projects or changed autonomy settings.
// Runtime overrides set via EnableProject/DisableProject are preserved
// across reloads — they represent explicit user intent.
func (m *Manager) Reload() {
	m.logger.Info().Msg("autonomous manager reloading")

	m.mu.Lock()
	for _, cancel := range m.cancelFns {
		cancel()
	}
	m.cancelFns = make(map[string]context.CancelFunc)
	m.taskCounts = make(map[string]int)
	m.hourReset = make(map[string]time.Time)
	m.mu.Unlock()

	m.waitWithGrace("autonomous manager reloaded")
	m.Start()
}

// ActiveLoops returns the number of project loops currently running.
func (m *Manager) ActiveLoops() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.cancelFns)
}

// EnableProject starts the autonomous loop for the given project.
// It updates autonomy.enabled=true in the in-memory registry and on disk so
// the change survives daemon restarts. Returns an error if the project is not
// found or has no autonomy goal configured.
func (m *Manager) EnableProject(projectID string) error {
	if m.registry == nil {
		return fmt.Errorf("registry not configured")
	}
	p := m.registry.GetProject(projectID)
	if p == nil {
		return fmt.Errorf("project %q not found", projectID)
	}
	if p.Autonomy.Goal == "" && p.NormalizedAutonomyMode() != registry.AutonomyModeBacklog {
		return fmt.Errorf("project %q has no autonomy.goal configured; set it in the project YAML before enabling autopilot", projectID)
	}

	// Persist the change (in-memory + YAML file).
	if err := m.registry.SetProjectAutonomyEnabled(projectID, true); err != nil {
		m.logger.Warn().Err(err).Str("project", projectID).Msg("autopilot enabled in memory but could not save to config file")
	}

	m.mu.Lock()
	_, alreadyRunning := m.cancelFns[projectID]
	var ctx context.Context
	var cancel context.CancelFunc
	if !alreadyRunning {
		ctx, cancel = context.WithCancel(context.Background())
		m.cancelFns[projectID] = cancel
	}
	m.mu.Unlock()

	if cancel != nil {
		m.wg.Add(1)
		go m.projectLoop(ctx, p)
		m.logger.Info().Str("project", projectID).Msg("autopilot enabled")
	}
	return nil
}

// DisableProject stops the autonomous loop for the given project.
// It updates autonomy.enabled=false in the in-memory registry and on disk.
func (m *Manager) DisableProject(projectID string) error {
	if m.registry == nil {
		return fmt.Errorf("registry not configured")
	}
	if p := m.registry.GetProject(projectID); p == nil {
		return fmt.Errorf("project %q not found", projectID)
	}

	// Persist the change (in-memory + YAML file).
	if err := m.registry.SetProjectAutonomyEnabled(projectID, false); err != nil {
		m.logger.Warn().Err(err).Str("project", projectID).Msg("autopilot disabled in memory but could not save to config file")
	}

	m.mu.Lock()
	cancel, running := m.cancelFns[projectID]
	if running {
		cancel()
		delete(m.cancelFns, projectID)
	}
	m.mu.Unlock()

	if running {
		m.logger.Info().Str("project", projectID).Msg("autopilot disabled")
	}
	return nil
}

// IsAutonomyEnabled reports whether autonomy is currently active for a project,
// reading directly from the registry (which reflects any /autopilot changes).
func (m *Manager) IsAutonomyEnabled(projectID string) bool {
	if m.registry == nil {
		return false
	}
	p := m.registry.GetProject(projectID)
	if p == nil {
		return false
	}
	return p.Autonomy.Enabled
}

func (m *Manager) projectLoop(ctx context.Context, project *registry.Project) {
	defer m.wg.Done()

	interval := 5 * time.Minute
	if project.Autonomy.PollInterval != "" {
		d, err := time.ParseDuration(project.Autonomy.PollInterval)
		switch {
		case err != nil:
			// Loud warning — pre-fix this fell through silently
			// and the operator-configured cadence was ignored.
			// Live evidence: vornik-autocoder.yaml had
			// `pollInterval: "60"` (missing unit) and polled
			// every 5m for weeks before anyone noticed the gap
			// between intent and behaviour. Go's time.ParseDuration
			// requires a unit suffix — "60s", "60m", "1h", etc.
			m.logger.Warn().
				Err(err).
				Str("project", project.ID).
				Str("raw_pollInterval", project.Autonomy.PollInterval).
				Dur("falling_back_to", interval).
				Msg("autonomy: pollInterval failed to parse — using default. Valid: \"60s\", \"5m\", \"1h\", etc.")
		case d <= 0:
			m.logger.Warn().
				Str("project", project.ID).
				Str("raw_pollInterval", project.Autonomy.PollInterval).
				Dur("falling_back_to", interval).
				Msg("autonomy: pollInterval parsed to ≤0 — using default")
		default:
			interval = d
		}
	}

	m.logger.Info().
		Str("project", project.ID).
		Str("goal", project.Autonomy.Goal).
		Dur("interval", interval).
		Msg("autonomous loop started for project")

	// Initial evaluation BEFORE the ticker loop — but ONLY when the
	// last persisted eval is at least `interval` old. Pre-fix the
	// initial eval fired unconditionally on every loop start, which
	// meant every daemon restart and every SIGHUP reload (both
	// re-spawn the per-project loops) triggered an immediate eval.
	// An operator who restarted the daemon a few times in an hour
	// could rack up 5+ evals on a project configured for a 5h cadence
	// — burning rate-limit budget on duplicate work and producing
	// the symptom "janka scheduled 4 tasks in the last hour despite
	// 5h pollInterval" (operator-reported 2026-05-05).
	//
	// The data lives in autonomy_evaluations (every eval, terminal
	// outcome or otherwise, gets a row). Query the most recent and
	// gate the initial-fire on it. When the repo isn't wired (tests
	// without persistence), fall through to the legacy fire-on-start
	// behaviour so we don't change test semantics silently.
	shouldFireInitial := true
	initialDelay := time.Duration(0)
	if m.evalRepo != nil {
		pid := project.ID
		recent, err := m.evalRepo.List(ctx, persistence.AutonomyEvaluationFilter{
			ProjectID: &pid,
			PageSize:  1,
		})
		if err == nil && len(recent) > 0 {
			elapsed := time.Since(recent[0].CreatedAt)
			if elapsed < interval {
				shouldFireInitial = false
				initialDelay = interval - elapsed
				m.logger.Info().
					Str("project", project.ID).
					Dur("since_last", elapsed.Round(time.Second)).
					Dur("interval", interval).
					Dur("first_eval_in", initialDelay.Round(time.Second)).
					Msg("autonomous loop: skipping initial eval — last persisted eval is recent")
			}
		}
	}

	// Bounded by ctx so a SIGTERM during startup doesn't block on a
	// running evaluation. The eval itself respects ctx through its
	// own LLM-call timeout.
	if shouldFireInitial {
		if err := m.evaluate(ctx, project); err != nil {
			// ctx.Err() means SIGTERM during the initial eval — return
			// without falling through to the ticker loop. Anything else
			// is logged and the loop continues; the next ticker tick is
			// the natural retry.
			if ctx.Err() != nil {
				m.logger.Info().Str("project", project.ID).Msg("autonomous loop stopped")
				return
			}
			m.logger.Warn().Err(err).Str("project", project.ID).Msg("autonomous initial evaluation failed")
		}
	} else {
		// Wait out the remaining interval before falling into the
		// ticker. Without this delay the ticker would still fire one
		// `interval` from NOW (not from `last+interval`), drifting the
		// schedule forward by `elapsed` on every restart.
		select {
		case <-ctx.Done():
			m.logger.Info().Str("project", project.ID).Msg("autonomous loop stopped")
			return
		case <-time.After(initialDelay):
		}
		if err := m.evaluate(ctx, project); err != nil {
			if ctx.Err() != nil {
				m.logger.Info().Str("project", project.ID).Msg("autonomous loop stopped")
				return
			}
			m.logger.Warn().Err(err).Str("project", project.ID).Msg("autonomous delayed-initial evaluation failed")
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	projectID := project.ID
	for {
		select {
		case <-ctx.Done():
			m.logger.Info().Str("project", projectID).Msg("autonomous loop stopped")
			return
		case <-ticker.C:
			// Leader gate (2026.8.0 horizontal-scaling). Skip
			// the evaluate call entirely when another daemon
			// holds the autonomy_manager lock — running two
			// would double-schedule via create_task. Nil gate
			// = single-process deployment, always run.
			if m.leaderGate != nil && !m.leaderGate.IsLeader() {
				continue
			}
			// Re-fetch the project from the registry on every
			// tick. The legacy code captured the *Project pointer
			// at loop start and never refreshed it — so changes
			// to rate limits, goal text, allowed task types, or
			// budget caps only took effect after a full daemon
			// restart, even though Reload() was meant to re-stage
			// edits live. A nil result means the project was
			// removed from config: log and exit so the loop
			// supervisor can drop the goroutine on the next
			// reconcile pass.
			fresh := m.registry.GetProject(projectID)
			if fresh == nil {
				m.logger.Info().Str("project", projectID).Msg("autonomous loop stopping: project removed from registry")
				return
			}
			if err := m.evaluate(ctx, fresh); err != nil {
				m.logger.Warn().Err(err).Str("project", projectID).Msg("autonomous evaluation failed")
			}
		}
	}
}

// evaluate runs a single autonomy tick: gathers state, asks the LLM
// what to schedule, and creates any returned task. The per-tick ctx
// derives from the caller (project loop's shutdown ctx) so SIGTERM
// cancels in-flight LLM/DB work promptly.
//
// Timeout defaults to m.defaultTimeout (configured via autonomy.default_evaluate_timeout
// in config.yaml, or 5m compiled default) and can be overridden
// per project via autonomy.evaluate_timeout in the project YAML.
func (m *Manager) evaluate(parentCtx context.Context, project *registry.Project) error {
	evalStart := time.Now()
	if !m.checkRateLimit(project) {
		m.logger.Debug().Str("project", project.ID).Msg("autonomous rate limit reached, skipping")
		m.recordEvaluation(parentCtx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeRateLimited,
			reason:    "project autonomy.maxTasksPerHour reached",
			start:     evalStart,
		})
		return nil
	}

	// Rate-limit gate: if the project has per-minute/per-hour caps on
	// task creation and the cap is already hit, skip this tick without
	// charging an LLM call.
	if m.rateLimiter != nil {
		if d := m.rateLimiter.Check(project, time.Now()); d.Blocked {
			m.logger.Debug().
				Str("project", project.ID).
				Str("reason", d.Reason).
				Int("minute", d.MinuteCount).
				Int("hour", d.HourCount).
				Msg("autonomy skipped: rate limit reached")
			m.recordEvaluation(parentCtx, evalRecord{
				projectID: project.ID,
				outcome:   persistence.AutonomyOutcomeRateLimited,
				reason:    d.Reason,
				start:     evalStart,
			})
			return nil
		}
	}

	// Budget gate: if the project has a configured LLM spend cap and the
	// daily or monthly hard cap is exceeded, skip this tick. Runs before
	// the evaluate timeout budget is even allocated — a blocked tick costs
	// one DB sum, not an LLM call. A soft breach emits a warning but lets
	// the tick proceed.
	if m.llmUsageRepo != nil {
		decision, err := budget.Check(parentCtx, m.llmUsageRepo, project, time.Now().UTC())
		if err != nil {
			m.logger.Warn().Err(err).Str("project", project.ID).Msg("budget check failed — proceeding unblocked")
		} else if decision.Blocked {
			m.logger.Warn().
				Str("project", project.ID).
				Str("reason", decision.Reason).
				Float64("daily_usd", decision.DailyUSD).
				Float64("monthly_usd", decision.MonthlyUSD).
				Msg("autonomy skipped: project over budget")
			if m.budgetNotifier != nil {
				period, level := decision.Period()
				m.budgetNotifier.NotifyBudgetBreach(parentCtx, project.ID, level, period, decision)
			}
			m.recordEvaluation(parentCtx, evalRecord{
				projectID: project.ID,
				outcome:   persistence.AutonomyOutcomeBudgetBlocked,
				reason:    decision.Reason,
				start:     evalStart,
			})
			return nil
		} else if decision.SoftBreached {
			m.logger.Warn().
				Str("project", project.ID).
				Str("reason", decision.Reason).
				Msg("autonomy proceeding despite soft budget breach")
			if m.budgetNotifier != nil {
				period, level := decision.Period()
				m.budgetNotifier.NotifyBudgetBreach(parentCtx, project.ID, level, period, decision)
			}
		}
	}

	// Pre-LLM deterministic gate. Runs after rate-limit and
	// budget checks (those are cheaper) but BEFORE the LLM
	// call, so a doomed tick costs zero. Trading projects
	// configure preCheck="trading-rth" to get market-hours +
	// broker-reachable + remaining-RTH-buffer skipping. Empty
	// preCheck name passes through (back-compat).
	if pre := m.runPreCheck(parentCtx, project); pre.Skip {
		m.logger.Debug().
			Str("project", project.ID).
			Str("reason", pre.Reason).
			Msg("autonomy skipped: preCheck refused tick")
		m.recordEvaluation(parentCtx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomePreCheckSkipped,
			reason:    pre.Reason,
			start:     evalStart,
		})
		return nil
	}

	start := time.Now()
	if m.metrics != nil {
		m.metrics.EvaluationsTotal.WithLabelValues(project.ID).Inc()
	}

	timeout := m.defaultTimeout
	if project.Autonomy.EvaluateTimeout != "" {
		if d, err := time.ParseDuration(project.Autonomy.EvaluateTimeout); err == nil && d > 0 {
			timeout = d
		} else {
			m.logger.Warn().
				Str("project", project.ID).
				Str("evaluate_timeout", project.Autonomy.EvaluateTimeout).
				Msg("invalid autonomy.evaluate_timeout — using default")
		}
	}

	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	// Gather current state: recent tasks and their results
	stateContext, hasActive, err := m.buildStateContext(ctx, project)
	if err != nil {
		m.recordEvaluation(parentCtx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeDBError,
			reason:    "build state context: " + err.Error(),
			start:     evalStart,
		})
		return fmt.Errorf("failed to build state context: %w", err)
	}

	// Do not schedule new work while tasks are still queued or running.
	// Wait for all in-flight work to complete before deciding what to do next,
	// so the LLM has full context and can't create duplicate backlog entries.
	if hasActive {
		m.logger.Debug().Str("project", project.ID).Msg("autonomous evaluation skipped: tasks still active")
		m.recordEvaluation(parentCtx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeActiveTasks,
			reason:    "tasks still queued or running",
			start:     evalStart,
		})
		return nil
	}

	// Mode dispatch. cron and backlog bypass the LLM evaluate
	// call entirely — they're deterministic engines. llm
	// (default) falls through to the legacy lead-evaluation
	// path below. _ = stateContext for non-llm modes because
	// only the LLM path consumes it.
	switch project.NormalizedAutonomyMode() {
	case registry.AutonomyModeCron:
		_ = stateContext
		return m.tickCron(ctx, project, evalStart)
	case registry.AutonomyModeBacklog:
		_ = stateContext
		return m.tickBacklog(ctx, project, evalStart)
	}

	// Build the LLM prompt
	allowedTypes := ""
	if len(project.Autonomy.AllowedTaskTypes) > 0 {
		allowedTypes = fmt.Sprintf("\nAllowed task types: %s\nYou MUST use one of these types. Any other type will be rejected.", strings.Join(project.Autonomy.AllowedTaskTypes, ", "))
	}

	contextFile := autonomyContextFilePath(project)
	// Cron-style projects (duplicateWindow == 0) deliberately fire the
	// same prompt every tick. The "NEVER schedule identical prompts"
	// rule is wrong for them — flash-tier models read the strong
	// prohibition and ignore the buried "UNLESS cron-style" exception,
	// returning NO_ACTION on a perfectly valid tick (observed
	// 2026-05-07: ibkr-trader skipped a tick at 16:43:27 with
	// content_len=9, tool_calls=0). Swap the rule wholesale based on
	// the daemon-side cron flag rather than asking the LLM to infer it.
	cronStyle := autonomyDuplicateWindow(project) == 0
	identicalPromptRule := `- NEVER schedule a task whose prompt text is identical or nearly identical to a prompt in the "Already completed tasks" list. For backlog-style projects (development, refactors, content creation): if the only available work matches a completed prompt, respond NO_ACTION.`
	if cronStyle {
		identicalPromptRule = `- This project is CRON-STYLE: the same prompt is expected to fire every tick (per the project goal). Identical prompts in the recent task history are NORMAL and EXPECTED — do NOT treat them as a duplicate-prevention signal. Schedule the tick if the goal procedure says so; the daemon-side preCheck already gates window/holiday/dependency conditions.`
	}
	systemPrompt := fmt.Sprintf(`You are the autonomous lead agent for project "%s".

Project goal: %s
%s
You evaluate the current state of the project and decide what work to schedule next.
If the goal is already achieved or there is nothing useful to do right now, respond with just the text "NO_ACTION".

If you decide work is needed, use the create_task tool to schedule it. The prompt you provide should be a clear, actionable instruction for the agent that will execute it.

%s

IMPORTANT RULES:
- Only create tasks that make meaningful progress toward the project goal.
%s
- When the context file (%s) content is shown above, use it to identify the next specific, named backlog item. The task prompt MUST name the concrete feature or change — never use vague phrases like "implement next feature from backlog" or "read backlog and identify next item". Cross-reference the completed task list to avoid re-doing finished work. If every backlog item is already completed, respond NO_ACTION.
- If the context file (%s) is shown above as NOT FOUND AND your project goal describes a setup/scout/bootstrap step, that bootstrap is your FIRST priority — schedule it before any development work. If the goal does not describe a bootstrap step (cron-style projects often don't), proceed normally; the missing file is just informational.
- Do not create duplicate or redundant tasks (check the recent task list).
- Do NOT create diagnostic or investigative tasks to debug prior failures. If recent tasks failed, move on to different work or respond NO_ACTION.
- Do NOT repeatedly schedule the same kind of task that has been failing. If a task type has failed multiple times recently, skip it and try something else or respond NO_ACTION.`, project.ID, project.Autonomy.Goal, allowedTypes, untrusted.Prelude, identicalPromptRule, contextFile, contextFile)

	// State context is built from prior task prompts, failure reasons,
	// and PROJECT_CONTEXT.md. All three can contain text the operator
	// did not write (an LLM-authored task prompt from a previous tick
	// can echo an injection from a scraped page). Wrap to mark as data.
	userMessage := "Here is the current project state:\n\n" +
		untrusted.WrapLabeled("project_state", stateContext) +
		"\n\nBased on the project goal and current state, what should be done next?"

	messages := []chat.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userMessage},
	}

	tools := []chat.Tool{
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        "create_task",
				Description: "Schedule a new task for the project. The prompt is the instruction the agent will execute.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"prompt":{"type":"string","description":"Clear instruction for the agent"},
						"type":{"type":"string","description":"Task type identifier"},
						"workflow_id":{"type":"string","description":"Workflow to use (omit for project default)"}
					},
					"required":["prompt","type"]
				}`),
			},
		},
	}

	// Call the LLM
	if m.client == nil {
		return fmt.Errorf("chat client not configured for autonomy")
	}
	resp, err := m.client.CompleteWithTools(ctx, messages, tools)
	if m.metrics != nil {
		m.metrics.EvalDuration.WithLabelValues(project.ID).Observe(time.Since(start).Seconds())
	}
	if err != nil {
		// A cancelled CALLER context (context.Canceled) means the
		// autonomy loop was torn down mid-eval — config reload,
		// loop restart, or daemon shutdown — NOT an LLM failure.
		// Record it as the benign ABORTED outcome, don't bump the
		// error metric, and return nil: the loop re-evaluates on its
		// next start/tick. (context.DeadlineExceeded — a real eval
		// timeout — is NOT context.Canceled and stays an LLM_ERROR.)
		if errors.Is(err, context.Canceled) {
			m.recordEvaluation(parentCtx, evalRecord{
				projectID: project.ID,
				outcome:   persistence.AutonomyOutcomeAborted,
				reason:    "evaluation aborted: autonomy loop torn down (reload/shutdown)",
				start:     evalStart,
			})
			return nil
		}
		if m.metrics != nil {
			m.metrics.ErrorsTotal.WithLabelValues(project.ID).Inc()
		}
		m.recordEvaluation(parentCtx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeLLMError,
			reason:    err.Error(),
			start:     evalStart,
		})
		return fmt.Errorf("LLM call failed: %w", err)
	}

	if len(resp.Choices) == 0 {
		m.logger.Warn().Str("project", project.ID).Msg("autonomous evaluation: LLM returned no choices")
		m.recordEvaluation(parentCtx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeLLMError,
			reason:    "LLM returned no choices",
			start:     evalStart,
		})
		return nil
	}

	choice := resp.Choices[0]
	content := strings.TrimSpace(choice.Message.Content)

	m.logger.Info().
		Str("project", project.ID).
		Str("finish_reason", choice.FinishReason).
		Int("tool_calls", len(choice.Message.ToolCalls)).
		Int("content_len", len(content)).
		Msg("autonomous evaluation: LLM response received")

	// Process tool calls — check these first regardless of finish_reason,
	// because some providers set finish_reason="stop" even with tool_calls.
	if len(choice.Message.ToolCalls) > 0 {
		for _, tc := range choice.Message.ToolCalls {
			if tc.Function.Name != "create_task" {
				m.logger.Debug().Str("tool", tc.Function.Name).Msg("autonomous evaluation: ignoring non-create_task tool call")
				continue
			}
			m.logger.Info().Str("project", project.ID).Str("args", tc.Function.Arguments).Msg("autonomous evaluation: create_task tool call")
			if err := m.createAutonomousTask(ctx, project, tc.Function.Arguments, evalStart); err != nil {
				m.logger.Warn().Err(err).Str("project", project.ID).Msg("failed to create autonomous task")
			}
		}
		return nil
	}

	// No tool calls — check for NO_ACTION in text response
	if isNoActionSentinel(content) {
		if m.metrics != nil {
			m.metrics.NoActionTotal.WithLabelValues(project.ID).Inc()
		}
		m.logger.Info().Str("project", project.ID).Msg("autonomous lead decided no action needed")
		m.recordEvaluation(parentCtx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeNoAction,
			reason:    "lead returned NO_ACTION",
			start:     evalStart,
		})
		return nil
	}

	// LLM responded with text but no tool call. Some models embed JSON
	// in their text response instead of using the tool_calls field.
	// Try to extract a create_task call from the text. Log parse failures
	// — silent swallow makes it impossible to tell whether the model
	// produced malformed JSON (a tuning issue) or genuinely declined to
	// act (a semantic decision).
	if idx := strings.Index(content, "{"); idx >= 0 {
		jsonStr := content[idx:]
		if end := strings.LastIndex(jsonStr, "}"); end >= 0 {
			jsonStr = jsonStr[:end+1]
			var extracted struct {
				Prompt     string `json:"prompt"`
				Type       string `json:"type"`
				WorkflowID string `json:"workflow_id"`
			}
			if err := json.Unmarshal([]byte(jsonStr), &extracted); err != nil {
				m.logger.Debug().
					Err(err).
					Str("project", project.ID).
					Str("snippet", truncate(jsonStr, 200)).
					Msg("autonomous evaluation: text JSON block failed to parse")
				m.recordEvaluation(parentCtx, evalRecord{
					projectID: project.ID,
					outcome:   persistence.AutonomyOutcomeParseError,
					reason:    "text JSON block failed to parse: " + err.Error(),
					start:     evalStart,
				})
				return nil
			} else if extracted.Prompt != "" {
				// Mirror the text-only NO_ACTION gate above for the
				// JSON-embedded path. Without this, a planner that
				// emitted {"prompt": "NO_ACTION"} (obeying the autonomy
				// prompt's hard rule but wrapping in JSON) created a
				// real task that wasted a worker iteration on a no-op
				// prompt — observed 2026-05-12 on janka.
				if isNoActionSentinel(extracted.Prompt) {
					if m.metrics != nil {
						m.metrics.NoActionTotal.WithLabelValues(project.ID).Inc()
					}
					m.logger.Info().Str("project", project.ID).Msg("autonomous lead returned NO_ACTION inside JSON; suppressing task creation")
					m.recordEvaluation(parentCtx, evalRecord{
						projectID: project.ID,
						outcome:   persistence.AutonomyOutcomeNoAction,
						reason:    "lead returned NO_ACTION (JSON-embedded)",
						start:     evalStart,
					})
					return nil
				}
				m.logger.Info().Str("project", project.ID).Str("prompt", truncate(extracted.Prompt, 80)).Msg("autonomous evaluation: extracted task from text response")
				return m.createAutonomousTask(ctx, project, jsonStr, evalStart)
			}
		}
	}

	m.logger.Info().Str("project", project.ID).Str("response", truncate(content, 200)).Msg("autonomous lead responded without actionable tool call")
	m.recordEvaluation(parentCtx, evalRecord{
		projectID: project.ID,
		outcome:   persistence.AutonomyOutcomeParseError,
		reason:    "no tool call and no NO_ACTION marker in response",
		start:     evalStart,
	})
	return nil
}

// tickCron handles Mode="cron" projects: skip the LLM evaluation
// step and fire project.Autonomy.Goal verbatim as the task prompt.
// All upstream safety (rate limit, budget, preCheck, hasActive) has
// already run by the time we land here. The createAutonomousTask
// path retains the duplicateWindow / active-task / type-allowlist
// checks, so a cron project that sets duplicateWindow="0" gets the
// "fire every tick" semantics, and one that leaves the default gets
// the natural 24h debounce.
//
// Why a separate function: it bypasses the LLM completely. The legacy
// llm-mode body builds messages, calls CompleteWithTools, parses the
// response — all dead-weight for a cron loop where the operator has
// already decided what the prompt is.
func (m *Manager) tickCron(ctx context.Context, project *registry.Project, evalStart time.Time) error {
	prompt := strings.TrimSpace(project.Autonomy.Goal)
	if prompt == "" {
		// Defence in depth: registry validation rejects this, but
		// hot-reload or a malformed in-memory mutation could land
		// us here. Skip cleanly rather than crashing the loop.
		m.recordEvaluation(ctx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeParseError,
			reason:    "cron-mode tick: autonomy.goal empty",
			start:     evalStart,
		})
		return nil
	}
	args := struct {
		Prompt     string `json:"prompt"`
		Type       string `json:"type"`
		WorkflowID string `json:"workflow_id"`
	}{
		Prompt: prompt,
		Type:   project.ResolveCronTaskType(),
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		// json.Marshal on a flat string-struct can't fail in practice;
		// surface the error so triage isn't blind if it ever does.
		return fmt.Errorf("cron-mode tick: marshal args: %w", err)
	}
	m.logger.Info().
		Str("project", project.ID).
		Str("mode", registry.AutonomyModeCron).
		Str("task_type", args.Type).
		Msg("autonomous tick: dispatching cron-mode task")
	return m.createAutonomousTask(ctx, project, string(argsJSON), evalStart)
}

// tickBacklog handles Mode="backlog" projects: read the top
// uncompleted item from BACKLOG.md (or operator-configured path),
// fire it as the task prompt, and mark it `- [x]` in the file so
// the next tick skips past it. Operators add/reorder work by
// editing the file via normal git workflows — no LLM in the loop.
//
// File format: markdown checklist. A pending item is any line
// matching `^\s*-\s+\[\s\]\s+(.+)$`. The first such line is the
// next prompt; the helper rewrites just that line to `- [x] …`.
// Other lines (headers, prose, completed items, blank lines) are
// preserved verbatim so the file remains operator-readable.
func (m *Manager) tickBacklog(ctx context.Context, project *registry.Project, evalStart time.Time) error {
	if m.workspacePath == "" {
		m.recordEvaluation(ctx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeDBError,
			reason:    "backlog-mode tick: workspace path not configured",
			start:     evalStart,
		})
		return nil
	}
	rel := project.ResolveBacklogFilePath()
	if rel == "" {
		m.recordEvaluation(ctx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeParseError,
			reason:    "backlog-mode tick: backlogFilePath invalid",
			start:     evalStart,
		})
		return nil
	}
	// safepath.JoinUnder resolves symlinks in the deepest existing
	// prefix and asserts the candidate stays inside the workspace
	// root. A bare filepath.Join here would follow symlinks an
	// in-container agent plants under <workspace>/<projectID>/...
	// and let a backlogFilePath like "link" → /etc/shadow read or
	// overwrite arbitrary host files.
	abs, err := safepath.JoinUnder(m.workspacePath, project.ID, rel)
	if err != nil {
		m.recordEvaluation(ctx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeParseError,
			reason:    "backlog-mode tick: backlogFilePath escapes workspace",
			start:     evalStart,
		})
		m.logger.Warn().
			Err(err).
			Str("project", project.ID).
			Str("path", rel).
			Msg("backlog-mode tick: path traversal rejected")
		return nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		// Missing file = empty backlog. Operator can add a BACKLOG.md
		// at any time and the next tick will pick it up. Don't fail
		// the loop on the cold-start case.
		if os.IsNotExist(err) {
			m.logger.Debug().
				Str("project", project.ID).
				Str("path", abs).
				Msg("autonomous tick: backlog file not found — nothing to do")
			m.recordEvaluation(ctx, evalRecord{
				projectID: project.ID,
				outcome:   persistence.AutonomyOutcomeNoAction,
				reason:    "backlog-mode tick: BACKLOG.md absent",
				start:     evalStart,
			})
			return nil
		}
		m.recordEvaluation(ctx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeDBError,
			reason:    "backlog-mode tick: read backlog: " + err.Error(),
			start:     evalStart,
		})
		return fmt.Errorf("read backlog file: %w", err)
	}
	prompt, newContent, ok := consumeNextBacklogItem(string(data))
	if !ok {
		// File exists but no pending `- [ ]` items. Equivalent
		// semantically to NO_ACTION on the llm path.
		m.recordEvaluation(ctx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeNoAction,
			reason:    "backlog-mode tick: no pending items",
			start:     evalStart,
		})
		return nil
	}
	args := struct {
		Prompt     string `json:"prompt"`
		Type       string `json:"type"`
		WorkflowID string `json:"workflow_id"`
	}{
		Prompt: prompt,
		Type:   project.ResolveCronTaskType(),
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("backlog-mode tick: marshal args: %w", err)
	}
	m.logger.Info().
		Str("project", project.ID).
		Str("mode", registry.AutonomyModeBacklog).
		Str("backlog_path", rel).
		Str("prompt", truncate(prompt, 80)).
		Msg("autonomous tick: dispatching backlog-mode task")
	if err := m.createAutonomousTask(ctx, project, string(argsJSON), evalStart); err != nil {
		// Task creation failed (rate limit, dup, workflow invalid,
		// etc.). DO NOT mark the item consumed — the operator
		// wants this work to be retried on the next tick. Leaving
		// the line as `- [ ]` keeps the backlog truthful.
		return err
	}
	// Persist the consumed-marker only after the task was accepted.
	// A best-effort write: if disk is full or perms broke, the task
	// is already queued and the operator can fix the file by hand;
	// logging the failure is enough.
	// 0o600 — backlog can contain task prompts referencing tickers,
	// account IDs, and other operator-private context. The daemon
	// owns the file.
	if err := os.WriteFile(abs, []byte(newContent), 0o600); err != nil {
		m.logger.Warn().
			Err(err).
			Str("project", project.ID).
			Str("path", abs).
			Msg("backlog-mode tick: task queued but failed to mark item consumed; manual edit recommended")
	}
	return nil
}

// consumeNextBacklogItem finds the first `- [ ]` line in content,
// returns its inline text as prompt, and emits a new content blob
// with that line rewritten to `- [x] …`. Returns ok=false when no
// pending item exists. Whitespace + bullet variants accepted: leading
// indent, `-`/`*` bullets, `[ ]` (one space inside). Items
// already `- [x]` are skipped. Blank lines, prose, headers, indented
// notes under an item are preserved as-is.
func consumeNextBacklogItem(content string) (prompt, newContent string, ok bool) {
	lines := strings.Split(content, "\n")
	pendingRE := backlogPendingRE
	for i, line := range lines {
		matches := pendingRE.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		prompt = strings.TrimSpace(matches[2])
		if prompt == "" {
			// Empty checkbox item — skip it; operator typo.
			continue
		}
		// Rewrite by replacing the first `[ ]` with `[x]`. Keep
		// indent + bullet + the rest of the line so operator
		// formatting survives round-trip.
		lines[i] = strings.Replace(line, "[ ]", "[x]", 1)
		return prompt, strings.Join(lines, "\n"), true
	}
	return "", "", false
}

func (m *Manager) buildStateContext(ctx context.Context, project *registry.Project) (string, bool, error) {
	var b strings.Builder

	// Check whether the project context file exists in the project workspace.
	// When it does, include its content so the lead can pick a specific next
	// item without delegating that decision to the executing agent. Path is
	// per-project configurable (autonomy.contextFilePath); default is the
	// hidden ".autonomy/PROJECT_CONTEXT.md" namespace so the daemon's
	// bookkeeping doesn't collide with project files at the workspace root.
	if m.workspacePath != "" {
		relPath := autonomyContextFilePath(project)
		pcPath := filepath.Join(m.workspacePath, project.ID, relPath)
		data, err := os.ReadFile(pcPath)
		if err == nil {
			const maxContextBytes = 6000
			content := string(data)
			truncated := ""
			if len(content) > maxContextBytes {
				content = content[:maxContextBytes]
				truncated = "\n[...truncated]"
			}
			fmt.Fprintf(&b, "%s:\n```\n", relPath)
			b.WriteString(content)
			b.WriteString(truncated)
			b.WriteString("\n```\n")
		} else {
			// Neutral wording. Pre-2026-05-06 this said "a scout/setup
			// task must be scheduled before any development work." That
			// hard imperative in the data block contradicted the softened
			// system-prompt rule (bootstrap only when the goal asks for
			// it), and the LLM followed the imperative — observed: a
			// cron-style ibkr-trader tick at 18:44:59 created a "setup"
			// task that was rejected because allowedTaskTypes is
			// ["trading"]. The system-prompt rule decides policy now;
			// this line just reports the file's absence as data.
			fmt.Fprintf(&b, "%s: NOT FOUND (no project context file present)\n", relPath)
		}
	}

	// Recent tasks — fetch more so the LLM has a fuller history to avoid repeats.
	pid := project.ID
	tasks, err := m.taskRepo.List(ctx, persistence.TaskFilter{
		ProjectID: &pid,
		PageSize:  50,
	})
	if err != nil {
		return "", false, err
	}

	var hasActive bool
	if len(tasks) == 0 {
		b.WriteString("No tasks have been created yet for this project.\n")
	} else {
		// Separate tasks by status and summarize to avoid overwhelming the lead
		// with a wall of failures that triggers a diagnostic loop.
		var completed, inProgress, queued, failed []*persistence.Task
		for _, t := range tasks {
			switch t.Status {
			case persistence.TaskStatusCompleted:
				completed = append(completed, t)
			case persistence.TaskStatusRunning, persistence.TaskStatusLeased,
				persistence.TaskStatusWaitingForChildren:
				inProgress = append(inProgress, t)
			case persistence.TaskStatusQueued, persistence.TaskStatusPending,
				persistence.TaskStatusAwaitingApproval:
				queued = append(queued, t)
			case persistence.TaskStatusFailed:
				failed = append(failed, t)
			}
		}
		hasActive = len(inProgress) > 0 || len(queued) > 0

		if len(inProgress) > 0 || len(queued) > 0 {
			b.WriteString("Active/queued tasks (do NOT duplicate these):\n")
			for _, t := range inProgress {
				prompt := extractPrompt(t.Payload)
				fmt.Fprintf(&b, "- [%s] status=%s prompt=%q\n", t.ID, t.Status, truncate(prompt, 120))
			}
			for _, t := range queued {
				prompt := extractPrompt(t.Payload)
				fmt.Fprintf(&b, "- [%s] status=%s prompt=%q\n", t.ID, t.Status, truncate(prompt, 120))
			}
		}

		if len(completed) > 0 {
			if autonomyDuplicateWindow(project) == 0 {
				fmt.Fprintf(&b, "\nRecent task history (this project is cron-style — the same prompt is expected to repeat every tick; the daemon allows it):\n")
			} else {
				fmt.Fprintf(&b, "\nAlready completed tasks — DO NOT schedule these prompts again:\n")
			}
			// Fetch execution rows for the top N completed tasks in
			// one round-trip. The prior code looped GetByTaskID per
			// task — N+1 round-trips per autonomy tick, visible in
			// pg_stat_statements as the hottest query on a project
			// with a long backlog. Cap stays at 10 for context-size
			// control; beyond that the lead doesn't need history.
			const maxResultLookups = 10
			lookupIDs := make([]string, 0, maxResultLookups)
			for i, t := range completed {
				if i >= maxResultLookups {
					break
				}
				lookupIDs = append(lookupIDs, t.ID)
			}
			var execMap map[string]*persistence.Execution
			if len(lookupIDs) > 0 {
				if m, err := m.execRepo.GetByTaskIDs(ctx, lookupIDs); err == nil {
					execMap = m
				}
			}
			for _, t := range completed {
				prompt := extractPrompt(t.Payload)
				line := fmt.Sprintf("- [%s] prompt=%q", t.ID, truncate(prompt, 120))
				if exec, ok := execMap[t.ID]; ok && exec != nil && len(exec.Result) > 0 {
					msg := extractResultMessage(exec.Result)
					if msg != "" {
						line += fmt.Sprintf(" result=%q", truncate(msg, 200))
					}
				}
				b.WriteString(line + "\n")
			}
		}

		if len(failed) > 0 {
			// Summarize failures concisely — don't list each one to avoid
			// triggering a diagnostic loop where the lead keeps creating
			// investigative tasks about prior failures.
			fmt.Fprintf(&b, "\nFailed tasks: %d (do NOT create diagnostic tasks to investigate these failures — move on to new work instead)\n", len(failed))
			// Collect unique prompts from failures so the lead can avoid repeating them
			seen := make(map[string]int)
			for _, t := range failed {
				prompt := truncate(extractPrompt(t.Payload), 80)
				if prompt != "" {
					seen[prompt]++
				}
			}
			if len(seen) > 0 {
				b.WriteString("Recently failed prompts (avoid repeating these):\n")
				for prompt, count := range seen {
					fmt.Fprintf(&b, "- %q (failed %d time(s))\n", prompt, count)
				}
			}
		}

		if len(completed) == 0 && len(inProgress) == 0 && len(queued) == 0 && len(failed) == 0 {
			b.WriteString("No actionable tasks found.\n")
		}
	}

	// List available workflows so the lead can choose via workflow_id.
	if m.registry != nil {
		workflows := m.registry.ListWorkflows()
		if len(workflows) > 0 {
			b.WriteString("\nAvailable workflows:\n")
			for _, w := range workflows {
				name := w.DisplayName
				if name == "" {
					name = w.ID
				}
				fmt.Fprintf(&b, "- %s: %s (entrypoint: %s)\n", w.ID, name, w.Entrypoint)
			}
		}
	}

	return b.String(), hasActive, nil
}

func (m *Manager) createAutonomousTask(ctx context.Context, project *registry.Project, argsJSON string, evalStart time.Time) error {
	var args struct {
		Prompt     string `json:"prompt"`
		Type       string `json:"type"`
		WorkflowID string `json:"workflow_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		m.recordEvaluation(ctx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeParseError,
			reason:    "invalid create_task arguments: " + err.Error(),
			start:     evalStart,
		})
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	if args.Prompt == "" {
		m.recordEvaluation(ctx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeParseError,
			reason:    "create_task called without prompt",
			taskType:  args.Type,
			start:     evalStart,
		})
		return fmt.Errorf("prompt is required")
	}
	args.Prompt = strings.TrimSpace(args.Prompt)
	// Defence-in-depth: every other branch that reaches here
	// already filters NO_ACTION, but a future caller (e.g. tool_calls
	// path getting the sentinel from a confused planner) would
	// otherwise create a no-op task. Suppress consistently.
	if isNoActionSentinel(args.Prompt) {
		if m.metrics != nil {
			m.metrics.NoActionTotal.WithLabelValues(project.ID).Inc()
		}
		m.logger.Info().Str("project", project.ID).Msg("create_task called with NO_ACTION sentinel as prompt; suppressing")
		m.recordEvaluation(ctx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeNoAction,
			reason:    "create_task arguments carried NO_ACTION sentinel",
			taskType:  args.Type,
			start:     evalStart,
		})
		return nil
	}

	// Enforce allowedTaskTypes when configured.
	if len(project.Autonomy.AllowedTaskTypes) > 0 {
		if strings.TrimSpace(args.Type) == "" {
			m.recordEvaluation(ctx, evalRecord{
				projectID: project.ID,
				outcome:   persistence.AutonomyOutcomeTypeRejected,
				reason:    fmt.Sprintf("task type is required when allowedTaskTypes is configured %v", project.Autonomy.AllowedTaskTypes),
				start:     evalStart,
			})
			return fmt.Errorf("task type is required when allowedTaskTypes is configured %v", project.Autonomy.AllowedTaskTypes)
		}
		allowed := false
		for _, t := range project.Autonomy.AllowedTaskTypes {
			if t == args.Type {
				allowed = true
				break
			}
		}
		if !allowed {
			m.logger.Warn().
				Str("project", project.ID).
				Str("type", args.Type).
				Strs("allowed", project.Autonomy.AllowedTaskTypes).
				Msg("autonomous task rejected: type not in allowedTaskTypes")
			m.recordEvaluation(ctx, evalRecord{
				projectID:  project.ID,
				outcome:    persistence.AutonomyOutcomeTypeRejected,
				reason:     fmt.Sprintf("type %q not in allowedTaskTypes %v", args.Type, project.Autonomy.AllowedTaskTypes),
				taskType:   args.Type,
				workflowID: args.WorkflowID,
				prompt:     args.Prompt,
				start:      evalStart,
			})
			return fmt.Errorf("task type %q not in allowedTaskTypes %v", args.Type, project.Autonomy.AllowedTaskTypes)
		}
	}

	// When workflow_id is empty, fall back to the project's default
	// workflow. The previous behaviour here matched args.Type against
	// loaded workflows and silently promoted it to workflow_id — which
	// turned common task-type words into lethal routing accidents. The
	// worst case was type="research" on a project whose swarm has no
	// "researcher" role: every autonomous tick produced an
	// "autonomous task rejected: workflow role not in project swarm"
	// warning and the audit log stayed empty because no task was ever
	// persisted. defaultWorkflowId is the field operators already set
	// per project for exactly this purpose; use it.
	effectiveWorkflowID := args.WorkflowID
	if effectiveWorkflowID == "" {
		effectiveWorkflowID = project.DefaultWorkflowID
	}

	// Validate that the resolved workflow_id is compatible with the project's
	// swarm: every agent role in the workflow must exist in the swarm.
	// This catches misconfigured defaults (operator pointed a project at
	// a workflow whose roles its swarm doesn't support) as well as any
	// workflow the lead picks explicitly.
	if effectiveWorkflowID != "" && m.registry != nil {
		wf := m.registry.GetWorkflow(effectiveWorkflowID)
		if wf == nil {
			m.logger.Warn().
				Str("project", project.ID).
				Str("workflow_id", effectiveWorkflowID).
				Msg("autonomous task rejected: workflow not found")
			m.recordEvaluation(ctx, evalRecord{
				projectID:  project.ID,
				outcome:    persistence.AutonomyOutcomeWorkflowInvalid,
				reason:     fmt.Sprintf("workflow %q not found in registry", effectiveWorkflowID),
				taskType:   args.Type,
				workflowID: effectiveWorkflowID,
				prompt:     args.Prompt,
				start:      evalStart,
			})
			return fmt.Errorf("workflow %q not found in registry", effectiveWorkflowID)
		}
		swarm := m.registry.GetSwarm(project.SwarmID)
		if swarm != nil {
			swarmRoles := make(map[string]struct{}, len(swarm.Roles))
			for _, r := range swarm.Roles {
				swarmRoles[r.Name] = struct{}{}
			}
			for stepID, step := range wf.Steps {
				if step.Type == "agent" && step.Role != "" {
					if _, ok := swarmRoles[step.Role]; !ok {
						m.logger.Warn().
							Str("project", project.ID).
							Str("workflow_id", effectiveWorkflowID).
							Str("step", stepID).
							Str("missing_role", step.Role).
							Str("swarm", project.SwarmID).
							Msg("autonomous task rejected: workflow role not in project swarm")
						m.recordEvaluation(ctx, evalRecord{
							projectID:  project.ID,
							outcome:    persistence.AutonomyOutcomeWorkflowInvalid,
							reason:     fmt.Sprintf("workflow %q step %q requires role %q not in swarm %q", effectiveWorkflowID, stepID, step.Role, project.SwarmID),
							taskType:   args.Type,
							workflowID: effectiveWorkflowID,
							prompt:     args.Prompt,
							start:      evalStart,
						})
						return fmt.Errorf("workflow %q step %q requires role %q not present in swarm %q",
							effectiveWorkflowID, stepID, step.Role, project.SwarmID)
					}
				}
			}

			// Forecast gate: predict the autonomous task's USD cost
			// and refuse if forecast + already-spent would breach
			// the project's hard cap. The per-tick budget.Check at
			// evaluate() above is reactive (already-spent vs. cap);
			// this is preventive (would-be-spent vs. cap).
			//
			// Skipped when no usage repo (no history), no caps
			// configured (nothing to check against), or the
			// forecast itself errors (we don't want a transient DB
			// hiccup to block legitimate autonomy).
			if m.llmUsageRepo != nil && (project.Budget.DailyHardUSD > 0 || project.Budget.MonthlyHardUSD > 0) {
				now := time.Now().UTC()
				current, cerr := budget.Check(ctx, m.llmUsageRepo, project, now)
				if cerr != nil {
					m.logger.Warn().Err(cerr).Str("project", project.ID).Msg("autonomy: budget snapshot for forecast failed — proceeding")
				} else {
					forecast, ferr := budget.ForecastTask(ctx, m.llmUsageRepo, m.pricing, budget.ForecastInput{
						Workflow:     wf,
						Swarm:        swarm,
						DefaultModel: m.defaultModel,
					}, now)
					if ferr != nil {
						m.logger.Warn().Err(ferr).Str("project", project.ID).Msg("autonomy: forecast failed — proceeding without preventive gate")
					} else if d := budget.CheckForecast(project, forecast, current); d.Refused {
						m.logger.Warn().
							Str("project", project.ID).
							Str("workflow_id", effectiveWorkflowID).
							Float64("forecast_usd", forecast.USD).
							Float64("daily_spent_usd", current.DailyUSD).
							Str("reason", d.Reason).
							Msg("autonomy: refusing autonomous task — forecast would breach hard cap")
						m.recordEvaluation(ctx, evalRecord{
							projectID:  project.ID,
							outcome:    persistence.AutonomyOutcomeBudgetBlocked,
							reason:     d.Reason,
							taskType:   args.Type,
							workflowID: effectiveWorkflowID,
							prompt:     args.Prompt,
							start:      evalStart,
						})
						return fmt.Errorf("forecast refused: %s", d.Reason)
					}
				}
			}
		}
	}

	// Hold the manager mutex across the dedup check AND the create so two
	// concurrent autonomy ticks for the same project can't both pass the
	// duplicate/circuit/cooldown guards with the same stale snapshot. The
	// DB-layer idempotency key is a belt-and-braces second line of defence
	// but only covers the same hour bucket — this lock closes the window
	// across bucket boundaries.
	m.mu.Lock()
	defer m.mu.Unlock()

	recentTasks, err := m.taskRepo.List(ctx, persistence.TaskFilter{
		ProjectID: &project.ID,
		PageSize:  50,
	})
	if err != nil {
		m.recordEvaluation(ctx, evalRecord{
			projectID: project.ID,
			outcome:   persistence.AutonomyOutcomeDBError,
			reason:    "list recent tasks: " + err.Error(),
			taskType:  args.Type,
			prompt:    args.Prompt,
			start:     evalStart,
		})
		return fmt.Errorf("failed to inspect recent tasks: %w", err)
	}

	if reason, blocked := autonomyCircuitOpen(recentTasks); blocked {
		m.logger.Warn().Str("project", project.ID).Str("reason", reason).Msg("autonomous task suppressed by circuit breaker")
		m.recordEvaluation(ctx, evalRecord{
			projectID:  project.ID,
			outcome:    persistence.AutonomyOutcomeCircuitOpen,
			reason:     reason,
			taskType:   args.Type,
			workflowID: effectiveWorkflowID,
			prompt:     args.Prompt,
			start:      evalStart,
		})
		return nil
	}

	if reason, duplicate := findAutonomyDuplicate(recentTasks, args.Type, args.WorkflowID, args.Prompt, autonomyDuplicateWindow(project)); duplicate {
		m.logger.Info().Str("project", project.ID).Str("reason", reason).Msg("autonomous task suppressed as duplicate")
		m.recordEvaluation(ctx, evalRecord{
			projectID:  project.ID,
			outcome:    persistence.AutonomyOutcomeDuplicate,
			reason:     reason,
			taskType:   args.Type,
			workflowID: effectiveWorkflowID,
			prompt:     args.Prompt,
			start:      evalStart,
		})
		return nil
	}

	if reason, coolingDown := autonomyFailureCooldown(recentTasks, args.Type, args.WorkflowID, args.Prompt); coolingDown {
		m.logger.Info().Str("project", project.ID).Str("reason", reason).Msg("autonomous task suppressed by failure cooldown")
		m.recordEvaluation(ctx, evalRecord{
			projectID:  project.ID,
			outcome:    persistence.AutonomyOutcomeCooldown,
			reason:     reason,
			taskType:   args.Type,
			workflowID: effectiveWorkflowID,
			prompt:     args.Prompt,
			start:      evalStart,
		})
		return nil
	}

	// Idempotency-key dedup is a redundant hour-bucketed safety net
	// on top of findAutonomyDuplicate. Backlog-style autonomy (the
	// 24h default duplicateWindow) wants both: the dedup window
	// catches rephrased re-fires, the hour-bucket idempotency key
	// catches an exact-match double-fire within the bucket.
	//
	// Cron-style autonomy (duplicateWindow=0) explicitly opts out of
	// duplicate detection — trading ticks, heartbeats, etc. fire
	// the same prompt every poll on purpose. With the hour-bucket
	// active in that mode, the first tick consumes the bucket and
	// every subsequent tick in the same hour returned
	// IDEMPOTENCY_HIT — the 5-minute poll on ibkr-trader produced
	// 1 task/hour instead of 12. Mirror findAutonomyDuplicate's
	// opt-out: cron mode (duplicateWindow == 0) skips the
	// idempotency check entirely and the created task carries no
	// idempotency_key (the unique index `(project_id,
	// idempotency_key) WHERE NOT NULL` lets the column stay NULL
	// per-row, so collisions don't surface at Create time either).
	var idempotencyKey string
	cronStyle := autonomyDuplicateWindow(project) == 0
	if !cronStyle {
		idempotencyKey = buildAutonomyIdempotencyKey(project.ID, args.Type, args.WorkflowID, args.Prompt, time.Now().UTC())
		if existing, err := m.taskRepo.GetByIdempotencyKey(ctx, project.ID, idempotencyKey); err == nil && existing != nil {
			m.logger.Info().Str("project", project.ID).Str("task_id", existing.ID).Msg("autonomous task suppressed by idempotency key")
			m.recordEvaluation(ctx, evalRecord{
				projectID:  project.ID,
				outcome:    persistence.AutonomyOutcomeIdempotencyHit,
				reason:     "task with this idempotency key already exists: " + existing.ID,
				taskID:     existing.ID,
				taskType:   args.Type,
				workflowID: effectiveWorkflowID,
				prompt:     args.Prompt,
				start:      evalStart,
			})
			return nil
		} else if err != nil && err != persistence.ErrNotFound {
			m.recordEvaluation(ctx, evalRecord{
				projectID: project.ID,
				outcome:   persistence.AutonomyOutcomeDBError,
				reason:    "idempotency lookup: " + err.Error(),
				taskType:  args.Type,
				prompt:    args.Prompt,
				start:     evalStart,
			})
			return fmt.Errorf("failed to check autonomous idempotency key: %w", err)
		}
	}

	payload, _ := json.Marshal(map[string]any{
		"taskType": args.Type,
		"context": map[string]any{
			"prompt": args.Prompt,
		},
	})

	status := persistence.TaskStatusQueued
	if project.Autonomy.RequireApproval {
		// Park for manual approval. AWAITING_APPROVAL (not PENDING) so
		// the task surfaces in the operator inbox and can be resolved
		// via approve (→ QUEUED) / reject (→ CANCELLED). PENDING is
		// never leased and was invisible to the UI — approval-gated
		// tasks waited forever (operator report 2026-06-09). See
		// https://docs.vornik.io
		status = persistence.TaskStatusAwaitingApproval
	}

	task := &persistence.Task{
		ID:             persistence.GenerateID("task"),
		ProjectID:      project.ID,
		CreationSource: persistence.TaskCreationSourceAutonomous,
		Status:         status,
		Priority:       project.DefaultPriority,
		Payload:        payload,
		Attempt:        1,
		MaxAttempts:    3,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if idempotencyKey != "" {
		// Only set on non-cron tasks; cron-style autonomy leaves
		// the column NULL so the partial unique index lets every
		// tick land as a distinct row.
		task.IdempotencyKey = &idempotencyKey
	}
	if args.WorkflowID != "" {
		task.WorkflowID = &args.WorkflowID
	}

	// Leader epoch fence (review B1). The cheap IsLeader() gate at the
	// top of the tick can report a STALE true: a TTL-expired-but-paused
	// leader resuming after a GC/scheduler stall still carries a cached
	// leader bit. Re-read the lock epoch here — the last gate before the
	// write — so a successor's epoch bump fences this stale leader out
	// instead of letting it double-write. nil / IsLeader-only gates are
	// pre-fence and proceed unchanged.
	if proceed, reason := leaderelection.DangerousWriteAllowed(ctx, m.leaderGate); !proceed {
		m.logger.Warn().Str("project", project.ID).Str("reason", reason).Msg("autonomy: leader epoch fence — skipping task creation")
		leaderelection.LeaderFenceRejected("autonomy_manager")
		m.recordEvaluation(ctx, evalRecord{
			projectID:  project.ID,
			outcome:    persistence.AutonomyOutcomeNoAction,
			reason:     "leader epoch fence: " + reason,
			taskType:   args.Type,
			workflowID: effectiveWorkflowID,
			prompt:     args.Prompt,
			start:      evalStart,
		})
		return nil
	}

	// Atomic hard-cap reservation (trading-hardening §1): claim this task's
	// estimated spend before inserting so concurrent autonomy ticks +
	// other admission paths can't collectively overshoot the cap. FAIL OPEN
	// on a ledger error; a Blocked decision records a budget-blocked outcome.
	if m.reservRepo != nil && m.llmUsageRepo != nil {
		decision, rerr := budget.Reserve(ctx, m.reservRepo, m.llmUsageRepo, project, task.ID, time.Now().UTC())
		if rerr != nil {
			m.logger.Warn().Err(rerr).Str("project", project.ID).Msg("autonomy: budget reserve failed — proceeding")
		} else if decision.Blocked {
			if m.budgetNotifier != nil {
				period, level := decision.Period()
				m.budgetNotifier.NotifyBudgetBreach(ctx, project.ID, level, period, decision)
			}
			m.recordEvaluation(ctx, evalRecord{
				projectID:  project.ID,
				outcome:    persistence.AutonomyOutcomeBudgetBlocked,
				reason:     decision.Reason,
				taskType:   args.Type,
				workflowID: effectiveWorkflowID,
				prompt:     args.Prompt,
				start:      evalStart,
			})
			return nil
		}
	}

	callStart := time.Now()
	if err := m.taskRepo.Create(ctx, task); err != nil {
		m.recordEvaluation(ctx, evalRecord{
			projectID:  project.ID,
			outcome:    persistence.AutonomyOutcomeDBError,
			reason:     "task create: " + err.Error(),
			taskType:   args.Type,
			workflowID: effectiveWorkflowID,
			prompt:     args.Prompt,
			start:      evalStart,
		})
		return fmt.Errorf("failed to create task: %w", err)
	}

	m.recordTaskCreatedLocked(project.ID)
	// If the task was parked for approval, push a steering prompt to the
	// originating chat (no-op for autonomy-only tasks with no ChatTurnID;
	// fires for any future chat-originated approval-gated task).
	if status == persistence.TaskStatusAwaitingApproval && m.steering != nil {
		m.steering.NotifySteeringRequired(ctx, task, string(persistence.TaskStatusAwaitingApproval))
	}
	if m.rateLimiter != nil {
		m.rateLimiter.Record(project.ID, time.Now())
	}
	if m.metrics != nil {
		m.metrics.TasksCreated.WithLabelValues(project.ID).Inc()
	}

	m.logger.Info().
		Str("project", project.ID).
		Str("task_id", task.ID).
		Str("prompt", truncate(args.Prompt, 100)).
		Str("status", string(status)).
		Msg("autonomous task created")

	// Durable audit row for the success path. The caller already has
	// m.logger.Info() for operator visibility — this is the persisted
	// counterpart, queryable via GET /api/v1/projects/{p}/autonomy/evaluations.
	m.recordEvaluation(ctx, evalRecord{
		projectID:  project.ID,
		outcome:    persistence.AutonomyOutcomeCreated,
		reason:     fmt.Sprintf("task %s created (%s)", task.ID, status),
		taskID:     task.ID,
		taskType:   args.Type,
		workflowID: effectiveWorkflowID,
		prompt:     args.Prompt,
		start:      evalStart,
	})

	if m.auditRepo != nil {
		entry := &persistence.ToolAuditEntry{
			ID:         persistence.GenerateID("ta"),
			ProjectID:  project.ID,
			TaskID:     task.ID,
			StepID:     "autonomy",
			ToolName:   "create_task",
			ToolInput:  argsJSON,
			ToolOutput: task.ID,
			DurationMs: persistence.ClampToolAuditDurationMs(time.Since(callStart).Milliseconds()),
			CreatedAt:  time.Now(),
		}
		if logErr := m.auditRepo.Log(ctx, entry); logErr != nil {
			m.logger.Warn().Err(logErr).Str("task_id", task.ID).Msg("autonomy: failed to write tool audit entry")
		}
	}

	return nil
}

type autonomyTaskSummary struct {
	id        string
	taskType  string
	workflow  string
	prompt    string
	status    persistence.TaskStatus
	source    persistence.TaskCreationSource
	lastError string
	createdAt time.Time
	updatedAt time.Time
}

func summarizeAutonomyTask(task *persistence.Task) autonomyTaskSummary {
	summary := autonomyTaskSummary{
		id:        task.ID,
		status:    task.Status,
		source:    task.CreationSource,
		createdAt: task.CreatedAt,
		updatedAt: task.UpdatedAt,
	}
	if task.WorkflowID != nil {
		summary.workflow = *task.WorkflowID
	}
	if task.LastError != nil {
		summary.lastError = *task.LastError
	}
	if len(task.Payload) == 0 {
		return summary
	}
	var payload struct {
		TaskType string `json:"taskType"`
		Context  struct {
			Prompt string `json:"prompt"`
		} `json:"context"`
	}
	if json.Unmarshal(task.Payload, &payload) == nil {
		summary.taskType = payload.TaskType
		summary.prompt = payload.Context.Prompt
	}
	return summary
}

func normalizeAutonomyPrompt(prompt string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(prompt))), " ")
}

// autonomyPromptStopWords is the small list of tokens that get
// stripped from a prompt before computing token-set similarity in
// autonomyFailureCooldown. Discriminative words (feature names,
// component names, verbs of intent) survive; filler ("the", "and",
// "with"), template boilerplate ("commit", "task", "project"), and
// the universal verbs that appear in every Implement-template prompt
// ("implement", "complete") are dropped so similarity is driven by
// what's actually different between prompts.
//
// Conservative on the stopword list: false-negatives ("topic
// detected as different when it's the same") are acceptable because
// they only delay suppression by one tick; false-positives
// ("unrelated topics flagged as the same") would silently kill real
// work. Hence the threshold is high (0.55) and the stopword list is
// short.
var autonomyPromptStopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "from": {}, "this": {},
	"that": {}, "into": {}, "when": {}, "what": {}, "which": {}, "where": {},
	"have": {}, "will": {}, "must": {}, "should": {}, "can": {}, "are": {},
	"its": {}, "was": {}, "were": {}, "been": {}, "being": {},
	"all": {}, "any": {}, "each": {}, "some": {}, "one": {}, "two": {},
	"per": {}, "via": {}, "use": {}, "uses": {}, "used": {}, "using": {},
	// Template boilerplate from the Implement-template prompts —
	// these appear in nearly every autonomy task and don't carry
	// topical signal.
	"implement": {}, "complete": {}, "completion": {}, "task": {},
	"project": {}, "commit": {}, "commits": {}, "committed": {},
	"file": {}, "files": {}, "modify": {}, "modifies": {}, "update": {},
	"updates": {}, "add": {}, "adds": {}, "added": {}, "create": {},
	"creates": {}, "created": {}, "read": {}, "write": {}, "writes": {},
	"run": {}, "runs": {}, "test": {}, "tests": {}, "testing": {},
	"acceptance": {}, "criteria": {}, "current": {}, "context": {},
	"meaningful": {}, "message": {}, "relevant": {}, "code": {},
	"changes": {}, "change": {}, "command": {}, "exists": {}, "exist": {},
	"only": {}, "once": {}, "stop": {}, "analysis": {}, "planning": {},
}

// autonomyPromptTitle extracts the topical "headline" of an
// autonomy prompt: the text before the first colon, capped at 120
// chars so a colonless prose-only prompt still produces a stable
// title rather than the whole body. The topic of an Implement-
// template task lives in the title ("Implement Ghost Mode
// Enhancement"); the body is detail and varies wildly between
// rephrases of the same feature, which is why title-only
// comparison catches "same topic, different wording" reliably
// where whole-prompt comparison drowns in description noise.
func autonomyPromptTitle(prompt string) string {
	s := strings.TrimSpace(prompt)
	if i := strings.IndexByte(s, ':'); i >= 0 && i <= 120 {
		s = s[:i]
	} else if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// autonomyPromptTokens turns a prompt into a stable set of
// discriminative lowercase tokens for similarity comparison.
// Stop-words and very short tokens are dropped because they don't
// carry topical signal and would inflate the union, dragging the
// Jaccard score toward zero on otherwise identical topics.
func autonomyPromptTokens(prompt string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, w := range strings.Fields(normalizeAutonomyPrompt(prompt)) {
		// Strip surrounding punctuation that survived the lowercase/
		// whitespace pass — keeps "ghost-mode," and "ghost-mode"
		// hashing the same.
		w = strings.Trim(w, ".,;:!?\"'`()[]{}")
		if len(w) < 3 {
			continue
		}
		if _, skip := autonomyPromptStopWords[w]; skip {
			continue
		}
		out[w] = struct{}{}
	}
	return out
}

// autonomyPromptSimilarity returns the Jaccard similarity between
// two prompts' title token sets — |A∩B| / |A∪B|, where A and B
// are the discriminative-token sets of each prompt's title (text
// before the first colon). 1.0 means the titles share every
// discriminative token; 0.0 means they share none.
//
// Title-only is deliberate: comparing whole prompts dilutes the
// topical signal because Implement-template tasks all share
// hundreds of boilerplate description tokens ("read or write
// project/CURRENT_TASK.md", "run the test command", "commit the
// changes"), and the description side of two same-topic prompts
// can diverge wildly when the autonomy LLM rephrases. The title
// is where the topic actually lives.
func autonomyPromptSimilarity(a, b string) float64 {
	left := autonomyPromptTokens(autonomyPromptTitle(a))
	right := autonomyPromptTokens(autonomyPromptTitle(b))
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	intersect := 0
	for w := range left {
		if _, ok := right[w]; ok {
			intersect++
		}
	}
	union := len(left) + len(right) - intersect
	if union == 0 {
		return 0
	}
	return float64(intersect) / float64(union)
}

// autonomyFailureSimilarityThreshold is the title-Jaccard score at
// which two prompts are treated as the same topic for cooldown
// counting. 0.30 was tuned against the snake project's Ghost Mode
// incident: two rephrased Ghost Mode titles score 0.50 against
// each other and 0.33 against the most-different rephrase, while
// unrelated feature titles ("Implement Network-based Leaderboard"
// vs "Implement Configurable Effect Toggles") score 0.0 — so 0.30
// catches the topical-restatement case cleanly without
// false-positives on legitimately-new features.
const autonomyFailureSimilarityThreshold = 0.30

func buildAutonomyIdempotencyKey(projectID, taskType, workflowID, prompt string, now time.Time) string {
	bucket := now.UTC().Format("2006010215")
	base := projectID + "|" + taskType + "|" + workflowID + "|" + normalizeAutonomyPrompt(prompt)
	sum := sha256.Sum256([]byte(base))
	return fmt.Sprintf("auto:%s:%x", bucket, sum[:8])
}

// autonomyContextFilePath resolves the per-project workspace-relative
// path to the project context markdown the daemon includes in the
// autonomy lead's prompt. Defaults to ".autonomy/PROJECT_CONTEXT.md"
// — a hidden directory so daemon bookkeeping doesn't collide with
// the project's own root-level files.
//
// Safety: rejects absolute paths and paths containing ".." segments
// to prevent a misconfigured project YAML from reading arbitrary
// host files. Falls back to the default on rejection.
func autonomyContextFilePath(p *registry.Project) string {
	const def = ".autonomy/PROJECT_CONTEXT.md"
	if p == nil {
		return def
	}
	v := strings.TrimSpace(p.Autonomy.ContextFilePath)
	if v == "" {
		return def
	}
	if filepath.IsAbs(v) {
		return def
	}
	cleaned := filepath.Clean(v)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return def
	}
	return cleaned
}

// autonomyDuplicateWindow resolves the per-project completion-dedup
// window. Empty / unset → 24h default (backlog-style projects). "0" or
// "0s" → completion dedup disabled (cron-style projects whose lead
// deliberately fires the same prompt every tick — trading,
// observability heartbeats, etc.). The active-task check is unaffected
// either way.
func autonomyDuplicateWindow(p *registry.Project) time.Duration {
	if p == nil || strings.TrimSpace(p.Autonomy.DuplicateWindow) == "" {
		return 24 * time.Hour
	}
	d, err := time.ParseDuration(p.Autonomy.DuplicateWindow)
	if err != nil || d < 0 {
		return 24 * time.Hour
	}
	return d
}

func findAutonomyDuplicate(tasks []*persistence.Task, taskType, workflowID, prompt string, completionWindow time.Duration) (string, bool) {
	normalized := normalizeAutonomyPrompt(prompt)
	for _, task := range tasks {
		summary := summarizeAutonomyTask(task)
		if normalizeAutonomyPrompt(summary.prompt) != normalized {
			continue
		}
		if summary.taskType != taskType || summary.workflow != workflowID {
			continue
		}
		switch summary.status {
		case persistence.TaskStatusQueued, persistence.TaskStatusPending, persistence.TaskStatusAwaitingApproval, persistence.TaskStatusLeased, persistence.TaskStatusRunning, persistence.TaskStatusWaitingForChildren:
			return fmt.Sprintf("matching active task %s already exists with status %s", summary.id, summary.status), true
		case persistence.TaskStatusCompleted:
			if completionWindow <= 0 {
				continue
			}
			if time.Since(summary.updatedAt) < completionWindow {
				return fmt.Sprintf("matching task %s completed within the last %s", summary.id, completionWindow), true
			}
		}
	}
	return "", false
}

// autonomyFailureCooldown suppresses an autonomous task when 2+
// recently-failed tasks targeted the same topic. "Same topic" is
// either an exact normalized-prompt match (the strict, original
// behaviour) OR a Jaccard token-set similarity ≥ the configured
// threshold (catches restated prompts that target the same
// feature with different wording — see snake project's Ghost Mode
// incident, where the autonomy LLM rephrased "Implement Ghost
// Mode" twice and the strict-equality check missed both).
//
// Counts each FAILED task record once. The task model already
// retries internally up to max_attempts before flipping to
// FAILED, so two FAILED records represents 6+ attempts and is a
// fair "stop trying" signal.
func autonomyFailureCooldown(tasks []*persistence.Task, taskType, workflowID, prompt string) (string, bool) {
	normalized := normalizeAutonomyPrompt(prompt)
	failures := 0
	var sampleID string
	for _, task := range tasks {
		summary := summarizeAutonomyTask(task)
		if summary.status != persistence.TaskStatusFailed {
			continue
		}
		if time.Since(summary.createdAt) > 3*time.Hour {
			continue
		}
		if summary.taskType != taskType || summary.workflow != workflowID {
			continue
		}
		// Strict match — fast path, also robust against the
		// pathological case where token-set is too small for a
		// meaningful Jaccard.
		if normalizeAutonomyPrompt(summary.prompt) == normalized {
			failures++
			if sampleID == "" {
				sampleID = summary.id
			}
			continue
		}
		// Fuzzy match — same topic, different wording.
		if autonomyPromptSimilarity(prompt, summary.prompt) >= autonomyFailureSimilarityThreshold {
			failures++
			if sampleID == "" {
				sampleID = summary.id
			}
		}
	}
	if failures >= 2 {
		return fmt.Sprintf("%d similar tasks failed within the last 3h (e.g. %s) — autonomy must propose a different topic", failures, sampleID), true
	}
	return "", false
}

func autonomyCircuitOpen(tasks []*persistence.Task) (string, bool) {
	var terminal []autonomyTaskSummary
	for _, task := range tasks {
		summary := summarizeAutonomyTask(task)
		if summary.source != persistence.TaskCreationSourceAutonomous {
			continue
		}
		switch summary.status {
		case persistence.TaskStatusCompleted, persistence.TaskStatusFailed, persistence.TaskStatusCancelled:
			terminal = append(terminal, summary)
		}
	}
	if len(terminal) < 8 {
		return "", false
	}
	sort.Slice(terminal, func(i, j int) bool {
		return terminal[i].createdAt.After(terminal[j].createdAt)
	})
	if len(terminal) > 12 {
		terminal = terminal[:12]
	}
	failures := 0
	completed := 0
	for _, task := range terminal {
		switch task.status {
		case persistence.TaskStatusCompleted:
			completed++
		case persistence.TaskStatusFailed, persistence.TaskStatusCancelled:
			failures++
		}
	}
	if len(terminal) >= 8 && failures >= 6 && failures > completed*2 {
		return fmt.Sprintf("recent autonomous completion ratio too low (%d failed/cancelled, %d completed)", failures, completed), true
	}
	return "", false
}

func (m *Manager) checkRateLimit(project *registry.Project) bool {
	if project.Autonomy.MaxTasksPerHour <= 0 {
		return true // no limit
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.checkRateLimitLocked(project)
}

// checkRateLimitLocked decides whether a new autonomous task can
// be scheduled given the project's MaxTasksPerHour cap.
//
// Source of truth is the DB, not the in-memory counter — the
// counter is reset to zero on every daemon restart, which used
// to silently grant the project an extra task per restart. The
// in-memory counter is kept as a fast-path so an active project
// doesn't hit the DB on every tick.
//
// Refresh strategy: if the cache hasn't been seeded from the DB
// in the last 5 minutes, re-seed by counting autonomous tasks
// created in the last rolling hour. 5 minutes is long enough
// that the hot path stays cheap (cached DB count between
// successive ticks of the same project loop, since project
// loops tick at most every 60m+) and short enough that
// out-of-band task creation (manual API, tests) gets reflected
// promptly. A miss on the cache costs one indexed COUNT-style
// query against tasks(project_id, created_at).
func (m *Manager) checkRateLimitLocked(project *registry.Project) bool {
	now := time.Now()
	reset, ok := m.hourReset[project.ID]
	const cacheTTL = 5 * time.Minute
	if !ok || now.Sub(reset) >= cacheTTL {
		count, err := m.countAutonomousTasksLastHour(project.ID, now)
		if err != nil {
			// DB hiccup: fall back to the in-memory count.
			// Operator-facing rate-limit accuracy degrades to
			// "best-effort across restarts" on DB outage, but
			// the daemon never wedges and the next tick will
			// re-attempt the seed.
			m.logger.Warn().Err(err).Str("project", project.ID).Msg("autonomy rate-limit DB seed failed; using in-memory count")
		} else {
			m.taskCounts[project.ID] = count
		}
		m.hourReset[project.ID] = now
	}

	return m.taskCounts[project.ID] < project.Autonomy.MaxTasksPerHour
}

// countAutonomousTasksLastHour queries the task repository for
// how many AUTONOMOUS-source tasks the project created in the
// past hour. Used by the rate-limit check to survive daemon
// restarts (the in-memory counter resets to zero on every
// restart and previously granted an extra task per restart per
// project).
func (m *Manager) countAutonomousTasksLastHour(projectID string, now time.Time) (int, error) {
	if m.taskRepo == nil {
		return 0, fmt.Errorf("task repository not configured")
	}
	cutoff := now.Add(-1 * time.Hour)
	// PageSize 100 is well above MaxTasksPerHour for any sane
	// project — operators don't set per-hour caps in the
	// hundreds — so a full hour's tasks fit even if every one
	// was scheduled at the cap.
	tasks, err := m.taskRepo.List(context.Background(), persistence.TaskFilter{
		ProjectID: &projectID,
		PageSize:  100,
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, t := range tasks {
		if t == nil {
			continue
		}
		if t.CreationSource != persistence.TaskCreationSourceAutonomous {
			continue
		}
		if t.CreatedAt.Before(cutoff) {
			continue
		}
		count++
	}
	return count, nil
}

func (m *Manager) recordTaskCreated(projectID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordTaskCreatedLocked(projectID)
}

func (m *Manager) recordTaskCreatedLocked(projectID string) {
	m.taskCounts[projectID]++
}

func extractPrompt(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		Context struct {
			Prompt string `json:"prompt"`
		} `json:"context"`
	}
	if json.Unmarshal(payload, &p) == nil {
		return p.Context.Prompt
	}
	return ""
}

func extractResultMessage(result []byte) string {
	if len(result) == 0 {
		return ""
	}
	var r struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(result, &r) == nil {
		return r.Message
	}
	return ""
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
