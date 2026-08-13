package executor

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The aggregate budget for injected guidance blocks (LLD 09 §13.3(2)).
//
// This is the bound that binds. Per-block ceilings let N blocks each inside its own
// budget compose one large prompt — the exact failure the whole token-reduction line
// of work started from.
const injectedBlocksAggregateBudget = 3000

func TestInjectedBlocks_AggregateStaysBounded(t *testing.T) {
	total := injectedBlocksAggregateBytes()
	if total > injectedBlocksAggregateBudget {
		var b strings.Builder
		for _, blk := range injectedBlocks {
			b.WriteString("\n  ")
			b.WriteString(blk.Name)
			b.WriteString(": ")
			b.WriteString(itoa(len(blk.Text)))
		}
		t.Errorf("injected blocks total %d bytes, over the %d-byte budget.%s\n"+
			"A new block must fit the whole allowance, not just its own — adding blocks is "+
			"how a token-reduction feature turns into a token cost. Trim an existing block "+
			"rather than raising this bound.", total, injectedBlocksAggregateBudget, b.String())
	}
	t.Logf("injected guidance: %d/%d bytes used", total, injectedBlocksAggregateBudget)
}

// TestInjectedBlockRegistry_IsComplete is what makes the budget enforceable rather
// than advisory.
//
// The budget test sums the registry, so a block that is injected but NOT registered
// is invisible to it — the prompt grows and CI stays green. Absence would be read as
// a value, again. This reads the package source and requires every declared
// *SystemPromptBlock constant to appear in the registry, so the only way to add a
// block is to add it to the budget too.
func TestInjectedBlockRegistry_IsComplete(t *testing.T) {
	declared := declaredBlockConstants(t)
	if len(declared) == 0 {
		t.Fatal("found no *SystemPromptBlock constants in the package — the scan is broken, " +
			"which would make this law silently vacuous")
	}

	registered := map[string]bool{}
	for _, b := range injectedBlocks {
		registered[b.Const] = true
	}
	for _, name := range declared {
		if !registered[name] {
			t.Errorf("%s is declared but not in injectedBlocks. An unregistered block is "+
				"excluded from the aggregate budget, so the prompt can grow past %d bytes "+
				"with CI green. Add it to the registry with its advisory/invariant class.",
				name, injectedBlocksAggregateBudget)
		}
	}
	for _, b := range injectedBlocks {
		if b.Name == "" || b.Class == "" {
			t.Errorf("registry entry %+v is missing a name or class; the class decides what an "+
				"operator may suppress (LLD 09 §13.3(4))", b)
		}
		if b.Class != blockAdvisory && b.Class != blockInvariant {
			t.Errorf("block %q has class %q, which is neither advisory nor invariant", b.Name, b.Class)
		}
	}
}

// TestInjectedBlockRegistry_ReportingIntegrityIsInvariant pins the one classification
// that carries a rule rather than a hint. If this block were ever reclassified
// advisory, an operator could suppress being TOLD about a check that still runs — a
// deployment misdescribing a rule it is subject to.
func TestInjectedBlockRegistry_ReportingIntegrityIsInvariant(t *testing.T) {
	for _, b := range injectedBlocks {
		if b.Const == "claimVerificationSystemPromptBlock" {
			if b.Class != blockInvariant {
				t.Errorf("reporting integrity is classed %q; verifyRoleClaims runs whatever the "+
					"prompt says, so suppressing the block removes the warning and not the rule",
					b.Class)
			}
			return
		}
	}
	t.Error("reporting-integrity block is not registered at all")
}

var blockConstRE = regexp.MustCompile(`(?m)^\s*(?:const\s+)?(\w*SystemPromptBlock)\s*=`)

// declaredBlockConstants scans the package's own non-test sources for block constant
// declarations. Source-level because the property is about what EXISTS in the package,
// which no amount of runtime reflection over a registry can observe.
func declaredBlockConstants(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range blockConstRE.FindAllStringSubmatch(string(src), -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
