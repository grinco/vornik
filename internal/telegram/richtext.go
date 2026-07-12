package telegram

import (
	"regexp"
	"strings"
)

// richtext.go — turns a plain bot message into tap-to-copy-friendly Telegram
// HTML. Two things become <code> spans (which Telegram renders monospace with
// a one-tap copy affordance):
//
//   - markdown `backtick` spans — so an agent (or the system) can mark a value
//     like a PageDrop viewing password as copyable just by wrapping it in
//     backticks. This runs AFTER the outbound secret-redactor, so it never
//     weakens redaction: a secret the redactor catches is already [REDACTED]
//     by the time we get here; only a value that legitimately survives (and is
//     backtick-marked) becomes copyable.
//   - recognised bot commands (/status <id>, /tasks, …) — so the command a
//     message references can be copied and re-sent without retyping.
//
// A message with nothing to wrap is returned unchanged and plain (no parse
// mode), so the vast majority of traffic is untouched.

// botCommands is the recognised slash-command set. Only these are wrapped, so
// a filesystem path like /tmp/x or an "and/or" is never mistaken for a command.
var botCommands = map[string]bool{
	"help": true, "inbox": true, "new": true, "project": true, "reset": true,
	"start": true, "autopilot": true, "cancel": true, "context": true,
	"forget": true, "link": true, "load": true, "pin": true, "retry": true,
	"save": true, "search": true, "status": true, "summarize": true,
	"tasks": true, "undo": true, "verbose": true,
}

// commandRE matches "/word" optionally followed by one ID-like argument token,
// so "/status task_123" is wrapped whole (copy → resend as-is). The argument
// must contain a digit, underscore, or colon so an ordinary following word
// ("/tasks for the list") is not swallowed as an argument.
var commandRE = regexp.MustCompile(`/([a-z_]+)(?: [A-Za-z.-]*[0-9_:][A-Za-z0-9._:-]*)?`)

// inlineCodeRE matches a single-line markdown backtick span.
var inlineCodeRE = regexp.MustCompile("`([^`\n]+)`")

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// wrapCommands HTML-escapes s and wraps any recognised bot command in <code>.
// Returns the rendered fragment and whether any command was wrapped.
func wrapCommands(s string) (string, bool) {
	var b strings.Builder
	changed := false
	last := 0
	for _, loc := range commandRE.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(htmlEscape(s[last:loc[0]]))
		match := s[loc[0]:loc[1]]
		name := s[loc[2]:loc[3]]
		if botCommands[name] {
			b.WriteString("<code>")
			b.WriteString(htmlEscape(match))
			b.WriteString("</code>")
			changed = true
		} else {
			b.WriteString(htmlEscape(match))
		}
		last = loc[1]
	}
	b.WriteString(htmlEscape(s[last:]))
	return b.String(), changed
}

// renderTelegramHTML renders text to Telegram HTML with backtick spans and bot
// commands as <code>. The bool reports whether any <code> was emitted — the
// caller sets parse_mode=HTML only then, leaving untouched messages plain.
func renderTelegramHTML(text string) (string, bool) {
	var b strings.Builder
	changed := false
	last := 0
	for _, loc := range inlineCodeRE.FindAllStringSubmatchIndex(text, -1) {
		frag, _ := wrapCommands(text[last:loc[0]]) // gap before the code span
		b.WriteString(frag)
		// A code span is always a change, so the gap's own result is moot here.
		b.WriteString("<code>")
		b.WriteString(htmlEscape(text[loc[2]:loc[3]]))
		b.WriteString("</code>")
		changed = true
		last = loc[1]
	}
	frag, fc := wrapCommands(text[last:])
	b.WriteString(frag)
	changed = changed || fc
	if !changed {
		return text, false // nothing to wrap — send plain, unescaped
	}
	return b.String(), true
}
