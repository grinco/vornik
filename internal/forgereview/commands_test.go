package forgereview

import "testing"

// The grammar itself, isolated from the webhook plumbing.
func TestParseCommand(t *testing.T) {
	for _, tc := range []struct {
		body string
		want Command
	}{
		{"@vornik review", CmdReview},
		{"@vornik  review", CmdReview},
		{"@Vornik REVIEW", CmdReview},
		{"@vornik full review", CmdFullReview},
		// A near-miss phrasing still gets a review, just the incremental one.
		// Better than CmdNone: the human plainly asked for a review, and
		// silently ignoring them over word order is the "looks broken"
		// failure §7 warns about. The cost of guessing wrong here is a
		// narrower review, not a missing one.
		{"@vornik review full", CmdReview},
		{"@vornik pause", CmdPause},
		{"@vornik resume", CmdResume},
		{"hey @vornik review this please", CmdReview},
		{"@vornik", CmdNone},
		{"@vornik thoughts?", CmdNone},
		{"reviewing @vornik", CmdNone}, // the word review must follow the mention
	} {
		t.Run(tc.body, func(t *testing.T) {
			if got := ParseCommand(tc.body); got != tc.want {
				t.Errorf("ParseCommand(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
