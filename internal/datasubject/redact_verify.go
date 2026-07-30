package datasubject

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

// The deterministic post-condition on a generative step.
//
// Art 17 redaction of a SHARED memory chunk cannot be a string replacement:
// "Called Jane about her scan results; she sounded relieved" still identifies Jane
// once the name is gone. So the rewrite is an LLM operation — non-deterministic, on
// a destructive path. The check is what makes that acceptable: after the model
// returns, we assert MECHANICALLY that no known identifier of the subject survives.
// If one does, the redaction failed, nothing is written, and the chunk stays
// deferred for a human.
//
// This is the same shape as Export.LeaksForeignContent re-asserting the Art 15(4)
// property on the finished artefact rather than trusting the code that built it.
//
// FAILURE DIRECTION IS DELIBERATE. A false positive defers a chunk to an operator;
// a false negative writes text that still identifies the subject and reports
// success. Every ambiguous case therefore resolves toward "deferred", which is why
// the alphanumeric comparison below is allowed to over-match.
//
// see LLD § https://docs.vornik.io §3
//
// WHAT THIS DOES NOT CATCH (§3.2), because the subject-facing report depends on the
// boundary being stated rather than assumed: pronoun and role references, Recital 26
// quasi-identifiers, reverse-searchability, and Unicode confusables. The honest claim
// is "the subject's known identifiers are provably gone, and a model was asked to
// remove the rest" — which is exactly what the report says.

// minAlnumIdentifierLen is the shortest identifier compared through the
// punctuation-stripped form. Below it, stripping produces a fragment that appears
// inside unrelated words ("jo" inside "job"), which would defer every chunk in the
// project. Short identifiers are still compared verbatim and word-wise.
const minAlnumIdentifierLen = 4

// RedactionVerificationError reports that the rewrite still contains identifiers of
// the subject it was supposed to remove.
//
// Error() deliberately does NOT include the identifiers: this is the subject's
// personal data, and an erasure path that writes it to a log has created a fresh
// copy of the thing it was asked to delete. The values live in Surviving for the
// request record, which is a controlled store the subject can be told about.
type RedactionVerificationError struct {
	// Surviving holds the identifiers found in the output, for the request record.
	Surviving []string
}

func (e *RedactionVerificationError) Error() string {
	return fmt.Sprintf("redaction verification failed: %d of the subject's identifiers "+
		"still appear in the rewritten text (values omitted from this message on "+
		"purpose; see the request record)", len(e.Surviving))
}

// VerifyRedaction reports whether output is free of every identifier in ids.
//
// Returns a *RedactionVerificationError when any identifier survives, and a plain
// error when there is nothing to verify — no identifiers, or an empty rewrite. Both
// of those are fail-closed cases rather than successes: with no identifiers there is
// no basis for the guarantee the report makes, and an empty rewrite has destroyed the
// other subjects' data that redaction exists to preserve.
func VerifyRedaction(ids []string, output string) error {
	live := liveIdentifiers(ids)
	if len(live) == 0 {
		return fmt.Errorf("redaction cannot be verified: the subject has no usable " +
			"identifiers recorded, so no guarantee can be made that their data is gone")
	}
	if strings.TrimSpace(normaliseForRedaction(output)) == "" {
		return fmt.Errorf("redaction produced empty text: that is not a redaction, it " +
			"discards the other subjects' data the chunk was kept for")
	}

	variants := textVariants(output)
	var surviving []string
	for _, id := range live {
		if identifierSurvives(id, variants) {
			surviving = append(surviving, id)
		}
	}
	if len(surviving) > 0 {
		return &RedactionVerificationError{Surviving: surviving}
	}
	return nil
}

// liveIdentifiers drops blank entries. A blank identifier is contained in every
// string, so treating it as real would defer every chunk forever.
func liveIdentifiers(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			out = append(out, id)
		}
	}
	return out
}

// identifierSurvives checks one identifier against every normalised form of the text.
func identifierSurvives(id string, variants []string) bool {
	needle := normaliseForRedaction(id)
	if needle == "" {
		return false
	}
	alnum := alnumOnly(needle)
	for _, v := range variants {
		if strings.Contains(v, needle) {
			return true
		}
		// The punctuation-stripped comparison is what catches `jane at example dot
		// com` and `jane[at]example.com`. Gated on length because a short fragment
		// matches inside unrelated words.
		if len(alnum) >= minAlnumIdentifierLen && strings.Contains(alnumOnly(v), alnum) {
			return true
		}
		// A short identifier is still caught when it stands as its own word.
		if len(alnum) > 0 && len(alnum) < minAlnumIdentifierLen && containsWord(v, needle) {
			return true
		}
	}
	return false
}

