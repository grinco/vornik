package forgereview

import "testing"

// The trigger handle is DEPLOYMENT-SPECIFIC and must be configurable.
//
// It was hardcoded to "@vornik", which is wrong twice over: github.com/vornik is
// a real user account (registered 2013), so every command notified a stranger
// and any legitimate mention of them looked like a command to us; and every CE
// customer installs their own GitHub App under their own name, whose slug the
// hardcoded handle would never match.
func TestParseCommandFor_HonoursTheConfiguredHandle(t *testing.T) {
	const h = "vornik-companion"
	if got := ParseCommandFor(h, "@vornik-companion review"); got != CmdReview {
		t.Errorf("configured handle did not match: got %v", got)
	}
	// The old hardcoded handle must NOT act once a handle is configured,
	// or the stranger-notification problem survives the fix.
	if got := ParseCommandFor(h, "@vornik review"); got != CmdNone {
		t.Errorf("@vornik still triggered under handle %q: got %v", h, got)
	}
}

// A handle is a substring of a longer one; only the whole handle counts.
func TestParseCommandFor_DoesNotMatchALongerHandle(t *testing.T) {
	if got := ParseCommandFor("vornik", "@vornik-development-companion review"); got != CmdNone {
		t.Errorf("@vornik matched inside a longer handle: got %v", got)
	}
	if got := ParseCommandFor("vornik-development-companion", "@vornik-development-companion review"); got != CmdReview {
		t.Errorf("the full app slug did not match: got %v", got)
	}
}

// Handles are written with or without the @, and with any casing.
func TestParseCommandFor_NormalisesTheHandle(t *testing.T) {
	for _, h := range []string{"vornik-companion", "@vornik-companion", "  @Vornik-Companion  "} {
		if got := ParseCommandFor(h, "@vornik-companion full review"); got != CmdFullReview {
			t.Errorf("handle %q did not match: got %v", h, got)
		}
	}
}

// An unset handle must not fall back to something that acts on a stranger's
// name. Nothing is safer than the wrong thing.
func TestParseCommandFor_EmptyHandleNeverMatches(t *testing.T) {
	for _, body := range []string{"@vornik review", "@ review", "review"} {
		if got := ParseCommandFor("", body); got != CmdNone {
			t.Errorf("empty handle matched %q: got %v", body, got)
		}
	}
}

// Regex metacharacters in a configured handle must not become pattern syntax.
func TestParseCommandFor_HandleIsQuoted(t *testing.T) {
	if got := ParseCommandFor("a.c", "@abc review"); got != CmdNone {
		t.Errorf("the dot in the handle acted as a wildcard: got %v", got)
	}
}
