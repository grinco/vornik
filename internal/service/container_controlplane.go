package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/configrecon"
	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/safepath"
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
	// Project config summary (actionable-proposals §4.6): the exact
	// workflow/step/role/server names + current timeouts/models the model
	// may reference in a structured config_change — it must select from
	// names it has SEEN, never invent them.
	addSec("project config (workflows/roles/mcp)", o.configSummary(project))
	return b, nil
}

// configSummary renders a compact, bounded view of the project's workflow
// steps (id, role, timeout), swarm roles (name, model), and MCP servers
// (name, timeout_seconds) so a diagnose config_change can only cite real
// names. Best-effort; empty when the registry isn't wired.
func (o diagnoseObserver) configSummary(project string) string {
	if o.c.Registry == nil {
		return ""
	}
	p := o.c.Registry.GetProject(project)
	if p == nil {
		return ""
	}
	var sb strings.Builder
	wfIDs := append([]string{p.DefaultWorkflowID}, p.AdaptiveCandidateWorkflows...)
	seen := map[string]bool{}
	for _, id := range wfIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		wf := o.c.Registry.GetWorkflow(id)
		if wf == nil {
			continue
		}
		fmt.Fprintf(&sb, "workflow %s steps:", wf.ID)
		stepIDs := make([]string, 0, len(wf.Steps))
		for stepID := range wf.Steps {
			stepIDs = append(stepIDs, stepID)
		}
		sort.Strings(stepIDs) // deterministic prompt content
		for _, stepID := range stepIDs {
			st := wf.Steps[stepID]
			timeout := st.Timeout
			if timeout == "" {
				timeout = "default"
			}
			fmt.Fprintf(&sb, " %s(role=%s, timeout=%s)", stepID, st.Role, timeout)
		}
		sb.WriteString("\n")
	}
	if sw := o.c.Registry.GetSwarm(p.SwarmID); sw != nil {
		fmt.Fprintf(&sb, "swarm %s roles:", sw.ID)
		for _, r := range sw.Roles {
			fmt.Fprintf(&sb, " %s(model=%s)", r.Name, r.Model)
		}
		sb.WriteString("\n")
	}
	{
		// Live catalog: this summary feeds the diagnoser, and describing the
		// boot-time server set on a daemon that has since reloaded would have
		// it reason about a deployment that no longer exists.
		for _, srv := range o.c.daemonMCPServers() {
			t := srv.TimeoutSeconds
			if t == 0 {
				t = 30
			}
			fmt.Fprintf(&sb, "mcp server %s (daemon scope) timeout_seconds=%d\n", srv.Name, t)
		}
	}
	const summaryCap = 4 * 1024
	s := sb.String()
	if len(s) > summaryCap {
		s = s[:summaryCap] + "\n[config summary truncated]\n"
	}
	return s
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

