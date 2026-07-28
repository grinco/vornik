package email

import (
	"regexp"
	"strings"
)

// Quoted-trailer detection. Mail clients append the message being replied to
// underneath an attribution line; the reply's genuinely-new content is
// everything above it. Feeding the trailer to the LLM caused a concrete
// failure (incident 2026-07-28): a correspondent replied "4" to choose from a
// numbered list, and the lead — which has no idea what its own address is —
// read the quoted copy of its own previous reply as a third party's statement
// and asked the user what they meant.
//
// The patterns below are deliberately anchored to line starts and kept to the
// separators that real clients emit. Over-matching is the dangerous direction:
// a false positive silently truncates a user's actual question, which is worse
// than a verbose prompt. Anything ambiguous is left alone.
var quoteMarkers = []*regexp.Regexp{
	// Outlook "Original Message" divider, English + localised. Outlook
	// translates the phrase but keeps the ASCII dashes, so an English-only
	// pattern silently leaves the whole trailer in place for a
	// German/French/Czech correspondent (companion review finding 1c).
	// Extend this list rather than loosening the surrounding dashes.
	regexp.MustCompile(`(?m)^-{2,}\s*(?i:` + strings.Join([]string{
		`Original Message`,
		`Ursprüngliche Nachricht`, // de
		`Message d'origine`,       // fr
		`Mensaje original`,        // es
		`Messaggio originale`,     // it
		`Původní zpráva`,          // cs
		`Oorspronkelijk bericht`,  // nl
		`Wiadomość oryginalna`,    // pl
		`Ursprungligt meddelande`, // sv
		`元のメッセージ`,                 // ja
	}, `|`) + `)\s*-{2,}\s*$`),
	// Outlook web / OWA divider: a run of underscores, then the quoted
	// headers. Matched on the divider alone — it only appears in this role.
	regexp.MustCompile(`(?m)^_{10,}\s*$`),
	// Gmail's forward divider.
	regexp.MustCompile(`(?m)^-{2,}\s*Forwarded message\s*-{2,}\s*$`),
	// Some clients emit a bare "From: ..." header block with no divider. Only
	// treat it as a boundary when the next line is one of the sibling headers,
	// which a prose line starting with "From:" would not be.
	regexp.MustCompile(`(?m)^From:\s.*\r?\n(Sent|Date|To|Subject):\s`),
}

// attributionLine matches the Gmail / Apple Mail / Thunderbird / Roundcube
// "On <date>, <someone> wrote:" line. Requires the line to START with "On" and
// END with "wrote:" so a mid-sentence "wrote" survives. [\s\S] rather than .
// because Gmail wraps this line at ~78 columns.
//
// Matching the shape alone is NOT sufficient: ordinary prose can hit it
// ("On the matter of the bug report, John wrote:"), and truncating there eats
// the user's real question — the one failure direction this file must never
// take (companion review finding 1a; regression-tested). Candidates are
// therefore validated by isAttribution below.
//
// The trailing class must include \r: real mail is CRLF-terminated, and Go's
// (?m)$ matches before \n but not before \r, so [ \t]*$ silently stops
// matching every genuine wire-format message (caught by the end-to-end
// poll-loop test, which feeds real CRLF bytes).
var attributionLine = regexp.MustCompile(`(?m)^On\s[\s\S]{0,200}?\swrote:[ \t\r]*$`)

// isAttribution reports whether a line matching attributionLine is really a
// client-generated attribution rather than prose that happens to fit.
//
// Discriminator: every real attribution carries a date or timestamp, so the
// span must contain a digit. "On Tue, Jul 28, 2026 at 3:04 PM … wrote:" and
// "On 28 Jul 2026, at 15:04, … wrote:" qualify; "On the matter of the bug
// report, John wrote:" does not. The span is also capped at one newline —
// Gmail wraps at most once, whereas an unbounded [\s\S] run could otherwise
// bridge a prose "On …" line to a "wrote:" many lines below it.
func isAttribution(span string) bool {
	if strings.Count(span, "\n") > 1 {
		return false
	}
	return strings.ContainsAny(span, "0123456789")
}

// leadingQuoteRun matches a trailing block of quote-prefixed lines (with any
// blank lines between them). Catches clients that quote without an
// attribution line at all.
var leadingQuoteRun = regexp.MustCompile(`(?m)^>.*(?:\r?\n(?:>.*|[ \t]*))*$`)

// StripQuotedReply returns body with the quoted-reply trailer removed, so the
// dispatcher's user turn carries only what the correspondent actually typed
// on this turn.
//
// Safety contract, in priority order:
//
//  1. Never return empty for non-empty input. When a marker matches at the
//     very top (a reply with no new text, or a quote-only forward), the
//     original body is returned untouched — an over-long prompt beats an
//     empty user turn that the LLM cannot act on at all.
//  2. Never truncate on an ambiguous match. Every pattern is line-anchored
//     and structural; prose that merely mentions "wrote:" or begins a line
//     with "From:" is left intact.
//  3. Idempotent: stripping already-stripped text is a no-op.
func StripQuotedReply(body string) string {
	if strings.TrimSpace(body) == "" {
		return body
	}

	cut := len(body)
	for _, re := range quoteMarkers {
		if loc := re.FindStringIndex(body); loc != nil && loc[0] < cut {
			cut = loc[0]
		}
	}
	// Attribution lines need the extra validation pass, so walk every
	// candidate and take the earliest one that is genuinely an attribution —
	// a prose false positive must not shadow a real attribution below it.
	for _, loc := range attributionLine.FindAllStringIndex(body, -1) {
		if loc[0] < cut && isAttribution(body[loc[0]:loc[1]]) {
			cut = loc[0]
			break
		}
	}

	// A trailing run of ">"-prefixed lines is a quote even with no
	// attribution. Only consider runs that reach the end of the body so an
	// inline quotation the user is writing *around* survives.
	if loc := leadingQuoteRun.FindStringIndex(body); loc != nil &&
		strings.TrimSpace(body[loc[1]:]) == "" && loc[0] < cut {
		cut = loc[0]
	}

	if cut == len(body) {
		// No marker found — return the body with trailing blank lines
		// trimmed so the "unchanged" path still normalises whitespace.
		return strings.TrimRight(body, " \t\r\n")
	}

	stripped := strings.TrimRight(body[:cut], " \t\r\n")
	if stripped == "" {
		// Safety rule 1: the whole body was quote. Keep it.
		return strings.TrimRight(body, " \t\r\n")
	}
	return stripped
}
