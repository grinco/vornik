package contractreg

import (
	"bytes"
	"fmt"
	"os"
	"testing"
)

// TestDumpUnassertedDoubles is an operator aid, not a gate: run it with
// -run TestDumpUnassertedDoubles to regenerate the allowlist after a cleanup.
func TestDumpUnassertedDoubles(t *testing.T) {
	if os.Getenv("DUMP_REPO_DOUBLES") == "" {
		t.Skip("set DUMP_REPO_DOUBLES=1 to regenerate the allowlist")
	}
	a, err := AuditRepoDoubles(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	n, permissive := 0, 0
	for _, d := range a.Doubles {
		if a.Asserted[d.Package][d.Key] {
			continue
		}
		fmt.Fprintln(&buf, d.ID())
		n++
		if d.Permissive {
			permissive++
		}
	}
	t.Logf("permissive (looser than production): %d", permissive)
	if err := os.WriteFile(os.Getenv("DUMP_REPO_DOUBLES"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("doubles=%d unasserted=%d", len(a.Doubles), n)
}
