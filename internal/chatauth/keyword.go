package chatauth

import "strings"

// KeywordLetters reduces a chat field to its letters, lower-cased, so a
// command keyword is recognised through whatever decoration a composer wrapped
// it in.
//
// It lives here rather than in one channel package because BOTH channels and
// BOTH keywords need it: §5.2's `link <code>` redemption and the configuration
// assistant's `config <request>` entrypoint (config-assistant design §6.3.3).
// It was `slack.linkKeyword` until 2026-09-15 — already general, since the
// comparison to the literal keyword happens at the call site, but named as
// though it were not (review-20260915-2353 F4). A second copy would be the
// two-implementations shape, and the copy that drifts is the one nobody
// exercises.
//
// WHY LETTERS, and not a list of characters to strip. The first version of the
// link recogniser trimmed a fixed cutset — backtick, asterisk, underscore,
// tilde — and was wrong in a way that is worth stating wherever this function
// is read. Slack's composer preserves formatting on paste and serialises it
// back into the payload, and the set of characters that can arrive that way is
// not enumerable: "“link ACDE2345”" with smart quotes still declined.
// Declining is not harmless on these paths, because the text then carries on
// to the dispatcher — for a link code, that puts a live one-time credential
// into a model prompt and a conversation transcript with nothing reporting it.
// Every character absent from the list was another silent instance of the same
// leak, so the list could only ever be caught up with, never finished.
//
// Keeping letters ends the class instead of one member of it: nothing that is
// not a letter can change whether a field reads as its keyword.
//
// WHAT IT DELIBERATELY DOES NOT DO: it does not sort, dedupe or otherwise
// normalise ORDER. "ocnfig" and "config" are different, and must stay
// different — a letter-bag would accept every anagram of a keyword, which is a
// much wider recogniser than any caller wants. Callers pin this with an
// anagram near-miss, because a superstring test ("configure") passes against a
// letter-bag implementation too and so cannot tell the two apart.
func KeywordLetters(field string) string {
	var b strings.Builder
	b.Grow(len(field))
	for _, r := range strings.ToLower(field) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
