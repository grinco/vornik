package contractreg

import (
	"strings"
	"testing"
)

// UngatedByDesign's own doc comment says exemptions are "encoded as DATA with a
// reason rather than as inline string comparisons in the shell". The execution
// gate does exactly that anyway:
//
//	entrypoint.sh:  [ "$name" != "tool_search" ] && [ "$name" != "tool_result_read" ] \
//	                  && is_builtin_tool "$name" && ! builtin_tool_allowed "$name"
//
// So the exemption vocabulary lives in TWO places and nothing compared them.
// Adding a third name to the Go map would not exempt it at runtime; removing one
// from the map would leave it exempt in the shell. That is a fifth instance of
// the "several hand-maintained registries of one vocabulary" fault that the
// 2026.8.1 allowlist-bypass fix introduced contractreg to end — sitting inside
// the fix itself.
//
// This closes it the same way the original did: extract the shell's list and
// fail when the two disagree, in either direction.
func TestCheckUngatedExemptionAgreement(t *testing.T) {
	t.Run("agreement is silent", func(t *testing.T) {
		tbl := New()
		for name := range UngatedByDesign {
			tbl.Add(KindAgentToolInlineExempt, name, "entrypoint.sh:1705")
		}
		if f := CheckUngatedExemptionAgreement(tbl); len(f) != 0 {
			t.Errorf("matching lists must produce no findings, got %+v", f)
		}
	})

	t.Run("shell exempts a name the registry does not", func(t *testing.T) {
		tbl := New()
		for name := range UngatedByDesign {
			tbl.Add(KindAgentToolInlineExempt, name, "entrypoint.sh:1705")
		}
		tbl.Add(KindAgentToolInlineExempt, "run_shell", "entrypoint.sh:1705")
		findings := CheckUngatedExemptionAgreement(tbl)
		if len(findings) == 0 {
			t.Fatal("a shell-only exemption is an UNREVIEWED allowlist bypass and must fail")
		}
		joined := findingText(findings)
		if !strings.Contains(joined, "run_shell") {
			t.Errorf("the finding must name the offending tool; got %q", joined)
		}
	})

	t.Run("registry exempts a name the shell does not", func(t *testing.T) {
		tbl := New()
		for name := range UngatedByDesign {
			tbl.Add(KindAgentToolInlineExempt, name, "entrypoint.sh:1705")
		}
		// Drop one: the registry promises an exemption the runtime does not honour.
		tbl2 := New()
		first := true
		for name := range UngatedByDesign {
			if first {
				first = false
				continue
			}
			tbl2.Add(KindAgentToolInlineExempt, name, "entrypoint.sh:1705")
		}
		if len(CheckUngatedExemptionAgreement(tbl2)) == 0 {
			t.Error("a registry exemption the shell does not honour must also fail — the " +
				"drift is a bug in either direction, and this one silently gates a tool " +
				"the design says must stay reachable")
		}
	})

	t.Run("no inline exemptions extracted at all", func(t *testing.T) {
		// Guards the check going inert: if the regex stops matching, an empty
		// table would otherwise look like perfect agreement with an empty shell.
		if len(CheckUngatedExemptionAgreement(New())) == 0 {
			t.Error("extracting zero inline exemptions must fail — the parse broke, and a " +
				"check that silently compares nothing reads as passing")
		}
	})
}

func findingText(fs []Finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(f.String())
		b.WriteString(" ")
	}
	return b.String()
}
