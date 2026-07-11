package projectwizard

import (
	"bytes"
	"errors"
	"fmt"
	"text/template"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/rolelibrary"
)

// composedRoleImage is the container image every composed role's
// runtime carries. The role library deliberately doesn't declare an
// image (§5.3's frontmatter only pins cpu/memory/maxTokens) — every
// composed automation runs the same agent image the rest of the
// fleet does. registry.Swarm.Validate hard-requires a non-empty
// runtime.image, so materialization always fills this in.
const composedRoleImage = "vornik-agent:latest"

// bundleSwarmDoc / bundleRole decode the loose bundle.Swarm map into
// the composer's own DSL shape. Unlike registry.Swarm, a bundle role
// references a role-library archetype by ID rather than declaring its
// own runtime/tools/prompt — buildRegistrySwarm expands it.
type bundleSwarmDoc struct {
	SwarmID     string       `yaml:"swarmId"`
	DisplayName string       `yaml:"displayName"`
	LeadRole    string       `yaml:"leadRole"`
	Roles       []bundleRole `yaml:"roles"`
}

type bundleRole struct {
	Name string `yaml:"name"`
	// ArchetypeID selects the role-library entry this role
	// parameterises. Required — the composer never mints a role from
	// nothing (design §5.3).
	ArchetypeID string `yaml:"archetypeId"`
	// AllowedTools, when set, is the LLM's chosen SUBSET of the
	// archetype's allowlist. Omitted → the full archetype allowlist is
	// used (a mechanical fill, §5.4). Any entry NOT in the archetype's
	// allowlist is a meaning-changing guardrail violation — collected,
	// never silently dropped.
	AllowedTools []string `yaml:"allowedTools"`
	// Params fills the archetype prompt's declared splice points
	// ({{.topic}}, …). Missing declared params render as "" rather
	// than failing — see renderArchetypePrompt.
	Params map[string]string `yaml:"params"`
}

// bundleWorkflowDoc / bundleWorkflowStep decode one entry of
// bundle.Workflows. The composer's step DSL is deliberately simpler
// than registry.WorkflowStep: a linear "next" pointer plus a
// "terminal" flag, normalised into registry's Steps/Terminals split by
// buildRegistryWorkflow.
type bundleWorkflowDoc struct {
	WorkflowID  string               `yaml:"workflowId"`
	DisplayName string               `yaml:"displayName"`
	Description string               `yaml:"description"`
	Entrypoint  string               `yaml:"entrypoint"`
	Steps       []bundleWorkflowStep `yaml:"steps"`
}

type bundleWorkflowStep struct {
	ID      string `yaml:"id"`
	Type    string `yaml:"type"`
	Role    string `yaml:"role"`
	Handler string `yaml:"handler"`
	// Prompt is an optional per-step instruction. Agent steps require
	// a non-empty WorkflowStep.Prompt at the WORKFLOW.md layer (the
	// role's archetype-derived SystemPrompt is a separate, swarm-level
	// concern); when the bundle omits one, buildRegistryWorkflow fills
	// a generic default rather than failing the turn over it.
	Prompt   string `yaml:"prompt"`
	Next     string `yaml:"next"`
	Terminal bool   `yaml:"terminal"`
}

// roleToolViolation names one role/tool pair where the bundle's
// declared allowedTools reached beyond the archetype's allowlist — a
// meaning-changing guardrail violation (design §5.4), collected here
// (never silently dropped) so the caller can bounce the turn into a
// corrective re-prompt instead of mutating what the plan promised.
type roleToolViolation struct {
	Role string
	Tool string
}

