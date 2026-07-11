package controlplane

// actionize.go — deterministic change rendering for control-plane proposals
// (LLD 2026-07-11-control-plane-actionable-proposals §4). A renderer takes a
// typed change + the current deployed file and returns the full new file
// bytes plus review metadata (diff, base hash, one-line summary), so the
// proposal it lands on flows through the SHIPPED apply engine unchanged
// (whole-file replace, base-hash staleness, validate → reload-or-rollback).
// Rendering is best-effort by contract: callers degrade any error to the
// informational proposal they file today — a detector never goes silent
// because rendering failed, and a half-rendered change never reaches the
// ledger.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// ErrChangeNotUseful means the computed change would not improve on the
// current value (suggested ≤ current, or a no-op) — the caller files the
// informational proposal instead. Distinct from a real render error so
// callers can log the two differently.
var ErrChangeNotUseful = errors.New("control-plane: computed change is not an improvement over the current value")

// RenderedChange is a concrete, applyable config edit (design §4.3). The
// caller stamps ApplyTarget/ApplyContent/Diff onto the proposal and merges
// {"base_hash": BaseHash, "change": Change} into its Evidence.
type RenderedChange struct {
	ApplyTarget  string         // rel to the deployed config root (dir of config.yaml)
	ApplyContent string         // full new file bytes (whole-file replace)
	Diff         string         // compact old→new for review
	BaseHash     string         // sha256 of the current bytes (ErrStaleBase gate)
	Summary      string         // one line, e.g. `steps[implement].timeout: "10m" → "20m"`
	BlastRadius  string         // persistence.ProposalScope*
	LiveApply    bool           // skip the busy gate (MCP catalog edits only)
	Clamped      bool           // the suggested value was clamped to a bound
	Change       map[string]any // typed params, recorded in Evidence for apply-time re-validation
}

// Actionizer renders the three allowlisted change kinds. Deps are funcs so
// the container wires real loaders and tests drive every path in-memory.
type Actionizer struct {
	// ReadFile reads a path relative to the deployed config root.
	ReadFile func(rel string) ([]byte, error)
	// ValidateWorkflow / ValidateSwarm parse the FULL edited file through the
	// daemon's actual loaders (registry.ParseWorkflowMarkdown /
	// ParseSwarmMarkdown) so a corrupt render never reaches the ledger.
	// Nil → skipped (unit tests; apply's validate+reload still backs it).
	ValidateWorkflow func(filename string, content []byte) error
	ValidateSwarm    func(filename string, content []byte) error
	// KnownModel reports whether a model id exists in the daemon's model
	// universe (pricing table). Nil → every model refused (fail closed: the
	// model catalog is the whole point of the check).
	KnownModel func(model string) bool
	// MaxSuggestedStepTimeout caps a rendered step timeout absolutely
	// (design §4.5; 0 → 2h).
	MaxSuggestedStepTimeout time.Duration
	Logger                  zerolog.Logger
}

func (a *Actionizer) maxStepTimeout() time.Duration {
	if a.MaxSuggestedStepTimeout > 0 {
		return a.MaxSuggestedStepTimeout
	}
	return 2 * time.Hour
}

// safeIdent rejects identifiers that could steer a rel path outside its
// directory (defence in depth: the container ReadFile and the apply engine
// both prefix-guard, but an LLM-selected workflow/swarm/project name must
// never reach path assembly with separators or dot-dot in it).
func safeIdent(id string) bool {
	return id != "" && !strings.ContainsAny(id, "/\\") && !strings.Contains(id, "..")
}

func workflowRel(workflowID string) (string, error) {
	if !safeIdent(workflowID) {
		return "", fmt.Errorf("control-plane: invalid workflow id %q", workflowID)
	}
	return "configs/workflows/" + workflowID + ".md", nil
}

func swarmRel(swarmID string) (string, error) {
	if !safeIdent(swarmID) {
		return "", fmt.Errorf("control-plane: invalid swarm id %q", swarmID)
	}
	return "configs/swarms/" + swarmID + ".md", nil
}

func projectRel(projectID string) (string, error) {
	if !safeIdent(projectID) {
		return "", fmt.Errorf("control-plane: invalid project id %q", projectID)
	}
	return "configs/projects/" + projectID + ".yaml", nil
}

