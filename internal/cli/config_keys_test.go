package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func runKeys(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	// Drive the RunE directly with the package flags set, rather than
	// re-registering the flag set on a throwaway command: the flags are
	// package-level (the cobra shape this file already uses), so a second
	// registration would be a second source of truth for the same flag.
	cmd := &cobra.Command{RunE: runConfigKeys}
	cmd.SetOut(&out)
	configKeysSince, configJSON = "", false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--since" {
			configKeysSince = args[i+1]
		}
	}
	if err := runConfigKeys(cmd, nil); err != nil {
		t.Fatalf("config keys: %v", err)
	}
	return out.String()
}

// The full list is the answer to "would my config load", and it must be
// non-empty and derived — a schema-derived list that came back empty would mean
// the walk broke, which is worse than a wrong key.
func TestConfigKeys_ListsTheSchema(t *testing.T) {
	got := runKeys(t)
	if !strings.Contains(got, "swarmId") || !strings.Contains(got, "KEY") {
		t.Fatalf("the key list is missing the schema:\n%s", got)
	}
	if !strings.Contains(got, "predating per-key release provenance") {
		t.Fatalf("the untagged count is not reported:\n%s", got)
	}
}

// An empty --since result must say WHY it is empty. "No keys are newer" and "no
// key carries provenance yet" are different answers, and an operator planning
// an upgrade must not read the second as the first.
func TestConfigKeys_EmptySinceExplainsItself(t *testing.T) {
	// A release NOBODY has shipped yet, so the result is empty by
	// construction. It used to name a past release, which stopped being
	// empty the moment a key was tagged newer than it (`dependencies`,
	// 2026.9.6) — a fixture that silently stops exercising the branch it
	// was written for.
	got := runKeys(t, "--since", "2999.1.0")
	if !strings.Contains(got, "NOT a statement that nothing changed") {
		t.Fatalf("an empty --since did not explain itself:\n%s", got)
	}
}

// The converse, which the case above cannot pin once it is empty by
// construction: a --since that DOES match must list the key and not the
// disclaimer.
func TestConfigKeys_SinceListsNewerKeys(t *testing.T) {
	got := runKeys(t, "--since", "2026.9.5")
	if !strings.Contains(got, "dependencies") {
		t.Fatalf("--since did not list a key tagged newer than it:\n%s", got)
	}
	if strings.Contains(got, "NOT a statement that nothing changed") {
		t.Fatalf("a non-empty --since printed the empty-result disclaimer:\n%s", got)
	}
}
