package service

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
)

// diagnoseObserver assembles the diagnose evidence bundle from the daemon's
// existing read sources (LLD 2026-07-08-diagnose §Observe). Best-effort: a
// failing source becomes a noted gap, never a hard fail.
type diagnoseObserver struct{ c *Container }

const (
	diagLogsCap  = 16 * 1024
	diagExecsCap = 12 * 1024
)

func (o diagnoseObserver) Observe(ctx context.Context, focus string) (*controlplane.DiagnoseBundle, error) {
	project, err := o.resolveFocus(ctx, focus)
	if err != nil {
		return nil, err
	}
	b := &controlplane.DiagnoseBundle{Focus: focus, ProjectID: project}

	addGap := func(src string, e error) {
		b.Gaps = append(b.Gaps, controlplane.DiagnoseGap{Source: src, Error: e.Error()})
	}
	addSec := func(name, content string) {
		if strings.TrimSpace(content) != "" {
			b.Sections = append(b.Sections, controlplane.DiagnoseSection{Name: name, Content: content})
		}
	}

	if o.c.repos != nil && o.c.repos.Executions != nil {
		addSec("recent failed executions", o.execSummary(ctx, project, persistence.ExecutionStatusFailed, 5, addGap))
		addSec("recent successful executions", o.execSummary(ctx, project, persistence.ExecutionStatusCompleted, 3, addGap))
		addSec("metrics", o.metricsSummary(ctx, project, addGap))
	}
	addSec("recent logs", diagJournal(project))
	if o.c.repos != nil && o.c.repos.Skills != nil {
		addSec("known failure patterns", o.skillHints(ctx))
	}
	return b, nil
}

