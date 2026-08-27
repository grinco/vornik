package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"vornik.io/vornik/internal/secrets"
)

// syntheticKey has the SHAPE of a Google API key and is not a real credential.
// Assertions compare hashes or absence, never the value itself.
const syntheticKey = "AIzaSyDUMMYKEYFORTESTINGONLY0123456789A"

func corpus() []secrets.Pattern { return secrets.DefaultPatterns() }

// D1: `strong` is IsHeuristicType inverted. The whole point of the change is
// that an operator can redact the 24 typed findings without rewriting the
// 7,270 heuristic ones.
func TestRuleSelectionStrongExcludesHeuristics(t *testing.T) {
	findings := []secrets.Finding{
		{Type: "jwt", Match: "a.b.c", Start: 0, End: 5},
		{Type: secrets.FindingTypeEntropy, Match: "xxxxxxxxx", Start: 10, End: 19},
		{Type: secrets.FindingTypeGenericKV, Match: "k=v", Start: 20, End: 23},
	}

	cases := []struct {
		spec string
		want []string
	}{
		{"strong", []string{"jwt"}},
		{"all", []string{"jwt", secrets.FindingTypeEntropy, secrets.FindingTypeGenericKV}},
		{secrets.FindingTypeEntropy, []string{secrets.FindingTypeEntropy}},
		{"jwt,generic_kv", []string{"jwt", secrets.FindingTypeGenericKV}},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			sel, err := parseRuleSelection(tc.spec, corpus())
			if err != nil {
				t.Fatalf("parseRuleSelection(%q): %v", tc.spec, err)
			}
			got := []string{}
			for _, f := range selectFindings(findings, sel) {
				got = append(got, f.Type)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("selected %v, want %v", got, tc.want)
			}
		})
	}
}

// A rule name nobody recognises must be an ERROR, not an empty selection: an
// empty selection would redact nothing and report success, which is the
// silent-control shape this codebase keeps paying for.
func TestUnknownRuleNameIsAnError(t *testing.T) {
	_, err := parseRuleSelection("nosuchrule", corpus())
	if err == nil {
		t.Fatal("an unknown rule name must be rejected, not silently select nothing")
	}
	if !strings.Contains(err.Error(), "nosuchrule") {
		t.Errorf("error does not name the offending rule: %v", err)
	}
	if !strings.Contains(err.Error(), "jwt") {
		t.Errorf("error does not name the valid set: %v", err)
	}
}

// THE REGRESSION this change exists for. Before it, --apply rewrote every
// finding, so redacting one jwt meant destroying every entropy match in the
// same row — 7,221 of them table-wide, irreversibly.
func TestApplyRewritesOnlySelectedFindings(t *testing.T) {
	const caseID = "s1_case_1_aaaaaaaaaaaaaaaa"
	text := `{"token":"a.b.c","test_case_ids":["` + caseID + `"]}`
	findings := []secrets.Finding{
		{Type: "jwt", Match: "a.b.c", Start: 10, End: 15},
		{Type: secrets.FindingTypeEntropy, Match: caseID,
			Start: strings.Index(text, caseID), End: strings.Index(text, caseID) + len(caseID)},
	}

	sel, err := parseRuleSelection("strong", corpus())
	if err != nil {
		t.Fatalf("parseRuleSelection: %v", err)
	}
	out := string(secrets.Redact([]byte(text), selectFindings(findings, sel)))

	if strings.Contains(out, "a.b.c") {
		t.Error("the selected strong finding was not redacted")
	}
	if !strings.Contains(out, caseID) {
		t.Error("an UNSELECTED heuristic match was redacted — this is the destruction the scoping exists to prevent")
	}
}

// G2: the matched bytes must never reach stdout. Compared by hash, per the
// backlog item's own rule about not reproducing token values.
func TestSampleNeverPrintsTheMatch(t *testing.T) {
	text := `{"key":"` + syntheticKey + `","note":"harmless"}`
	start := strings.Index(text, syntheticKey)
	f := secrets.Finding{Type: "google_api_key", Match: syntheticKey, Start: start, End: start + len(syntheticKey)}

	line := sampleLine(newRunSalt(), "row-1", "mcp__scraper__web_fetch", text, []secrets.Finding{f}, f)

	if strings.Contains(line, syntheticKey) {
		t.Fatal("the matched value reached the sample output")
	}
	if !strings.Contains(line, fmt.Sprintf("len=%d", len(syntheticKey))) {
		t.Errorf("sample does not carry the length: %q", line)
	}
	if !strings.Contains(line, "harmless") {
		t.Errorf("sample dropped the surrounding context that makes it judgeable: %q", line)
	}
}

