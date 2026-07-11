package service

// Task 3.3 — real wiring for the Fix-It Doctor's deny-by-default action
// dispatcher (internal/fixitdoctor/dispatch.go). Each adapter here wraps
// an EXISTING rollback-capable pipeline — never a new mutation path:
//
//   - config_apply_gate -> featuredoctor.PlanEnable/ApplyEnable, the
//     SAME pipeline the admin-only /api/v1/doctor/features/{id}/enable
//     endpoint drives (internal/api/featuredoctor_enable_handler.go).
//   - config_apply (EE) -> the ControlPlaneProposal ledger +
//     controlplane.ApplyEngine, the SAME pipeline the operator console
//     drives (internal/ui/admin_control_plane*.go).
//   - retry_task -> persistence.TaskRepository.RequeueTerminalTask, the
//     SAME first-wins/409 primitive internal/api/handlers.go's RetryTask
//     handler uses.
//   - set_secret -> projectdoctor.Doctor.SetSecret's own declared-names
//     gate (internal/projectdoctor/doctor.go) — this adapter does not
//     re-implement the gate, only maps its "not declared" refusal onto
//     fixitdoctor.ErrActionConflict.
//
// reprobe_integration -> ui.Server.ReprobeIntegrationLive, the SAME live
// probe path IntegrationRecheck (POST /integrations/{kind}/recheck)
// drives (internal/ui/integrations.go), wired via
// fixitIntegrationReprober (fixit_ui_bridge_adapter.go) — task 3.3 left
// this UNWIRED (nil IntegrationReprober, Dispatch failing closed with
// "not configured") because the live per-kind candidate-config resolver
// (ui.Server.currentIntegrationValues) lived behind unexported Server
// state the dispatch adapter had no seam to reach; task 3.4 adds that
// seam (internal/ui/fixit_bridge.go) and wires it here.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/fixitdoctor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/projectdoctor"
)

// --- config_apply_gate (CE) ------------------------------------------------

// fixitConfigReader adapts *config.Config to featuredoctor.ConfigReader,
// mirroring api package's unexported configGateReader (kept separate —
// that type is api-package-private and this package must not reach
// into it).
type fixitConfigReader struct{ cfg *config.Config }

func (r fixitConfigReader) GateValue(key string) (any, bool) {
	return config.LookupByPath(r.cfg, key)
}

// fixitTaskLister adapts persistence.TaskRepository to
// featuredoctor.TaskLister, mirroring api package's taskListerAdapter.
type fixitTaskLister struct{ repo persistence.TaskRepository }

func (t fixitTaskLister) HasActiveTasks(ctx context.Context) (bool, error) {
	if t.repo == nil {
		return false, nil
	}
	sRunning := persistence.TaskStatusRunning
	nRunning, err := t.repo.Count(ctx, persistence.TaskFilter{Status: &sRunning})
	if err != nil {
		return false, err
	}
	if nRunning > 0 {
		return true, nil
	}
	sLeased := persistence.TaskStatusLeased
	nLeased, err := t.repo.Count(ctx, persistence.TaskFilter{Status: &sLeased})
	if err != nil {
		return false, err
	}
	return nLeased > 0, nil
}

// fixitConfigReloaderAdapter adapts *config.ConfigReloader to
// featuredoctor.Reloader, mirroring api.configReloaderAdapter /
// ui.integrationsReloaderAdapter — the underlying reload is synchronous
// and bounded internally, so ctx is accepted but ignored.
type fixitConfigReloaderAdapter struct {
	r *config.ConfigReloader
}

func (a fixitConfigReloaderAdapter) Reload(_ context.Context) error {
	if a.r == nil {
		return nil
	}
	return a.r.Reload()
}

// fixitGatePipeline implements fixitdoctor.GatePipeline over
// featuredoctor.PlanEnable/ApplyEnable — the exact pipeline the admin
// EnableFeature endpoint drives, applied to whichever registered
// Feature declares key as one of its Gates.
type fixitGatePipeline struct {
	deps       featuredoctor.Deps
	configPath string
	reloader   featuredoctor.Reloader
}

// findGateFeature resolves key to the Feature that declares it as a
// Gate — the "only keys the registry knows as gates" restriction
// (design §5.3): a key that isn't any registered feature's gate cannot
// be applied through this pipeline at all.
func findGateFeature(key string) (featuredoctor.Feature, bool) {
	for _, f := range featuredoctor.Registry() {
		for _, g := range f.Gates {
			if g.Key == key {
				return f, true
			}
		}
	}
	return featuredoctor.Feature{}, false
}

func (p *fixitGatePipeline) Plan(ctx context.Context, key string) (string, error) {
	f, ok := findGateFeature(key)
	if !ok {
		return "", fmt.Errorf("%w: %q is not a registered feature-doctor gate", fixitdoctor.ErrActionConflict, key)
	}
	plan, err := featuredoctor.PlanEnable(ctx, f, p.deps)
	if err != nil {
		return "", err
	}
	return renderGateDiff(plan.Changes), nil
}