// resolveFocus maps focus → a project id. task_ prefix → the task's project;
// an exact known project id → itself; else free-text match over the registry,
// erroring on ambiguity.
func (o diagnoseObserver) resolveFocus(ctx context.Context, focus string) (string, error) {
	f := strings.TrimSpace(focus)
	if strings.HasPrefix(f, "task_") && o.c.repos != nil && o.c.repos.Tasks != nil {
		if t, err := o.c.repos.Tasks.Get(ctx, f); err == nil && t != nil {
			return t.ProjectID, nil
		}
	}
	if o.c.Registry == nil {
		return f, nil
	}
	if o.c.Registry.GetProject(f) != nil {
		return f, nil
	}
	var matches []string
	for _, p := range o.c.Registry.ListProjects() {
		if strings.Contains(strings.ToLower(p.ID), strings.ToLower(f)) {
			matches = append(matches, p.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return f, nil // no match — diagnose the literal focus (may yield gaps)
	default:
		return "", controlplane.ErrDiagnoseAmbiguousFocus
	}
}

func (o diagnoseObserver) execSummary(ctx context.Context, project string, status persistence.ExecutionStatus, limit int, addGap func(string, error)) string {
	pid := project
	execs, err := o.c.repos.Executions.List(ctx, persistence.ExecutionFilter{ProjectID: &pid, Status: &status, PageSize: limit})
	if err != nil {
		addGap("executions:"+string(status), err)
		return ""
	}
	var sb strings.Builder
	for _, e := range execs {
		errMsg := ""
		if e.ErrorMessage != nil {
			errMsg = *e.ErrorMessage
		}
		step := ""
		if e.CurrentStepID != nil {
			step = *e.CurrentStepID
		}
		fmt.Fprintf(&sb, "- %s step=%s %s\n", e.ID, step, truncateDiag(errMsg, 300))
		if sb.Len() > diagExecsCap {
			break
		}
	}
	return sb.String()
}

func (o diagnoseObserver) metricsSummary(ctx context.Context, project string, addGap func(string, error)) string {
	rates, err := o.c.repos.Executions.FailedRateByProject(ctx, time.Now().Add(-6*time.Hour))
	if err != nil {
		addGap("metrics", err)
		return ""
	}
	rate := func(s persistence.ExecFailedRate) float64 {
		if s.Total == 0 {
			return 0
		}
		return float64(s.Failed) / float64(s.Total)
	}
	breaching := 0
	for _, s := range rates {
		if s.Total >= 5 && rate(s) >= 0.5 {
			breaching++
		}
	}
	me := rates[project]
	return fmt.Sprintf("project failed-rate: %.0f%% (%d/%d) over 6h. %d project(s) daemon-wide are breaching (a daemon-wide cause if many).",
		rate(me)*100, me.Failed, me.Total, breaching)
}

func (o diagnoseObserver) skillHints(ctx context.Context) string {
	skills, err := o.c.repos.Skills.ListAcrossProjects(ctx, []string{persistence.SkillMaturityActive, persistence.SkillMaturityTrusted}, 20)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for _, s := range skills {
		if s.Domain == "control-plane" {
			fmt.Fprintf(&sb, "- %s: %s\n", s.Name, s.Description)
		}
	}
	return sb.String()
}

// diagJournal reads recent daemon journal lines mentioning the project
// (best-effort; empty on non-systemd hosts).
func diagJournal(project string) string {
	out, err := exec.Command("journalctl", "--user", "-u", "vornik", "-n", "300", "--no-pager").CombinedOutput()
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for _, line := range strings.Split(string(out), "\n") {
		if project == "" || strings.Contains(line, project) {
			if len(line) > 400 {
				line = line[:400] + "…"
			}
			sb.WriteString(line + "\n")
			if sb.Len() > diagLogsCap {
				break
			}
		}
	}
	return sb.String()
}

func truncateDiag(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// newDiagnoser builds the diagnose engine (nil when the chat client or
// proposal ledger isn't wired).
func (c *Container) newDiagnoser() *controlplane.Diagnoser {
	if c == nil || c.ChatClient == nil || c.repos == nil || c.repos.Proposals == nil {
		return nil
	}
	var hasSecret func(string) bool
	if det, _, derr := buildSecretsDetector(c.Config.Secrets); derr == nil && det != nil {
		hasSecret = func(s string) bool { return len(det.Scan([]byte(s))) > 0 }
	}
	return &controlplane.Diagnoser{
		LLM:       c.ChatClient,
		Observe:   diagnoseObserver{c: c},
		Proposals: c.repos.Proposals,
		HasSecret: hasSecret,
		Logger:    c.Logger.With().Str("component", "control-plane").Str("engine", "diagnose").Logger(),
	}
}

// applyContentValidate is the apply engine's cheap pre-write syntactic gate.
// It only YAML-parses files that are actually YAML (config.yaml, project
// YAML). Swarm/workflow markdown files (frontmatter + a markdown body) are
// NOT valid single-document YAML — yaml.Unmarshal fails on the body — so a
// scaffold proposal carrying a swarm `.md` create-op would otherwise be
// wrongly rejected (and its whole bundle reversed) before the project could
// be created. For non-YAML paths the reload is the authoritative validator:
// it Parse*Markdown-s the file and auto-rolls-back on rejection.
func applyContentValidate(path, content string) error {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".yaml" && ext != ".yml" {
		return nil
	}
	var v any
	return yaml.Unmarshal([]byte(content), &v)
}

// newProposalApplier builds the Phase-2 apply/rollback engine (LLD
// 2026-07-08-control-plane-phase2). Returns nil when the proposal ledger
// isn't wired. Deps are late-bound so the reloader can be set after this.
func (c *Container) newProposalApplier() *controlplane.ApplyEngine {
	if c == nil || c.repos == nil || c.repos.Proposals == nil {
		return nil
	}
	return &controlplane.ApplyEngine{
		Proposals: c.repos.Proposals,
		// apply_target is resolved under the config.yaml dir, so both
		// "config.yaml" and "configs/swarms/<x>.md" resolve correctly.
		ConfigDir: filepath.Dir(c.ConfigPath),
		Reload: func() error {
			if c.ConfigReloader != nil {
				return c.ConfigReloader.Reload()
			}
			return nil
		},
		// Cheap pre-write syntactic gate; the reload is the authoritative
		// validator (and auto-rolls-back on rejection).
		Validate: applyContentValidate,
		HasActiveTasks: func(ctx context.Context, projectID string) (bool, error) {
			if c.repos.Tasks == nil {
				return false, nil
			}
			counts, err := c.repos.Tasks.CountByStatus(ctx, projectID)
			if err != nil {
				return false, err
			}
			return counts[persistence.TaskStatusRunning]+counts[persistence.TaskStatusLeased] > 0, nil
		},
		Logger: c.Logger.With().Str("component", "control-plane").Str("engine", "apply").Logger(),
	}
}

// Control-plane server-side workers (LLD 2026-07-07-control-plane-design,
// Phase 1). Currently the Tune detector: a leader-gated scan that raises a
// DRAFT proposal when a project's failed-task rate stays high. It never
// mutates config — proposing is the only action.

// tuneScanInterval / tuneWindow are the MVP cadence + look-back. Hourly scan
// over the trailing 6h keeps the signal responsive without over-reacting to a
// single bad task.
const (
	tuneScanInterval = time.Hour
	tuneWindow       = 6 * time.Hour
)

// execMetricsSource adapts the ExecutionRepository's windowed failed-rate + latency
// query (+ the ToolAudit tool-latency query) to the controlplane.MetricsSource
// the Tune worker consumes.
type execMetricsSource struct {
	execs     persistence.ExecutionRepository
	toolAudit persistence.ToolAuditRepository
	window    time.Duration
}

func (s execMetricsSource) FailedTaskRates(ctx context.Context) (map[string]controlplane.RateSample, error) {
	stats, err := s.execs.FailedRateByProject(ctx, time.Now().Add(-s.window))
	if err != nil {
		return nil, err
	}
	out := make(map[string]controlplane.RateSample, len(stats))
	for project, st := range stats {
		rate := 0.0
		if st.Total > 0 {
			rate = float64(st.Failed) / float64(st.Total)
		}
		out[project] = controlplane.RateSample{Failed: int(st.Failed), Total: int(st.Total), Rate: rate}
	}
	return out, nil
}

func (s execMetricsSource) LatencyP95s(ctx context.Context) (map[string]controlplane.LatencySample, error) {
	stats, err := s.execs.LatencyP95ByProject(ctx, time.Now().Add(-s.window))
	if err != nil {
		return nil, err
	}
	out := make(map[string]controlplane.LatencySample, len(stats))
	for project, st := range stats {
		out[project] = controlplane.LatencySample{P95Seconds: st.P95Seconds, Count: int(st.Count)}
	}
	return out, nil
}

// ToolLatencies feeds the operational-instinct tool-timeout signal. Nil-safe:
// no tool-audit repo wired → empty slice → the instinct never fires.
func (s execMetricsSource) ToolLatencies(ctx context.Context) ([]controlplane.ToolLatencySample, error) {
	if s.toolAudit == nil {
		return nil, nil
	}
	stats, err := s.toolAudit.ToolLatencyP95ByProjectTool(ctx, time.Now().Add(-s.window))
	if err != nil {
		return nil, err
	}
	out := make([]controlplane.ToolLatencySample, 0, len(stats))
	for _, st := range stats {
		out = append(out, controlplane.ToolLatencySample{
			Key:        controlplane.ProjectToolKey{Project: st.ProjectID, Tool: st.ToolName},
			P95Seconds: st.P95Seconds, Count: int(st.Count),
		})
	}
	return out, nil
}

// startTuneWorker wires + starts the control-plane Tune detector, leader-gated
// so only one replica scans. Nil-safe: a no-op when the proposal ledger or
// execution repo isn't wired (minimal harnesses).
func (c *Container) startTuneWorker(ctx context.Context) {
	if c == nil || c.repos == nil || c.repos.Proposals == nil || c.repos.Executions == nil {
		return
	}
	w := &controlplane.TuneWorker{
		Proposals: c.repos.Proposals,
		Metrics:   execMetricsSource{execs: c.repos.Executions, toolAudit: c.repos.ToolAudit, window: tuneWindow},
		Interval:  tuneScanInterval,
		// When self-heal is enabled + a diagnoser is wired it OWNS the
		// failed-rate signal; the Tune worker yields it (design §5).
		SkipFailedRate: c.selfHealActive(),
		Logger:         c.Logger.With().Str("component", "control-plane").Str("worker", "tune").Logger(),
	}
	if elector := c.initWorkerElector("control_plane_tune"); elector != nil {
		w.LeaderGate = elector
		elector.BootstrapAcquire(ctx)
		go elector.Run(ctx)
	}
	go w.Run(collectorsCtxFrom(ctx, c))
}

// selfHealActive reports whether self-healing should own the failed-rate signal
// — config opt-in AND a diagnoser (chat client + ledger) is wired.
func (c *Container) selfHealActive() bool {
	return c != nil && c.Config.ControlPlane.SelfHealEnabled && c.newDiagnoser() != nil
}

// startSelfHealWorker wires + starts the self-healing incident detector,
// leader-gated. No-op unless opt-in (self_heal_enabled) AND a diagnoser +
// execution repo are wired.
func (c *Container) startSelfHealWorker(ctx context.Context) {
	if !c.selfHealActive() || c.repos == nil || c.repos.Executions == nil {
		return
	}
	// Tag auto-opened incidents "self-heal" (distinct from operator diagnoses).
	diag := c.newDiagnoser()
	diag.ProposedBy = "self-heal"
	var alert func(subject, body string)
	if n := c.operatorAlertNotifier(); n != nil {
		alert = func(subject, body string) { n.NotifyOperator(ctx, subject, body) }
	}
	w := &controlplane.SelfHealWorker{
		Proposals:           c.repos.Proposals,
		Metrics:             execMetricsSource{execs: c.repos.Executions, toolAudit: c.repos.ToolAudit, window: tuneWindow},
		Diagnose:            diag,
		Alert:               alert,
		Interval:            tuneScanInterval,
		SystemProjectID:     c.Config.ControlPlane.SystemProjectID,
		MaxIncidentsPerHour: c.Config.ControlPlane.MaxIncidentsPerHour,
		Logger:              c.Logger.With().Str("component", "control-plane").Str("worker", "self-heal").Logger(),
	}
	if elector := c.initWorkerElector("control_plane_self_heal"); elector != nil {
		w.LeaderGate = elector
		elector.BootstrapAcquire(ctx)
		go elector.Run(ctx)
	}
	go w.Run(collectorsCtxFrom(ctx, c))
}
