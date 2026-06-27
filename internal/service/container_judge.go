package service

// Hallucination judge + post-mortem wiring extracted from
// container.go as part of the 2026-05-16 service-package split.
// Owns the Phase-3 judge runner construction + the post-mortem
// explainer adapter that the UI consumes.

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/postmortem"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/ui"
)

// logJudgeStartupSummary surfaces the Phase 3 wiring state at
// startup so operators don't have to wait for a task to
// terminate before learning whether the layer is live. Counts
// projects that opted in via HallucinationJudge.Enabled and
// reports either:
//
//   - "judge: enabled, N projects opt in" — runner wired AND at
//     least one project enabled. The expected steady state.
//   - "judge: runner wired but no projects opt in" — runner is
//     ready but every project has Enabled=false; verdict panel
//     will stay empty by design.
//   - "judge: NOT wired" — runner is nil (chat client unavailable
//     at executor build time). Phase 3 is dormant; verdict panel
//     will stay empty regardless of project config.
//
// Cheap (single registry walk) and runs once at startup.
func (c *Container) logJudgeStartupSummary() {
	if c == nil {
		return
	}
	if !c.judgeRunnerWired {
		c.Logger.Warn().Msg("judge: NOT wired — Phase 3 verdicts disabled (no chat client at executor build time)")
		return
	}
	enabled := 0
	var enabledProjects []string
	if c.Registry != nil {
		for _, p := range c.Registry.ListProjects() {
			if p == nil || !p.HallucinationJudge.Enabled {
				continue
			}
			enabled++
			enabledProjects = append(enabledProjects, p.ID)
		}
	}
	if enabled == 0 {
		c.Logger.Info().Msg("judge: runner wired but 0 projects opt in (set hallucinationJudge.enabled: true in a project YAML)")
		return
	}
	c.Logger.Info().
		Int("projects_enabled", enabled).
		Strs("project_ids", enabledProjects).
		Msg("judge: enabled — Phase 3 verdicts will fire on every terminal task in these projects")
}

// storeJudgeRunner caches the runner reference on the container
// and returns it untouched so the caller can keep chaining inside
// executor.NewWithOptions. Nil-passthrough.
func (c *Container) storeJudgeRunner(r *hallucination.JudgeRunner) *hallucination.JudgeRunner {
	if c != nil {
		c.judgeRunner = r
	}
	return r
}

// buildJudgeRunner constructs the Phase 3 LLM-as-judge runner if
// the daemon has a chat client wired. Returns nil when no chat
// client is available — Phase 3 is opt-in per project AND
// requires daemon-level LLM access; the absence of either keeps
// the layer dormant. The judge model on each call comes from the
// project's HallucinationJudge config (resolved at runtime, not
// here), so this returns one runner shared across projects.
//
// Logs the wiring outcome at startup — pre-2026-05-04 operators
// had no way to know whether the layer was active without waiting
// for a task to terminate; if the chat client failed to wire,
// every task silently skipped the judge and the verdict panel
// stayed empty. The startup line answers "is this layer turned on
// at all?" directly.
func (c *Container) buildJudgeRunner() *hallucination.JudgeRunner {
	if c == nil || c.ChatClient == nil {
		if c != nil {
			c.Logger.Warn().Msg("judge: runner NOT wired — no chat client available; Phase 3 verdicts will not be produced")
		}
		return nil
	}
	c.judgeRunnerWired = true
	verdicts := c.repos.JudgeVerdicts
	audits := c.repos.ToolAudit
	artifacts := c.repos.Artifacts
	executions := c.repos.Executions
	// Default judge model: when a project enables judging but
	// doesn't override the model, fall back to the daemon's
	// resolved AgentLLM model — the same baseline used elsewhere.
	defaultModel := ""
	if c.Config != nil {
		llm := c.Config.ResolvedAgentLLM()
		defaultModel = llm.Model
	}
	judge := &LazyProjectJudge{
		Client:    c.ChatClient,
		Default:   defaultModel,
		Resolver:  c.Registry,
		LogPrefix: "judge",
		Pricing:   c.pricingTable,
	}
	return &hallucination.JudgeRunner{
		Judge:        judge,
		Verdicts:     verdicts,
		Audits:       audits,
		Artifacts:    artifacts,
		Executions:   executions,
		Logger:       c.Logger.With().Str("component", "judge").Logger(),
		JudgeRoleTag: "judge",
		// Token + cost accounting: same pricing table as
		// dispatcher / worker + the same usage repo. Each
		// judge call lands a task_llm_usage row with
		// source="judge" so the spend dashboard splits judge
		// cost from worker + dispatcher cost.
		Pricing:  c.pricingTable,
		LLMUsage: c.repos.LLMUsage,
	}
}

