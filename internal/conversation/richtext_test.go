package conversation

import "testing"

// OPERATOR REQUEST 2026-07-30: hyperlink the words, do not paste a thousand characters
// of percent-encoded URL into the channel.
func TestLink(t *testing.T) {
	const url = "https://github.com/grinco/vornik/issues/new?body=%23%23%23+long"
	for _, tc := range []struct {
		channel, want string
	}{
		{"slack", "<" + url + "|open the report>"},
		{"Slack", "<" + url + "|open the report>"},
		{"telegram", "[open the report](" + url + ")"},
		{"email", "[open the report](" + url + ")"},
		{"webchat", "open the report (" + url + ")"},
		{"", "open the report (" + url + ")"},
	} {
		if got := Link(tc.channel, url, "open the report"); got != tc.want {
			t.Errorf("Link(%q) = %q, want %q", tc.channel, got, tc.want)
		}
	}
}

// A label carrying Slack's own delimiters would silently truncate the link, and mrkdwn
// has no escape for them — so they are removed rather than passed through.
func TestLink_SlackLabelCannotBreakTheSyntax(t *testing.T) {
	got := Link("slack", "https://x.example", "a <b> | c")
	if got != "<https://x.example|a b / c>" {
		t.Errorf("got %q", got)
	}
}

// Degenerate input must never produce broken markup.
func TestLink_DegenerateInput(t *testing.T) {
	if got := Link("slack", "", "label"); got != "label" {
		t.Errorf("no url: got %q, want the bare label", got)
	}
	if got := Link("slack", "https://x.example", ""); got != "https://x.example" {
		t.Errorf("no label: got %q, want the bare url", got)
	}
}