// CurrentStepTimeout reads a workflow step's explicit timeout. explicit=false
// (nil error) means the step exists but has no timeout of its own — the
// caller takes the informational branch (design §4.4: the renderer never
// authors a first timeout over inherited defaults). Workflow frontmatter
// keys steps as a MAP (steps.<id>.…, registry.Workflow.Steps), not a list.
func (a *Actionizer) CurrentStepTimeout(workflowID, stepID string) (d time.Duration, explicit bool, err error) {
	rel, err := workflowRel(workflowID)
	if err != nil {
		return 0, false, err
	}
	raw, err := a.ReadFile(rel)
	if err != nil {
		return 0, false, err
	}
	var val string
	var stepExists bool
	_, ferr := config.EditFrontmatter(raw, func(fm []byte) ([]byte, error) {
		// Every parsed step has a required `type` — its presence is the
		// existence check (the step node itself is a mapping, not a scalar).
		stepExists = config.GetYAMLString(fm, "steps."+stepID+".type") != ""
		val = config.GetYAMLString(fm, "steps."+stepID+".timeout")
		return fm, nil
	})
	if ferr != nil {
		return 0, false, ferr
	}
	if !stepExists {
		return 0, false, fmt.Errorf("control-plane: workflow %s has no step %q", workflowID, stepID)
	}
	if strings.TrimSpace(val) == "" {
		return 0, false, nil
	}
	d, perr := time.ParseDuration(val)
	if perr != nil {
		return 0, false, fmt.Errorf("control-plane: step %s timeout %q unparseable: %w", stepID, val, perr)
	}
	return d, true, nil
}

// RenderStepTimeout renders a workflow_step_timeout change: bounds the
// suggested value against the step's current explicit timeout
// ([30s, max(5m, 2×current)], absolute cap MaxSuggestedStepTimeout), applies
// it via the comment-preserving frontmatter editor, and re-parses the result.
func (a *Actionizer) RenderStepTimeout(workflowID, stepID string, suggested time.Duration) (*RenderedChange, error) {
	rel, err := workflowRel(workflowID)
	if err != nil {
		return nil, err
	}
	raw, err := a.ReadFile(rel)
	if err != nil {
		return nil, err
	}
	current, explicit, err := a.CurrentStepTimeout(workflowID, stepID)
	if err != nil {
		return nil, err
	}
	if !explicit {
		return nil, fmt.Errorf("control-plane: step %s has no explicit timeout; not rendering a first one", stepID)
	}
	bounded, clamped := boundStepTimeout(suggested, current, a.maxStepTimeout())
	if bounded <= current {
		return nil, ErrChangeNotUseful
	}
	newVal := formatDurationShort(bounded)
	edited, err := config.EditFrontmatter(raw, func(fm []byte) ([]byte, error) {
		out, _, serr := config.SetYAMLKey(fm, "steps."+stepID+".timeout", newVal)
		return out, serr
	})
	if err != nil {
		return nil, err
	}
	if a.ValidateWorkflow != nil {
		if verr := a.ValidateWorkflow(rel, edited); verr != nil {
			return nil, fmt.Errorf("control-plane: rendered workflow failed to parse: %w", verr)
		}
	}
	rc := &RenderedChange{
		ApplyTarget:  rel,
		ApplyContent: string(edited),
		BlastRadius:  persistence.ProposalScopeProject,
		Clamped:      clamped,
		Summary:      fmt.Sprintf("steps[%s].timeout: %q → %q", stepID, formatDurationShort(current), newVal),
		Change: map[string]any{
			"kind": "workflow_step_timeout", "workflow": workflowID, "step": stepID, "timeout": newVal,
		},
	}
	return a.finishRender(rc, raw, edited)
}

