package configassist

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

// Registry is the read surface grounding needs. *registry.Registry
// satisfies it.
type Registry interface {
	GetProject(id string) *registry.Project
	GetSwarm(id string) *registry.Swarm
	GetWorkflow(id string) *registry.Workflow
	ListProjects() []*registry.Project
}

// Measurement is one evidence block as grounding renders it (design §4.1.1
// rule 1): PRESENT with data, ABSENT (not measured), or NOT DECLARED.
type Measurement struct {
	Name string
	// State is one of "present", "not measured", "not declared".
	State string
	// Data is the measurement, rendered as JSON when present.
	Data any
	// Note explains the state (e.g. "no autonomy.feeds declared").
	Note string
}

// Evidence is the read-only source of the deployment's measurements
// (design §4.1). Every method reports (data, present): an absent series is
// NOT MEASURED, never zero, and the renderer labels it so. Implemented by
// an adapter over the daemon's existing surfaces — the assistant grows no
// SQL of its own for a measurement that already has an implementation.
type Evidence interface {
	// AutonomyHealth is the autonomy/health table for the project (outcome
	// monotony, feed lag and cadence breaches, delivery, route churn).
	AutonomyHealth(ctx context.Context, projectID string) (data any, present bool, err error)
	// RatingsRollup is the execution-ratings rollup WITH its
	// counterfactual arms. A rollup without arms is not evidence.
	RatingsRollup(ctx context.Context, projectID string) (data any, present bool, err error)
	// QualityPercentiles are the measured per-(swarm, role) prompt-token
	// and step-duration percentiles.
	QualityPercentiles(ctx context.Context, projectID string) (data any, present bool, err error)
	// JudgeVerdicts are recent pass/fail/abstain verdicts.
	JudgeVerdicts(ctx context.Context, projectID string) (data any, present bool, err error)
}

// Grounding is the injected fact the model is given (design §4): the real
// schema, the real resolved config (redacted), the step graph, the class
// table, and the evidence with its honesty labels.
type Grounding struct {
	ProjectID      string
	SchemaKeys     []string
	ResolvedConfig string // YAML, redacted
	StepGraph      string
	Roles          string
	Feeds          string
	Measurements   []Measurement
	ProjectFiles   []string // relative paths this project reads (authorized subset)
	Warnings       []string
	evidenceRender string
}

// BuildGrounding assembles the grounding from data the daemon already
// holds. It executes nothing.
func BuildGrounding(ctx context.Context, reg Registry, ev Evidence, projectID string) (*Grounding, error) {
	g := &Grounding{ProjectID: projectID, SchemaKeys: registry.ProjectConfigKeys()}
	if reg == nil {
		return nil, fmt.Errorf("configassist: no registry")
	}
	p := reg.GetProject(projectID)
	if p == nil {
		return nil, fmt.Errorf("configassist: project %q not found", projectID)
	}
	g.ResolvedConfig = redactedYAML(p)
	g.ProjectFiles = projectFiles(p)
	if sw := reg.GetSwarm(p.SwarmID); sw != nil {
		g.Roles = renderRoles(sw)
	} else {
		g.Warnings = append(g.Warnings, "swarm "+p.SwarmID+" not loaded")
	}
	if wf := reg.GetWorkflow(p.DefaultWorkflowID); wf != nil {
		g.StepGraph = renderStepGraph(wf)
	} else {
		g.Warnings = append(g.Warnings, "workflow "+p.DefaultWorkflowID+" not loaded")
	}
	g.Feeds = renderFeeds(p)
	g.Measurements = collectMeasurements(ctx, ev, p)
	return g, nil
}

// projectFiles lists the relative paths a project reads: its own file(s),
// the swarm and the workflow it references. The snapshot authorizer uses
// this for a project-limited caller.
func projectFiles(p *registry.Project) []string {
	out := []string{"projects/" + p.ID + ".yaml", "projects/" + p.ID + ".yml", "projects/" + p.ID + "/"}
	if p.SwarmID != "" {
		out = append(out, "swarms/"+p.SwarmID+".md")
	}
	if p.DefaultWorkflowID != "" {
		out = append(out, "workflows/"+p.DefaultWorkflowID+".md")
	}
	if p.Autonomy.WorkflowID != "" {
		out = append(out, "workflows/"+p.Autonomy.WorkflowID+".md")
	}
	return out
}