// materializedBundle is the fully-expanded, registry-shaped result of
// a tier-3 bundle: real registry.Project/Swarm/Workflow values ready
// for guardrail bound-filling and staged validation. Zero-value
// bound fields (MaxStepVisits/MaxIterations/MaxWallClock, Budget) are
// intentionally left unfilled here — filling defaults is the
// guardrail pass's job (guardrail.go), not materialization's, so the
// two concerns test independently.
type materializedBundle struct {
	Project   *registry.Project
	Swarm     *registry.Swarm
	Workflows []*registry.Workflow
	// RoleModelTiers is role name -> archetype modelTier
	// (trivial|standard|complex), captured during buildRegistrySwarm
	// since registry.SwarmRole itself carries no tier field (it's a
	// role-library/composer-only concept, not part of the on-disk
	// swarm.md schema). Consumed by the grounded cost estimate
	// (cost_estimate.go, task 1.3) — never rendered to disk.
	RoleModelTiers map[string]string
}

// materializeBundle expands a ComposedBundle's loose maps into real
// registry structs. archetypes is keyed by ArchetypeID. Returns any
// tool-allowlist violations found while expanding roles (§5.4) — a
// non-empty violation list does not stop materialization (the caller
// decides whether to re-prompt or proceed), but the returned Swarm
// still carries the (potentially over-broad) declared tools so the
// caller can render exactly what would be rejected in a corrective
// hint.
func materializeBundle(bundle *ComposedBundle, archetypes map[string]*rolelibrary.RoleArchetype) (*materializedBundle, []roleToolViolation, error) {
	if bundle == nil {
		return nil, nil, errors.New("bundle is empty")
	}
	project, err := buildRegistryProject(bundle.Project)
	if err != nil {
		return nil, nil, fmt.Errorf("project: %w", err)
	}

	swarmDoc, err := parseBundleSwarm(bundle.Swarm)
	if err != nil {
		return nil, nil, fmt.Errorf("swarm: %w", err)
	}
	swarm, tiers, violations, err := buildRegistrySwarm(swarmDoc, archetypes)
	if err != nil {
		return nil, nil, fmt.Errorf("swarm: %w", err)
	}

	workflows := make([]*registry.Workflow, 0, len(bundle.Workflows))
	for i, wfMap := range bundle.Workflows {
		doc, err := parseBundleWorkflow(wfMap)
		if err != nil {
			return nil, nil, fmt.Errorf("workflows[%d]: %w", i, err)
		}
		wf, err := buildRegistryWorkflow(doc)
		if err != nil {
			return nil, nil, fmt.Errorf("workflows[%d]: %w", i, err)
		}
		workflows = append(workflows, wf)
	}

	return &materializedBundle{Project: project, Swarm: swarm, Workflows: workflows, RoleModelTiers: tiers}, violations, nil
}

// BuildTransientWorkflows parses each of bundle.Workflows into a
// real registry.Workflow, for read-only preview rendering (task
// 1.2a's Graph tab) rather than commit-path materialization. Unlike
// materializeBundle it does not touch bundle.Project/Swarm and needs
// no role-library archetype map — the Graph tab only needs
// entrypoint/steps/terminals to lay out, not expanded role prompts.
// Returns an error naming the offending index on a malformed
// workflow entry; callers should treat that as "can't render a
// preview graph for this bundle" rather than surfacing raw parse
// errors to the operator.
func BuildTransientWorkflows(bundle *ComposedBundle) ([]*registry.Workflow, error) {
	if bundle == nil {
		return nil, errors.New("bundle is empty")
	}
	out := make([]*registry.Workflow, 0, len(bundle.Workflows))
	for i, wfMap := range bundle.Workflows {
		doc, err := parseBundleWorkflow(wfMap)
		if err != nil {
			return nil, fmt.Errorf("workflows[%d]: %w", i, err)
		}
		wf, err := buildRegistryWorkflow(doc)
		if err != nil {
			return nil, fmt.Errorf("workflows[%d]: %w", i, err)
		}
		out = append(out, wf)
	}
	return out, nil
}

