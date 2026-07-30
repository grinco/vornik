package conversation

import "strings"

// Link renders a hyperlink in the syntax the named channel understands.
//
// OPERATOR REQUEST 2026-07-30: "the giant link could have been a hyperlink on the
// 'open this link' part of the message". A bare URL in a chat message is both ugly
// and unreadable — a prefilled GitHub issue URL is over a thousand characters of
// percent-encoding, and Slack renders every one of them.
//
// Per-channel because the syntaxes genuinely differ and there is no lowest common
// denominator worth having:
//
//   - Slack mrkdwn: <url|label>
//   - Telegram / email / anything Markdown-shaped: [label](url)
//   - Unknown channel: "label (url)", which reads acceptably as plain text and
//     never renders as broken markup. Fail-safe rather than fail-pretty.
//
// An empty label falls back to the URL, because a link with no anchor text is
// worse than a visible one.
func Link(channelName, url, label string) string {
	url = strings.TrimSpace(url)
	label = strings.TrimSpace(label)
	if url == "" {
		return label
	}
	if label == "" {
		return url
	}
	switch strings.ToLower(strings.TrimSpace(channelName)) {
	case "slack":
		// Slack forbids the delimiters inside either half; a label carrying one
		// would truncate the link silently, so they are stripped rather than
		// escaped (there is no escape for them in mrkdwn).
		return "<" + url + "|" + strings.NewReplacer("<", "", ">", "", "|", "/").Replace(label) + ">"
	case "telegram", "email":
		return "[" + label + "](" + url + ")"
	default:
		return label + " (" + url + ")"
	}
}
