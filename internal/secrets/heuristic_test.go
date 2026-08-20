package secrets

import "testing"

// The heuristic/strong split is a security boundary — the allowlist may rescue a
// value from heuristic redaction but never from a strong prefix-anchored
// pattern, and a provenance-trusted tool output may drop heuristic findings but
// never strong ones. It was stated three times as an inline pair of constants
// (allowlistEligible here, dropHeuristicFindings in the executor, and it was
// about to be a third in outputguard) before becoming one predicate.
func TestIsHeuristicType(t *testing.T) {
	for _, name := range []string{FindingTypeGenericKV, FindingTypeEntropy} {
		if !IsHeuristicType(name) {
			t.Errorf("%q must classify as heuristic — the allowlist and the trusted-output "+
				"exemption both key on this", name)
		}
	}

	// Every shipped pattern that is NOT one of the two heuristics is strong by
	// definition. Asserting over the corpus rather than a hand-listed sample
	// means a new pattern cannot be silently mis-classified.
	strong := 0
	for _, p := range DefaultPatterns() {
		if p.Name == FindingTypeGenericKV || p.Name == FindingTypeEntropy {
			continue
		}
		strong++
		if IsHeuristicType(p.Name) {
			t.Errorf("shipped pattern %q must classify as STRONG — a strong pattern that "+
				"reads as heuristic becomes allowlist-suppressible, which is the security "+
				"boundary this predicate draws", p.Name)
		}
	}
	if strong == 0 {
		t.Fatal("no strong patterns found — the corpus parse is broken and this test is vacuous")
	}

	if IsHeuristicType("no_such_pattern") {
		t.Error("an unknown type must not be treated as heuristic — unknown means unrecognised, " +
			"and treating it as the weak class would make it allowlist-suppressible")
	}
}

// StrongPatterns is what outputguard consumes: it must never hand back a
// heuristic rule, because outputguard redacts what the MODEL sees and a
// heuristic false positive corrupts the agent's working data rather than merely
// dirtying an audit row.
func TestStrongPatternsExcludesHeuristics(t *testing.T) {
	got := StrongPatterns(DefaultPatterns())
	if len(got) == 0 {
		t.Fatal("StrongPatterns returned nothing")
	}
	for _, p := range got {
		if IsHeuristicType(p.Name) {
			t.Errorf("StrongPatterns leaked heuristic pattern %q", p.Name)
		}
	}

	// It must actually carry the credential shapes, or the outputguard gap this
	// exists to close stays open.
	want := map[string]bool{"google_api_key": false, "github_pat": false, "openai_key": false}
	for _, p := range got {
		if _, ok := want[p.Name]; ok {
			want[p.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("StrongPatterns is missing %q", name)
		}
	}

	// Operator configuration flows through: a disabled pattern must not reappear
	// via this path, or outputguard would enforce what the operator switched off.
	narrowed := StrongPatterns(EffectivePatterns([]string{"google_api_key"}, nil))
	for _, p := range narrowed {
		if p.Name == "google_api_key" {
			t.Error("a pattern disabled by the operator must not come back through StrongPatterns")
		}
	}
}

// allowlistEligible is the original site of the split; it must now agree with
// the shared predicate rather than carry its own copy.
func TestAllowlistEligibleUsesSharedPredicate(t *testing.T) {
	cases := map[string]bool{
		FindingTypeGenericKV: true,
		FindingTypeEntropy:   true,
		"google_api_key":     false,
		"github_pat":         false,
	}
	for typ, want := range cases {
		if got := allowlistEligible(Finding{Type: typ}); got != want {
			t.Errorf("allowlistEligible(%q) = %v, want %v", typ, got, want)
		}
	}
}

// ToolNameMatchesPrefix decides which tools get the trusted-output exemption,
// so its boundary is a security boundary: an operator granting "…_publish"
// must not thereby grant "…_publisher_evil".
func TestToolNameMatchesPrefix(t *testing.T) {
	const prefix = "mcp__pagedrop__pagedrop_publish"
	cases := map[string]bool{
		"mcp__pagedrop__pagedrop_publish":        true,  // exact
		"mcp__pagedrop__pagedrop_publish_page":   true,  // delimiter
		"mcp__pagedrop__pagedrop_publisher_evil": false, // look-alike
		"mcp__pagedrop__pagedrop_publishing":     false, // look-alike
		"mcp__scraper__web_fetch":                false,
	}
	for tool, want := range cases {
		if got := ToolNameMatchesPrefix(tool, prefix); got != want {
			t.Errorf("ToolNameMatchesPrefix(%q, %q) = %v, want %v", tool, prefix, got, want)
		}
	}
	if ToolNameMatchesPrefix("anything", "") {
		t.Error("an empty prefix must never match — it would exempt every tool")
	}
}
