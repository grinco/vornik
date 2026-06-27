package service

// Adapters for api.WorkflowArchitect — bridges *memetic.Architect
// (returns *persistence.WorkflowProposal) to the api package's
// narrow interface (returns any), and supplies the
// WorkflowSource / ExecutionLookup / TelemetrySource / ProposalSink
// dependencies the architect needs. Kept in the service container so
// the api package stays free of imports on memetic / persistence
// repositories / the workflowtelemetry adapter.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/memetic"
	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowtelemetry"
)

// workflowArchitectAdapter is the api.WorkflowArchitect concrete.
// Wraps a *memetic.Architect and exposes Propose with `any` return
// so the api package isn't entangled with the persistence type.
type workflowArchitectAdapter struct {
	arch *memetic.Architect
	// instincts is non-nil only when Consumer B's gate is on. It backs
	// the rejection write-back recorder (RejectionRecorder); nil → no
	// write-back wired.
	instincts persistence.InstinctRepository
}

// RejectionRecorder returns the api.WorkflowRejectionRecorder for this
// architect, or nil when Consumer B is not wired (instinct repo
// absent). The api Server treats a nil recorder as "no write-back".
func (a *workflowArchitectAdapter) RejectionRecorder() *workflowRejectionRecorder {
	if a == nil || a.arch == nil || a.instincts == nil {
		return nil
	}
	return &workflowRejectionRecorder{arch: a.arch, instincts: a.instincts}
}

func (a *workflowArchitectAdapter) Propose(ctx context.Context, workflowID string) (any, error) {
	if a == nil || a.arch == nil {
		return nil, fmt.Errorf("workflow architect not wired")
	}
	return a.arch.Propose(ctx, workflowID)
}

func (a *workflowArchitectAdapter) ProposeWithEvidence(ctx context.Context, workflowID string, evidenceRunIDs []string) (any, error) {
	if a == nil || a.arch == nil {
		return nil, fmt.Errorf("workflow architect not wired")
	}
	return a.arch.ProposeWithEvidence(ctx, workflowID, evidenceRunIDs)
}

// UI returns a ui.MemeticArchitectUI-shaped view of the same
// underlying architect. Distinct from the api-facing Propose because
// the UI handler stamps the trigger with the proposal_id and needs
// the typed *persistence.WorkflowProposal back (not `any`).
func (a *workflowArchitectAdapter) UI() *workflowArchitectUIAdapter {
	if a == nil || a.arch == nil {
		return nil
	}
	return &workflowArchitectUIAdapter{arch: a.arch}
}

// workflowArchitectUIAdapter satisfies ui.MemeticArchitectUI — the
// "Generate candidate" button on the Black Box trigger detail page.
type workflowArchitectUIAdapter struct {
	arch *memetic.Architect
}

func (a *workflowArchitectUIAdapter) Propose(ctx context.Context, workflowID string) (*persistence.WorkflowProposal, error) {
	if a == nil || a.arch == nil {
		return nil, fmt.Errorf("workflow architect not wired")
	}
	return lowConfidenceIsNoProposal(a.arch.Propose(ctx, workflowID))
}

func (a *workflowArchitectUIAdapter) ProposeWithEvidence(ctx context.Context, workflowID string, evidenceRunIDs []string) (*persistence.WorkflowProposal, error) {
	if a == nil || a.arch == nil {
		return nil, fmt.Errorf("workflow architect not wired")
	}
	return lowConfidenceIsNoProposal(a.arch.ProposeWithEvidence(ctx, workflowID, evidenceRunIDs))
}

// lowConfidenceIsNoProposal maps the architect's deliberate low-confidence
// PASS verdict to the benign "no proposal" shape (nil, nil). ErrLowConfidence
// is not a failure: the prompt instructs the model to emit confidence 0.0 when
// the evidence supports no structural change ("PROPOSE OR PASS"), and the
// system filters those out. Returning (nil, nil) lets the UI handler render it
// via its existing "no proposal" branch — an informational notice — instead of
// logging WARN "architect failed" and showing the operator a "confidence below
// threshold" error banner (2026-06-12 mislabeling). This mirrors the API's
// 204-No-Content treatment in mapArchitectError. Every other error — including
// real operational failures — passes through unchanged.
func lowConfidenceIsNoProposal(p *persistence.WorkflowProposal, err error) (*persistence.WorkflowProposal, error) {
	if errors.Is(err, memetic.ErrLowConfidence) {
		return nil, nil
	}
	return p, err
}

// fsWorkflowSource implements memetic.WorkflowSource by reading
// <configDir>/workflows/<id>.md from disk. configDir is the
// deployed configs tree (~/.config/vornik/configs in dev), NOT the
// source tree, so the daemon always reads what the operator
// promoted — matches the broader two-trees discipline.
type fsWorkflowSource struct {
	configDir string
}