func (p *fixitGatePipeline) Apply(ctx context.Context, key string) (string, error) {
	f, ok := findGateFeature(key)
	if !ok {
		return "", fmt.Errorf("%w: %q is not a registered feature-doctor gate", fixitdoctor.ErrActionConflict, key)
	}
	if p.configPath == "" {
		return "", errors.New("config path not wired; cannot write gate changes")
	}
	plan, err := featuredoctor.PlanEnable(ctx, f, p.deps)
	if err != nil {
		return "", err
	}
	writer := &featuredoctor.FileConfigWriter{Path: p.configPath}
	result, err := featuredoctor.ApplyEnable(ctx, f, p.deps, plan, writer, p.reloader)
	if err != nil {
		// ApplyEnable already rolled back (Restore) before returning this
		// error — Dispatch classifies it as "failed", never "applied".
		return "", err
	}
	return result.Detail, nil
}

func renderGateDiff(changes []featuredoctor.GateChange) string {
	if len(changes) == 0 {
		return "no changes required — already at the target value"
	}
	var b strings.Builder
	for _, c := range changes {
		fmt.Fprintf(&b, "%s: %v -> %v\n", c.Key, c.From, c.To)
	}
	return b.String()
}

// --- config_apply (EE) ------------------------------------------------

// fixitConfigProposalPipeline implements fixitdoctor.ConfigProposalPipeline
// over the SAME ControlPlaneProposal ledger + ApplyEngine the operator
// console drives. File patches ONE key into the live config.yaml
// (config.SetYAMLKey, comment-preserving) and records the resulting
// whole-file replace as a DRAFT proposal with ProposedBy="fix_it_doctor".
// Apply then transitions DRAFT->APPROVED with actor=the human operator
// (never "fix_it_doctor") BEFORE driving ApplyEngine.Apply — the
// SetStatus call is what actually enforces proposer != approver
// (persistence.ErrProposalSelfApprove), not a check duplicated here.
type fixitConfigProposalPipeline struct {
	proposals  persistence.ProposalRepository
	applier    *controlplane.ApplyEngine
	configPath string
}

func blastRadiusForProject(projectID string) string {
	if projectID == "" {
		return persistence.ProposalScopeDaemon
	}
	return persistence.ProposalScopeProject
}

func (p *fixitConfigProposalPipeline) File(ctx context.Context, projectID, key, value string) (string, string, error) {
	if p.proposals == nil || p.configPath == "" {
		return "", "", errors.New("control-plane proposal pipeline not wired")
	}
	current, err := os.ReadFile(p.configPath) //nolint:gosec // operator-configured daemon config path, not user input
	if err != nil {
		return "", "", fmt.Errorf("read config: %w", err)
	}
	patched, _, err := config.SetYAMLKey(current, key, value)
	if err != nil {
		return "", "", fmt.Errorf("patch %q: %w", key, err)
	}
	diff := fmt.Sprintf("%s -> %s", key, value)
	proposal := &persistence.ControlPlaneProposal{
		ID:           persistence.GenerateID("cpp"),
		ProjectID:    projectID,
		Kind:         persistence.ProposalKindConfig,
		BlastRadius:  blastRadiusForProject(projectID),
		Title:        "Fix-It Doctor: " + key,
		Diff:         diff,
		Rationale:    "proposed by the Fix-It Doctor repair chat",
		ProposedBy:   "fix_it_doctor",
		ApplyTarget:  filepath.Base(p.configPath),
		ApplyContent: string(patched),
	}
	if err := p.proposals.Create(ctx, proposal); err != nil {
		return "", "", err
	}
	return proposal.ID, diff, nil
}

func (p *fixitConfigProposalPipeline) Apply(ctx context.Context, proposalID, actor string) error {
	if p.proposals == nil || p.applier == nil {
		return errors.New("control-plane apply engine not wired")
	}
	// The human operator approves what fix_it_doctor proposed — proposer
	// != approver is enforced HERE, by the repository's own guard, not
	// re-implemented by this adapter.
	if err := p.proposals.SetStatus(ctx, proposalID, persistence.ProposalStatusApproved, actor); err != nil {
		if errors.Is(err, persistence.ErrProposalSelfApprove) {
			return fmt.Errorf("%w: %v", fixitdoctor.ErrActionConflict, err)
		}
		return err
	}
	if err := p.applier.Apply(ctx, proposalID, actor, false); err != nil {
		if errors.Is(err, controlplane.ErrStaleBase) ||
			errors.Is(err, controlplane.ErrScaffoldConflict) ||
			errors.Is(err, controlplane.ErrBusy) ||
			errors.Is(err, controlplane.ErrReviewOnly) {
			return fmt.Errorf("%w: %v", fixitdoctor.ErrActionConflict, err)
		}
		return err
	}
	return nil
}

func (p *fixitConfigProposalPipeline) Rollback(ctx context.Context, proposalID string) error {
	if p.applier == nil {
		return errors.New("control-plane apply engine not wired")
	}
	return p.applier.Rollback(ctx, proposalID)
}

// --- retry_task ------------------------------------------------

