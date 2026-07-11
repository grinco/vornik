package projectwizard

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/rolelibrary"
)

func researcherArchetype() *rolelibrary.RoleArchetype {
	return &rolelibrary.RoleArchetype{
		ArchetypeID:        "researcher",
		DisplayName:        "Researcher",
		Description:        "Gathers information.",
		Tools:              []string{"file_read", "file_write", "memory_search"},
		RequiredOutputKeys: []string{"summary"},
		Runtime:            rolelibrary.ArchetypeRuntime{CPU: "1", Memory: "2Gi", MaxTokens: 4096},
		ModelTier:          rolelibrary.ModelTierStandard,
		PromptParams:       []string{"topic"},
		Prompt:             "Research: {{.topic}}.",
	}
}

func writerArchetype() *rolelibrary.RoleArchetype {
	return &rolelibrary.RoleArchetype{
		ArchetypeID:        "writer",
		Tools:              []string{"file_read", "file_write"},
		RequiredOutputKeys: []string{"summary"},
		Runtime:            rolelibrary.ArchetypeRuntime{CPU: "1", Memory: "1Gi", MaxTokens: 2048},
		ModelTier:          rolelibrary.ModelTierStandard,
		Prompt:             "Write it up.",
	}
}

func testArchetypes() map[string]*rolelibrary.RoleArchetype {
	return map[string]*rolelibrary.RoleArchetype{
		"researcher": researcherArchetype(),
		"writer":     writerArchetype(),
	}
}

func validComposedBundle() *ComposedBundle {
	return &ComposedBundle{
		Project: map[string]any{
			"projectId":         "ai-news-digest",
			"displayName":       "AI News Digest",
			"swarmId":           "ai-news-digest-swarm",
			"defaultWorkflowId": "research-digest",
		},
		Swarm: map[string]any{
			"swarmId":  "ai-news-digest-swarm",
			"leadRole": "researcher",
			"roles": []any{
				map[string]any{"name": "researcher", "archetypeId": "researcher"},
				map[string]any{"name": "writer", "archetypeId": "writer"},
			},
		},
		Workflows: []map[string]any{
			{
				"workflowId": "research-digest",
				"entrypoint": "gather",
				"steps": []any{
					map[string]any{"id": "gather", "type": "agent", "role": "researcher", "next": "write"},
					map[string]any{"id": "write", "type": "agent", "role": "writer", "terminal": true},
				},
			},
		},
		Plan: ComposedPlan{Steps: []string{"gather then write"}, CostBand: "~$0.10"},
	}
}

func TestMaterializeBundle_Valid(t *testing.T) {
	mb, violations, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected no violations, got %v", violations)
	}
	if mb.Project.ID != "ai-news-digest" || mb.Project.SwarmID != "ai-news-digest-swarm" {
		t.Errorf("project not mapped correctly: %+v", mb.Project)
	}
	if len(mb.Swarm.Roles) != 2 {
		t.Fatalf("expected 2 roles, got %d", len(mb.Swarm.Roles))
	}
	researcher := mb.Swarm.Roles[0]
	if researcher.Runtime.Image != composedRoleImage {
		t.Errorf("role image = %q, want %q", researcher.Runtime.Image, composedRoleImage)
	}
	if researcher.Runtime.CPU != "1" || researcher.Runtime.Memory != "2Gi" {
		t.Errorf("runtime not carried from archetype: %+v", researcher.Runtime)
	}
	if researcher.MaxTokens != 4096 {
		t.Errorf("maxTokens = %d, want 4096", researcher.MaxTokens)
	}
	if len(researcher.Permissions.AllowedTools) != 3 {
		t.Errorf("expected full archetype allowlist filled, got %v", researcher.Permissions.AllowedTools)
	}
	if len(mb.Workflows) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(mb.Workflows))
	}
	wf := mb.Workflows[0]
	if wf.Steps["gather"].OnSuccess != "write" {
		t.Errorf("gather.OnSuccess = %q, want write", wf.Steps["gather"].OnSuccess)
	}
	if wf.Steps["write"].OnSuccess != "done" {
		t.Errorf("terminal step OnSuccess = %q, want done", wf.Steps["write"].OnSuccess)
	}
	if wf.Terminals["done"].Status != "COMPLETED" {
		t.Errorf("done terminal status = %q, want COMPLETED", wf.Terminals["done"].Status)
	}
	if wf.Steps["gather"].OnFail != "failed" || wf.Terminals["failed"].Status != "FAILED" {
		t.Error("expected uniform on_fail wiring to a FAILED terminal")
	}
	if mb.RoleModelTiers["researcher"] != rolelibrary.ModelTierStandard || mb.RoleModelTiers["writer"] != rolelibrary.ModelTierStandard {
		t.Errorf("expected RoleModelTiers captured for the grounded cost estimate, got %+v", mb.RoleModelTiers)
	}
}