// RenderStepTimeoutReduction renders a workflow_step_timeout change that
// LOWERS a step's timeout — the reclaim-capacity counterpart to
// RenderStepTimeout. It fires when observed p95 sits well below the
// configured timeout, so the timeout can be tightened to free scheduler
// headroom without risking healthy-run truncation. Guards:
//   - explicit timeout required (never authors a first timeout, same as the
//     raise path);
//   - suggested is floored at 30s;
//   - suggested must be strictly BELOW current, else ErrChangeNotUseful —
//     this is the mirror of the raise path's "> current" guard, so a
//     suggestion that wouldn't actually reduce is a no-op.
//
// The caller computes suggested as ceil(p95 × headroom) so the new timeout
// still clears observed runs with margin.
func (a *Actionizer) RenderStepTimeoutReduction(workflowID, stepID string, suggested time.Duration) (*RenderedChange, error) {
	rel, err := workflowRel(workflowID)
	if err != nil {
		return nil, err
	}
	raw, err := a.ReadFile(rel)
	if err != nil {
		return nil, err
	}
	current, explicit, err := a.CurrentStepTimeout(workflowID, stepID)
	if err != nil {
		return nil, err
	}
	if !explicit {
		return nil, fmt.Errorf("control-plane: step %s has no explicit timeout; nothing to reduce", stepID)
	}
	bounded := suggested
	clamped := false
	if bounded < 30*time.Second {
		bounded = 30 * time.Second
		clamped = true
	}
	if bounded >= current {
		return nil, ErrChangeNotUseful
	}
	newVal := formatDurationShort(bounded)
	edited, err := config.EditFrontmatter(raw, func(fm []byte) ([]byte, error) {
		out, _, serr := config.SetYAMLKey(fm, "steps."+stepID+".timeout", newVal)
		return out, serr
	})
	if err != nil {
		return nil, err
	}
	if a.ValidateWorkflow != nil {
		if verr := a.ValidateWorkflow(rel, edited); verr != nil {
			return nil, fmt.Errorf("control-plane: rendered workflow failed to parse: %w", verr)
		}
	}
	rc := &RenderedChange{
		ApplyTarget:  rel,
		ApplyContent: string(edited),
		BlastRadius:  persistence.ProposalScopeProject,
		Clamped:      clamped,
		Summary:      fmt.Sprintf("steps[%s].timeout: %q → %q", stepID, formatDurationShort(current), newVal),
		Change: map[string]any{
			"kind": "workflow_step_timeout", "workflow": workflowID, "step": stepID, "timeout": newVal,
		},
	}
	return a.finishRender(rc, raw, edited)
}

// boundStepTimeout clamps suggested to [30s, max(5m, 2×current)] then to the
// absolute cap. Reports whether any clamp engaged.
func boundStepTimeout(suggested, current, absCap time.Duration) (time.Duration, bool) {
	clamped := false
	relCap := 2 * current
	if relCap < 5*time.Minute {
		relCap = 5 * time.Minute
	}
	if suggested > relCap {
		suggested = relCap
		clamped = true
	}
	if suggested > absCap {
		suggested = absCap
		clamped = true
	}
	if suggested < 30*time.Second {
		suggested = 30 * time.Second
		clamped = true
	}
	return suggested, clamped
}

// RenderRoleModel renders a swarm_role_model change: the model must exist in
// the daemon's model universe and differ from the role's current model.
// BlastRadius=swarm — apply demands the explicit ack (design §4.5).
func (a *Actionizer) RenderRoleModel(swarmID, role, model string) (*RenderedChange, error) {
	if a.KnownModel == nil || !a.KnownModel(model) {
		return nil, fmt.Errorf("control-plane: model %q is not in the configured model universe", model)
	}
	rel, err := swarmRel(swarmID)
	if err != nil {
		return nil, err
	}
	raw, err := a.ReadFile(rel)
	if err != nil {
		return nil, err
	}
	var current string
	var roleExists bool
	if _, ferr := config.EditFrontmatter(raw, func(fm []byte) ([]byte, error) {
		current, _ = config.GetYAMLListItemField(fm, "roles", "name", role, "model")
		_, roleExists = config.GetYAMLListItemField(fm, "roles", "name", role, "name")
		return fm, nil
	}); ferr != nil {
		return nil, ferr
	}
	if !roleExists {
		return nil, fmt.Errorf("control-plane: swarm %s has no role %q", swarmID, role)
	}
	if current == model {
		return nil, ErrChangeNotUseful
	}
	edited, err := config.EditFrontmatter(raw, func(fm []byte) ([]byte, error) {
		return config.SetYAMLListItemField(fm, "roles", "name", role, "model", model)
	})
	if err != nil {
		return nil, err
	}
	if a.ValidateSwarm != nil {
		if verr := a.ValidateSwarm(rel, edited); verr != nil {
			return nil, fmt.Errorf("control-plane: rendered swarm failed to parse: %w", verr)
		}
	}
	rc := &RenderedChange{
		ApplyTarget:  rel,
		ApplyContent: string(edited),
		BlastRadius:  persistence.ProposalScopeSwarm,
		Summary:      fmt.Sprintf("roles[%s].model: %q → %q", role, current, model),
		Change: map[string]any{
			"kind": "swarm_role_model", "swarm": swarmID, "role": role, "model": model,
		},
	}
	return a.finishRender(rc, raw, edited)
}

