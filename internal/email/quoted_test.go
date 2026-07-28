package email

import "testing"

// Regression: incident 2026-07-28 — "the bot doesn't know its own email
// identity". A correspondent replied "4" to pick an option from the bot's
// numbered list; the mail client appended the bot's entire previous reply as
// a quoted trailer. The channel handed the whole thing to the LLM verbatim
// (channel.go buildChannelMessage: `Text: parsed.Body`), so the lead read its
// OWN quoted words as a third party's statement and answered "I see
// bot@vornik.io said ... — what did you want?" instead of acting on "4".
func TestStripQuotedReply(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "gmail attribution then quoted block",
			in: "4\n\nOn Tue, Jul 28, 2026 at 3:04 PM Vornik Assistant <bot@vornik.io> wrote:\n" +
				"> You tried uploading an epub, which I don't know how to process.\n" +
				"> Here is what I can do instead:\n> 1. ...\n> 4. ...\n",
			want: "4",
		},
		{
			name: "outlook original-message separator",
			in: "Please use option 2.\n\n-----Original Message-----\nFrom: bot@vornik.io\n" +
				"Sent: Tuesday, July 28, 2026\nSubject: Re: add these books to rag\n\nWhich would you like?\n",
			want: "Please use option 2.",
		},
		{
			name: "outlook horizontal rule then From header",
			in: "yes please\n\n________________________________\nFrom: Vornik <bot@vornik.io>\n" +
				"Sent: 28 July 2026 15:04\nTo: janka\n\nearlier body\n",
			want: "yes please",
		},
		{
			name: "apple mail attribution without angle brackets",
			in:   "option 4\n\nOn 28 Jul 2026, at 15:04, Vornik Assistant wrote:\n\n> pick one\n",
			want: "option 4",
		},
		{
			name: "bare quoted run with no attribution line",
			in:   "4\n\n> You tried uploading an epub\n> 1. ...\n",
			want: "4",
		},
		{
			name: "forwarded message separator",
			in:   "see below\n\n---------- Forwarded message ---------\nFrom: someone\n\nbody\n",
			want: "see below",
		},
		// Review finding 1a (companion review 2026-07-28, task
		// task_20260728155503_6b72e4e9edd87f03): a prose line that starts with
		// "On" and ends with "wrote:" but carries no date is NOT an
		// attribution line. The first cut of the pattern truncated everything
		// after it — silently eating the user's actual question, which is the
		// one failure direction this function must never take.
		{
			name: "prose line starting On and ending wrote: does not truncate",
			in: "Here is my question about the review.\n\n" +
				"On the matter of the bug report, John wrote:\nplease fix the null pointer.\n\n" +
				"What should I do about the second item?",
			want: "Here is my question about the review.\n\n" +
				"On the matter of the bug report, John wrote:\nplease fix the null pointer.\n\n" +
				"What should I do about the second item?",
		},
		{
			name: "attribution wrapped across two lines is still stripped",
			in: "sounds good\n\nOn Tue, Jul 28, 2026 at 3:04 PM Vornik Assistant\n" +
				"<bot@vornik.io> wrote:\n> earlier body\n",
			want: "sounds good",
		},
		// Review finding 1c: Outlook emits the separator localised. Czech
		// matters directly here — the operator's correspondents are in CZ.
		{
			name: "localised outlook separator (czech)",
			in:   "ano, prosím\n\n-----Původní zpráva-----\nFrom: bot@vornik.io\n\nearlier\n",
			want: "ano, prosím",
		},
		{
			name: "localised outlook separator (german)",
			in:   "ja bitte\n\n-----Ursprüngliche Nachricht-----\nFrom: bot@vornik.io\n\nearlier\n",
			want: "ja bitte",
		},
		{
			name: "localised outlook separator (french)",
			in:   "oui merci\n\n-----Message d'origine-----\nFrom: bot@vornik.io\n\nearlier\n",
			want: "oui merci",
		},
		// Real mail is CRLF-terminated. Go's (?m)$ matches before \n but not
		// before \r, so a trailing character class that omits \r stops
		// matching every genuine wire-format message.
		{
			name: "crlf line endings are handled",
			in: "4\r\n\r\nOn Tue, Jul 28, 2026 at 3:04 PM Vornik <bot@vornik.io> wrote:\r\n" +
				"> earlier reply\r\n",
			want: "4",
		},
		{
			name: "crlf outlook separator",
			in:   "yes\r\n\r\n-----Original Message-----\r\nFrom: bot@vornik.io\r\nSent: today\r\n",
			want: "yes",
		},
		{
			name: "no quoting is returned unchanged",
			in:   "Please add these books to RAG.\n\nThanks,\nJanka",
			want: "Please add these books to RAG.\n\nThanks,\nJanka",
		},
		{
			name: "body that merely mentions wrote is not truncated",
			in:   "I wrote: the report is done. Please review.",
			want: "I wrote: the report is done. Please review.",
		},
		{
			name: "quote-only body keeps the quote rather than going empty",
			in:   "On Tue, Jul 28, 2026 at 3:04 PM Vornik <bot@vornik.io> wrote:\n> pick one\n",
			want: "On Tue, Jul 28, 2026 at 3:04 PM Vornik <bot@vornik.io> wrote:\n> pick one",
		},
		{
			name: "empty body stays empty",
			in:   "",
			want: "",
		},
		{
			name: "inline reply above the attribution is preserved whole",
			in: "Answers inline.\n\nFirst one is fine.\nSecond one too.\n\n" +
				"On Tue, Jul 28, 2026 at 3:04 PM Vornik <bot@vornik.io> wrote:\n> two questions\n",
			want: "Answers inline.\n\nFirst one is fine.\nSecond one too.",
		},
		{
			name: "trailing whitespace-only lines are trimmed",
			in:   "4\n\n\n",
			want: "4",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripQuotedReply(tc.in); got != tc.want {
				t.Errorf("StripQuotedReply()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// StripQuotedReply must never destroy the only content in a message. If the
// stripper's markers match at the very top, the caller is better served by an
// over-long prompt than by an empty user turn.
func TestStripQuotedReplyNeverReturnsEmptyForNonEmptyInput(t *testing.T) {
	inputs := []string{
		"> just a quote",
		"On Tue, Jul 28, 2026, someone wrote:",
		"-----Original Message-----",
		"   \n> quoted\n",
	}
	for _, in := range inputs {
		if got := StripQuotedReply(in); got == "" {
			t.Errorf("StripQuotedReply(%q) returned empty; want non-empty fallback", in)
		}
	}
}
