package contractreg

import (
	"strconv"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agenttools"
)

func findingsText(fs []Finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(f.Name + ": " + f.Detail + "\n")
	}
	return b.String()
}

// The real tree must agree with itself: every RuntimeHelper tool has a handler
// and no bash case, the generated list matches the declaration, and the
// branch sits past the gate.
func TestHelperDispatch_RealTreeAgrees(t *testing.T) {
	tbl := New()
	tbl.AddAgentToolsGo()
	tbl.AddAgentLoopHandlers(agenttools.HelperNames()) // the real handler set is asserted equal in agentloop's own test
	if err := tbl.AddEntrypointSurfaces(realEntrypoint(t)); err != nil {
		t.Fatal(err)
	}
	if fs := CheckHelperDispatchAgreement(tbl); len(fs) != 0 {
		t.Errorf("real tree:\n%s", findingsText(fs))
	}
	if fs := CheckHelperBranchIsGated(tbl); len(fs) != 0 {
		t.Errorf("real tree:\n%s", findingsText(fs))
	}
	if tbl.Get(KindAgentToolAdvertisementFilter, "helper-list-present") == nil {
		t.Error("the generated registry must carry HELPER_TOOL_NAMES_JSON")
	}
}

func TestCheckHelperDispatchAgreement_Disagreements(t *testing.T) {
	// The findings are keyed on the declaration, so the fixture uses real
	// names in their declared runtime and fakes the other two views around
	// them: every helper tool has a handler except read_many_files; grep also
	// still has a bash case; run_shell (RuntimeShell) has a stray handler; the
	// generated list lacks glob and carries a name nobody declared.
	tbl := New()
	tbl.Add(KindAgentToolDispatch, "run_shell", "e.sh:1") // a shell tool's bash case: fine
	tbl.Add(KindAgentToolDispatch, "grep", "e.sh:2")
	handlers := []string{"run_shell", "not_a_tool"}
	for _, n := range agenttools.HelperNames() {
		if n != "read_many_files" {
			handlers = append(handlers, n)
		}
	}
	tbl.AddAgentLoopHandlers(handlers)
	for _, n := range agenttools.HelperNames() {
		if n != "glob" {
			tbl.Add(KindAgentToolHelperListed, n, "reg:HELPER")
		}
	}
	tbl.Add(KindAgentToolHelperListed, "ghost_tool", "reg:HELPER")
	got := findingsText(CheckHelperDispatchAgreement(tbl))
	for _, want := range []string{
		"grep: double dispatch",
		"read_many_files: declared RuntimeHelper but internal/agentloop.Handlers has no entry",
		"run_shell: internal/agentloop.Handlers implements it but the declaration says RuntimeShell",
		"not_a_tool: internal/agentloop.Handlers implements a name the declaration does not know",
		"glob: HELPER_TOOL_NAMES_JSON lists it: false; the declaration says RuntimeHelper: true",
		"ghost_tool: HELPER_TOOL_NAMES_JSON carries a name the declaration does not know",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "file_read:") {
		t.Errorf("a helper tool with a handler, no bash case and a listing is not a finding:\n%s", got)
	}
}

func branchTable(gate, fi, branch, caseAt int, twice bool) *Table {
	tbl := New()
	tbl.Add(KindAgentToolAdvertisementFilter, "helper-list-present", "reg")
	add := func(name string, line int) {
		if line > 0 {
			tbl.AddWithStatus(KindAgentToolHelperBranch, name, "e.sh", itoa(line))
		}
	}
	add("gate", gate)
	add("gate-end", fi)
	add("helper-branch#1", branch)
	add("case", caseAt)
	if twice {
		add("helper-branch#2", caseAt+5)
	}
	return tbl
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestCheckHelperBranchIsGated_Shapes(t *testing.T) {
	cases := []struct {
		name                     string
		gate, fi, branch, caseAt int
		twice                    bool
		wantFinding              bool
		wantDetail               string
	}{
		{"accepted shape", 10, 20, 24, 28, false, false, ""},
		{"branch before the gate closes", 10, 20, 15, 28, false, true, "AFTER the gate's fi"},
		{"branch after the case", 10, 20, 30, 28, false, true, "BEFORE the case"},
		{"branch on the gate's line", 10, 20, 10, 28, false, true, "each on its own line"},
		{"branch twice", 10, 20, 24, 28, true, true, "more than once"},
		{"no branch at all", 10, 20, 0, 28, false, true, "never consults tool_runs_in_helper"},
		{"gate not found", 0, 20, 24, 28, false, true, "could not locate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := CheckHelperBranchIsGated(branchTable(c.gate, c.fi, c.branch, c.caseAt, c.twice))
			if (len(fs) > 0) != c.wantFinding {
				t.Fatalf("findings = %v, want finding=%t", findingsText(fs), c.wantFinding)
			}
			if c.wantFinding && !strings.Contains(findingsText(fs), c.wantDetail) {
				t.Errorf("want %q in:\n%s", c.wantDetail, findingsText(fs))
			}
		})
	}
	if fs := CheckHelperBranchIsGated(New()); len(fs) != 0 {
		t.Errorf("a tree with no helper mechanism has nothing to order: %v", findingsText(fs))
	}
}
