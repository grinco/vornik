package chatauth

import "testing"

// TestKeywordLetters_SeesThroughComposerDecoration is the property the
// function exists for: a keyword wrapped in whatever a rich-text composer
// serialises back into the payload still reads as the keyword.
//
// The cases are the ones that actually reached this deployment (2026-09-15)
// plus the classes a character blacklist kept missing.
func TestKeywordLetters_SeesThroughComposerDecoration(t *testing.T) {
	for _, in := range []string{
		"link", "LINK", "Link",
		"`link", "`link`", // Slack/markdown code span — the reported incident
		"*link*", "_link_", "~link~", // bold, italic, strikethrough
		"“link”", "‘link’", // smart quotes: what the blacklist missed
		"link,", "link.", "(link)", // punctuation from prose
		"l-i-n-k", // separated; accepted, and the callers say so
		" link ",
	} {
		if got := KeywordLetters(in); got != "link" {
			t.Errorf("KeywordLetters(%q) = %q, want %q", in, got, "link")
		}
	}
}

// TestKeywordLetters_KeepsOrder is the invariant that bounds the widening, and
// the one a superstring test cannot see.
//
// "configure" != "config" passes against a letter-BAG implementation as well
// as against this one, so it does not distinguish them. An anagram does: a
// letter-bag would call "ocnfig" a match and hand every anagram of every
// keyword to the recogniser (review-20260915-2353 F3, and the same gap
// review-20260915-beff found in the link tests).
func TestKeywordLetters_KeepsOrder(t *testing.T) {
	for _, in := range []string{"ocnfig", "ifgcon", "gifnoc", "knil", "ilnk"} {
		got := KeywordLetters(in)
		if got == "config" || got == "link" {
			t.Errorf("KeywordLetters(%q) = %q — the reduce has become a letter bag, "+
				"which accepts every anagram of every keyword", in, got)
		}
	}
}

// TestKeywordLetters_DoesNotMakeNearMissesMatch — the reduce strips
// decoration, it does not shorten or extend a word. A caller comparing against
// a literal keeps its near-misses.
func TestKeywordLetters_DoesNotMakeNearMissesMatch(t *testing.T) {
	for in, want := range map[string]string{
		"configure": "configure",
		"unlink":    "unlink",
		"relink":    "relink",
		"linkx":     "linkx",
		"":          "",
		"12345":     "", // digits are not letters; a bare code reduces to nothing
		"你好":        "", // non-Latin letters are not kept, so they cannot forge a keyword
	} {
		if got := KeywordLetters(in); got != want {
			t.Errorf("KeywordLetters(%q) = %q, want %q", in, got, want)
		}
	}
}