func (s *fsWorkflowSource) Load(_ context.Context, workflowID string) ([]byte, error) {
	if workflowID == "" {
		return nil, fmt.Errorf("fsWorkflowSource: empty workflowID")
	}
	if s.configDir == "" {
		return nil, fmt.Errorf("fsWorkflowSource: configDir not set")
	}
	// Anchored join. workflowID is operator-supplied at the admin
	// endpoint, so we defend against `..` traversal — Clean
	// catches the obvious cases, then the prefix check pins the
	// final path inside workflowsDir even if Clean produced
	// something weird (Windows-style separator, NUL byte).
	workflowsDir := filepath.Join(s.configDir, "workflows")
	candidate := filepath.Clean(filepath.Join(workflowsDir, workflowID+".md"))
	if !strings.HasPrefix(candidate, workflowsDir+string(filepath.Separator)) {
		return nil, fmt.Errorf("fsWorkflowSource: workflowID escapes workflows directory")
	}
	data, err := os.ReadFile(candidate)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// sqlExecutionLookup answers BelongsTo directly from executions.
// executions.workflow_id is the same source the telemetry rollup and
// healing-trigger detector use, so validation matches the evidence
// surface shown to operators.
type sqlExecutionLookup struct {
	db *sql.DB
}

func (l *sqlExecutionLookup) BelongsTo(ctx context.Context, workflowID string, ids []string) ([]string, bool, error) {
	if l == nil || l.db == nil {
		return nil, false, fmt.Errorf("sqlExecutionLookup: nil db")
	}
	if len(ids) == 0 {
		return nil, true, nil
	}
	rows, err := l.db.QueryContext(ctx, `
		SELECT e.id
		FROM executions e
		WHERE e.id = ANY($1)
		  AND e.workflow_id = $2`,
		pq.Array(ids), workflowID,
	)
	if err != nil {
		return nil, false, fmt.Errorf("sqlExecutionLookup: query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var valid []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false, err
		}
		valid = append(valid, id)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return valid, len(valid) == len(ids), nil
}

// sharedInstinctMetrics returns the process-wide *observability.InstinctMetrics,
// creating it once against the observability registry and caching it on the
// Container (TRACK ARCH-METRIC). observability.NewInstinctMetrics registers
// collectors via promauto, which PANICS on a duplicate registration — so every
// consumer (worker, executor, recovery resolver, workflow architect) must share
// this one instance. Returns nil when observability is disabled (no registry);
// callers are nil-safe. Safe to call from the second initHTTPServer pass (where
// the registry already exists) and again from wireComponentMetrics — the second
// call returns the cached value rather than re-registering.
func (c *Container) sharedInstinctMetrics() *observability.InstinctMetrics {
	if c == nil {
		return nil
	}
	if c.instinctMetrics != nil {
		return c.instinctMetrics
	}
	reg := c.observabilityRegistry()
	if reg == nil {
		return nil
	}
	c.instinctMetrics = observability.NewInstinctMetrics(reg)
	return c.instinctMetrics
}

// newWorkflowArchitectAdapter wires the architect's dependencies
// out of the container's primitives and returns the api-package-
// facing adapter. Returns nil when prerequisites are missing
// (database absent / no chat client / no proposals repo) — the
// api handler is nil-safe and surfaces 503 in that case.
// instinctMetrics threads the shared *instinct.Metrics so the architect can
// bump vornik_instinct_applications_total{surface="architect_evidence"} as it
// records each application (TRACK ARCH-METRIC). It is created later than the
// other deps — wireComponentMetrics builds instinct.NewMetrics(reg) only after
// observability exists — so the caller passes it in once it has been created
// (see Container.sharedInstinctMetrics). nil-safe: nil disables only the
// counter, never the application ROWS.
func newWorkflowArchitectAdapter(
	db *sql.DB,
	chatClient chat.Provider,
	proposals persistence.WorkflowProposalRepository,
	configDir string,
	instincts persistence.InstinctRepository,
	instinctMetrics *observability.InstinctMetrics,
	logger zerolog.Logger,
) *workflowArchitectAdapter {
	if db == nil || chatClient == nil || proposals == nil || configDir == "" {
		return nil
	}
	cfg := memetic.DefaultConfig()
	// Slice 5 architect-pause: operator kill switch via env. A
	// non-empty value other than "0"/"false" pauses the architect
	// globally; Propose short-circuits with ErrArchitectPaused
	// before any LLM call.
	if v := os.Getenv("VORNIK_ARCHITECT_PAUSED"); v != "" && v != "0" && v != "false" {
		cfg.Paused = true
	}
	// LEVEL 3 of the three-level kill switch (§8.5): per-proposal-class
	// blocking. VORNIK_ARCHITECT_DISABLED_KINDS is a comma-separated
	// list of proposal kinds the operator refuses (e.g.
	// "add_step,change_role_assignment"). Empty = no class blocked.
	// LEVEL 2 (per-workflow architect_enabled frontmatter) is read by
	// the architect at Propose time from the workflow YAML.
	if v := strings.TrimSpace(os.Getenv("VORNIK_ARCHITECT_DISABLED_KINDS")); v != "" {
		cfg.DisabledKinds = map[string]bool{}
		for _, k := range strings.Split(v, ",") {
			if k = strings.TrimSpace(k); k != "" {
				cfg.DisabledKinds[k] = true
			}
		}
	}
	// Decision logging (component=memetic). Pairs with the chat
	// LoggingProvider: the architect logs WHY a turn ended (parsed
	// confidence, retry, rejecting verdict); the provider logs the
	// raw call + response. Together they make the "confidence 0.00"
	// path fully observable.
	opts := []memetic.ArchitectOption{
		memetic.WithLogger(logger.With().Str("component", "memetic").Logger()),
	}
	// Consumer B — wire workflow-domain instincts as evidence priors.
	// The caller only passes a non-nil repo when instinct.enabled &&
	// instinct.consumers.architect_priors; nil keeps the architect's
	// behaviour byte-for-byte unchanged.
	if instincts != nil {
		opts = append(opts, memetic.WithInstincts(instincts))
		// Slice 7 (surfacing half), review item W2 — log architect-evidence
		// instinct applications. The same repo that supplies the priors also
		// receives the accepted/rejected application rows (it satisfies
		// memetic.ApplicationWriter via RecordApplication). Gated by the same
		// architect_priors flag the caller already checks, so this never wires
		// when Consumer B is off.
		opts = append(opts, memetic.WithApplicationWriter(instincts))
		// TRACK ARCH-METRIC — wire the architect-evidence ApplicationsTotal
		// counter. Slice 7 left this dark because the instinct Metrics value did
		// not exist when this factory ran (boot order: the adapter is built in the
		// second initHTTPServer pass, BEFORE wireComponentMetrics creates
		// instinct.NewMetrics(reg)). The fix threads the SHARED *instinct.Metrics
		// in via the factory: Container.sharedInstinctMetrics() builds it once on
		// first use (observability is already up by the second initHTTPServer pass)
		// and caches it, so wireComponentMetrics reuses the same pointer for the
		// worker/executor/resolver and every applications counter lands on one
		// series. nil (observability off / metrics not yet created) keeps only the
		// counter dark; the application ROWS above are unaffected. Gated by the
		// same architect_priors flag — this block never runs when Consumer B is
		// off, so gate-off scoring is byte-for-byte unchanged.
		if instinctMetrics != nil {
			opts = append(opts, memetic.WithInstinctMetrics(instinctMetrics))
		}
	}
	arch := memetic.New(
		chatClient,
		&memeticTelemetrySource{svc: workflowtelemetry.NewService(db)},
		&fsWorkflowSource{configDir: configDir},
		&sqlExecutionLookup{db: db},
		proposals,
		cfg,
		opts...,
	)
	return &workflowArchitectAdapter{arch: arch, instincts: instincts}
}

// workflowRejectionRecorder satisfies api.WorkflowRejectionRecorder.
// It bridges the api package's opaque-proposal reject hook to
// *memetic.Architect.RecordRejection + the InstinctRepository sink,
// keeping the api package free of memetic / instinct imports. Returns
// nil (no-op) when either dependency is missing so the reject path
// degrades gracefully.
type workflowRejectionRecorder struct {
	arch      *memetic.Architect
	instincts persistence.InstinctRepository
}

func (r *workflowRejectionRecorder) RecordRejection(ctx context.Context, proposal any) error {
	if r == nil || r.arch == nil || r.instincts == nil {
		return nil
	}
	p, ok := proposal.(*persistence.WorkflowProposal)
	if !ok || p == nil {
		return fmt.Errorf("workflowRejectionRecorder: unexpected proposal type %T", proposal)
	}
	return r.arch.RecordRejection(ctx, r.instincts, p)
}

// memeticTelemetrySource bridges *workflowtelemetry.Service to
// memetic.TelemetrySource. Distinct from workflowTelemetryAdapter
// because the api-package adapter returns any (for JSON); memetic
// needs the typed *Rollup.
type memeticTelemetrySource struct {
	svc *workflowtelemetry.Service
}

func (m *memeticTelemetrySource) ForWorkflow(ctx context.Context, workflowID string, since time.Time) (*workflowtelemetry.Rollup, error) {
	return m.svc.ForWorkflow(ctx, workflowID, since)
}
