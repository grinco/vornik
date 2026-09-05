package contractreg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/pipeline"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const pipelineImport = `import "vornik.io/vornik/internal/pipeline"`

func TestPointIdent_MatchesTheDeclaredVariableNames(t *testing.T) {
	want := map[string]string{
		"dispatcher.pre_tool": "DispatcherPreTool", "dispatcher.post_tool": "DispatcherPostTool",
		"dispatcher.continuation": "DispatcherContinuation", "executor.step_outcome": "ExecutorStepOutcome",
	}
	for _, p := range pipeline.Points {
		if got := pointIdent(p.Name); got != want[p.Name] {
			t.Errorf("%s → %s, want %s", p.Name, got, want[p.Name])
		}
	}
}

func TestAuditPipelineConstructions_FindsGenericAndPlainCalls(t *testing.T) {
	root := writeTree(t, map[string]string{
		"internal/a/a.go":           "package a\n" + pipelineImport + "\nvar c = pipeline.NewDecide[int](pipeline.DispatcherPreTool, nil)\nvar d = pipeline.NewAround[int, string](pipeline.DispatcherPostTool, nil)\n",
		"internal/b/b.go":           "package b\nimport pl \"vornik.io/vornik/internal/pipeline\"\nvar p = pl.DispatcherContinuation\nvar c = pl.NewDecide[int](p, nil)\n",
		"internal/c/c_test.go":      "package c\n" + pipelineImport + "\nvar c = pipeline.NewDecide[int](pipeline.ExecutorStepOutcome, nil)\n",
		"internal/d/d.generated.go": "package d\n" + pipelineImport + "\nvar c = pipeline.NewDecide[int](pipeline.ExecutorStepOutcome, nil)\n",
		"internal/e/e.go":           "package e\nfunc NewDecide() {}\n",
	})
	cons, err := AuditPipelineConstructions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cons) != 4 {
		t.Fatalf("want 4 constructions (generated file skipped, unrelated NewDecide ignored), got %+v", cons)
	}
	byPkg := map[string][]PipelineConstruction{}
	for _, c := range cons {
		byPkg[c.Package] = append(byPkg[c.Package], c)
	}
	if a := byPkg["internal/a"]; len(a) != 2 || a[0].Point != "dispatcher.pre_tool" || a[0].Mode != pipeline.ModeDecide || a[1].Point != "dispatcher.post_tool" || a[1].Constructor != "NewAround" {
		t.Errorf("package a: %+v", a)
	}
	if b := byPkg["internal/b"]; len(b) != 1 || !b[0].Unresolvable {
		t.Errorf("a point passed through a variable must be unresolvable: %+v", b)
	}
	if c := byPkg["internal/c"]; len(c) != 1 || !c[0].Test || c[0].Point != "executor.step_outcome" {
		t.Errorf("test file: %+v", c)
	}
}

func TestCheckPipelinePoints_Findings(t *testing.T) {
	cons := []PipelineConstruction{
		{Constructor: "NewDecide", Mode: pipeline.ModeDecide, Point: "dispatcher.pre_tool", Package: "internal/x", Source: "internal/x/x.go:1"},
		{Constructor: "NewDecide", Mode: pipeline.ModeDecide, Point: "dispatcher.pre_tool", Package: "internal/y", Source: "internal/y/y.go:1"},
		{Constructor: "NewObserve", Mode: pipeline.ModeObserve, Point: "dispatcher.post_tool", Package: "internal/x", Source: "internal/x/x.go:2"},
		{Constructor: "NewAround", Mode: pipeline.ModeAround, Unresolvable: true, Package: "internal/x", Source: "internal/x/x.go:3"},
		{Constructor: "NewDecide", Mode: pipeline.ModeDecide, Point: "executor.step_outcome", Package: "internal/z", Source: "internal/z/z_test.go:1", Test: true},
	}
	allow := map[string]string{"dispatcher.continuation": "step 3 pending", "dispatcher.pre_tool": "stale"}
	got := CheckPipelinePoints(cons, allow)
	details := make([]string, 0, len(got))
	for _, f := range got {
		details = append(details, f.Name+": "+f.Detail)
	}
	joined := strings.Join(details, "\n")
	for _, want := range []string{
		"NewAround: point argument is not a literal pipeline.<Name> selector",
		"dispatcher.post_tool: NewObserve constructs \"dispatcher.post_tool\", which is declared around",
		"dispatcher.pre_tool: listed in the pipeline-point allowlist as not yet constructed, but it is",
		"dispatcher.pre_tool: constructed in more than one package (internal/x, internal/y)",
		"executor.step_outcome: declared in pipeline.Points but constructed nowhere outside tests",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing finding %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "dispatcher.continuation:") {
		t.Errorf("an allowlisted unconstructed point is not a finding:\n%s", joined)
	}
	if len(got) != 5 {
		t.Errorf("want exactly 5 findings, got %d:\n%s", len(got), joined)
	}
}

func TestParsePipelinePointAllowlist(t *testing.T) {
	got, err := ParsePipelinePointAllowlist("# c\n\ndispatcher.pre_tool # step 3\nexecutor.step_outcome   #  step 4\n")
	if err != nil || len(got) != 2 || got["executor.step_outcome"] != "step 4" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"dispatcher.pre_tool\n", "dispatcher.pre_tool # a\ndispatcher.pre_tool # b\n", "made.up # x\n"} {
		if _, err := ParsePipelinePointAllowlist(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// The real tree: every construction resolvable, modes agree, no point
// constructed in two packages, and every declared point either constructed
// or carried by the shrink-only allowlist while the conversion is in flight.
func TestCheckPipelinePoints_RealTree(t *testing.T) {
	root := filepath.Join("..", "..")
	cons, err := AuditPipelineConstructions(root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "cmd", "lint-lld-contracts", "pipeline_point_allowlist.txt"))
	allow := map[string]string{}
	if err == nil {
		if allow, err = ParsePipelinePointAllowlist(string(data)); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range CheckPipelinePoints(cons, allow) {
		t.Errorf("%s: %s %v", f.Name, f.Detail, f.Sources)
	}
}