// buildRegistryProject converts the bundle's project map into a
// registry.Project. The bundle mirrors the on-disk project.yaml keys
// 1:1 (projectId, swarmId, autonomy.*, budget.*, …), so a yaml
// remarshal round-trip through the real struct's yaml tags is exact —
// no field-by-field mapping needed.
func buildRegistryProject(raw map[string]any) (*registry.Project, error) {
	if len(raw) == 0 {
		return nil, errors.New("project is empty")
	}
	b, err := yaml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var p registry.Project
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if p.ID == "" {
		return nil, errors.New("projectId is required")
	}
	return &p, nil
}

func parseBundleSwarm(raw map[string]any) (*bundleSwarmDoc, error) {
	b, err := yaml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var doc bundleSwarmDoc
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return &doc, nil
}

func parseBundleWorkflow(raw map[string]any) (*bundleWorkflowDoc, error) {
	b, err := yaml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var doc bundleWorkflowDoc
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return &doc, nil
}

// buildRegistrySwarm expands each bundle role against its archetype.
// An archetypeId that doesn't resolve is a hard error (materialization
// cannot proceed — there is nothing to expand); an over-broad
// allowedTools declaration is collected as a violation instead, per
// the fill-vs-reprompt precedence (§5.4) — the caller decides what to
// do with it. The third return value maps role name -> archetype
// modelTier (task 1.3's cost-estimate heuristic input); it is NOT
// carried onto registry.SwarmRole itself since modelTier is a
// composer/role-library concept, not part of the on-disk swarm.md
// schema.
func buildRegistrySwarm(doc *bundleSwarmDoc, archetypes map[string]*rolelibrary.RoleArchetype) (*registry.Swarm, map[string]string, []roleToolViolation, error) {
	if doc == nil || doc.SwarmID == "" {
		return nil, nil, nil, errors.New("swarmId is required")
	}
	sw := &registry.Swarm{ID: doc.SwarmID, DisplayName: doc.DisplayName, LeadRole: doc.LeadRole}
	tiers := make(map[string]string, len(doc.Roles))
	var violations []roleToolViolation
	for _, r := range doc.Roles {
		if r.Name == "" {
			return nil, nil, nil, errors.New("role name is required")
		}
		arch, ok := archetypes[r.ArchetypeID]
		if !ok || arch == nil {
			return nil, nil, nil, fmt.Errorf("role %q references unknown archetype %q", r.Name, r.ArchetypeID)
		}
		allowed := toSet(arch.Tools)
		tools := r.AllowedTools
		if len(tools) == 0 {
			tools = append([]string(nil), arch.Tools...)
		} else {
			for _, t := range tools {
				if !allowed[t] {
					violations = append(violations, roleToolViolation{Role: r.Name, Tool: t})
				}
			}
		}
		sw.Roles = append(sw.Roles, registry.SwarmRole{
			Name:               r.Name,
			SystemPrompt:       renderArchetypePrompt(arch, r.Params),
			RequiredOutputKeys: append([]string(nil), arch.RequiredOutputKeys...),
			MaxTokens:          arch.Runtime.MaxTokens,
			Runtime: registry.SwarmRoleRuntime{
				Image:  composedRoleImage,
				CPU:    arch.Runtime.CPU,
				Memory: arch.Runtime.Memory,
			},
			Permissions: registry.SwarmRolePermissions{AllowedTools: tools},
		})
		tiers[r.Name] = arch.ModelTier
	}
	return sw, tiers, violations, nil
}

// renderArchetypePrompt executes the archetype's prompt body as a Go
// template against the role's params. Missing declared splice points
// render as "" (missingkey=zero on a map[string]string) rather than
// erroring — the library doctor check (CheckLibrary) is what enforces
// every splice point is DECLARED; a declared-but-unsupplied param at
// synthesis time is a normal, safe occurrence (the composer bundle's
// role entries don't always carry every param) and shouldn't fail the
// whole turn. Falls back to the raw prompt body on a template error,
// which should not happen in practice since CheckLibrary already
// verified the prompt parses.
func renderArchetypePrompt(arch *rolelibrary.RoleArchetype, params map[string]string) string {
	tmpl, err := template.New(arch.ArchetypeID).Option("missingkey=zero").Parse(arch.Prompt)
	if err != nil {
		return arch.Prompt
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, params); err != nil {
		return arch.Prompt
	}
	return buf.String()
}

