package voice

import (
	"regexp"
	"strings"
)

// ForSpeech converts chat-formatted text into something worth listening to.
//
// OPERATOR REQUEST 2026-07-30: rich text is wanted in Slack and Telegram — bold,
// links, emoji — with one stated exception: "when rendering the text as audio file
// for voice conversations it needs to be converted to plain text and handled with
// more care (also links probably shouldn't be dictated)."
//
// This runs on the TTS path only. Nothing here touches what a reader sees; it exists
// because a synthesiser handed `*Finished* — <https://…|the report>` will happily
// pronounce the asterisks and then spell a URL character by character, which is the
// difference between a voice note someone plays and one they never play twice.
//
// The rules, in the order they have to apply:
//
//   - A LABELLED link becomes its label. `<https://x|the report>` → "the report".
//     The label is what a human would have said out loud anyway.
//   - A BARE url is not dictated. On its own line the whole line goes, because a
//     line that was only a link contributes nothing once the link is gone; inline it
//     becomes "a link" so the surrounding sentence still parses.
//   - Slack ENTITY references (`<@U123>`, `<#C456>`) go entirely. We hold the id,
//     not the name, and "U zero B L P" is worse than silence.
//   - EMOJI, both `:shortcode:` and literal glyphs, go. A synthesiser either skips
//     them or says something absurd, and neither is worth the risk.
//   - FORMATTING markers go, their words stay.
//
// Deliberately NOT done: sentence rewriting, abbreviation expansion, or number
// normalisation. Those are the synthesiser's job and guessing at them here would
// fight whatever the provider already does well.
func ForSpeech(text string) string {
	s := text

	// Labelled links first, so the generic URL rules below cannot eat their labels.
	s = reSlackLabelledLink.ReplaceAllString(s, "$1")
	s = reMarkdownLink.ReplaceAllString(s, "$1")

	// Slack entity refs, before the bare-angle-bracket URL rule.
	s = reSlackEntity.ReplaceAllString(s, "")

	// A line that is nothing but a link says nothing once unspoken.
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		bare := strings.Trim(trimmed, "<>")
		if trimmed != "" && reBareURL.MatchString(bare) && len(reBareURL.FindString(bare)) == len(bare) {
			continue
		}
		kept = append(kept, line)
	}
	s = strings.Join(kept, "\n")

	// Inline URLs keep the sentence intact without being spelled out.
	s = reAngleURL.ReplaceAllString(s, "a link")
	s = reBareURL.ReplaceAllString(s, "a link")

	s = reEmojiShortcode.ReplaceAllString(s, "")
	s = reEmojiGlyph.ReplaceAllString(s, "")

	// Formatting markers. Code fences before inline backticks so a fenced block's
	// language hint does not survive as a word.
	s = reCodeFence.ReplaceAllString(s, "")
	s = reHeading.ReplaceAllString(s, "")
	s = reQuote.ReplaceAllString(s, "")
	s = reBullet.ReplaceAllString(s, "")
	s = reMarkers.ReplaceAllString(s, "")
	s = stripEmphasisUnderscores(s)

	return collapseSpeechWhitespace(s)
}

var (
	// <https://example.com|label> — Slack's mrkdwn link.
	reSlackLabelledLink = regexp.MustCompile(`<https?://[^>|\s]+\|([^>]*)>`)
	// [label](https://example.com) — Markdown, which Telegram and email use.
	reMarkdownLink = regexp.MustCompile(`\[([^\]]*)\]\(\s*https?://[^)\s]+\s*\)`)
	// <@U123>, <#C456|name>, <!here> — ids and specials, never names.
	reSlackEntity = regexp.MustCompile(`<[@#!][^>]*>`)
	// A URL wrapped in angle brackets, Slack's auto-link form.
	reAngleURL = regexp.MustCompile(`<https?://[^>\s]+>`)
	reBareURL  = regexp.MustCompile(`https?://[^\s<>|]+`)

	reEmojiShortcode = regexp.MustCompile(`:[a-z0-9_+\-]+:`)
	// Pictographic ranges plus variation selectors and ZWJ, which otherwise leave
	// stray joiners behind after the glyphs go.
	reEmojiGlyph = regexp.MustCompile(`[\x{1F000}-\x{1FAFF}\x{2600}-\x{27BF}\x{FE0F}\x{200D}\x{2190}-\x{21FF}\x{2B00}-\x{2BFF}]`)

	reCodeFence = regexp.MustCompile("(?m)^[ \t]{0,3}```[a-zA-Z0-9]*[ \t]*$\n?")
	reHeading   = regexp.MustCompile(`(?m)^[ \t]{0,3}#{1,6}[ \t]*`)
	reQuote     = regexp.MustCompile(`(?m)^[ \t]{0,3}>[ \t]*`)
	reBullet    = regexp.MustCompile(`(?m)^[ \t]{0,3}[-*•+][ \t]+`)
	// Emphasis, strikethrough and inline code markers. NOT underscore: see
	// stripEmphasisUnderscores for why that one cannot be a blanket delete.
	reMarkers = regexp.MustCompile("[*~`]")
)

// stripEmphasisUnderscores removes underscores used as Slack italic delimiters while
// keeping the ones inside identifiers.
//
// A blanket delete turned `task_42` into "task42" and `memory_search` into
// "memorysearch" — caught by test. That matters more than it looks: a task id read aloud
// is how someone quotes it back, and the completion notice is built around one.
//
// The rule is positional: an underscore BETWEEN two alphanumerics is part of a word;
// anywhere else it is punctuation Slack used for emphasis.
func stripEmphasisUnderscores(s string) string {
	runes := []rune(s)
	out := make([]rune, 0, len(runes))
	for i, r := range runes {
		if r != '_' {
			out = append(out, r)
			continue
		}
		var prev, next rune
		if i > 0 {
			prev = runes[i-1]
		}
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		if isWordRune(prev) && isWordRune(next) {
			out = append(out, r)
		}
	}
	return string(out)
}

func isWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// collapseSpeechWhitespace tidies what the substitutions left behind: a synthesiser
// pauses on blank lines, so a message reduced to formatting scaffolding should not be
// read as a series of silences.
func collapseSpeechWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" && len(out) > 0 && out[len(out)-1] == "" {
			continue // never more than one blank line in a row
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