// R2 / round-1 F4: a credential whose span STARTS BEFORE the window must still
// be masked. Masking by re-scanning the window substring would emit its tail.
func TestSampleMasksAStraddlingFinding(t *testing.T) {
	prefix := strings.Repeat("p", 200)
	text := prefix + syntheticKey + `,"target":"` + strings.Repeat("t", 20) + `"`
	keyStart := len(prefix)
	target := secrets.Finding{Type: secrets.FindingTypeEntropy, Match: strings.Repeat("t", 20),
		Start: strings.Index(text, strings.Repeat("t", 20)), End: strings.Index(text, strings.Repeat("t", 20)) + 20}
	all := []secrets.Finding{
		{Type: "google_api_key", Match: syntheticKey, Start: keyStart, End: keyStart + len(syntheticKey)},
		target,
	}

	got := maskedContext(text, all, target)
	if strings.Contains(got, syntheticKey[len(syntheticKey)-10:]) {
		t.Errorf("a credential straddling the window boundary leaked its tail: %q", got)
	}
}

// Round-1 F4: adjacency is a DIFFERENT predicate bug from straddling. Two
// findings meeting at one offset must both mask; an off-by-one in the overlap
// test drops one and prints it verbatim.
func TestSampleMasksAdjacentFindingsAtWindowBoundary(t *testing.T) {
	a, b := strings.Repeat("a", 12), strings.Repeat("b", 12)
	text := "head " + a + b + " tail"
	aStart := strings.Index(text, a)
	fa := secrets.Finding{Type: "jwt", Match: a, Start: aStart, End: aStart + len(a)}
	fb := secrets.Finding{Type: secrets.FindingTypeEntropy, Match: b, Start: aStart + len(a), End: aStart + len(a) + len(b)}

	got := maskedContext(text, []secrets.Finding{fa, fb}, fa)
	if strings.Contains(got, a) {
		t.Errorf("the target finding was not masked: %q", got)
	}
	if strings.Contains(got, b) {
		t.Errorf("the ADJACENT finding was emitted verbatim: %q", got)
	}
}

// Round-1 F1: the identity token must be useless once the run exits, or every
// archived sample becomes a "was THIS credential in the log?" oracle.
func TestSampleTokenIsSaltedPerRun(t *testing.T) {
	saltA, saltB := newRunSalt(), newRunSalt()

	first, second := identityToken(saltA, syntheticKey), identityToken(saltA, syntheticKey)
	if first != second {
		t.Error("the token is not stable within a run, so correlation is impossible")
	}
	if other := identityToken(saltB, syntheticKey); first == other {
		t.Error("the token survives across runs — an archived sample is a confirmation oracle")
	}
	// And it must not be a bare hash of the value.
	bare := sha256.Sum256([]byte(syntheticKey))
	if strings.HasPrefix(hex.EncodeToString(bare[:]), first) {
		t.Error("the token is an unsalted sha256 prefix — the round-1 defect")
	}
}

// The masking must be byte-exact, not merely "the value is gone". An
// off-by-one at the span START leaks the match's first byte and still passes
// every containment assertion above — found by mutation-testing this file,
// 2026-08-27. Pin the whole rendering.
func TestMaskedContextIsByteExact(t *testing.T) {
	text := `a="SECRET" b="ok"`
	start := strings.Index(text, "SECRET")
	f := secrets.Finding{Type: "jwt", Match: "SECRET", Start: start, End: start + len("SECRET")}

	got := maskedContext(text, []secrets.Finding{f}, f)
	want := `a="<REDACTED:jwt>" b="ok"`
	if got != want {
		t.Errorf("maskedContext =\n  %q\nwant\n  %q", got, want)
	}
}