// redactedYAML renders the resolved project through the same redactor
// `vornikctl config show` uses (secrets.RedactConfig), after dropping
// named_secrets values UNCONDITIONALLY (design §10a).
func redactedYAML(p *registry.Project) string {
	raw, err := yaml.Marshal(p)
	if err != nil {
		return "# (unrenderable)"
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return "# (unrenderable)"
	}
	dropNamedSecretValues(m)
	red := secrets.RedactConfig(m)
	out, err := yaml.Marshal(red)
	if err != nil {
		return "# (unrenderable)"
	}
	return string(out)
}

func dropNamedSecretValues(m map[string]any) {
	for k, v := range m {
		if strings.EqualFold(k, "named_secrets") || strings.EqualFold(k, "namedsecrets") {
			if list, ok := v.([]any); ok {
				for _, item := range list {
					if im, ok := item.(map[string]any); ok {
						if _, has := im["value"]; has {
							im["value"] = "[REDACTED]"
						}
					}
				}
			}
		}
		if sub, ok := v.(map[string]any); ok {
			dropNamedSecretValues(sub)
		}
	}
}

func renderStepGraph(wf *registry.Workflow) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "workflow %s (entrypoint %s)\n", wf.ID, wf.Entrypoint)
	ids := make([]string, 0, len(wf.Steps))
	for id := range wf.Steps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		st := wf.Steps[id]
		fmt.Fprintf(&sb, "  step %s: type=%s role=%s on_success=%s on_fail=%s timeout=%s\n", id, st.Type, st.Role, st.OnSuccess, st.OnFail, st.Timeout)
	}
	for id, term := range wf.Terminals {
		fmt.Fprintf(&sb, "  terminal %s: %s\n", id, term.Status)
	}
	return sb.String()
}

func renderRoles(sw *registry.Swarm) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "swarm %s\n", sw.ID)
	for _, r := range sw.Roles {
		tools := "UNRESTRICTED (no allowedTools declared)"
		if len(r.Permissions.AllowedTools) > 0 {
			tools = strings.Join(r.Permissions.AllowedTools, ",")
		}
		fmt.Fprintf(&sb, "  role %s: model=%s allowedTools=%s\n", r.Name, r.Model, tools)
	}
	return sb.String()
}

func renderFeeds(p *registry.Project) string {
	feeds := p.ResolveFeeds()
	if len(feeds) == 0 {
		return "autonomy.feeds: NOT DECLARED (no cadence expectation exists to measure against)"
	}
	var sb strings.Builder
	for _, f := range feeds {
		fmt.Fprintf(&sb, "  feed %s: cadence %s\n", f.Slug, f.Cadence)
	}
	return sb.String()
}

// collectMeasurements applies the three honesty rules (design §4.1.1).
func collectMeasurements(ctx context.Context, ev Evidence, p *registry.Project) []Measurement {
	var out []Measurement
	add := func(name string, data any, present bool, err error, notDeclared bool, note string) {
		m := Measurement{Name: name}
		switch {
		case err != nil:
			m.State, m.Note = "not measured", "read failed: "+err.Error()
		case notDeclared:
			m.State, m.Note = "not declared", note
		case !present:
			m.State, m.Note = "not measured", "no series present — this is NOT zero and NOT healthy"
		default:
			m.State, m.Data = "present", data
		}
		out = append(out, m)
	}
	if ev == nil {
		add("autonomy_health", nil, false, nil, false, "")
		add("ratings_rollup", nil, false, nil, false, "")
		add("quality_percentiles", nil, false, nil, false, "")
		add("judge_verdicts", nil, false, nil, false, "")
		return out
	}
	feedsDeclared := len(p.ResolveFeeds()) > 0
	d, ok, err := ev.AutonomyHealth(ctx, p.ID)
	add("autonomy_health", d, ok, err, !feedsDeclared && !p.Autonomy.Enabled, "project declares no autonomy.feeds and autonomy is off — there is no cadence to be measured against; this is not OK and not a fault")
	d, ok, err = ev.RatingsRollup(ctx, p.ID)
	add("ratings_rollup", d, ok, err, false, "")
	d, ok, err = ev.QualityPercentiles(ctx, p.ID)
	add("quality_percentiles", d, ok, err, false, "")
	d, ok, err = ev.JudgeVerdicts(ctx, p.ID)
	add("judge_verdicts", d, ok, err, false, "")
	return out
}

