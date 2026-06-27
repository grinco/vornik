// Tests for BuildSystemPrompt / BuildLeadSystemPrompt. Anchor
// the operator-visible contract — sections, headings, and the
// specific rules the dispatcher's behaviour depends on — so a
// future refactor that drops a section gets caught here rather
// than in production.

package dispatcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestBuildSystemPrompt_ContainsAntiPromiseSection pins the
// "DO NOT PROMISE — ACT" rule the dispatcher relies on to avoid
// the natural-language failure mode where the model narrates a
// future action ("I'll fetch X and recommend Y") and ends the
// turn without calling a tool. The user-reported bug today was
// the Amsterdam-weather conditional ask; this anchor stops that
// section drifting back out unnoticed.
func TestBuildSystemPrompt_ContainsAntiPromiseSection(t *testing.T) {
	out := BuildSystemPrompt("", nil)

	mustContain := []string{
		// Section heading.
		"DO NOT PROMISE — ACT",
		// Representative forbidden pattern.
		`"I will fetch X and then recommend Y."`,
		// The "I will / I'll" guard reminder.
		`"I will"`,
		"misleading them",
		// Positive example mentioning create_task / adaptive
		// workflow — research workflows go through create_task,
		// not direct scraper calls.
		"create_task",
		"adaptive",
		// Re-read guard.
		"Re-read your draft reply",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// TestBuildSystemPrompt_AntiPromiseFollowsFastPath — section
// ordering matters: the new rule sits AFTER the explicit-
// scheduling fast path because the fast path handles the
// imperative-verb branch first, and the anti-promise rule is
// the cleanup for the natural-language branch the fast path
// doesn't catch.
func TestBuildSystemPrompt_AntiPromiseFollowsFastPath(t *testing.T) {
	out := BuildSystemPrompt("", nil)
	fastPathIdx := strings.Index(out, "EXPLICIT SCHEDULING DIRECTIVES")
	antiPromiseIdx := strings.Index(out, "DO NOT PROMISE")
	if fastPathIdx == -1 {
		t.Fatal("fast-path section disappeared")
	}
	if antiPromiseIdx == -1 {
		t.Fatal("DO NOT PROMISE section missing")
	}
	if antiPromiseIdx <= fastPathIdx {
		t.Errorf("DO NOT PROMISE must follow the fast-path section; got fastPath=%d antiPromise=%d", fastPathIdx, antiPromiseIdx)
	}
}

// TestBuildSystemPrompt_WeatherExampleAnchored — the positive
// example uses the user-reported failure verbatim so the model
// has a concrete template for "research-with-recommendation"
// asks. If the example drifts, the model's mapping back to
// create_task may regress.
func TestBuildSystemPrompt_WeatherExampleAnchored(t *testing.T) {
	out := BuildSystemPrompt("", nil)
	mustContain := []string{
		"weather in amsterdam",
		"t-shirt",
		"long-sleeve",
		"hoodie",
		"Scheduling a research task",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("weather example missing %q", want)
		}
	}
}

// TestBuildSystemPrompt_PerProjectPrefixPrepended — anchor the
// existing per-project system-prefix injection (line ~46 of
// prompt.go). Touched-surface coverage rule: when we add a
// section, the surrounding "is the rest still there?" tests
// catch accidental deletion.
func TestBuildSystemPrompt_PerProjectPrefixPrepended(t *testing.T) {
	projects := []*registry.Project{
		{ID: "alpha", Chat: registry.ProjectChat{SystemPrefix: "PER-PROJECT BANNER"}},
	}
	out := BuildSystemPrompt("alpha", projects)
	if !strings.HasPrefix(strings.TrimSpace(out), "PER-PROJECT BANNER") {
		// The time-context preamble lives after the prefix; the
		// prefix should still appear before "CURRENT TIME:".
		bannerIdx := strings.Index(out, "PER-PROJECT BANNER")
		timeIdx := strings.Index(out, "CURRENT TIME:")
		if bannerIdx == -1 || timeIdx == -1 || bannerIdx > timeIdx {
			t.Errorf("per-project prefix not prepended; banner=%d time=%d out[:160]=%q",
				bannerIdx, timeIdx, out[:160])
		}
	}
}

// TestBuildSystemPrompt_EmptyProjectsRenders — minimal call
// (no active project, no projects list) still emits the body.
// Guards the no-context branch.
func TestBuildSystemPrompt_EmptyProjectsRenders(t *testing.T) {
	out := BuildSystemPrompt("", nil)
	for _, want := range []string{
		"CURRENT TIME:",
		"ACTIVE PROJECT: none",
		"HOW TO HELP",
		"HOW TASKS WORK",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("empty-project prompt missing %q", want)
		}
	}
}

// TestBuildLeadSystemPrompt_RendersProjectRole — lead-mode
// prompt is the other code path in prompt.go; anchor its
// scaffold so the touched-surface coverage rule holds for the
// whole file.
func TestBuildLeadSystemPrompt_RendersProjectRole(t *testing.T) {
	project := &registry.Project{ID: "alpha", DisplayName: "Alpha"}
	out := BuildLeadSystemPrompt(project, nil, "You are the alpha lead.", nil)
	for _, want := range []string{
		`lead agent for project "alpha"`,
		"You are the alpha lead.",
		"ACTIVE PROJECT: alpha",
		"HOW TO HELP",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lead prompt missing %q", want)
		}
	}
}