// newDiagnoser builds the diagnose engine. Nil when the chat client or
// proposal ledger isn't wired — or in a Community build: the Diagnoser is
// EE-gated via providers.ControlPlaneDiagnosis (actionable-proposals §6.3);
// this single gate point covers the operator API, the hub Diagnose tab, and
// the self-heal escalator, which all wire through here.
func (c *Container) newDiagnoser() *controlplane.Diagnoser {
	if c == nil || !c.providers.ControlPlaneDiagnosis ||
		c.ChatClient == nil || c.repos == nil || c.repos.Proposals == nil {
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
		// Structured config_change rendering (actionable-proposals §4.6).
		Actionize: c.newActionizer(),
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

// newProposalMirror builds the two-trees mirror hook (actionable-proposals
// §4.7): after a successful apply/rollback it propagates the final file
// states into the operator's source checkout (VORNIK_CONFIGS_SOURCE_DIR, the
// same seam the memetic applier uses) and makes ONE git commit per proposal.
// Nil when no source tree is configured (deployed-only deployments). Errors
// are the engine's to WARN on — the deployed tree is the source of truth.
func (c *Container) newProposalMirror() func(proposalID string, files map[string][]byte) error {
	sourceConfigsDir := os.Getenv("VORNIK_CONFIGS_SOURCE_DIR")
	if sourceConfigsDir == "" {
		return nil
	}
	sourceRoot := filepath.Dir(sourceConfigsDir) // holds config.yaml siblings of configs/
	logger := c.Logger.With().Str("component", "control-plane").Str("engine", "mirror").Logger()
	return func(proposalID string, files map[string][]byte) error {
		var staged []string
		var firstErr error
		// Collect the DISTINCT normalizer names that fired across all files in
		// this proposal, in first-seen order, so the commit message carries a
		// `mirror-normalized: <name>` trailer per normalizer (review A6) — the
		// operator who later `git revert`s a bad rewrite finds the audit at the
		// commit, not only in the daemon log.
		var normalizerOrder []string
		seenNormalizer := map[string]bool{}
		for rel, content := range files {
			target, ok, notes, err := mirrorOneFile(sourceRoot, sourceConfigsDir, rel, content, logger)
			if err != nil {
				if strings.Contains(err.Error(), "escapes") {
					return err
				}
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			for _, note := range notes {
				c.configMirrorMetrics.Inc(note.Name)
				if !seenNormalizer[note.Name] {
					seenNormalizer[note.Name] = true
					normalizerOrder = append(normalizerOrder, note.Name)
				}
			}
			if ok {
				staged = append(staged, target)
			}
		}
		if firstErr != nil {
			return firstErr
		}
		if len(staged) > 0 && isGitRepo(sourceConfigsDir) {
			msg := fmt.Sprintf("control-plane: apply %s", proposalID)
			if len(normalizerOrder) > 0 {
				msg += "\n" // blank line separating subject from the trailer block
				for _, name := range normalizerOrder {
					msg += fmt.Sprintf("\nmirror-normalized: %s", name)
				}
			}
			if err := gitCommitPaths(sourceConfigsDir, staged, msg); err != nil {
				return fmt.Errorf("mirror: git commit: %w", err)
			}
		}
		return nil
	}
}

// mirrorOneFile propagates one deployed rel path into the source tree:
// "configs/<sub>" maps into the source configs dir, anything else
// (config.yaml) beside it. nil content = delete. staged=false means the
// write was skipped (missing parent dir — never scaffold the operator's
// checkout from guesses).
func mirrorOneFile(sourceRoot, sourceConfigsDir, rel string, content []byte, logger zerolog.Logger) (target string, staged bool, notes []configrecon.NormalizationNote, err error) {
	// Canonicalize BEFORE prefix mapping (review: "configs//x" or an
	// embedded ".." must not choose the wrong branch), and refuse dot-dot
	// outright — the renderers already reject such identifiers (safeIdent)
	// and the apply engine Cleans its own target, so a ".." here is never
	// legitimate. The mirror otherwise reproduces the deployed state
	// VERBATIM — local uncommitted edits to a mirrored file are overwritten
	// by design (deployed reality wins; the git commit preserves history).
	rel = filepath.Clean(rel)
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return "", false, nil, fmt.Errorf("mirror: %q is not a safe relative path", rel)
	}
	// Config-tree round-trip guard (LLD 2026-07-24-config-tree-reconciliation-
	// design.md §3.2): normalise declared canonical-defect fields BEFORE the
	// source write + git commit, so a stale field (e.g. a bare agent `image:`
	// carried over from stale DEPLOYED content) can never round-trip into the
	// repo. Change-only: notes is empty and content is byte-identical when
	// nothing fired, preserving the deployed-wins-verbatim contract exactly.
	// The rel here is the config-root-relative path (e.g. "configs/swarms/x.md"
	// or "config.yaml"), which is the basis AppliesTo expects. Skipped for a
	// delete (nil content) — there is nothing to normalise.
	if content != nil {
		content, notes = configrecon.ApplyMirrorNormalizers(rel, content)
		for _, note := range notes {
			logger.Info().
				Str("normalizer", note.Name).
				Str("rel", rel).
				Str("message", note.Message).
				Msgf("mirror: normalized [%s] in %s before source write", note.Name, rel)
		}
	}
	// Map the deployed rel path to its source-tree home, then containment-check
	// through the canonical symlink-resolving guard (audit 2026-07-09 F-1):
	// JoinUnder resolves any symlink in the deepest existing prefix so a
	// planted symlink in the source checkout can't redirect the write out of
	// the tree — the lexical Clean+HasPrefix guard this replaces did not.
	var clean string
	if strings.HasPrefix(rel, "configs/") {
		clean, err = safepath.JoinUnderRel(sourceConfigsDir, strings.TrimPrefix(rel, "configs/"))
	} else {
		clean, err = safepath.JoinUnderRel(sourceRoot, rel)
	}
	if err != nil {
		return "", false, nil, fmt.Errorf("mirror: %q escapes the source tree: %w", rel, err)
	}
	if content == nil {
		if rmErr := os.Remove(clean); rmErr != nil && !os.IsNotExist(rmErr) {
			return "", false, nil, rmErr
		}
		return clean, true, nil, nil
	}
	if info, statErr := os.Stat(filepath.Dir(clean)); statErr != nil || !info.IsDir() {
		logger.Warn().Str("rel", rel).Msg("mirror: source-tree parent dir missing; skipped")
		return "", false, nil, nil
	}
	if wErr := os.WriteFile(clean, content, 0o644); wErr != nil { //nolint:gosec // operator-owned config text
		return "", false, nil, wErr
	}
	return clean, true, notes, nil
}

// gitCommitPaths stages the given absolute paths and makes one commit in the
// repo containing dir. `git add -A -- <paths>` records deletions too.
func gitCommitPaths(dir string, paths []string, message string) error {
	addArgs := append([]string{"-C", dir, "add", "-A", "--"}, paths...)
	if out, err := exec.Command("git", addArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %v: %s", err, out)
	}
	commitArgs := []string{"-C", dir, "commit", "-m", message,
		"--author", envOr("VORNIK_GIT_AUTHOR_NAME", "vornik-control-plane") + " <" + envOr("VORNIK_GIT_AUTHOR_EMAIL", "control-plane@vornik.local") + ">"}
	if out, err := exec.Command("git", commitArgs...).CombinedOutput(); err != nil {
		if strings.Contains(string(out), "nothing to commit") {
			return nil
		}
		return fmt.Errorf("git commit: %v: %s", err, out)
	}
	return nil
}

// newProposalApplier builds the Phase-2 apply/rollback engine (LLD
// 2026-07-08-control-plane-phase2). Returns nil when the proposal ledger
// isn't wired. Deps are late-bound so the reloader can be set after this.
func (c *Container) newProposalApplier() *controlplane.ApplyEngine {
	if c == nil || c.repos == nil || c.repos.Proposals == nil {
		return nil
	}
	actionizer := c.newActionizer()
	engine := &controlplane.ApplyEngine{
		// Apply-time semantic re-validation of actionized proposals
		// (actionable-proposals §4.5): existence + model universe against
		// CURRENT state; base-hash covers content drift.
		ValidateChange: func(_ context.Context, p *persistence.ControlPlaneProposal) error {
			if actionizer == nil {
				return nil
			}
			// Defense-in-depth: refuse a cost/quality-detector proposal that
			// targets a trading swarm BEFORE the semantic re-check (mirror of the
			// detector-side exclusion; review-20260721-a7bf #6). Returned before
			// RevalidateChange so the operator sees the specific trading sentinel,
			// not the generic re-validation wrap (design D5). Note this refusal is
			// itself conditional on a wired actionizer (the nil guard above); in the
			// nil-actionizer path coverage falls back to the detector-side exclusion
			// alone (review-20260724-24a2 F3).
			if err := actionizer.RefuseTradingTarget(p.ProposedBy, p.Evidence); err != nil {
				return err
			}
			return actionizer.RevalidateChange(p.ProjectID, p.Evidence)
		},
		// Two-trees mirror (§4.7) — nil-safe when no source tree configured.
		Mirror:    c.newProposalMirror(),
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
	// instinct_retire (2026-07-19-instinct-lift-measurement-design.md §4.5)
	// is a KindApplier-managed kind — retiring/unretiring an instinct
	// directly rather than rewriting a config file. Registered only when the
	// instinct repository is wired.
	if c.repos.Instincts != nil {
		engine.KindAppliers = map[string]controlplane.KindApplier{
			persistence.ProposalKindInstinctRetire: &controlplane.InstinctRetireApplier{Instincts: c.repos.Instincts},
		}
	}
	return engine
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
	execs        persistence.ExecutionRepository
	toolAudit    persistence.ToolAuditRepository
	stepOutcomes persistence.ExecutionStepOutcomeRepository
	window       time.Duration
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

// StepLatencies feeds the latency signal's slowest-step attribution
// (actionable-proposals §4.4). Nil-safe: no step-outcome repo wired → empty
// slice → latency proposals stay generic.
func (s execMetricsSource) StepLatencies(ctx context.Context) ([]controlplane.StepLatencySample, error) {
	if s.stepOutcomes == nil {
		return nil, nil
	}
	stats, err := s.stepOutcomes.StepLatencyP95ByStep(ctx, time.Now().Add(-s.window))
	if err != nil {
		return nil, err
	}
	out := make([]controlplane.StepLatencySample, 0, len(stats))
	for _, st := range stats {
		out = append(out, controlplane.StepLatencySample{
			Project: st.ProjectID, Workflow: st.WorkflowID, Step: st.StepID,
			Role: st.Role, Model: st.Model,
			P95Seconds: st.P95Seconds, Count: int(st.Count),
			// Carried so the reclaim guard is not inert: without MaxSeconds the
			// suggestion silently degrades to the old p95 basis, and without
			// DegradedCount a timing-out step still reads as over-provisioned
			// (LLD 2026-08-10-canary-class-registry-step-outcome §6.1).
			MaxSeconds: st.MaxSeconds, DegradedCount: int(st.DegradedCount),
			TimeoutCount: int(st.TimeoutCount),
		})
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

// newActionizer builds the deterministic change renderer (actionable-
// proposals §4). CE library code — the EE gates live at its callers.
func (c *Container) newActionizer() *controlplane.Actionizer {
	if c == nil {
		return nil
	}
	configDir := filepath.Dir(c.ConfigPath)
	return &controlplane.Actionizer{
		ReadFile: func(rel string) ([]byte, error) {
			base := filepath.Clean(configDir)
			full := filepath.Clean(filepath.Join(base, rel))
			if full != base && !strings.HasPrefix(full, base+string(os.PathSeparator)) {
				return nil, fmt.Errorf("actionizer: path %q escapes the config dir", rel)
			}
			return os.ReadFile(full) //nolint:gosec // guarded above
		},
		ValidateWorkflow: func(filename string, content []byte) error {
			_, err := registry.ParseWorkflowMarkdown(content, filename)
			return err
		},
		ValidateSwarm: func(filename string, content []byte) error {
			_, err := registry.ParseSwarmMarkdown(content, filename)
			return err
		},
		KnownModel: func(model string) bool {
			if c.pricingTable == nil {
				return false
			}
			_, known := c.pricingTable.Lookup(model)
			return known
		},
		// Injected so the applier-side trading refusal (RefuseTradingTarget)
		// shares the SAME classifier the detector-side exclusion uses — the two
		// can never diverge (design 2026-07-24-applier-trading-refusal D3).
		IsTradingSwarm: isTradingSwarm,
		Logger:         c.Logger.With().Str("component", "control-plane").Str("engine", "actionize").Logger(),
	}
}

// isTradingSwarm reports whether a swarm id is on the trading path, which the
// cost/quality detector must never touch (design §F, detector-side exclusion).
func isTradingSwarm(swarmID string) bool {
	s := strings.ToLower(swarmID)
	return strings.Contains(s, "trader") || strings.Contains(s, "broker") || strings.Contains(s, "trading")
}

// costTuningSwarmMap returns parallel (projectIDs, swarmIDs) from the registry
// for the cost/quality percentile query, EXCLUDING trading swarms.
func (c *Container) costTuningSwarmMap() (projectIDs, swarmIDs []string) {
	if c == nil || c.Registry == nil {
		return nil, nil
	}
	for _, p := range c.Registry.ListProjects() {
		if p == nil || p.SwarmID == "" || isTradingSwarm(p.SwarmID) {
			continue
		}
		projectIDs = append(projectIDs, p.ID)
		swarmIDs = append(swarmIDs, p.SwarmID)
	}
	return projectIDs, swarmIDs
}

// startCostQualityWorker wires + starts the propose-only prompt-token-budget
// detector (LLD 2026-07-21). No-op unless the proposal ledger, quality service,
// and registry are wired. Gated by control_plane.cost_tuning_enabled (default
// off); the worker is always constructed so the gate is the only switch.
func (c *Container) startCostQualityWorker(ctx context.Context) {
	if c == nil || c.repos == nil || c.repos.Proposals == nil || c.qualityService == nil || c.Registry == nil {
		return
	}
	enabled := c.Config != nil && c.Config.ControlPlane.CostTuningEnabled
	w := &controlplane.CostQualityWorker{
		Quality:     c.qualityService,
		Percentiles: postgres.NewQualityRepository(c.instrumentedDB()),
		Actionize:   c.newActionizer(),
		Proposals:   c.repos.Proposals,
		SwarmMap:    c.costTuningSwarmMap,
		Enabled:     enabled,
		Logger:      c.Logger.With().Str("component", "control-plane").Str("worker", "cost-quality").Logger(),
	}
	// Canary-aware skip (design §7): don't re-propose on a (swarm,role) with an
	// open canary or a knob still in cooldown after a regressed rollback.
	if c.repos.CostTuningCanaries != nil {
		w.Canaries = c.repos.CostTuningCanaries
		w.CooldownDuration = c.Config.ControlPlane.CostTuningCanary.Resolved().Cooldown
	}
	go w.Run(collectorsCtxFrom(ctx, c))
}

// liveCanaryEnabled returns the hot-reloadable live value of
// control_plane.cost_tuning_canary.enabled: the boot-time c.Config value until a
// hot-reload stages a new one (applyHotConfig). Read by the guard's discover +
// trip-time closure so a config reload flips the kill-switch live (design §9 #2).
func (c *Container) liveCanaryEnabled() bool {
	if v := c.cpCanaryEnabledLive.Load(); v != nil {
		return *v
	}
	return c.Config != nil && c.Config.ControlPlane.CostTuningCanary.Enabled
}

// canaryBlastRadius resolves (swarm) → (projects sharing it, workflows those
// projects run) from the registry — the A2 watch set (design §5), excluding
// trading swarms. Re-derived at open time, not from stale Evidence.
func (c *Container) canaryBlastRadius(swarm string) (projectIDs, workflowIDs []string) {
	if c == nil || c.Registry == nil || swarm == "" || isTradingSwarm(swarm) {
		return nil, nil
	}
	wfSeen := map[string]bool{}
	for _, p := range c.Registry.ListProjects() {
		if p == nil || p.SwarmID != swarm {
			continue
		}
		projectIDs = append(projectIDs, p.ID)
		for _, wf := range append([]string{p.DefaultWorkflowID}, p.AdaptiveCandidateWorkflows...) {
			if wf == "" || wfSeen[wf] {
				continue
			}
			wfSeen[wf] = true
			workflowIDs = append(workflowIDs, wf)
		}
	}
	sort.Strings(projectIDs)
	sort.Strings(workflowIDs)
	return projectIDs, workflowIDs
}

// startCanaryGuardWorker wires + starts the cost/quality canary + regression
// auto-rollback guard, leader-gated (per the Tune/SelfHeal precedent — NOT the
// un-leader-gated cost-quality worker, because the guard MUTATES). No-op unless
// the proposal ledger, canary store, quality service, apply engine, and registry
// are wired. The enable + swarm allow-list are consulted live at discover AND
// trip time (design §9 #2).
func (c *Container) startCanaryGuardWorker(ctx context.Context) {
	if c == nil || c.repos == nil || c.repos.Proposals == nil || c.repos.CostTuningCanaries == nil ||
		c.qualityService == nil || c.Registry == nil {
		return
	}
	engine := c.newProposalApplier()
	if engine == nil {
		return // no apply engine → nothing to roll back through
	}
	cfg := c.Config.ControlPlane.CostTuningCanary.Resolved()
	allowed := map[string]bool{}
	for _, s := range cfg.Swarms {
		allowed[s] = true
	}
	swarmAllowed := func(swarm string) bool {
		if len(allowed) == 0 {
			return true // empty allow-list = all non-trading swarms
		}
		return allowed[swarm]
	}
	var metrics *controlplane.CanaryMetrics
	if reg := c.observabilityRegistry(); reg != nil {
		metrics = controlplane.NewCanaryMetrics(reg)
	}
	w := &controlplane.CanaryGuardWorker{
		Quality:        c.qualityService,
		Canaries:       c.repos.CostTuningCanaries,
		Proposals:      c.repos.Proposals,
		Rollback:       engine.Rollback,
		BlastRadius:    c.canaryBlastRadius,
		Enabled:        c.liveCanaryEnabled,
		SwarmAllowed:   swarmAllowed,
		IsTradingSwarm: isTradingSwarm,
		MinSamples:     cfg.MinSamples,
		A2MinSamples:   cfg.A2MinSamples,
		A2Subwindows:   cfg.A2Subwindows,
		MarginA1:       cfg.MarginA1,
		MarginA2:       cfg.MarginA2,
		MarginCost:     cfg.MarginCost,
		Window:         cfg.Window,
		MaxCanaryAge:   cfg.MaxCanaryAge,
		Interval:       cfg.ScanInterval,
		Metrics:        metrics,
		Logger:         c.Logger.With().Str("component", "control-plane").Str("worker", "canary-guard").Logger(),
	}
	if elector := c.initWorkerElector("control_plane_canary_guard"); elector != nil {
		w.LeaderGate = elector
		elector.BootstrapAcquire(ctx)
		go elector.Run(ctx)
	}
	go w.Run(collectorsCtxFrom(ctx, c))
}

// liveCostAutoApplyEnabled returns the hot-reloadable live value of
// control_plane.cost_tuning_auto_apply.enabled: the boot-time c.Config value
// until a hot-reload stages a new one. Read by the auto-apply worker's per-tick +
// apply-time closure so a reload flips the kill-switch live (auto-apply §D5).
func (c *Container) liveCostAutoApplyEnabled() bool {
	if v := c.cpCostAutoApplyEnabledLive.Load(); v != nil {
		return *v
	}
	return c.Config != nil && c.Config.ControlPlane.CostTuningAutoApply.Enabled
}

// startCostAutoApplyWorker wires + starts the Phase-4 cost/quality auto-apply
// worker, leader-gated (it MUTATES — same posture as the canary guard). No-op
// unless the proposal ledger, canary store, and apply engine are wired. The
// enable + swarm allow-list are consulted live at scan AND apply time; the empty
// allow-list means NONE (safety inversion), and the canary guard is a hard
// prerequisite (auto-apply design D4). See https://docs.vornik.io
// 2026-07-24-cost-quality-auto-apply-design.md.
func (c *Container) startCostAutoApplyWorker(ctx context.Context) {
	if c == nil || c.repos == nil || c.repos.Proposals == nil || c.repos.CostTuningCanaries == nil {
		return
	}
	engine := c.newProposalApplier()
	if engine == nil {
		return // no apply engine → nothing to drive
	}
	aaCfg := c.Config.ControlPlane.CostTuningAutoApply.Resolved()
	// Empty allow-list = NONE (safety inversion vs the canary guard): auto-apply
	// is opt-in per swarm. The closure is deny-by-default so a nil/empty map never
	// auto-applies.
	allowed := map[string]bool{}
	for _, s := range aaCfg.Swarms {
		allowed[s] = true
	}
	swarmAllowed := func(swarm string) bool { return allowed[swarm] }
	// F8: WARN for allow-list entries matching no known swarm — a typo is SAFE
	// (that swarm never auto-applies) but silently unmet intent, so surface it.
	// (A startup log rather than a dedicated doctor check — no cost-tuning doctor
	// surface exists yet; see the auto-apply LLD §6.)
	if c.Registry != nil && len(allowed) > 0 {
		known := map[string]bool{}
		for _, p := range c.Registry.ListProjects() {
			if p != nil && p.SwarmID != "" {
				known[p.SwarmID] = true
			}
		}
		for s := range allowed {
			if !known[s] {
				c.Logger.Warn().Str("swarm", s).
					Msg("cost-auto-apply: allow-list entry matches no known swarm — that swarm will never auto-apply (typo?)")
			}
		}
	}
	// Cooldown matches the canary guard's so the worker's skip agrees with the
	// detector's (design D7).
	cooldown := c.Config.ControlPlane.CostTuningCanary.Resolved().Cooldown
	var metrics *controlplane.CostAutoApplyMetrics
	if reg := c.observabilityRegistry(); reg != nil {
		metrics = controlplane.NewCostAutoApplyMetrics(reg)
	}
	// ReadFile reads the SAME deployed tree the apply engine writes (rel to the
	// config dir), for snapshot staging (D8) + the crash-signature check.
	actionizer := c.newActionizer()
	if actionizer == nil {
		return
	}
	w := &controlplane.CostAutoApplyWorker{
		Proposals:         c.repos.Proposals,
		Canaries:          c.repos.CostTuningCanaries,
		Apply:             engine.Apply,
		ReadFile:          actionizer.ReadFile,
		Enabled:           c.liveCostAutoApplyEnabled,
		CanaryEnabled:     c.liveCanaryEnabled, // D4 prerequisite
		SwarmAllowed:      swarmAllowed,
		IsTradingSwarm:    isTradingSwarm,
		MinPassedCanaries: aaCfg.MinPassedCanaries,
		CooldownDuration:  cooldown,
		Interval:          aaCfg.ScanInterval,
		Metrics:           metrics,
		Logger:            c.Logger.With().Str("component", "control-plane").Str("worker", "cost-auto-apply").Logger(),
	}
	if elector := c.initWorkerElector("control_plane_cost_auto_apply"); elector != nil {
		w.LeaderGate = elector
		elector.BootstrapAcquire(ctx)
		go elector.Run(ctx)
	}
	go w.Run(collectorsCtxFrom(ctx, c))
}

// instinctActive reports whether the EE Instinct subsystem is present — the
// established edition marker gating the instinct tool-timeout scan
// (actionable-proposals §6.3: Community's provider yields no subsystem).
func (c *Container) instinctActive() bool {
	return c != nil && c.providers.Instinct != nil && c.providers.Instinct.InstinctSubsystem() != nil
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
		Metrics:   execMetricsSource{execs: c.repos.Executions, toolAudit: c.repos.ToolAudit, stepOutcomes: c.repos.StepOutcomes, window: tuneWindow},
		Interval:  tuneScanInterval,
		// When self-heal is enabled + a diagnoser is wired it OWNS the
		// failed-rate signal; the Tune worker yields it (design §5). A
		// per-tick closure so flipping self_heal_enabled + reload hands the
		// signal back without a restart (actionable-proposals §7 — the brake
		// takes effect on the NEXT tick; a tick already past its gate
		// finishes, bounded by the self-heal rate cap). This seam must keep
		// calling through selfHealActive(): it is also the EE gate
		// (ControlPlaneDiagnosis) — a cached boolean would decouple them.
		SkipFailedRate: func() bool { return c.selfHealActive() },
		// Deterministic actionable rendering for latency/tool-timeout
		// breaches (actionable-proposals §4.4). CE-inclusive by design §6.1.
		Actionize: c.newActionizer(),
		Logger:    c.Logger.With().Str("component", "control-plane").Str("worker", "tune").Logger(),
	}
	// The instinct tool-timeout scan belongs to the EE Instinct subsystem
	// (actionable-proposals §6.3): without it, disable via the shipped
	// sentinel so `ProposedBy="instinct"` proposals never fire in CE.
	if !c.instinctActive() {
		w.ToolLatencyThresholdSeconds = -1
	}
	if elector := c.initWorkerElector("control_plane_tune"); elector != nil {
		w.LeaderGate = elector
		elector.BootstrapAcquire(ctx)
		go elector.Run(ctx)
	}
	go w.Run(collectorsCtxFrom(ctx, c))
}

// liveSelfHealEnabled returns the hot-reloadable live value of
// control_plane.self_heal_enabled: the boot-time c.Config value until a
// hot-reload stages a new one (applyHotConfig — c.Config itself is
// deliberately never mutated on reload).
func (c *Container) liveSelfHealEnabled() bool {
	if v := c.cpSelfHealLive.Load(); v != nil {
		return *v
	}
	return c.Config != nil && c.Config.ControlPlane.SelfHealEnabled
}

// selfHealActive reports whether self-healing should own the failed-rate signal
// — EE diagnosis capability (ControlPlaneDiagnosis) AND config opt-in AND a
// diagnoser (chat client + ledger) is wired. Read per tick via the workers'
// closures so a config reload flips it live (actionable-proposals §7).
func (c *Container) selfHealActive() bool {
	return c != nil && c.providers.ControlPlaneDiagnosis &&
		c.liveSelfHealEnabled() && c.newDiagnoser() != nil
}

// startSelfHealWorker wires + starts the self-healing incident detector,
// leader-gated. The worker starts whenever the EE diagnosis capability + a
// diagnoser + execution repo are wired; the per-tick Enabled closure reads
// control_plane.self_heal_enabled live, so the config flag is a hot brake,
// not a boot-time latch (actionable-proposals §7).
func (c *Container) startSelfHealWorker(ctx context.Context) {
	if c == nil || !c.providers.ControlPlaneDiagnosis || c.newDiagnoser() == nil ||
		c.repos == nil || c.repos.Executions == nil {
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
		Metrics:             execMetricsSource{execs: c.repos.Executions, toolAudit: c.repos.ToolAudit, stepOutcomes: c.repos.StepOutcomes, window: tuneWindow},
		Diagnose:            diag,
		Alert:               alert,
		Interval:            tuneScanInterval,
		Enabled:             func() bool { return c.liveSelfHealEnabled() },
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
