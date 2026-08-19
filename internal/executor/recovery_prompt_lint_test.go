package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A swarm that ships RECOVERY MODE instructions must tell the lead not to go
// looking for the artifact whose absence put it there.
//
// Measured on the recovery-probe fixture, 2026-08-19. The block already said
// "Do NOT retry the failed step yourself, do NOT spawn role steps, do NOT write
// artifacts" — but nothing about READING. So the lead took failure_reason, saw a
// path in it, and tried to file_read that path to understand the failure. Two
// consequences, both measured:
//
//   - Before the agent-side guard fix, that tripped the missing-prerequisite
//     bail and killed the hop outright — 7 of 20 recover attempts.
//   - After suppressing the guard, the lead simply kept hunting instead and
//     exhausted its iteration cap — iteration_exhausted 0 → 5 of 20. The guard
//     fix converted the failure rather than removing it.
//
// The agent-side suppression lets the lead continue; only the prompt can tell it
// to stop looking. The two halves are complementary, which is why this lint
// exists next to the guard rather than instead of it.
//
// Config-time, so a future edit that drops the instruction fails here rather
// than showing up as iteration exhaustion in a benchmark weeks later.
func TestRecoveryModePromptForbidsHuntingTheMissingArtifact(t *testing.T) {
	repoRoot := repoRootFromTest(t)
	swarmDir := filepath.Join(repoRoot, "configs", "swarms")
	entries, err := os.ReadDir(swarmDir)
	if err != nil {
		t.Fatalf("read %s: %v", swarmDir, err)
	}

	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(swarmDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		body := string(raw)
		if !strings.Contains(body, "RECOVERY MODE") {
			continue // swarm ships no recovery instructions
		}
		checked++
		t.Run(e.Name(), func(t *testing.T) {
			// The instruction must forbid investigating the filesystem. Accepting
			// several phrasings on purpose — this pins the REQUIREMENT, not one
			// operator's wording.
			lower := strings.ToLower(body)
			forbidsReading := strings.Contains(lower, "do not read") ||
				strings.Contains(lower, "do not investigate") ||
				strings.Contains(lower, "do not go looking") ||
				strings.Contains(lower, "do not search for")
			if !forbidsReading {
				t.Errorf("RECOVERY MODE block does not tell the lead to stop looking for "+
					"the missing artifact. It reads failure_reason, finds a path, and hunts "+
					"it — which cost 7 of 20 recover hops to the missing-prerequisite bail, "+
					"then 5 of 20 to iteration exhaustion once that bail was suppressed. "+
					"Add an explicit instruction that context.recovery is sufficient (%s)",
					e.Name())
			}
			// And it must say WHY, so the instruction survives a future edit that
			// cannot see the reasoning.
			if !strings.Contains(lower, "premise") && !strings.Contains(lower, "already failed") {
				t.Errorf("the instruction should state why — the artifact's absence is the "+
					"premise of the hop, not something to verify (%s)", e.Name())
			}
		})
	}
	if checked == 0 {
		t.Fatal("no swarm with RECOVERY MODE instructions found — the lint is inert, " +
			"which is worse than absent because it reads as passing")
	}
}