// TestBuildTransientWorkflows_Valid — the Graph tab's seam (task
// 1.2a): parses bundle.Workflows into real registry.Workflow values
// without needing Project/Swarm or a role-library archetype map.
func TestBuildTransientWorkflows_Valid(t *testing.T) {
	wfs, err := BuildTransientWorkflows(validComposedBundle())
	if err != nil {
		t.Fatalf("BuildTransientWorkflows: %v", err)
	}
	if len(wfs) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(wfs))
	}
	wf := wfs[0]
	if wf.ID != "research-digest" {
		t.Errorf("workflow id = %q, want research-digest", wf.ID)
	}
	if wf.Entrypoint != "gather" {
		t.Errorf("entrypoint = %q, want gather", wf.Entrypoint)
	}
	if wf.Steps["gather"].OnSuccess != "write" {
		t.Errorf("gather.OnSuccess = %q, want write", wf.Steps["gather"].OnSuccess)
	}
}

// TestBuildTransientWorkflows_MultiWorkflow covers the "1 <= len <= 2"
// v1 shape (design §5.4/§11 Q3) — the Graph tab renders one graph per
// workflow, so BuildTransientWorkflows must return every entry.
func TestBuildTransientWorkflows_MultiWorkflow(t *testing.T) {
	bundle := validComposedBundle()
	bundle.Workflows = append(bundle.Workflows, map[string]any{
		"workflowId": "second-wf",
		"entrypoint": "only",
		"steps": []any{
			map[string]any{"id": "only", "type": "agent", "role": "researcher", "terminal": true},
		},
	})
	wfs, err := BuildTransientWorkflows(bundle)
	if err != nil {
		t.Fatalf("BuildTransientWorkflows: %v", err)
	}
	if len(wfs) != 2 {
		t.Fatalf("expected 2 workflows, got %d", len(wfs))
	}
	if wfs[0].ID != "research-digest" || wfs[1].ID != "second-wf" {
		t.Errorf("unexpected workflow ids: %q, %q", wfs[0].ID, wfs[1].ID)
	}
}

func TestBuildTransientWorkflows_NilBundle(t *testing.T) {
	if _, err := BuildTransientWorkflows(nil); err == nil {
		t.Fatal("expected error for nil bundle")
	}
}

// TestBuildTransientWorkflows_MalformedWorkflow_NamesIndex — a
// malformed workflow entry (missing entrypoint) must fail with an
// error naming its index, so a multi-workflow bundle's failure is
// diagnosable.
func TestBuildTransientWorkflows_MalformedWorkflow_NamesIndex(t *testing.T) {
	bundle := validComposedBundle()
	bundle.Workflows = append(bundle.Workflows, map[string]any{
		"workflowId": "broken",
		// entrypoint deliberately omitted.
		"steps": []any{
			map[string]any{"id": "only", "type": "agent", "terminal": true},
		},
	})
	_, err := BuildTransientWorkflows(bundle)
	if err == nil || !strings.Contains(err.Error(), "workflows[1]") {
		t.Fatalf("expected error naming workflows[1], got: %v", err)
	}
}

