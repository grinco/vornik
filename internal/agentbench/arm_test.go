package agentbench

import (
	"strings"
	"testing"
)

func baseArm() ArmFields {
	return ArmFields{
		HarnessVersion: "1",
		Name:           "baseline",
		BinarySHA256:   "aaaa",
		ConfigSHA256:   "bbbb",
		Models:         map[string]string{"lead": "gpt-oss:120b", "worker": "qwen3.6:35b"},
		ContextPolicy:  "suppression=none;advert=gated",
		TaskSetSHA256:  "cccc",
		GoldSHA256:     "dddd",
		Probes:         []string{"tool-grant", "schema-following"},
	}
}

// Renaming an arm must not make it incomparable with itself.
func TestArmFields_NameIsNotPartOfTheKey(t *testing.T) {
	a := baseArm()
	b := baseArm()
	b.Name = "renamed"

	if a.Key() != b.Key() {
		t.Error("renaming an arm changed its key — an arm renamed is the same experiment")
	}
}

// Map iteration order is random in Go. Hashing the map directly would give one
// arm a different key on every run, which would refuse every comparison.
func TestArmFields_ModelMapOrderDoesNotAffectTheKey(t *testing.T) {
	a := baseArm()
	b := baseArm()
	b.Models = map[string]string{"worker": "qwen3.6:35b", "lead": "gpt-oss:120b"}

	if a.Key() != b.Key() {
		t.Error("the same models in a different insertion order produced a different key")
	}
}

func TestArmFields_ProbeOrderDoesNotAffectTheKey(t *testing.T) {
	a := baseArm()
	b := baseArm()
	b.Probes = []string{"schema-following", "tool-grant"}

	if a.Key() != b.Key() {
		t.Error("probe listing order changed the key")
	}
}

// A run scored by two probes is not a cheaper version of one scored by three:
// the third may have failed executions the others tolerated.
func TestArmFields_ProbeSetIsPartOfTheKey(t *testing.T) {
	a := baseArm()
	b := baseArm()
	b.Probes = append(b.Probes, "tool-use")

	if a.Key() == b.Key() {
		t.Error("adding a probe left the key unchanged")
	}
}

func TestCheckComparable_RefusesAndNamesEveryDifference(t *testing.T) {
	a := baseArm()
	b := baseArm()
	b.BinarySHA256 = "zzzz"
	b.ContextPolicy = "suppression=canonical-context;advert=gated"

	err := CheckComparable(a, b)
	if err == nil {
		t.Fatal("two different arms compared clean")
	}
	msg := err.Error()
	for _, want := range []string{"binary_sha256", "context_policy"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
	// Naming only the first would send an operator round a fix-and-rerun loop.
	if strings.Count(msg, "(") < 2 {
		t.Errorf("only one difference reported; want every differing axis: %v", err)
	}
}

func TestCheckComparable_AllowsIdenticalArms(t *testing.T) {
	if err := CheckComparable(baseArm(), baseArm()); err != nil {
		t.Fatalf("identical arms refused: %v", err)
	}
}

// A gold regeneration makes prior numbers incomparable. That is exactly why
// §5.3 fences regeneration behind a task-set hash.
func TestArmFields_GoldChangeSplitsTheKey(t *testing.T) {
	a := baseArm()
	b := baseArm()
	b.GoldSHA256 = "eeee"

	if a.Key() == b.Key() {
		t.Error("a regenerated gold set left the key unchanged — prior runs would " +
			"silently compare against a different ground truth")
	}
}

// An unverified key is not the same as a verified-identical one, and must be
// surfaced as such rather than quietly compared.
func TestArmFields_PartialWhenIdentityIsUnknown(t *testing.T) {
	for _, missing := range []string{"binary", "config", "taskset"} {
		a := baseArm()
		switch missing {
		case "binary":
			a.BinarySHA256 = ""
		case "config":
			a.ConfigSHA256 = ""
		case "taskset":
			a.TaskSetSHA256 = ""
		}
		if !a.Partial() {
			t.Errorf("an arm with no %s hash reported a complete key", missing)
		}
	}
	if baseArm().Partial() {
		t.Error("a fully identified arm reported a partial key")
	}
}

func TestTaskSetDigest(t *testing.T) {
	ids := []string{"t1", "t2"}
	bodies := map[string]string{"t1": "do a thing", "t2": "do another"}

	base := TaskSetDigest(ids, bodies)
	if base == "" {
		t.Fatal("digest of a non-empty set was empty")
	}

	t.Run("order independent", func(t *testing.T) {
		if got := TaskSetDigest([]string{"t2", "t1"}, bodies); got != base {
			t.Error("listing order changed the digest — the order tasks happen to be " +
				"listed in is not a property of the set")
		}
	})

	t.Run("a one-character edit changes it", func(t *testing.T) {
		edited := map[string]string{"t1": "do a thing.", "t2": "do another"}
		if got := TaskSetDigest(ids, edited); got == base {
			t.Error("an edited task body produced the same digest — the regeneration " +
				"fence would refuse a gold rebuild that is genuinely needed")
		}
	})

	t.Run("a rename cannot compensate for an edit", func(t *testing.T) {
		// Length-prefixing is what stops ("ab","c") and ("a","bc") colliding.
		a := TaskSetDigest([]string{"ab"}, map[string]string{"ab": "c"})
		b := TaskSetDigest([]string{"a"}, map[string]string{"a": "bc"})
		if a == b {
			t.Error("id/body boundary is forgeable")
		}
	})

	t.Run("empty set digests to empty, not to a hash of nothing", func(t *testing.T) {
		if got := TaskSetDigest(nil, nil); got != "" {
			t.Errorf("digest of an empty set = %q; a hash of nothing looks like a real set", got)
		}
	})
}
