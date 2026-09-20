package authz

import (
	"strings"
)

// The one-way marker — oidc-identity-permissions-design §5.2b.
//
// §5.2 left one case open: a code pasted bare, or introduced by a word that is
// not "link", is indistinguishable from an ordinary prompt, so it reaches a
// model and a conversation transcript while the code stays live. It also
// declined the obvious detector, for a reason that still holds: deciding
// whether a token is a LIVE code means asking the code store about arbitrary
// input, and a surface that answers that is the redemption oracle the generic
// "code not valid" rule exists to deny.
//
// A marker moves the decision off the store. "Is this code-SHAPED" becomes a
// prefix test and an arithmetic check — local, consulting nothing — which is
// exactly the property the declined heuristic lacked.

const (
	// linkCodeMarker prefixes every issued code. It is NOT secret and adds no
	// entropy; it exists so recognition is local.
	//
	// 'L' is absent from linkCodeAlphabet (it is one of the characters dropped
	// as visually ambiguous), so no legitimate code body can begin with this
	// prefix. That is what makes stripping it unambiguous, and what lets
	// pre-marker codes keep redeeming unchanged. A test pins the dependency.
	linkCodeMarker = "VLK"

	// linkCodeRedacted replaces a code-shaped token before the text reaches a
	// model. It says what happened, because a silently altered prompt is its
	// own kind of surprise.
	linkCodeRedacted = "[link code removed]"
)

// MarkLinkCode renders a code body in its presented form: marker, body, check
// character. The body keeps the unambiguous alphabet; the check character comes
// from the full 0-9A-Z set, because ISO 7064 Mod 37,36 is defined over it.
func MarkLinkCode(body string) string {
	return linkCodeMarker + body + string(iso7064Mod3736Check(body))
}

// splitMarkedLinkCode returns the body of a marked code, and whether the token
// is code-shaped at all. It consults nothing.
func splitMarkedLinkCode(token string) (string, bool) {
	filtered := filterAlnumUpper(token)
	if !strings.HasPrefix(filtered, linkCodeMarker) {
		return "", false
	}
	rest := filtered[len(linkCodeMarker):]
	if len(rest) != linkCodeLength+1 {
		return "", false
	}
	body, check := rest[:linkCodeLength], rest[linkCodeLength]
	for i := 0; i < len(body); i++ {
		if !strings.ContainsRune(linkCodeAlphabet, rune(body[i])) {
			return "", false
		}
	}
	if iso7064Mod3736Check(body) != check {
		return "", false
	}
	return body, true
}

// linkCodeBodyForRedemption recovers the body a REDEEMER meant, which is a
// different question from "is this code-shaped" and gets a different answer.
//
// Recognition must be strict: it runs over every word of every ordinary prompt,
// so a false positive scrubs a message nobody was trying to redeem.
// Redemption is reached only when someone deliberately typed the link command,
// so it can afford to tolerate a mistyped MARKER — 'L' is one of the characters
// people misread, and a prefix typo that failed closed would be a usability
// regression the marker introduced for nothing. The CHECK CHARACTER is what
// keeps that tolerance honest: the middle is accepted only if it validates, so
// this cannot turn an arbitrary 12-character token into a redemption attempt
// for something else.
func linkCodeBodyForRedemption(normalised string) string {
	if body, ok := splitMarkedLinkCode(normalised); ok {
		return body
	}
	if len(normalised) == len(linkCodeMarker)+linkCodeLength+1 {
		middle := normalised[len(linkCodeMarker) : len(linkCodeMarker)+linkCodeLength]
		check := normalised[len(normalised)-1]
		if iso7064Mod3736Check(middle) == check {
			return middle
		}
	}
	return normalised
}

// LooksLikeLinkCode reports whether a token is code-SHAPED. It never says
// whether the code exists, is live, or was ever issued — the store is not
// consulted, so there is no oracle to probe.
func LooksLikeLinkCode(token string) bool {
	_, ok := splitMarkedLinkCode(token)
	return ok
}

// ScrubLinkCodes replaces every code-shaped token in text and returns the
// tokens it removed, so the caller can revoke them.
//
// Scrubbing happens BEFORE dispatch: the model never sees the token, so it
// cannot reach a completion, a transcript, or the provider. Revocation is the
// caller's job because it needs the store, and this function deliberately does
// not.
func ScrubLinkCodes(text string) (string, []string) {
	if text == "" {
		return text, nil
	}
	fields := strings.Fields(text)
	var found []string
	for _, f := range fields {
		if LooksLikeLinkCode(f) {
			found = append(found, f)
		}
	}
	if len(found) == 0 {
		// Return the input unchanged rather than a re-joined copy: an ordinary
		// prompt must survive this path byte for byte, including whatever
		// spacing the speaker used.
		return text, nil
	}
	scrubbed := text
	for _, f := range found {
		scrubbed = strings.ReplaceAll(scrubbed, f, linkCodeRedacted)
	}
	return scrubbed, found
}

// iso7064Mod3736Check computes the ISO 7064 Mod 37,36 check character over a
// string of 0-9A-Z.
//
// NAMED IN THE DESIGN RATHER THAN CHOSEN HERE, because "a check character" is
// the kind of blank a hurried implementation fills with a sum mod 10 that
// misses a transposition. Mod 37,36 detects every single-character error and
// every adjacent transposition, and it needs no crypto — which is right,
// because this is false-positive suppression, not integrity protection. An
// attacker who can guess the body can compute the character; that is expected
// and costs nothing, since guessing the body is already the whole work of
// redeeming the code.
func iso7064Mod3736Check(s string) byte {
	const modulus, radix = 36, 37
	p := modulus
	for i := 0; i < len(s); i++ {
		v := alnumValue(s[i])
		if v < 0 {
			continue
		}
		sum := (p + v) % modulus
		if sum == 0 {
			sum = modulus
		}
		p = (2 * sum) % radix
	}
	return alnumChar((radix - p) % modulus)
}

func alnumValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'Z':
		return int(c-'A') + 10
	default:
		return -1
	}
}

func alnumChar(v int) byte {
	if v < 10 {
		return byte('0' + v)
	}
	return byte('A' + v - 10)
}

// filterAlnumUpper is the normalisation hashLinkCode already applies: upper
// case, keep [A-Z0-9], drop everything else. Shared rather than repeated,
// because a code rule with two implementations has one that drifts.
func filterAlnumUpper(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
