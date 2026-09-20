package authz

import (
	"strings"
	"testing"
)

// oidc-identity-permissions-design §5.2b. A link code pasted bare — or
// introduced by a word that is not "link" — was indistinguishable from an
// ordinary prompt, so it was dispatched to a model and landed in a conversation
// transcript, and the code stayed live until it expired.
//
// §5.2 declined the obvious detector for a good reason: deciding whether a
// token is a LIVE code means asking the code store about arbitrary input, and a
// surface that answers that is the redemption oracle the generic "code not
// valid" rule exists to deny. The marker makes code-SHAPE a purely local
// decision — a prefix and an arithmetic check, consulting nothing.

func TestMarkLinkCode_RoundTrips(t *testing.T) {
	body := "ACDE2346"
	marked := MarkLinkCode(body)

	if !strings.HasPrefix(marked, linkCodeMarker) {
		t.Fatalf("marked code lacks the marker: %q", marked)
	}
	got, ok := splitMarkedLinkCode(marked)
	if !ok {
		t.Fatalf("a freshly marked code did not validate: %q", marked)
	}
	if got != body {
		t.Fatalf("round trip changed the body: want %q, got %q", body, got)
	}
}

// The check character is a function OF the body, so a mistyped body almost
// always fails to validate. That is its whole job: false-positive suppression,
// so an ordinary eight-character token in prose is not mistaken for a code.
func TestSplitMarkedLinkCode_RejectsACorruptedBody(t *testing.T) {
	marked := MarkLinkCode("ACDE2346")
	corrupted := marked[:len(marked)-2] + string(marked[len(marked)-2]+1) + marked[len(marked)-1:]

	if _, ok := splitMarkedLinkCode(corrupted); ok {
		t.Fatalf("a corrupted body validated: %q", corrupted)
	}
}

// ISO 7064 Mod 37,36 detects every single-character substitution and every
// adjacent transposition. Both are what a human retyping a code actually does.
func TestCheckCharacter_CatchesSubstitutionsAndTranspositions(t *testing.T) {
	body := "ACDE2346"
	marked := MarkLinkCode(body)

	// Every single-character substitution in the body.
	for i := 0; i < len(body); i++ {
		for _, r := range linkCodeAlphabet {
			if byte(r) == body[i] {
				continue
			}
			mutated := []byte(body)
			mutated[i] = byte(r)
			if _, ok := splitMarkedLinkCode(linkCodeMarker + string(mutated) + string(marked[len(marked)-1])); ok {
				t.Fatalf("substitution at %d (%c) went undetected", i, r)
			}
		}
	}

	// Every adjacent transposition of two DIFFERENT characters.
	for i := 0; i+1 < len(body); i++ {
		if body[i] == body[i+1] {
			continue
		}
		swapped := []byte(body)
		swapped[i], swapped[i+1] = swapped[i+1], swapped[i]
		if _, ok := splitMarkedLinkCode(linkCodeMarker + string(swapped) + string(marked[len(marked)-1])); ok {
			t.Fatalf("transposition at %d went undetected", i)
		}
	}
}

// Recognition consults NOTHING. This is the property that makes the marker
// acceptable where §5.2's declined heuristic was not.
func TestLooksLikeLinkCode_IsLocalAndPrecise(t *testing.T) {
	marked := MarkLinkCode("ACDE2346")

	for _, yes := range []string{marked, strings.ToLower(marked), "  " + marked + ",", "`" + marked + "`"} {
		if !LooksLikeLinkCode(yes) {
			t.Fatalf("a code-shaped token was not recognised: %q", yes)
		}
	}
	// The prose §5.2 refused to swallow. "summarise ACDE2345" is two fields
	// and eight alphanumerics, and it must stay an ordinary prompt.
	for _, no := range []string{"ACDE2345", "summarise", "VLKNOTACODE", "", "VLK", linkCodeMarker + "ACDE2346Z9"} {
		if LooksLikeLinkCode(no) {
			t.Fatalf("an ordinary token was mistaken for a code: %q", no)
		}
	}
}