func TestMaterializeBundle_PromptRendersParams(t *testing.T) {
	bundle := validComposedBundle()
	bundle.Swarm["roles"] = []any{
		map[string]any{"name": "researcher", "archetypeId": "researcher", "params": map[string]any{"topic": "AI news"}},
		map[string]any{"name": "writer", "archetypeId": "writer"},
	}
	mb, _, err := materializeBundle(bundle, testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	if !strings.Contains(mb.Swarm.Roles[0].SystemPrompt, "AI news") {
		t.Errorf("expected rendered param in prompt, got %q", mb.Swarm.Roles[0].SystemPrompt)
	}
}

func TestMaterializeBundle_MissingParamRendersEmpty(t *testing.T) {
	// researcher's prompt declares {{.topic}} but no params supplied —
	// must render "" rather than error or leak "<no value>".
	mb, _, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	if strings.Contains(mb.Swarm.Roles[0].SystemPrompt, "no value") {
		t.Errorf("missing param should render empty, not <no value>: %q", mb.Swarm.Roles[0].SystemPrompt)
	}
}

func TestMaterializeBundle_UnknownArchetype(t *testing.T) {
	bundle := validComposedBundle()
	bundle.Swarm["roles"] = []any{
		map[string]any{"name": "researcher", "archetypeId": "does-not-exist"},
	}
	_, _, err := materializeBundle(bundle, testArchetypes())
	if err == nil || !strings.Contains(err.Error(), "unknown archetype") {
		t.Fatalf("expected unknown-archetype error, got %v", err)
	}
}

func TestMaterializeBundle_ToolOverreachCollectsViolation(t *testing.T) {
	bundle := validComposedBundle()
	bundle.Swarm["roles"] = []any{
		map[string]any{"name": "researcher", "archetypeId": "researcher", "allowedTools": []any{"file_read", "run_shell"}},
		map[string]any{"name": "writer", "archetypeId": "writer"},
	}
	mb, violations, err := materializeBundle(bundle, testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	if len(violations) != 1 || violations[0].Tool != "run_shell" || violations[0].Role != "researcher" {
		t.Fatalf("expected exactly one run_shell violation, got %v", violations)
	}
	// The over-broad set is still what was declared — never silently
	// stripped by materialization itself; the caller decides.
	if !toSet(mb.Swarm.Roles[0].Permissions.AllowedTools)["run_shell"] {
		t.Error("materialization must not silently strip the offending tool")
	}
}

func TestMaterializeBundle_SubsetToolsAllowed(t *testing.T) {
	bundle := validComposedBundle()
	bundle.Swarm["roles"] = []any{
		map[string]any{"name": "researcher", "archetypeId": "researcher", "allowedTools": []any{"file_read"}},
		map[string]any{"name": "writer", "archetypeId": "writer"},
	}
	mb, violations, err := materializeBundle(bundle, testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("subset selection must not violate, got %v", violations)
	}
	if len(mb.Swarm.Roles[0].Permissions.AllowedTools) != 1 {
		t.Errorf("expected the LLM's chosen subset preserved, got %v", mb.Swarm.Roles[0].Permissions.AllowedTools)
	}
}

func TestMaterializeBundle_NilBundle(t *testing.T) {
	if _, _, err := materializeBundle(nil, testArchetypes()); err == nil {
		t.Error("expected error for nil bundle")
	}
}

func TestBuildRegistryWorkflow_MissingEntrypoint(t *testing.T) {
	doc := &bundleWorkflowDoc{WorkflowID: "wf", Steps: []bundleWorkflowStep{{ID: "a", Type: "agent", Role: "r"}}}
	if _, err := buildRegistryWorkflow(doc); err == nil {
		t.Error("expected error for missing entrypoint")
	}
}

func TestBuildRegistryWorkflow_NoSteps(t *testing.T) {
	doc := &bundleWorkflowDoc{WorkflowID: "wf", Entrypoint: "a"}
	if _, err := buildRegistryWorkflow(doc); err == nil {
		t.Error("expected error for zero steps")
	}
}

func TestRenderMaterializedBundle_ProducesLoadableFiles(t *testing.T) {
	mb, _, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	files, err := renderMaterializedBundle(mb)
	if err != nil {
		t.Fatalf("renderMaterializedBundle: %v", err)
	}
	if _, ok := files["projects/ai-news-digest.yaml"]; !ok {
		t.Errorf("missing project file, got keys %v", keysOf(files))
	}
	if _, ok := files["swarms/ai-news-digest-swarm.md"]; !ok {
		t.Errorf("missing swarm file, got keys %v", keysOf(files))
	}
	if _, ok := files["workflows/research-digest.md"]; !ok {
		t.Errorf("missing workflow file, got keys %v", keysOf(files))
	}
}

func TestBuildRegistryProject_Empty(t *testing.T) {
	if _, err := buildRegistryProject(nil); err == nil {
		t.Error("expected error for nil project map")
	}
	if _, err := buildRegistryProject(map[string]any{"displayName": "no id"}); err == nil {
		t.Error("expected error when projectId is missing")
	}
}

func TestBuildRegistrySwarm_EmptySwarmID(t *testing.T) {
	if _, _, _, err := buildRegistrySwarm(&bundleSwarmDoc{}, testArchetypes()); err == nil {
		t.Error("expected error for missing swarmId")
	}
	if _, _, _, err := buildRegistrySwarm(nil, testArchetypes()); err == nil {
		t.Error("expected error for nil doc")
	}
}

func TestBuildRegistrySwarm_EmptyRoleName(t *testing.T) {
	doc := &bundleSwarmDoc{SwarmID: "s", Roles: []bundleRole{{ArchetypeID: "researcher"}}}
	if _, _, _, err := buildRegistrySwarm(doc, testArchetypes()); err == nil {
		t.Error("expected error for empty role name")
	}
}

func TestBuildRegistrySwarm_ReturnsRoleModelTiers(t *testing.T) {
	doc := &bundleSwarmDoc{SwarmID: "s", LeadRole: "researcher", Roles: []bundleRole{
		{Name: "researcher", ArchetypeID: "researcher"},
		{Name: "writer", ArchetypeID: "writer"},
	}}
	_, tiers, _, err := buildRegistrySwarm(doc, testArchetypes())
	if err != nil {
		t.Fatalf("buildRegistrySwarm: %v", err)
	}
	if tiers["researcher"] != rolelibrary.ModelTierStandard || tiers["writer"] != rolelibrary.ModelTierStandard {
		t.Errorf("expected both fixture archetypes' modelTier (standard) to be captured, got %+v", tiers)
	}
}

func TestRenderArchetypePrompt_TemplateErrorFallsBackToRaw(t *testing.T) {
	arch := &rolelibrary.RoleArchetype{ArchetypeID: "broken", Prompt: "{{.unterminated"}
	got := renderArchetypePrompt(arch, nil)
	if got != arch.Prompt {
		t.Errorf("expected fallback to raw prompt on template parse error, got %q", got)
	}
}

func TestRenderMaterializedBundle_Incomplete(t *testing.T) {
	if _, err := renderMaterializedBundle(nil); err == nil {
		t.Error("expected error for nil materialized bundle")
	}
	if _, err := renderMaterializedBundle(&materializedBundle{}); err == nil {
		t.Error("expected error when project/swarm are missing")
	}
}

func TestParseBundleSwarmAndWorkflow_EmptyMap(t *testing.T) {
	doc, err := parseBundleSwarm(map[string]any{})
	if err != nil {
		t.Fatalf("parseBundleSwarm: %v", err)
	}
	if doc.SwarmID != "" {
		t.Errorf("expected zero-value doc, got %+v", doc)
	}
	wfDoc, err := parseBundleWorkflow(map[string]any{})
	if err != nil {
		t.Fatalf("parseBundleWorkflow: %v", err)
	}
	if wfDoc.WorkflowID != "" {
		t.Errorf("expected zero-value doc, got %+v", wfDoc)
	}
}