// FindMCPServerScope resolves which config tree owns an MCP server entry.
// Daemon-first (design §4.4): a server present at both scopes resolves to the
// daemon catalog (that is the entry clients inherit). Returns ("", true) for
// daemon scope, (projectID, true) for the project file, (_, false) when the
// server exists at neither.
func (a *Actionizer) FindMCPServerScope(projectID, server string) (scope string, ok bool) {
	if raw, err := a.ReadFile("config.yaml"); err == nil {
		if _, found := config.GetYAMLListItemField(raw, "mcp.servers", "name", server, "name"); found {
			return "", true
		}
	}
	if projectID != "" {
		if rel, rerr := projectRel(projectID); rerr == nil {
			if raw, err := a.ReadFile(rel); err == nil {
				if _, found := config.GetYAMLListItemField(raw, "mcp.servers", "name", server, "name"); found {
					return projectID, true
				}
			}
		}
	}
	return "", false
}

// RenderMCPServerTimeout renders an mcp_server_timeout change on the daemon
// catalog (projectID == "") or a project's own mcp.servers entry. Raise-only:
// a suggestion at/below the current timeout_seconds is ErrChangeNotUseful.
// LiveApply is set — same posture as the hub's MCP edits (the catalog is
// injected at container start; in-flight tasks keep their client).
func (a *Actionizer) RenderMCPServerTimeout(projectID, server string, suggestedSeconds int) (*RenderedChange, error) {
	rel := "config.yaml"
	radius := persistence.ProposalScopeDaemon
	if projectID != "" {
		prel, perr := projectRel(projectID)
		if perr != nil {
			return nil, perr
		}
		rel = prel
		radius = persistence.ProposalScopeProject
	}
	raw, err := a.ReadFile(rel)
	if err != nil {
		return nil, err
	}
	if _, found := config.GetYAMLListItemField(raw, "mcp.servers", "name", server, "name"); !found {
		return nil, fmt.Errorf("control-plane: no mcp server %q in %s", server, rel)
	}
	current := 30 // the client's default when timeout_seconds is 0/absent
	if cur, found := config.GetYAMLListItemField(raw, "mcp.servers", "name", server, "timeout_seconds"); found {
		if n, cerr := strconv.Atoi(cur); cerr == nil && n > 0 {
			current = n
		}
	}
	if suggestedSeconds <= current {
		return nil, ErrChangeNotUseful
	}
	edited, err := config.SetYAMLListItemField(raw, "mcp.servers", "name", server, "timeout_seconds", suggestedSeconds)
	if err != nil {
		return nil, err
	}
	rc := &RenderedChange{
		ApplyTarget:  rel,
		ApplyContent: string(edited),
		BlastRadius:  radius,
		LiveApply:    true,
		Summary:      fmt.Sprintf("mcp.servers[%s].timeout_seconds: %d → %d", server, current, suggestedSeconds),
		Change: map[string]any{
			"kind": "mcp_server_timeout", "project": projectID, "server": server, "timeout_seconds": suggestedSeconds,
		},
	}
	return a.finishRender(rc, raw, edited)
}

// finishRender fills the shared review metadata (size cap, base hash, diff).
func (a *Actionizer) finishRender(rc *RenderedChange, oldRaw, newRaw []byte) (*RenderedChange, error) {
	if len(newRaw) > persistence.ProposalMaxFieldBytes {
		return nil, fmt.Errorf("control-plane: rendered content %d bytes exceeds the %d cap", len(newRaw), persistence.ProposalMaxFieldBytes)
	}
	sum := sha256.Sum256(oldRaw)
	rc.BaseHash = hex.EncodeToString(sum[:])
	rc.Diff = compactDiff(string(oldRaw), string(newRaw))
	return rc, nil
}

// RevalidateChange is the apply-time semantic re-check (design §4.5, review
// #6): given a proposal's Evidence JSON, it re-verifies the typed "change"
// against CURRENT state — the referenced workflow step / swarm role / MCP
// server must still exist and a model must still be in the universe. Content
// drift is the base-hash gate's job; this catches the world changing under an
// approved proposal (e.g. a model deprecated between draft and apply).
// Evidence without a "change" object passes (not an actionized proposal).
func (a *Actionizer) RevalidateChange(projectID, evidence string) error {
	if strings.TrimSpace(evidence) == "" {
		return nil
	}
	var env struct {
		Change *DiagnoseConfigChange `json:"change"`
	}
	if err := json.Unmarshal([]byte(evidence), &env); err != nil || env.Change == nil {
		return nil // not an actionized proposal
	}
	cc := env.Change
	switch cc.Kind {
	case "workflow_step_timeout":
		return a.revalidateStepTimeout(cc)
	case "swarm_role_model":
		return a.revalidateRoleModel(cc)
	case "mcp_server_timeout":
		return a.revalidateMCPServerTimeout(projectID, cc)
	default:
		return fmt.Errorf("revalidate: change kind %q not in the allowlist", cc.Kind)
	}
}