// containsWord reports whether needle appears in haystack delimited by
// non-alphanumerics, so a 2-3 character identifier matches "Jo" but not "job".
func containsWord(haystack, needle string) bool {
	for i := 0; ; {
		j := strings.Index(haystack[i:], needle)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(needle)
		beforeOK := start == 0 || !isAlnumByteBoundary(haystack, start-1)
		afterOK := end == len(haystack) || !isAlnumByteBoundary(haystack, end)
		if beforeOK && afterOK {
			return true
		}
		i = start + 1
		if i >= len(haystack) {
			return false
		}
	}
}

func isAlnumByteBoundary(s string, i int) bool {
	r := rune(s[i])
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// obfuscations maps the word-and-bracket spellings of email punctuation back to the
// punctuation, so `jane at example dot com` reduces to the address itself. Applied
// before the alphanumeric comparison, which would otherwise keep the literal "at"
// and "dot" and fail to match.
var obfuscations = []struct{ from, to string }{
	{" at ", "@"}, {"[at]", "@"}, {"(at)", "@"}, {"{at}", "@"}, {" @ ", "@"},
	{" dot ", "."}, {"[dot]", "."}, {"(dot)", "."}, {"{dot}", "."},
	{"[.]", "."}, {"(.)", "."},
}

var base64ish = regexp.MustCompile(`[A-Za-z0-9+/]{12,}={0,2}`)

// textVariants returns every normalised form of the candidate text that must be
// searched: the plain normalisation, a de-obfuscated form, and percent- and
// base64-decoded copies. Ingested content genuinely carries encoded copies of
// addresses, and an identifier hidden in one of them is not erased.
func textVariants(output string) []string {
	base := normaliseForRedaction(output)
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		if s == "" {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}

	add(base)
	add(deobfuscate(base))

	// Percent-decoding: `jane%40example.com`. Both unescapers are tried because
	// they differ on '+' handling and either may be what the source used.
	if dec, err := url.QueryUnescape(base); err == nil {
		add(normaliseForRedaction(dec))
		add(deobfuscate(normaliseForRedaction(dec)))
	}
	if dec, err := url.PathUnescape(base); err == nil {
		add(normaliseForRedaction(dec))
	}

	// Base64: decode plausible tokens rather than the whole text, since the copy is
	// typically embedded in surrounding prose. Non-text results are discarded.
	for _, tok := range base64ish.FindAllString(output, -1) {
		for _, enc := range []*base64.Encoding{
			base64.StdEncoding, base64.RawStdEncoding,
			base64.URLEncoding, base64.RawURLEncoding,
		} {
			if dec, err := enc.DecodeString(tok); err == nil && mostlyPrintable(dec) {
				add(normaliseForRedaction(string(dec)))
			}
		}
	}
	return out
}

// mostlyPrintable filters base64 decodes that produced binary rather than text, so
// random tokens do not contribute noise variants.
func mostlyPrintable(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	printable := 0
	for _, c := range b {
		if c == '\t' || c == '\n' || c == '\r' || (c >= 0x20 && c < 0x7f) {
			printable++
		}
	}
	return printable*10 >= len(b)*8
}

func deobfuscate(s string) string {
	for _, o := range obfuscations {
		s = strings.ReplaceAll(s, o.from, o.to)
	}
	return s
}

// caseFolder uses Unicode simple case folding rather than ASCII ToLower, so
// non-ASCII names fold correctly (e.g. Turkish dotless i, German ß).
var caseFolder = cases.Fold()

// normaliseForRedaction canonicalises text so formatting cannot hide an identifier:
// NFKC (collapsing full-width and ligature forms), zero-width characters stripped,
// Unicode case folding, and whitespace runs collapsed to a single space.
//
// Zero-width stripping matters more than it looks: U+200B inside an address defeats
// substring matching while being completely invisible to the reader who would
// recognise the person.
func normaliseForRedaction(s string) string {
	s = norm.NFKC.String(s)
	s = stripZeroWidth(s)
	s = caseFolder.String(s)
	return collapseWhitespace(s)
}

func stripZeroWidth(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\ufeff', '\u2060', '\u00ad':
			return -1
		}
		return r
	}, s)
}

func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// alnumOnly reduces text to letters and digits. This is what makes the comparison
// punctuation-insensitive, and it is allowed to over-match: a false positive defers
// the chunk to a human, which is the safe direction.
func alnumOnly(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// caseFoldLanguage pins the folding locale so behaviour does not vary with the
// process locale. Declared for clarity; cases.Fold is locale-independent.
var _ = language.Und
