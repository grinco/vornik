package service

import (
	"context"
	"time"

	"vornik.io/vornik/internal/narrator"
)

// initNarrator constructs the Narrated Execution worker (task 2.1)
// when narrator.enabled AND every required collaborator is wired:
// the live-event publisher (c.livePub), the execution_narration
// store, and ExecutionRepository (needed both to resolve project_id/
// task_id for a new execution_id and to sample terminal status —
// see internal/narrator.ExecutionLookup's doc comment on why the
// narrator polls rather than waiting for a bus event that doesn't
// exist).
//
// Called from NewContainer right after c.livePub is built (the
// narrator's Sub/Pub seam) and after c.pricingTable + c.ChatClient
// are available — the same ordering LLMConsolidateWorker relies on
// in container_scheduler.go.
func (c *Container) initNarrator() {
	if !c.Config.Narrator.Enabled {
		c.Logger.Info().Msg("narrator: disabled (narrator.enabled=false)")
		return
	}
	if c.livePub == nil || c.repos == nil || c.repos.ExecutionNarration == nil || c.repos.Executions == nil {
		c.Logger.Warn().Msg("narrator: enabled but live publisher / execution_narration store / execution repo not wired — narrator NOT started")
		return
	}

	n := &narrator.Narrator{
		Sub:        c.livePub,
		Pub:        c.livePub,
		Store:      c.repos.ExecutionNarration,
		Executions: c.repos.Executions,
		Client:     c.ChatClient,
		Model:      c.Config.Narrator.Model,
		LLMUsage:   c.repos.LLMUsage,
		Scanner:    c.secretsDetector,
		Logger:     c.Logger.With().Str("component", "narrator").Logger(),
		// Chat push (task 2.3) — Tasks/Audit/Resolver are the SAME
		// chatorigin.Resolve collaborators steeringNotifier() wires,
		// shared via internal/chatorigin so the two callers can't drift.
		// Any nil (e.g. no chat-audit repo wired) leaves chat push a
		// silent no-op — narration itself is unaffected.
		Tasks:           c.repos.Tasks,
		Audit:           c.repos.ChatAudit,
		Resolver:        &containerChannelResolver{c: c},
		ProjectSettings: c.narratorProjectSettings,
		BaseURL:         c.Config.Auth.ExternalBaseURL,
	}
	if len(c.Config.Narrator.ChatMilestoneKinds) > 0 {
		n.ChatMilestoneKinds = c.Config.Narrator.ChatMilestoneKinds
	}
	if c.pricingTable != nil {
		n.Pricing = c.pricingTable
	}
	if s := c.Config.Narrator.DebounceSeconds; s > 0 {
		n.Debounce = time.Duration(s) * time.Second
	}
	if s := c.Config.Narrator.LongToolThresholdSeconds; s > 0 {
		n.LongToolThresh = time.Duration(s) * time.Second
	}
	if s := c.Config.Narrator.MinLineIntervalSeconds; s > 0 {
		n.MinLineInterval = time.Duration(s) * time.Second
	}
	if c.Config.Narrator.MaxLines > 0 {
		n.MaxLines = c.Config.Narrator.MaxLines
	}
	if c.Config.Narrator.MaxCostUSD > 0 {
		n.MaxCostUSD = c.Config.Narrator.MaxCostUSD
	}
	c.narratorWorker = n

	if c.observabilityRegistry() != nil {
		n.Metrics = narrator.NewMetrics(c.observabilityRegistry())
	}

	// Startup visibility for the active budget/line-cap (design §9
	// Q1's rollout checklist item — "startup logs the active
	// budget/line-cap").
	c.Logger.Info().
		Str("model", c.Config.Narrator.Model).
		Dur("debounce", n.Debounce).
		Dur("long_tool_threshold", n.LongToolThresh).
		Dur("min_line_interval", n.MinLineInterval).
		Int("max_lines", n.MaxLines).
		Float64("max_cost_usd", n.MaxCostUSD).
		Msg("narrator: wired")
}

// narratorProjectSettings resolves a project's Narrated Execution
// opt-in/opt-out flags (registry.Project.Narrator — chat_push,
// no_narration; task 2.3) from the container's registry. Mirrors
// LazyProjectJudge's lazy-lookup pattern (container_judge.go) so a YAML
// reload picks up config changes without a daemon restart. An unknown
// project id (or a nil Registry) resolves to the package defaults (chat
// push off, narration on) — the same behaviour as an un-configured
// narrator block.
func (c *Container) narratorProjectSettings(projectID string) narrator.ProjectNarratorSettings {
	if c == nil || c.Registry == nil {
		return narrator.ProjectNarratorSettings{}
	}
	p := c.Registry.GetProject(projectID)
	if p == nil {
		return narrator.ProjectNarratorSettings{}
	}
	return narrator.ProjectNarratorSettings{ChatPush: p.Narrator.ChatPush, NoNarration: p.Narrator.NoNarration}
}

// wireNarratorArtifacts late-binds the narrator's completion-push artifact
// lister (task 2.3, §5.7 point 4 — deliverable-led completion push) once
// c.artifactStore exists. Required because initScheduler (which builds
// artifactStore) runs AFTER initNarrator in NewContainer; calling this
// right after initScheduler succeeds closes that ordering gap. No-op when
// the narrator wasn't constructed or the store isn't available — the
// completion push then falls back to the plain narration text.
func (c *Container) wireNarratorArtifacts() {
	if c == nil || c.narratorWorker == nil || c.artifactStore == nil {
		return
	}
	c.narratorWorker.Artifacts = c.artifactStore
}

// startNarratorWorker launches the narrator's Run loop as an
// in-daemon goroutine, mirroring how LLMConsolidateWorker is
// started. Nil-safe: Run() itself no-ops when the worker wasn't
// constructed (disabled or missing wiring).
func (c *Container) startNarratorWorker(ctx context.Context) {
	if c.narratorWorker == nil {
		return
	}
	go c.narratorWorker.Run(collectorsCtxFrom(ctx, c))
}
