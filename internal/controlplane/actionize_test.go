package controlplane

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// Actionizer tests (LLD 2026-07-11-control-plane-actionable-proposals §4).
// Fixtures mirror the real deployed-tree shapes: workflow/swarm markdown with
// YAML frontmatter, project YAML, daemon config.yaml.

const wfFixture = `---
workflowId: dev-pipeline
steps:
  implement:
    type: agent
    role: coder
    timeout: "10m"
  review:
    type: agent
    role: reviewer
    timeout: "15m"
  untimed:
    type: agent
    role: coder
---

# Dev pipeline body
`

const swarmFixture = `---
swarmId: dev-swarm
roles:
  - name: coder
    model: "old-model"
  - name: reviewer
    model: "reviewer-model"
---

# Swarm body
`

const daemonCfgFixture = `mcp:
  servers:
    - name: scraper
      transport: sse
      timeout_seconds: 30
`

const projectCfgFixture = `projectId: "p1"
mcp:
  servers:
    - name: local-tool
      transport: stdio
      timeout_seconds: 20
`

func testActionizer(files map[string]string) *Actionizer {
	return &Actionizer{
		ReadFile: func(rel string) ([]byte, error) {
			s, ok := files[rel]
			if !ok {
				return nil, errors.New("not found: " + rel)
			}
			return []byte(s), nil
		},
		KnownModel: func(m string) bool { return m != "hallucinated-model" },
		Logger:     zerolog.Nop(),
	}
}

func stdFiles() map[string]string {
	return map[string]string{
		"configs/workflows/dev-pipeline.md": wfFixture,
		"configs/swarms/dev-swarm.md":       swarmFixture,
		"config.yaml":                       daemonCfgFixture,
		"configs/projects/p1.yaml":          projectCfgFixture,
	}
}

func TestCurrentStepTimeout(t *testing.T) {
	a := testActionizer(stdFiles())
	d, explicit, err := a.CurrentStepTimeout("dev-pipeline", "implement")
	if err != nil || !explicit || d != 10*time.Minute {
		t.Fatalf("got %v explicit=%v err=%v; want 10m explicit", d, explicit, err)
	}
	// Step without explicit timeout.
	_, explicit, err = a.CurrentStepTimeout("dev-pipeline", "untimed")
	if err != nil || explicit {
		t.Fatalf("untimed step: explicit=%v err=%v; want explicit=false", explicit, err)
	}
	// Missing step.
	if _, _, err := a.CurrentStepTimeout("dev-pipeline", "ghost"); err == nil {
		t.Fatal("missing step must error")
	}
	// Missing workflow file.
	if _, _, err := a.CurrentStepTimeout("nope", "implement"); err == nil {
		t.Fatal("missing workflow must error")
	}
}

