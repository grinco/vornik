package agentbench

import (
	"strings"
	"testing"
)

func baseArm() ArmFields {
	return ArmFields{
		// The CONSTANT, not a literal: a version test that hardcodes the current
		// value compares a number with itself and passes forever.
		HarnessVersion: HarnessVersion,
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

// A scoring change makes old numbers incomparable even when every other axis
// matches. The key is what turns that from something to remember into something
// enforced — but only if the version actually moves when semantics do.
func TestArmFields_HarnessVersionSplitsTheKey(t *testing.T) {
	a := baseArm()
	b := baseArm()
	b.HarnessVersion = "0-previous"

	if a.Key() == b.Key() {
		t.Fatal("two harness versions produced the same key")
	}
	err := CheckComparable(a, b)
	if err == nil || !strings.Contains(err.Error(), "harness_version") {
		t.Errorf("want a refusal naming harness_version, got: %v", err)
	}
}

// Observed models, not declared ones. A router fallback that serves a different
// model on a different provider must split the key, or two runs compare clean
// having measured different systems.
func TestArmFields_AnObservedModelChangeSplitsTheKey(t *testing.T) {
	a := baseArm()
	b := baseArm()
	b.Models = map[string]string{"lead": "zai.glm-5", "worker": "qwen3.6:35b"} // fell back to Bedrock

	if a.Key() == b.Key() {
		t.Fatal("a silent provider fallback left the key unchanged — the runs would " +
			"compare clean having used different models")
	}
	if err := CheckComparable(a, b); err == nil || !strings.Contains(err.Error(), "models") {
		t.Errorf("want a refusal naming models, got: %v", err)
	}
}

// Without a declared independent variable the benchmark cannot do the one thing
// it exists for: comparing two RELEASES means the binary differs by definition,
// and CheckComparable refuses any difference — so `bench agent compare` refused
// every release comparison the README advertises it for.
func TestCheckComparableExcept_AllowsTheDeclaredAxisOnly(t *testing.T) {
	a, b := baseArm(), baseArm()
	b.BinarySHA256 = "a-newer-build"

	if err := CheckComparableExcept(a, b, []string{"binary_sha256"}); err != nil {
		t.Fatalf("a declared release comparison was refused: %v", err)
	}
	// An undeclared axis moving alongside it must still refuse — otherwise the
	// comparison silently has two variables.
	b.ContextPolicy = "suppression=canonical-context;advert=gated"
	err := CheckComparableExcept(a, b, []string{"binary_sha256"})
	if err == nil {
		t.Fatal("an undeclared axis moved and the comparison was accepted")
	}
	if !strings.Contains(err.Error(), "context_policy") {
		t.Errorf("refusal does not name the undeclared axis: %v", err)
	}
	if strings.Contains(err.Error(), "binary_sha256") {
		t.Errorf("refusal names the DECLARED axis, which is the one allowed to move: %v", err)
	}
}

// Forgiving everything compares nothing.
func TestCheckComparableExcept_RefusesDeclaringEveryAxis(t *testing.T) {
	a, b := baseArm(), baseArm()
	b.BinarySHA256 = "other"

	err := CheckComparableExcept(a, b, ComparabilityAxes())
	if err == nil || !strings.Contains(err.Error(), "compares nothing") {
		t.Fatalf("declaring every axis was accepted: %v", err)
	}
}

// A typo must not silently widen what is forgiven.
func TestCheckComparableExcept_RefusesAnUnknownAxis(t *testing.T) {
	a, b := baseArm(), baseArm()
	b.BinarySHA256 = "other"

	err := CheckComparableExcept(a, b, []string{"binary_sha"})
	if err == nil || !strings.Contains(err.Error(), "unknown independent axis") {
		t.Fatalf("unknown axis accepted: %v", err)
	}
}

// No declaration keeps the strict behaviour every existing run relies on.
func TestCheckComparableExcept_EmptyDeclarationIsStrict(t *testing.T) {
	a, b := baseArm(), baseArm()
	b.BinarySHA256 = "other"

	if err := CheckComparableExcept(a, b, nil); err == nil {
		t.Fatal("an undeclared binary change compared clean")
	}
}