// LazyProjectJudge resolves the per-project judge model on each
// call by looking up the task's project config in the registry.
// This is what makes Phase 3 per-project configurable: one judge
// instance covers every project, but each gets its own model
// based on the project's HallucinationJudge.Model field.
//
// Wraps an LLMJudge under the hood. The lazy lookup means
// operators can change the per-project judge model via YAML
// reload without bouncing the daemon.
type LazyProjectJudge struct {
	Client    chat.Provider
	Default   string
	Resolver  hallucinationProjectResolver
	LogPrefix string
	Pricing   *pricing.Table
}

// hallucinationProjectResolver is the narrow registry surface the
// lazy judge needs. Defined locally so the service package
// doesn't pull a wider registry coupling.
type hallucinationProjectResolver interface {
	GetProject(string) *registry.Project
}

// Evaluate dispatches to a per-project LLMJudge. When the
// project hasn't customised model/prompt, falls back to the
// daemon default.
func (j *LazyProjectJudge) Evaluate(ctx context.Context, in hallucination.JudgeInput) (*hallucination.Verdict, *hallucination.JudgeMetrics, error) {
	model := j.Default
	prompt := ""
	if j.Resolver != nil && in.Task != nil {
		if p := j.Resolver.GetProject(in.Task.ProjectID); p != nil {
			if p.HallucinationJudge.Model != "" {
				model = p.HallucinationJudge.Model
			}
			if p.HallucinationJudge.Prompt != "" {
				prompt = p.HallucinationJudge.Prompt
			}
		}
	}
	llm := &hallucination.LLMJudge{
		Client:  j.Client,
		Model:   model,
		Prompt:  prompt,
		Pricing: j.Pricing,
	}
	return llm.Evaluate(ctx, in)
}

// buildPostMortemExplainer constructs the failed-task
// explainer adapter wired into the UI server. Returns nil
// when chat client / executor / repos aren't available — the
// UI gates the "Explain this failure" button on
// PostMortemAvailable, which the WithPostMortemExplainer
// wiring sets only when this returns non-nil.
func (c *Container) buildPostMortemExplainer() ui.PostMortemExplainer {
	if c == nil || c.ChatClient == nil {
		return nil
	}
	model := ""
	if c.Config != nil {
		llm := c.Config.ResolvedAgentLLM()
		model = llm.Model
	}
	if model == "" {
		return nil
	}
	logSrc, _ := any(c.Executor).(postmortem.LogTailFetcher)
	exp := &postmortem.Explainer{
		Tasks:       c.repos.Tasks,
		Executions:  c.repos.Executions,
		Outcomes:    c.repos.StepOutcomes,
		Audits:      c.repos.ToolAudit,
		PostMortems: c.repos.PostMortems,
		LLMUsage:    c.repos.LLMUsage,
		Logs:        logSrc,
		Chat:        c.ChatClient,
		Model:       model,
		Pricing:     c.pricingTable,
		Logger:      c.Logger.With().Str("component", "postmortem").Logger(),
	}
	return &postMortemAdapter{e: exp}
}

// postMortemAdapter wraps the real *postmortem.Explainer so it
// satisfies ui.PostMortemExplainer without the ui package
// importing internal/postmortem (which would pull chat +
// pricing into the UI binary's import graph). The conversion
// from postmortem.Result to ui.PostMortemResult is field-
// for-field; the two types track each other.
type postMortemAdapter struct {
	e *postmortem.Explainer
}

func (a *postMortemAdapter) Generate(ctx context.Context, taskID string, force bool) (*ui.PostMortemResult, error) {
	if a == nil || a.e == nil {
		return nil, fmt.Errorf("post-mortem explainer not configured")
	}
	res, err := a.e.Generate(ctx, taskID, force)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	return &ui.PostMortemResult{Cached: res.Cached, PostMortem: res.PostMortem}, nil
}