// The window is byte arithmetic over text that is frequently JSON containing
// non-ASCII. Found by an adversarial alignment sweep, 2026-08-27: at 4 of 6
// pad offsets the window landed mid-rune and emitted broken bytes — which in
// --json output is invalid UTF-8, not merely ugly.
func TestMaskedContextIsValidUTF8AtEveryAlignment(t *testing.T) {
	for pad := 0; pad < 6; pad++ {
		prefix := strings.Repeat("あ", 40) + strings.Repeat("x", pad)
		m := strings.Repeat("z", 10)
		text := prefix + m + strings.Repeat("あ", 40)
		start := strings.Index(text, m)
		f := secrets.Finding{Type: "jwt", Match: m, Start: start, End: start + len(m)}

		got := maskedContext(text, []secrets.Finding{f}, f)
		if !utf8.ValidString(got) {
			t.Errorf("pad=%d: masked context is not valid UTF-8: %q", pad, got)
		}
		if strings.Contains(got, m) {
			t.Errorf("pad=%d: match not masked: %q", pad, got)
		}
	}
}

// Code review 2026-08-27 (review-20260827-0487) called maskedContext unsound
// for adjacent and nested findings, tracing a path in which the stride to
// f.End skips an earlier finding's bytes unmasked. It does not: the stride
// lands exactly past the span whose placeholder was just written. These cases
// are the reviewer's own, kept as permanent coverage because the topologies
// deserve pinning whatever the verdict was — and asserted in BOTH slice
// orders, since findingAt returns the first match in slice order.
func TestMaskedContextHandlesEveryOverlapTopology(t *testing.T) {
	probe := func(t *testing.T, text string, a, b secrets.Finding, targets []secrets.Finding, leaks ...string) {
		t.Helper()
		for _, order := range [][]secrets.Finding{{a, b}, {b, a}} {
			for _, target := range targets {
				got := maskedContext(text, order, target)
				for _, leak := range leaks {
					if strings.Contains(got, leak) {
						t.Errorf("leaked %q (first=%s, target=%s): %q", leak, order[0].Type, target.Type, got)
					}
				}
			}
		}
	}

	t.Run("adjacent inside the window", func(t *testing.T) {
		text := `{"a":"SECRET1SECRET2","b":"ok"}`
		s1 := strings.Index(text, "SECRET1")
		a := secrets.Finding{Type: "jwt", Match: "SECRET1", Start: s1, End: s1 + 7}
		b := secrets.Finding{Type: secrets.FindingTypeEntropy, Match: "SECRET2", Start: s1 + 7, End: s1 + 14}
		probe(t, text, a, b, []secrets.Finding{a, b}, "SECRET1", "SECRET2")
	})

	t.Run("nested", func(t *testing.T) {
		text := `x="OUTERqqqINNERqqqOUTER" y`
		outer := secrets.Finding{Type: "jwt", Match: "OUTERqqqINNERqqqOUTER", Start: 3, End: 24}
		inner := secrets.Finding{Type: secrets.FindingTypeEntropy, Match: "INNER", Start: 11, End: 16}
		probe(t, text, outer, inner, []secrets.Finding{outer, inner}, "OUTER", "INNER")
	})

	t.Run("partially overlapping", func(t *testing.T) {
		text := `z="AAAABBBBCCCC" w`
		a := secrets.Finding{Type: "jwt", Match: "AAAABBBB", Start: 3, End: 11}
		b := secrets.Finding{Type: secrets.FindingTypeEntropy, Match: "BBBBCCCC", Start: 7, End: 15}
		probe(t, text, a, b, []secrets.Finding{a, b}, "AAAA", "CCCC")
	})
}

// Window arithmetic goes wrong at the ends.
func TestSampleContextWindowTruncatesAtRowBoundary(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"at offset zero", strings.Repeat("z", 10) + " tail"},
		{"at end of row", "head " + strings.Repeat("z", 10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := strings.Repeat("z", 10)
			start := strings.Index(tc.text, m)
			f := secrets.Finding{Type: secrets.FindingTypeEntropy, Match: m, Start: start, End: start + len(m)}
			got := maskedContext(tc.text, []secrets.Finding{f}, f)
			if strings.Contains(got, m) {
				t.Errorf("match not masked: %q", got)
			}
			if got == "" {
				t.Error("empty context window")
			}
		})
	}
}
