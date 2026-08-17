package controlplane

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
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
	// Regression T-eca0: workflow definitions are registry-global. A proposal
	// derived from vornik-marketing's 2s route p95 changed adaptive.route for
	// assistant too, but was labelled project-scoped and required no daemon
	// acknowledgement. Every workflow edit must disclose daemon scope.
	if rc.BlastRadius != persistence.ProposalScopeDaemon {
		t.Fatalf("blast radius: %s, want daemon for a shared workflow file", rc.BlastRadius)
	}
}

func TestRenderStepTimeoutReduction_SharedWorkflowHasDaemonBlastRadius(t *testing.T) {
	a := testActionizer(stdFiles())
	rc, err := a.RenderStepTimeoutReduction("dev-pipeline", "implement", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rc.BlastRadius != persistence.ProposalScopeDaemon {
		t.Fatalf("blast radius: %s, want daemon for a shared workflow file", rc.BlastRadius)
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

func TestRenderRoleEnv(t *testing.T) {
	swarm := `swarmId: "assistant-swarm"
roles:
    - name: "researcher"
      runtime:
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "25"
    - name: "writer"
      runtime:
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "50"
`
	a := testActionizer(map[string]string{"configs/swarms/assistant-swarm.md": swarm})

	rc, err := a.RenderRoleEnv("assistant-swarm", "researcher", "VORNIK_STEP_PROMPT_TOKEN_BUDGET", "900000")
	if err != nil {
		t.Fatalf("RenderRoleEnv: %v", err)
	}
	if rc.ApplyTarget != "configs/swarms/assistant-swarm.md" {
		t.Errorf("ApplyTarget = %q", rc.ApplyTarget)
	}
	if rc.BlastRadius != persistence.ProposalScopeSwarm {
		t.Errorf("BlastRadius = %q, want swarm", rc.BlastRadius)
	}
	if !rc.LiveApply {
		t.Errorf("LiveApply = false, want true (env injected at container start; non-disruptive)")
	}
	if !strings.Contains(rc.ApplyContent, `VORNIK_STEP_PROMPT_TOKEN_BUDGET: "900000"`) {
		t.Errorf("ApplyContent missing the new key:\n%s", rc.ApplyContent)
	}
	if rc.BaseHash == "" || rc.Diff == "" {
		t.Errorf("finishRender did not stamp BaseHash/Diff")
	}

	// Setting an already-present value is a no-op → ErrChangeNotUseful.
	if _, err := a.RenderRoleEnv("assistant-swarm", "researcher", "VORNIK_MAX_TOOL_ITERATIONS", "25"); !errors.Is(err, ErrChangeNotUseful) {
		t.Errorf("no-op render err = %v, want ErrChangeNotUseful", err)
	}
	// Unknown role errors.
	if _, err := a.RenderRoleEnv("assistant-swarm", "ghost", "K", "v"); err == nil {
		t.Error("unknown role must error")
	}
}

// The apply engine re-validates a proposal's typed change before writing; the
// swarm_role_env kind must be in the allowlist (live apply bug 2026-07-21: the
// default branch rejected it and every apply auto-rolled-back).
func TestRevalidateChange_RoleEnv(t *testing.T) {
	// Real SWARM.md files carry a --- frontmatter fence (revalidate reads it via
	// config.EditFrontmatter, like revalidateRoleModel).
	swarm := `---
swarmId: "assistant-swarm"
roles:
    - name: "researcher"
      runtime:
        envVars:
            VORNIK_MAX_TOOL_ITERATIONS: "25"
---
`
	a := testActionizer(map[string]string{"configs/swarms/assistant-swarm.md": swarm})

	ev := `{"change":{"kind":"swarm_role_env","swarm":"assistant-swarm","role":"researcher","key":"VORNIK_STEP_PROMPT_TOKEN_BUDGET","value":"900000"}}`
	if err := a.RevalidateChange("", ev); err != nil {
		t.Errorf("valid swarm_role_env revalidation failed: %v", err)
	}
	evGhost := `{"change":{"kind":"swarm_role_env","swarm":"assistant-swarm","role":"ghost","key":"K","value":"v"}}`
	if err := a.RevalidateChange("", evGhost); err == nil {
		t.Error("swarm_role_env for a vanished role should fail revalidation")
	}
}

// TestRefuseTradingTarget covers the applier-side defense-in-depth trading
// refusal — the mirror of the detector-side exclusion at the ApplyEngine
// ValidateChange seam. Regression: companion finding review-20260721-a7bf #6
// ("trading-path exclusion is stated twice but never enforced at the seam").
func TestRefuseTradingTarget(t *testing.T) {
	// Faithful copy of service.isTradingSwarm (unexported there); the wiring
	// test in the service package asserts newActionizer injects the real one.
	trading := func(s string) bool {
		ls := strings.ToLower(s)
		return strings.Contains(ls, "trader") || strings.Contains(ls, "broker") || strings.Contains(ls, "trading")
	}
	const det = costQualityDetectorProposedBy
	envFor := func(swarm string) string {
		return `{"change":{"kind":"swarm_role_env","swarm":"` + swarm + `","role":"researcher","key":"K","value":"v"}}`
	}
	// Capture logs so the refusal-branch WARN (the high-severity event) is pinned,
	// not merely present (review-20260724-24a2 #3).
	var refBuf bytes.Buffer
	a := &Actionizer{IsTradingSwarm: trading, Logger: zerolog.New(&refBuf)}

	// Detector proposal targeting a trading swarm → refused with the sentinel.
	for _, sw := range []string{"ibkr-trader-swarm", "trading-swarm", "BROKER-swarm"} {
		if err := a.RefuseTradingTarget(det, envFor(sw)); !errors.Is(err, ErrTradingSwarmRefused) {
			t.Errorf("detector proposal to trading swarm %q must be refused, got %v", sw, err)
		}
	}
	if !strings.Contains(refBuf.String(), "refusing to apply") {
		t.Errorf("an actual refusal must emit a WARN, got logs: %q", refBuf.String())
	}
	// Detector proposal to a non-trading swarm → passes (the happy path).
	if err := a.RefuseTradingTarget(det, envFor("assistant-swarm")); err != nil {
		t.Errorf("detector proposal to non-trading swarm must pass, got %v", err)
	}
	// Non-detector origin → never blocked (operator/other paths, D4).
	for _, pb := range []string{"self-heal", "tune-detector", "operator", ""} {
		if err := a.RefuseTradingTarget(pb, envFor("ibkr-trader-swarm")); err != nil {
			t.Errorf("non-detector origin %q must not be blocked, got %v", pb, err)
		}
	}
	// Kind-agnostic within detector-origin (D4): swarm_role_model to trading → refused.
	modelEv := `{"change":{"kind":"swarm_role_model","swarm":"trading-swarm","role":"strategist","model":"m"}}`
	if err := a.RefuseTradingTarget(det, modelEv); !errors.Is(err, ErrTradingSwarmRefused) {
		t.Errorf("detector swarm_role_model to trading swarm must be refused, got %v", err)
	}
	// Change carrying no swarm (e.g. workflow_step_timeout) → passes (swarm-targeted).
	noSwarm := `{"change":{"kind":"workflow_step_timeout","workflow":"wf","step":"s","timeout":"30s"}}`
	if err := a.RefuseTradingTarget(det, noSwarm); err != nil {
		t.Errorf("change with empty swarm must pass, got %v", err)
	}
	// Empty / malformed / change-less evidence → passes.
	for _, ev := range []string{"", "   ", "not json", `{"foo":1}`, `{"change":null}`} {
		if err := a.RefuseTradingTarget(det, ev); err != nil {
			t.Errorf("evidence %q must pass, got %v", ev, err)
		}
	}
	// Nil classifier → fail-open (nil) AND a WARN is emitted (D3 observability).
	var buf bytes.Buffer
	aNil := &Actionizer{IsTradingSwarm: nil, Logger: zerolog.New(&buf)}
	if err := aNil.RefuseTradingTarget(det, envFor("ibkr-trader-swarm")); err != nil {
		t.Errorf("nil classifier must fail-open (nil), got %v", err)
	}
	if !strings.Contains(buf.String(), "classifier unwired") {
		t.Errorf("nil classifier must emit a WARN, got logs: %q", buf.String())
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

// TestRenderStepTimeoutReduction — the reclaim-capacity path lowers an
// over-provisioned timeout, mirrors the raise path's no-op guard, floors
// at 30s, and requires an explicit timeout. Backlog item 5.
func TestRenderStepTimeoutReduction(t *testing.T) {
	a := testActionizer(stdFiles())

	// implement has a 10m timeout; suggest 5m → a real reduction.
	rc, err := a.RenderStepTimeoutReduction("dev-pipeline", "implement", 5*time.Minute)
	if err != nil {
		t.Fatalf("reduction should apply: %v", err)
	}
	if !strings.Contains(rc.ApplyContent, `timeout: "5m"`) {
		t.Fatalf("content must carry the reduced 5m timeout:\n%s", rc.ApplyContent)
	}
	if !strings.Contains(rc.ApplyContent, `timeout: "15m"`) {
		t.Fatal("other steps must be untouched")
	}

	// Suggested >= current → not a reduction → ErrChangeNotUseful.
	if _, err := a.RenderStepTimeoutReduction("dev-pipeline", "implement", 10*time.Minute); !errors.Is(err, ErrChangeNotUseful) {
		t.Fatalf("equal-to-current: want ErrChangeNotUseful, got %v", err)
	}
	if _, err := a.RenderStepTimeoutReduction("dev-pipeline", "implement", 20*time.Minute); !errors.Is(err, ErrChangeNotUseful) {
		t.Fatalf("above current: want ErrChangeNotUseful, got %v", err)
	}

	// Sub-floor suggestion is clamped up to 30s (still < 10m → applies).
	rc, err = a.RenderStepTimeoutReduction("dev-pipeline", "implement", 5*time.Second)
	if err != nil {
		t.Fatalf("floored reduction should apply: %v", err)
	}
	if !strings.Contains(rc.ApplyContent, `timeout: "30s"`) || !rc.Clamped {
		t.Fatalf("want 30s floor clamped, got clamped=%v content:\n%s", rc.Clamped, rc.ApplyContent)
	}

	// No explicit timeout → nothing to reduce → error.
	if _, err := a.RenderStepTimeoutReduction("dev-pipeline", "untimed", 30*time.Second); err == nil {
		t.Fatal("absent explicit timeout must error")
	}
}
