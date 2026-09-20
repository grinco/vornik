package registry

import (
	"strings"
	"testing"
)

// §13.4. §11 declined auto-heal entirely because deleting an offending key
// restores a project by silently disabling whatever the operator was
// configuring. That argument holds for an unknown key and NOT for the class
// §11.1 already separates: a key that is a misspelling of one this binary knows
// has an unambiguous correct end state, because the operator's intent is
// legible in the key they typed.

const skewedProject = `projectId: demo
display_name: Demo
swarmId: dev-swarm
`

func TestHealMisspelledKeys_RewritesOnlyTheKey(t *testing.T) {
	diag := SkewDiagnosis{
		UnknownKeys: []string{"display_name"},
		Suggestions: map[string]string{"display_name": "display_namee"},
	}

	healed, changes, err := HealMisspelledKeys([]byte(skewedProject), diag)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].From != "display_name" || changes[0].To != "display_namee" {
		t.Fatalf("want one display_name->display_namee change, got %+v", changes)
	}
	got := string(healed)
	if !strings.Contains(got, "display_namee: Demo") {
		t.Fatalf("the key was not corrected:\n%s", got)
	}
	// Everything else is byte-identical. A repair that reformats a customer's
	// file is a repair they cannot review.
	for _, keep := range []string{"projectId: demo", "swarmId: dev-swarm"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("healing altered an unrelated line (%q missing):\n%s", keep, got)
		}
	}
}

// A key unknown under EVERY spelling is a deploy-ordering problem, and
// "fixing" it means deleting it — the thing §11 refused and still refuses.
func TestHealMisspelledKeys_LeavesUnknownKeysAlone(t *testing.T) {
	diag := SkewDiagnosis{
		UnknownKeys:       []string{"brandNewKey"},
		Suggestions:       map[string]string{},
		LikelyVersionSkew: true,
	}
	healed, changes, err := HealMisspelledKeys([]byte("projectId: demo\nbrandNewKey: 1\n"), diag)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("an unknown key was healed: %+v", changes)
	}
	if string(healed) != "projectId: demo\nbrandNewKey: 1\n" {
		t.Fatalf("the file changed:\n%s", healed)
	}
}

// A mixed file heals the misspelling and leaves the unknown key — and reports
// both, so the operator is not told the project is fixed when it is not.
func TestHealMisspelledKeys_MixedFileHealsOnlyWhatItCan(t *testing.T) {
	diag := SkewDiagnosis{
		UnknownKeys: []string{"display_name", "brandNewKey"},
		Suggestions: map[string]string{"display_name": "display_namee"},
	}
	healed, changes, err := HealMisspelledKeys([]byte("display_name: Demo\nbrandNewKey: 1\n"), diag)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("want exactly one change, got %+v", changes)
	}
	got := string(healed)
	if !strings.Contains(got, "display_namee: Demo") || !strings.Contains(got, "brandNewKey: 1") {
		t.Fatalf("mixed heal is wrong:\n%s", got)
	}
}

// The key must be corrected where it is DECLARED, not wherever the string
// happens to appear — a value that contains the misspelling is data.
func TestHealMisspelledKeys_DoesNotRewriteValues(t *testing.T) {
	diag := SkewDiagnosis{
		UnknownKeys: []string{"display_name"},
		Suggestions: map[string]string{"display_name": "display_namee"},
	}
	src := "description: \"the display_name typo is described here\"\ndisplay_name: Demo\n"
	healed, changes, err := HealMisspelledKeys([]byte(src), diag)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("want one change, got %+v", changes)
	}
	if !strings.Contains(string(healed), "the display_name typo is described here") {
		t.Fatalf("a VALUE containing the key name was rewritten:\n%s", healed)
	}
}

// Nothing to heal is not an error, and must not rewrite the file.
func TestHealMisspelledKeys_NoSuggestionsIsANoOp(t *testing.T) {
	healed, changes, err := HealMisspelledKeys([]byte(skewedProject), SkewDiagnosis{})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 || string(healed) != skewedProject {
		t.Fatalf("a no-op heal changed something: %+v\n%s", changes, healed)
	}
}