func TestRenderStepTimeout_Applyable(t *testing.T) {
	a := testActionizer(stdFiles())
	// p95 24m → suggested 36m, clamp max(5m, 2×10m)=20m → 20m, > current.
	rc, err := a.RenderStepTimeout("dev-pipeline", "implement", 36*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rc.ApplyTarget != "configs/workflows/dev-pipeline.md" {
		t.Fatalf("target: %s", rc.ApplyTarget)
	}
	if !strings.Contains(rc.ApplyContent, "20m") {
		t.Fatalf("content must carry the clamped 20m timeout:\n%s", rc.ApplyContent)
	}
	if !strings.Contains(rc.ApplyContent, `timeout: "15m"`) {
		t.Fatal("other steps must be untouched")
	}
	if !strings.Contains(rc.ApplyContent, "# Dev pipeline body") {
		t.Fatal("markdown body must be preserved")
	}
	if rc.BaseHash == "" || rc.Diff == "" || rc.Summary == "" {
		t.Fatalf("BaseHash/Diff/Summary must be filled: %+v", rc)
	}
	if !rc.Clamped {
		t.Fatal("clamp to 2×current must be reported")
	}
	if rc.BlastRadius != "project" {
		t.Fatalf("blast radius: %s", rc.BlastRadius)
	}
}

// TestRenderStepTimeout_InclusiveBoundaryConstantsHold pins the bounds table:
// suggested ≤ current is not useful; absolute cap applies.
func TestRenderStepTimeout_BoundsAndNoOp(t *testing.T) {
	a := testActionizer(stdFiles())
	// Suggested below current → ErrChangeNotUseful.
	if _, err := a.RenderStepTimeout("dev-pipeline", "implement", 5*time.Minute); !errors.Is(err, ErrChangeNotUseful) {
		t.Fatalf("below current: want ErrChangeNotUseful, got %v", err)
	}
	// Equal to current → ErrChangeNotUseful (no-op guard).
	if _, err := a.RenderStepTimeout("dev-pipeline", "implement", 10*time.Minute); !errors.Is(err, ErrChangeNotUseful) {
		t.Fatalf("equal: want ErrChangeNotUseful, got %v", err)
	}
	// Absolute cap: current 15m, suggested enormous → clamped to
	// min(MaxSuggestedStepTimeout=2h default, max(5m,2×15m)=30m) = 30m.
	rc, err := a.RenderStepTimeout("dev-pipeline", "review", 100*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rc.ApplyContent, "30m") || !rc.Clamped {
		t.Fatalf("want 30m clamped, got clamped=%v content:\n%s", rc.Clamped, rc.ApplyContent)
	}
	// Step with no explicit timeout → error (informational branch upstream).
	if _, err := a.RenderStepTimeout("dev-pipeline", "untimed", 30*time.Minute); err == nil {
		t.Fatal("absent explicit timeout must error")
	}
}

func TestRenderStepTimeout_ValidatorGate(t *testing.T) {
	a := testActionizer(stdFiles())
	a.ValidateWorkflow = func(_ string, _ []byte) error { return errors.New("parse fail") }
	if _, err := a.RenderStepTimeout("dev-pipeline", "implement", 36*time.Minute); err == nil {
		t.Fatal("validator rejection must abort the render")
	}
}

func TestRenderRoleModel(t *testing.T) {
	a := testActionizer(stdFiles())
	rc, err := a.RenderRoleModel("dev-swarm", "coder", "new-model")
	if err != nil {
		t.Fatal(err)
	}
	if rc.ApplyTarget != "configs/swarms/dev-swarm.md" {
		t.Fatalf("target: %s", rc.ApplyTarget)
	}
	if !strings.Contains(rc.ApplyContent, "new-model") || !strings.Contains(rc.ApplyContent, "reviewer-model") {
		t.Fatalf("edit must touch only the matched role:\n%s", rc.ApplyContent)
	}
	if rc.BlastRadius != "swarm" {
		t.Fatalf("blast radius: %s", rc.BlastRadius)
	}
	// Unknown model refused.
	if _, err := a.RenderRoleModel("dev-swarm", "coder", "hallucinated-model"); err == nil {
		t.Fatal("unknown model must be refused")
	}
	// Same model → no-op.
	if _, err := a.RenderRoleModel("dev-swarm", "coder", "old-model"); !errors.Is(err, ErrChangeNotUseful) {
		t.Fatalf("same model: want ErrChangeNotUseful, got %v", err)
	}
	// Missing role.
	if _, err := a.RenderRoleModel("dev-swarm", "ghost", "new-model"); err == nil {
		t.Fatal("missing role must error")
	}
}

func TestRenderMCPServerTimeout(t *testing.T) {
	a := testActionizer(stdFiles())
	// Daemon scope.
	rc, err := a.RenderMCPServerTimeout("", "scraper", 90)
	if err != nil {
		t.Fatal(err)
	}
	if rc.ApplyTarget != "config.yaml" || rc.BlastRadius != "daemon" || !rc.LiveApply {
		t.Fatalf("daemon scope wrong: %+v", rc)
	}
	if !strings.Contains(rc.ApplyContent, "timeout_seconds: 90") {
		t.Fatalf("content:\n%s", rc.ApplyContent)
	}
	// Project scope.
	rc, err = a.RenderMCPServerTimeout("p1", "local-tool", 60)
	if err != nil {
		t.Fatal(err)
	}
	if rc.ApplyTarget != "configs/projects/p1.yaml" || rc.BlastRadius != "project" {
		t.Fatalf("project scope wrong: %+v", rc)
	}
	// Raise-only: suggested ≤ current.
	if _, err := a.RenderMCPServerTimeout("", "scraper", 30); !errors.Is(err, ErrChangeNotUseful) {
		t.Fatalf("raise-only: want ErrChangeNotUseful, got %v", err)
	}
	// Unknown server.
	if _, err := a.RenderMCPServerTimeout("", "ghost", 90); err == nil {
		t.Fatal("unknown server must error")
	}
}

func TestFindMCPServerScope_DaemonFirst(t *testing.T) {
	files := stdFiles()
	// Same server name at both scopes: daemon must win (design §4.4).
	files["configs/projects/p1.yaml"] = "projectId: \"p1\"\nmcp:\n  servers:\n    - name: scraper\n      timeout_seconds: 10\n"
	a := testActionizer(files)
	scope, ok := a.FindMCPServerScope("p1", "scraper")
	if !ok || scope != "" {
		t.Fatalf("daemon-first: got scope=%q ok=%v; want daemon (empty scope)", scope, ok)
	}
	// Project-only server resolves to the project.
	files["configs/projects/p1.yaml"] = projectCfgFixture
	scope, ok = a.FindMCPServerScope("p1", "local-tool")
	if !ok || scope != "p1" {
		t.Fatalf("project resolution: got scope=%q ok=%v", scope, ok)
	}
	// Unknown anywhere.
	if _, ok := a.FindMCPServerScope("p1", "ghost"); ok {
		t.Fatal("unknown server must not resolve")
	}
}

func TestParseMCPToolName(t *testing.T) {
	if s, tool, ok := ParseMCPToolName("mcp__scraper__web_fetch"); !ok || s != "scraper" || tool != "web_fetch" {
		t.Fatalf("got %q %q %v", s, tool, ok)
	}
	if _, _, ok := ParseMCPToolName("file_read"); ok {
		t.Fatal("builtin tool must not parse as MCP")
	}
}

func TestRenderedChangeSizeCap(t *testing.T) {
	files := stdFiles()
	big := strings.Repeat("# pad\n", 20000) // > 64KiB body
	files["configs/workflows/dev-pipeline.md"] = strings.Replace(wfFixture, "# Dev pipeline body\n", big, 1)
	a := testActionizer(files)
	if _, err := a.RenderStepTimeout("dev-pipeline", "implement", 36*time.Minute); err == nil {
		t.Fatal("oversize rendered content must be refused")
	}
}

// LLM-selected identifiers must never steer a rel path (defence in depth on
// top of the container ReadFile prefix guard and the apply engine's
// resolveTarget).
func TestRenderers_RejectPathTraversalIdents(t *testing.T) {
	a := testActionizer(stdFiles())
	if _, _, err := a.CurrentStepTimeout("../secrets", "implement"); err == nil {
		t.Fatal("dot-dot workflow id must be rejected")
	}
	if _, err := a.RenderStepTimeout("a/b", "implement", 20*time.Minute); err == nil {
		t.Fatal("slash workflow id must be rejected")
	}
	if _, err := a.RenderRoleModel("..\\swarm", "coder", "new-model"); err == nil {
		t.Fatal("backslash swarm id must be rejected")
	}
	if _, err := a.RenderMCPServerTimeout("../p1", "scraper", 90); err == nil {
		t.Fatal("dot-dot project id must be rejected")
	}
	if err := a.RevalidateChange("janka", `{"change":{"kind":"swarm_role_model","swarm":"../x","role":"coder","model":"new-model"}}`); err == nil {
		t.Fatal("revalidate must reject traversal idents too")
	}
	// A traversal ident inside FindMCPServerScope's project probe must not
	// resolve (and must not read outside the tree).
	if _, ok := a.FindMCPServerScope("../p1", "local-tool"); ok {
		t.Fatal("traversal project id must not resolve a scope")
	}
}