// fixitTaskRetrier implements fixitdoctor.TaskRetrier over
// persistence.TaskRepository.RequeueTerminalTask — the same first-wins/
// 409 primitive internal/api/handlers.go's RetryTask handler uses
// (attempt bump + atomic terminal->QUEUED transition).
type fixitTaskRetrier struct {
	tasks persistence.TaskRepository
}

func (t fixitTaskRetrier) Retry(ctx context.Context, projectID, taskID string) (string, error) {
	if t.tasks == nil {
		return "", errors.New("task repository not wired")
	}
	task, err := t.tasks.Get(ctx, taskID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return "", fmt.Errorf("%w: task not found", fixitdoctor.ErrActionConflict)
		}
		return "", err
	}
	if task.ProjectID != projectID {
		return "", fmt.Errorf("%w: task not in scope", fixitdoctor.ErrActionConflict)
	}
	terminal := map[persistence.TaskStatus]bool{
		persistence.TaskStatusFailed:    true,
		persistence.TaskStatusCancelled: true,
		persistence.TaskStatusCompleted: true,
	}
	if !terminal[task.Status] {
		return "", fmt.Errorf("%w: task is %s, not in a retryable terminal state", fixitdoctor.ErrActionConflict, task.Status)
	}
	attempt := task.Attempt + 1
	maxAttempts := task.MaxAttempts
	if attempt > maxAttempts {
		maxAttempts = attempt
	}
	transitioned, err := t.tasks.RequeueTerminalTask(ctx, taskID, attempt, maxAttempts)
	if err != nil {
		return "", err
	}
	if !transitioned {
		return "", fmt.Errorf("%w: task is no longer in a retryable terminal state (already retried)", fixitdoctor.ErrActionConflict)
	}
	return fmt.Sprintf("task requeued (attempt %d)", attempt), nil
}

// --- set_secret ------------------------------------------------

// fixitSecretSetter implements fixitdoctor.SecretSetter over
// projectdoctor.Doctor.SetSecret — the declared-names gate lives
// entirely inside SetSecret; this adapter only translates its
// "not declared" refusal into fixitdoctor.ErrActionConflict.
type fixitSecretSetter struct {
	doctor *projectdoctor.Doctor
}

func (s fixitSecretSetter) Set(_ context.Context, projectID, field, value string) error {
	if s.doctor == nil {
		return errors.New("project doctor not wired")
	}
	err := s.doctor.SetSecret(projectID, field, value)
	if err != nil && strings.Contains(err.Error(), "is not declared by project") {
		return fmt.Errorf("%w: %v", fixitdoctor.ErrActionConflict, err)
	}
	return err
}

// --- composition -----------------------------------------------

// wireFixItDispatcher extends svc (already built by buildFixItDoctorOrNil)
// with the task 3.3 action-dispatch pipelines. Split out of that
// constructor because projectDoctor — needed for set_secret — isn't
// built until later in initHTTPServer (see container_http.go's call
// site comment). Nil-safe throughout: any missing Container piece just
// leaves the corresponding pipeline nil, and Dispatch fails closed for
// that one ActionKind rather than panicking.
func wireFixItDispatcher(svc *fixitdoctor.Service, c *Container, projectDoctor *projectdoctor.Doctor) {
	if svc == nil || c == nil {
		return
	}
	deps := featuredoctor.Deps{
		Config:    fixitConfigReader{cfg: c.Config},
		Instincts: reposInstincts(c),
		Tasks:     reposTaskLister(c),
		Logger:    c.Logger,
	}
	reloader := fixitConfigReloaderAdapter{r: c.ConfigReloader}
	svc.GatePipeline = &fixitGatePipeline{deps: deps, configPath: c.ConfigPath, reloader: reloader}

	if c.repos != nil && c.repos.Proposals != nil {
		svc.ConfigProposals = &fixitConfigProposalPipeline{
			proposals:  c.repos.Proposals,
			applier:    c.newProposalApplier(),
			configPath: c.ConfigPath,
		}
	}
	if c.repos != nil && c.repos.Tasks != nil {
		svc.ActionTaskRetrier = fixitTaskRetrier{tasks: c.repos.Tasks}
	}
	if projectDoctor != nil {
		svc.SecretSetter = fixitSecretSetter{doctor: projectDoctor}
	}
	if c.repos != nil && c.repos.AdminAudit != nil {
		svc.Audit = c.repos.AdminAudit
	}
	// Task 3.4: IntegrationReprober, wired against *Container.uiServer
	// (read lazily — see fixit_ui_bridge_adapter.go's file-level doc
	// comment; uiServer doesn't exist yet at this call site).
	svc.IntegrationReprober = fixitIntegrationReprober{c: c}
}

func reposInstincts(c *Container) persistence.InstinctRepository {
	if c.repos == nil {
		return nil
	}
	return c.repos.Instincts
}

func reposTaskLister(c *Container) featuredoctor.TaskLister {
	if c.repos == nil || c.repos.Tasks == nil {
		return nil
	}
	return fixitTaskLister{repo: c.repos.Tasks}
}