// TestBuildLeadSystemPrompt_AllOptionalBranchesFire exercises
// the four conditional branches inside BuildLeadSystemPrompt
// that the simple-render test doesn't reach: DisplayName,
// PROJECT GOAL, OTHER PROJECTS (with a sibling that has a
// DisplayName and one that doesn't), and the empty-leadPrompt
// branch. Without this test the function's coverage sits at
// 60%; touched-surface rule needs ≥80%.
func TestBuildLeadSystemPrompt_AllOptionalBranchesFire(t *testing.T) {
	project := &registry.Project{
		ID:          "alpha",
		DisplayName: "Alpha Project",
		Autonomy: registry.ProjectAutonomy{
			Enabled: true,
			Goal:    "ship the slice",
		},
	}
	siblings := []*registry.Project{
		project,
		{ID: "beta", DisplayName: "Beta"},
		{ID: "gamma"}, // No DisplayName — falls back to ID.
	}
	out := BuildLeadSystemPrompt(project, nil, "", siblings)

	for _, want := range []string{
		"(Alpha Project)",
		"PROJECT GOAL",
		"ship the slice",
		"OTHER PROJECTS",
		"  - beta (Beta)",
		"  - gamma (gamma)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lead prompt branch missing %q", want)
		}
	}
	// Empty leadPrompt branch: YOUR ROLE block omitted.
	if strings.Contains(out, "YOUR ROLE AND KNOWLEDGE") {
		t.Errorf("empty leadPrompt should suppress YOUR ROLE block")
	}
}

// TestBuildLeadSystemPrompt_InboundAttachmentsDirective pins the
// section that tells the lead LLM what to do when the user message
// carries an [Attached files] block. Without this directive the
// dispatcher LLM observes the artifact_id, copies the literal string
// into the create_task prompt, and forgets to populate input_files —
// the bug operators saw on task_20260521111852_8016a4a902b4f959 where
// the worker container received the prompt but never had the EPUB
// bytes staged. Anchor the heading + the two load-bearing rules
// (pass artifact_id into input_files; the worker can't reach the DB
// itself) so a future refactor doesn't quietly drop them.
//
// Also pins the auto-extract trailer: when extraction landed at
// channel-receive time the lead must recognise the "ingested into
// project memory" marker and skip scheduling a redundant ingest
// task. The trailer phrasing must match enrichUserContent's
// output verbatim — if either drifts the LLM stops recognising
// the signal.
func TestBuildLeadSystemPrompt_InboundAttachmentsDirective(t *testing.T) {
	project := &registry.Project{ID: "alpha", DisplayName: "Alpha"}
	out := BuildLeadSystemPrompt(project, nil, "you are the alpha lead.", nil)
	for _, want := range []string{
		"INBOUND ATTACHMENTS",
		"[Attached files]",
		"artifact_id=email-att-",
		"pass every artifact_id into input_files",
		"/app/workspace/artifacts/in/",
		// Auto-extract path markers (case a / case b).
		"ingested into project memory",
		"extracted_document_id=",
		"memory_search",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lead prompt missing inbound-attachments marker %q", want)
		}
	}
}

// TestResolveLeadPrompt_Behaviours pins the four branches of
// ResolveLeadPrompt: nil registry, missing project, missing
// swarm, and the happy path. Touched-surface rule applies to
// the whole prompt.go.
func TestResolveLeadPrompt_Behaviours(t *testing.T) {
	if p, r := ResolveLeadPrompt(nil, "anything"); p != "" || r != "" {
		t.Errorf("nil registry must return empty pair, got (%q,%q)", p, r)
	}
	if p, r := ResolveLeadPrompt(&registry.Registry{}, ""); p != "" || r != "" {
		t.Errorf("empty projectID must return empty pair, got (%q,%q)", p, r)
	}
}

// TestResolveLeadPrompt_LoadedRegistry exercises the rest of
// ResolveLeadPrompt's branches against a real loaded registry:
// happy path (returns lead role prompt + name), and the
// "project exists but swarm has no LeadRole" branch. Uses
// fixtures on disk because the Registry's mutation surface is
// package-private — Load() is the supported test entry point.
func TestResolveLeadPrompt_LoadedRegistry(t *testing.T) {
	tmpDir := t.TempDir()
	for _, subdir := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(tmpDir, subdir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", subdir, err)
		}
	}

	swarmYAML := "---\n" + `swarmId: "lead-swarm"
leadRole: "lead"
roles:
  - name: "lead"
    systemPrompt: "you are the lead"
    runtime:
      image: "img:latest"
  - name: "worker"
    runtime:
      image: "img:latest"
` + "---\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "swarms", "lead.md"), []byte(swarmYAML), 0o644); err != nil {
		t.Fatalf("write swarm: %v", err)
	}

	workflowYAML := "---\n" + `workflowId: "wf"
entrypoint: "start"
steps:
  start:
    type: "agent"
    prompt: "go"
    role: "lead"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
` + "---\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "workflows", "wf.md"), []byte(workflowYAML), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	projectYAML := `projectId: "led"
swarmId: "lead-swarm"
defaultWorkflowId: "wf"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "projects", "led.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project: %v", err)
	}

	reg := registry.New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("load: %v", err)
	}

	// Happy path.
	prompt, name := ResolveLeadPrompt(reg, "led")
	if prompt != "you are the lead" {
		t.Errorf("happy path: prompt = %q, want %q", prompt, "you are the lead")
	}
	if name != "lead" {
		t.Errorf("happy path: role name = %q, want %q", name, "lead")
	}

	// Missing project branch.
	if p, n := ResolveLeadPrompt(reg, "nonexistent"); p != "" || n != "" {
		t.Errorf("missing project: got (%q,%q), want empties", p, n)
	}
}