// RenderEvidence renders the measurements for the prompt and for the
// proposal's evidence record. An absent series is written as NOT MEASURED,
// never as a number (test 25); not-declared is written as such, never OK
// (test 26).
func (g *Grounding) RenderEvidence() string {
	if g.evidenceRender != "" {
		return g.evidenceRender
	}
	var sb strings.Builder
	for _, m := range g.Measurements {
		switch m.State {
		case "present":
			b, _ := json.MarshalIndent(m.Data, "  ", "  ")
			fmt.Fprintf(&sb, "%s: PRESENT\n  %s\n", m.Name, string(b))
		case "not declared":
			fmt.Fprintf(&sb, "%s: NOT DECLARED — %s\n", m.Name, m.Note)
		default:
			fmt.Fprintf(&sb, "%s: NOT MEASURED — %s\n", m.Name, m.Note)
		}
	}
	g.evidenceRender = sb.String()
	return g.evidenceRender
}

// HasMeasurement reports whether any evidence block is present — a
// proposal with none says so and is distinguishable in the ledger (test 28).
func (g *Grounding) HasMeasurement() bool {
	for _, m := range g.Measurements {
		if m.State == "present" {
			return true
		}
	}
	return false
}

// SystemPrompt renders the grounding into the assistant's system prompt
// (design §4: injected fact, not an instruction to be careful).
func (g *Grounding) SystemPrompt(classTable string) string {
	var sb strings.Builder
	sb.WriteString("You are vornik's configuration assistant. You edit a PRIVATE COPY of this deployment's configuration tree with the file tools you are given; your edits become a reviewable proposal, never a direct write. You cannot run programs, reach the network, delete files, or edit secret values (secret values appear as opaque VORNIK_SECRET_PLACEHOLDER_ tokens — leave every token exactly where it is).\n\n")
	sb.WriteString("PROCEDURE. First reply with one line `PLAN: [\"path/one\", \"path/two\"]` listing every file you will touch, relative to the configuration root (projects/, swarms/, workflows/). The file projects/<projectID>/PROJECT_CONTEXT.md, when present, is a virtual view of that project's workspace .autonomy/PROJECT_CONTEXT.md and may be edited only for descriptive source/context guidance. Then read what you need, make the edit with file_edit or file_write, re-read your edit to check it, and finish with a short plain-language summary of what changed and why. Touch only the files in your PLAN. If the request needs a configuration key that is not in the accepted key set below, do NOT invent it: explain that the binary must be upgraded first (\"binary first, then config\"). If the request is ambiguous (e.g. \"the busiest feed\"), say which reading you took.\n\n")
	fmt.Fprintf(&sb, "PROJECT: %s\n\n", g.ProjectID)
	sb.WriteString("ACCEPTED PROJECT KEYS (every dotted key this binary accepts; anything else fails validation):\n")
	sb.WriteString(strings.Join(g.SchemaKeys, ", "))
	sb.WriteString("\n\nRESOLVED PROJECT CONFIG (as the daemon sees it, secrets redacted):\n")
	sb.WriteString(g.ResolvedConfig)
	sb.WriteString("\nWORKFLOW STEP GRAPH:\n")
	sb.WriteString(g.StepGraph)
	sb.WriteString("\nSWARM ROLES:\n")
	sb.WriteString(g.Roles)
	sb.WriteString("\nDECLARED FEEDS:\n")
	sb.WriteString(g.Feeds)
	sb.WriteString("\n\nMEASUREMENTS (evidence). Rules: an absent series is NOT MEASURED — never treat it as zero or healthy; 'not declared' is not 'OK'; a raw up/down ratio is a vanity metric — cite the rollup with its counterfactual arms or cite nothing. State the measurement that motivates each change, or say plainly that no measurement supports it.\n")
	sb.WriteString(g.RenderEvidence())
	sb.WriteString("\nBLAST-RADIUS CLASSES (what this surface may do; a refusal is your first option when a request falls outside them):\n")
	sb.WriteString(classTable)
	for _, w := range g.Warnings {
		sb.WriteString("\nWARNING: " + w)
	}
	return sb.String()
}

// ClassTable is the operator-facing class table rendered into the prompt.
const ClassTable = `A tuning (feed cadence longer, timeouts/retries down, maxTasksPerHour down): proposed; may auto-apply with opt-in and a judge pass.
B1 steering prose (autonomy.goal, step prompts): operator entrypoints only.
B2 descriptive prose (PROJECT_CONTEXT.md, descriptions): proposed; may auto-apply with opt-in.
C topology (add/remove steps, transitions, feeds, new workflow/swarm): proposed, never auto-applied.
D spend (budget caps up, cadence shorter, model changes, retries/timeouts up): proposed, never auto-applied.
E authority (allowedTools, permissions.*, MCP grants, admin.*, secrets, trading guardrails): proposed for mandatory human approval, never auto-applied; unavailable through chat.`