// The scrubber replaces every code-shaped token and reports what it found, so
// the caller can revoke. The model must never see the token.
func TestScrubLinkCodes_ReplacesAndReports(t *testing.T) {
	marked := MarkLinkCode("ACDE2346")
	text := "here is the thing " + marked + " please use it"

	scrubbed, found := ScrubLinkCodes(text)

	if strings.Contains(scrubbed, marked) {
		t.Fatalf("the code survived scrubbing: %q", scrubbed)
	}
	if !strings.Contains(scrubbed, linkCodeRedacted) {
		t.Fatalf("scrubbed text does not mark the removal: %q", scrubbed)
	}
	if len(found) != 1 || found[0] != marked {
		t.Fatalf("want the code reported for revocation, got %v", found)
	}
	// The surrounding prompt is untouched — this path must not quietly rewrite
	// an ordinary message.
	if !strings.Contains(scrubbed, "here is the thing") || !strings.Contains(scrubbed, "please use it") {
		t.Fatalf("scrubbing altered the surrounding text: %q", scrubbed)
	}
}

func TestScrubLinkCodes_LeavesOrdinaryTextExactlyAsItWas(t *testing.T) {
	text := "summarise ACDE2345 and the VLK report"
	scrubbed, found := ScrubLinkCodes(text)

	if scrubbed != text {
		t.Fatalf("ordinary text was altered: %q", scrubbed)
	}
	if len(found) != 0 {
		t.Fatalf("ordinary text produced revocation candidates: %v", found)
	}
}

// Redemption must accept both shapes for as long as unmarked codes can still be
// live, and there must be ONE implementation of the code rule — the marker is
// stripped at hashLinkCode exactly as the legibility dashes already are.
func TestHashLinkCode_MarkedAndBareFormsAgree(t *testing.T) {
	body := "ACDE2346"
	if hashLinkCode(MarkLinkCode(body)) != hashLinkCode(body) {
		t.Fatal("a marked code and its bare body hash differently; redemption would split in two")
	}
	// Dashed and lower-cased marked forms too, since that is what a human types.
	marked := MarkLinkCode(body)
	dashed := marked[:3] + "-" + marked[3:7] + "-" + marked[7:]
	if hashLinkCode(strings.ToLower(dashed)) != hashLinkCode(body) {
		t.Fatalf("a dashed, lower-cased marked code does not redeem: %q", dashed)
	}
}

// A pre-marker code must keep redeeming: they are live for up to their TTL
// across the upgrade, and the class closes as they expire rather than by a
// migration.
func TestHashLinkCode_UnmarkedCodesAreUnaffected(t *testing.T) {
	// An old code can never begin with the marker, because 'L' is absent from
	// linkCodeAlphabet — which is what makes stripping the prefix unambiguous.
	if strings.ContainsRune(linkCodeAlphabet, 'L') {
		t.Fatal("the marker's unambiguity rests on 'L' being absent from the code alphabet")
	}
	bare := "ACDE2346"
	if hashLinkCode(bare) != hashLinkCode(strings.ToLower(bare)) {
		t.Fatal("bare-code normalisation changed")
	}
}

// Generated codes carry the marker, so the class closes at issuance.
func TestNewLinkCode_IsMarked(t *testing.T) {
	code, err := newLinkCode()
	if err != nil {
		t.Fatal(err)
	}
	if !LooksLikeLinkCode(code) {
		t.Fatalf("a freshly issued code is not recognisable as one: %q", code)
	}
	body, ok := splitMarkedLinkCode(code)
	if !ok || len(body) != linkCodeLength {
		t.Fatalf("issued code has the wrong shape: %q (body %q)", code, body)
	}
	for _, r := range body {
		if !strings.ContainsRune(linkCodeAlphabet, r) {
			t.Fatalf("issued body uses a character outside the unambiguous alphabet: %q", body)
		}
	}
}
