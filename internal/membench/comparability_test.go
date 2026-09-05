package membench

import "testing"

// Comparability key from design §5.6. Model choice is configurable with no
// default, so the key is the mechanism that stops two incomparable runs being
// plotted on one axis. Every field it hashes is a field that changes what the
// numbers mean.

func baseFields() ComparabilityFields {
	return ComparabilityFields{
		HarnessVersion:       "1",
		DatasetName:          "longmemeval",
		DatasetSHA256:        "abc123",
		ItemSelection:        "category=multi-session;max=100",
		OurExtractionModel:   "qwen3.6:35b",
		TheirExtractionModel: "qwen3.6:35b",
		AnswerModel:          "qwen3.6:35b",
		JudgeModel:           "claude-opus-5",
		RecallParams:         "max_tokens=4096;budget=mid;rerank=on",
		AnswerPromptSHA256:   "p1",
		JudgePromptSHA256:    "p2",
		ExternalConfigSHA256: "ext1",
		// Present so Partial() exercises the EXTERNAL-config path these tests are
		// about; an unreported embedder is separately partial by design.
		ObservedEmbedder:     "bedrock/cohere.embed-v4:0",
		ObservedRecallMethod: "vector_rerank",
		CorpusSHA256:         "corpus1",
		// Also present for the same reason: an unobserved corpus regime and an
		// unknown daemon build are each separately partial by design (v5), so a
		// fixture missing them would make every Partial() test pass for the
		// wrong reason.
		CorpusRegime:   CorpusRegimeCold,
		DaemonRevision: "abc1234",
	}
}

// TestComparabilityKey_Stable — same inputs, same key. Without this, no two runs
// are ever comparable and the whole mechanism is noise.
func TestComparabilityKey_Stable(t *testing.T) {
	a := baseFields().Key()
	b := baseFields().Key()
	if a != b {
		t.Errorf("key not stable across identical inputs: %s vs %s", a, b)
	}
	if a == "" {
		t.Error("key is empty")
	}
}

// TestComparabilityKey_EveryFieldMatters is the test that gives the mechanism
// teeth. Round-2 review found the external system's config missing from the
// field set — it could swap its embedding model between runs and the key would
// still match. One subtest per field so a future addition that isn't hashed
// fails loudly here.
func TestComparabilityKey_EveryFieldMatters(t *testing.T) {
	mutations := map[string]func(*ComparabilityFields){
		"harness version":        func(f *ComparabilityFields) { f.HarnessVersion = "2" },
		"dataset name":           func(f *ComparabilityFields) { f.DatasetName = "locomo" },
		"dataset hash":           func(f *ComparabilityFields) { f.DatasetSHA256 = "def456" },
		"item selection":         func(f *ComparabilityFields) { f.ItemSelection = "max=50" },
		"our extraction model":   func(f *ComparabilityFields) { f.OurExtractionModel = "other" },
		"their extraction model": func(f *ComparabilityFields) { f.TheirExtractionModel = "other" },
		"answer model":           func(f *ComparabilityFields) { f.AnswerModel = "other" },
		"judge model":            func(f *ComparabilityFields) { f.JudgeModel = "other" },
		"recall params":          func(f *ComparabilityFields) { f.RecallParams = "max_tokens=8192" },
		"observed recall method": func(f *ComparabilityFields) { f.ObservedRecallMethod = "lexical" },
		"answer prompt":          func(f *ComparabilityFields) { f.AnswerPromptSHA256 = "p9" },
		"judge prompt":           func(f *ComparabilityFields) { f.JudgePromptSHA256 = "p9" },
		"external config":        func(f *ComparabilityFields) { f.ExternalConfigSHA256 = "ext9" },
		"single system":          func(f *ComparabilityFields) { f.SingleSystem = true },
	}

	base := baseFields().Key()
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			f := baseFields()
			mutate(&f)
			if got := f.Key(); got == base {
				t.Errorf("changing %s did not change the key — two incomparable "+
					"runs would be diffed as if identical", name)
			}
		})
	}
}

// TestComparabilityKey_FieldsCannotCollide — concatenating fields without a
// separator lets ("ab","c") and ("a","bc") hash alike. Unlikely with hashes,
// entirely plausible with model names and selection strings.
func TestComparabilityKey_FieldsCannotCollide(t *testing.T) {
	a := ComparabilityFields{DatasetName: "ab", DatasetSHA256: "c"}
	b := ComparabilityFields{DatasetName: "a", DatasetSHA256: "bc"}
	if a.Key() == b.Key() {
		t.Error("adjacent fields collide; the key needs a delimiter between fields")
	}
}

// TestComparabilityFields_PartialWhenExternalConfigUnknown — round-2 resolution:
// an unverifiable external config is not a verified-identical one, and the
// manifest must say so rather than implying full comparability.
func TestComparabilityFields_PartialWhenExternalConfigUnknown(t *testing.T) {
	f := baseFields()
	if f.Partial() {
		t.Error("fully-populated fields reported partial")
	}

	f.ExternalConfigSHA256 = ""
	if !f.Partial() {
		t.Error("missing external config must mark the key PARTIAL — silently " +
			"treating 'could not verify' as 'unchanged' is the failure mode " +
			"round-2 review flagged")
	}
}

// TestComparabilityFields_PartialIgnoresExternalWhenSingleSystem — a
// vornik-only regression run has no external system, so its absence is not a
// gap in comparability.
func TestComparabilityFields_PartialIgnoresExternalWhenSingleSystem(t *testing.T) {
	f := baseFields()
	f.TheirExtractionModel = ""
	f.ExternalConfigSHA256 = ""
	f.SingleSystem = true

	if f.Partial() {
		t.Error("a single-system run was marked partial for lacking external " +
			"config it never had")
	}
}

// TestCompare_RefusesDifferentKeys — the enforcement point. Naming the differing
// field is the difference between a usable error and a dead end.
func TestCompare_RefusesDifferentKeys(t *testing.T) {
	a := baseFields()
	b := baseFields()
	b.JudgeModel = "some-other-judge"

	err := CheckComparable(a, b)
	if err == nil {
		t.Fatal("CheckComparable accepted two runs with different judge models")
	}
	if !contains(err.Error(), "judge_model") {
		t.Errorf("error %q does not name the differing field", err)
	}
}

// TestCompare_AcceptsIdenticalKeys — and the happy path.
func TestCompare_AcceptsIdenticalKeys(t *testing.T) {
	if err := CheckComparable(baseFields(), baseFields()); err != nil {
		t.Errorf("CheckComparable rejected identical runs: %v", err)
	}
}

// TestCompare_ReportsAllDifferingFields — when several differ, listing one and
// stopping sends the operator round the loop repeatedly.
func TestCompare_ReportsAllDifferingFields(t *testing.T) {
	a := baseFields()
	b := baseFields()
	b.JudgeModel = "x"
	b.AnswerModel = "y"

	err := CheckComparable(a, b)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"judge_model", "answer_model"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q omits %s", err, want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