// buildRegistryWorkflow normalises the bundle's linear step DSL into
// registry's Steps/Terminals split. Every step uniformly gets
// on_fail="failed" wired to a shared FAILED terminal (a safety-net
// default — a step failure with no on_fail hard-fails the whole
// execution, which is a worse default for a composed automation the
// user didn't hand-author). A step marked `terminal: true` routes
// on_success to a shared "done" COMPLETED terminal instead of `next`.
func buildRegistryWorkflow(doc *bundleWorkflowDoc) (*registry.Workflow, error) {
	if doc == nil || doc.WorkflowID == "" {
		return nil, errors.New("workflowId is required")
	}
	if doc.Entrypoint == "" {
		return nil, errors.New("entrypoint is required")
	}
	if len(doc.Steps) == 0 {
		return nil, errors.New("at least one step is required")
	}
	steps := make(map[string]registry.WorkflowStep, len(doc.Steps))
	hasDone := false
	for _, s := range doc.Steps {
		if s.ID == "" {
			return nil, errors.New("workflow step is missing id")
		}
		step := registry.WorkflowStep{Type: s.Type, Role: s.Role, Handler: s.Handler, Prompt: s.Prompt, OnFail: "failed"}
		if step.Type == "agent" && step.Prompt == "" {
			step.Prompt = fmt.Sprintf("Perform the %q step as the %s role, per the swarm's role prompt and the overall plan.", s.ID, s.Role)
		}
		if s.Terminal {
			step.OnSuccess = "done"
			hasDone = true
		} else if s.Next != "" {
			step.OnSuccess = s.Next
		}
		steps[s.ID] = step
	}
	terminals := map[string]registry.WorkflowTerminal{
		"failed": {Status: "FAILED", Message: "composed workflow failed"},
	}
	if hasDone {
		terminals["done"] = registry.WorkflowTerminal{Status: "COMPLETED"}
	}
	return &registry.Workflow{
		ID:          doc.WorkflowID,
		DisplayName: doc.DisplayName,
		Description: doc.Description,
		Entrypoint:  doc.Entrypoint,
		Steps:       steps,
		Terminals:   terminals,
	}, nil
}

// renderMaterializedBundle marshals a materializedBundle into the
// on-disk file set (project YAML + swarm/workflow Markdown), keyed by
// path relative to a configs root — the same shape
// templates.Catalog.MaterialiseFiles / Compose produce, ready for
// staged validation (staged_validate.go) or, in a later phase, a
// commit write.
func renderMaterializedBundle(mb *materializedBundle) (map[string]string, error) {
	if mb == nil || mb.Project == nil || mb.Swarm == nil {
		return nil, errors.New("materialized bundle is incomplete")
	}
	files := map[string]string{}

	projBytes, err := yaml.Marshal(mb.Project)
	if err != nil {
		return nil, fmt.Errorf("marshal project: %w", err)
	}
	files["projects/"+mb.Project.ID+".yaml"] = string(projBytes)

	swarmBytes, err := registry.MarshalSwarmMarkdown(mb.Swarm)
	if err != nil {
		return nil, fmt.Errorf("marshal swarm: %w", err)
	}
	files["swarms/"+mb.Swarm.ID+".md"] = string(swarmBytes)

	for _, wf := range mb.Workflows {
		wfBytes, err := registry.MarshalWorkflowMarkdown(wf)
		if err != nil {
			return nil, fmt.Errorf("marshal workflow %s: %w", wf.ID, err)
		}
		files["workflows/"+wf.ID+".md"] = string(wfBytes)
	}
	return files, nil
}

func toSet(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}
	return out
}