func (a *Actionizer) revalidateStepTimeout(cc *DiagnoseConfigChange) error {
	if _, err := time.ParseDuration(cc.Timeout); err != nil {
		return fmt.Errorf("revalidate: timeout %q unparseable: %w", cc.Timeout, err)
	}
	_, explicit, err := a.CurrentStepTimeout(cc.Workflow, cc.Step)
	if err != nil {
		return fmt.Errorf("revalidate: %w", err)
	}
	if !explicit {
		return fmt.Errorf("revalidate: step %s no longer has an explicit timeout", cc.Step)
	}
	return nil
}

func (a *Actionizer) revalidateRoleModel(cc *DiagnoseConfigChange) error {
	if a.KnownModel == nil || !a.KnownModel(cc.Model) {
		return fmt.Errorf("revalidate: model %q is no longer in the configured model universe", cc.Model)
	}
	srel, err := swarmRel(cc.Swarm)
	if err != nil {
		return fmt.Errorf("revalidate: %w", err)
	}
	raw, err := a.ReadFile(srel)
	if err != nil {
		return fmt.Errorf("revalidate: %w", err)
	}
	var roleExists bool
	if _, ferr := config.EditFrontmatter(raw, func(fm []byte) ([]byte, error) {
		_, roleExists = config.GetYAMLListItemField(fm, "roles", "name", cc.Role, "name")
		return fm, nil
	}); ferr != nil {
		return fmt.Errorf("revalidate: %w", ferr)
	}
	if !roleExists {
		return fmt.Errorf("revalidate: swarm %s no longer has role %q", cc.Swarm, cc.Role)
	}
	return nil
}

func (a *Actionizer) revalidateMCPServerTimeout(projectID string, cc *DiagnoseConfigChange) error {
	scope := cc.Project
	if scope == "" && projectID != "" {
		// The change may predate the project stamp; fall back to the
		// proposal's project for scope resolution.
		s, ok := a.FindMCPServerScope(projectID, cc.Server)
		if !ok {
			return fmt.Errorf("revalidate: mcp server %q no longer exists", cc.Server)
		}
		scope = s
	}
	rel := "config.yaml"
	if scope != "" {
		prel, perr := projectRel(scope)
		if perr != nil {
			return fmt.Errorf("revalidate: %w", perr)
		}
		rel = prel
	}
	raw, err := a.ReadFile(rel)
	if err != nil {
		return fmt.Errorf("revalidate: %w", err)
	}
	if _, found := config.GetYAMLListItemField(raw, "mcp.servers", "name", cc.Server, "name"); !found {
		return fmt.Errorf("revalidate: mcp server %q no longer in %s", cc.Server, rel)
	}
	return nil
}

// ParseMCPToolName splits the executor's qualified MCP tool name
// (mcp__{server}__{tool}) into its parts; ok=false for builtin tools.
func ParseMCPToolName(name string) (server, tool string, ok bool) {
	if !strings.HasPrefix(name, "mcp__") {
		return "", "", false
	}
	rest := name[len("mcp__"):]
	i := strings.Index(rest, "__")
	if i <= 0 || i+2 >= len(rest) {
		return "", "", false
	}
	return rest[:i], rest[i+2:], true
}

// formatDurationShort renders a duration the way operators write them in
// workflow files: whole minutes as "24m", sub-minute as seconds ("90s" stays
// "1m30s"-free by rounding up to whole seconds).
func formatDurationShort(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(math.Ceil(d.Seconds())))
}

// compactDiff is a minimal line diff for the review surface: lines removed
// from old and added in new, in order. Our renders change one or two lines,
// so a full LCS diff buys nothing.
func compactDiff(oldS, newS string) string {
	oldLines := strings.Split(oldS, "\n")
	newLines := strings.Split(newS, "\n")
	oldSet := make(map[string]int, len(oldLines))
	for _, l := range oldLines {
		oldSet[l]++
	}
	newSet := make(map[string]int, len(newLines))
	for _, l := range newLines {
		newSet[l]++
	}
	var sb strings.Builder
	for _, l := range oldLines {
		if newSet[l] == 0 {
			sb.WriteString("- " + l + "\n")
		} else {
			newSet[l]--
		}
	}
	for _, l := range newLines {
		if oldSet[l] == 0 {
			sb.WriteString("+ " + l + "\n")
		} else {
			oldSet[l]--
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}
