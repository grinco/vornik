package forgereview

import (
	"regexp"
	"strings"
)

// Command is an instruction addressed to the bot in a change-request comment.
//
// Lives in this provider-neutral package rather than internal/github (where it
// was first written) because it parses PROSE: nothing about "@vornik review" is
// GitHub-specific, and a GitLab note hook needs the identical grammar. Moved as
// part of §13.4 rather than left to be moved later.
//
// Design: https://docs.vornik.io §7.
type Command int

const (
	// CmdNone means the comment mentions the bot but asks for none of the
	// commands below. It is NOT an error: the comment falls through to the
	// conversational path exactly as it did before commands existed, so an
	// unrecognised phrasing degrades to "a human gets an answer" rather than
	// to silence.
	CmdNone Command = iota
	// CmdReview asks for a review of what changed since the last one.
	CmdReview
	// CmdFullReview asks for the whole change request, ignoring the baseline.
	CmdFullReview
	// CmdPause suppresses automatic review for this change request only;
	// explicit commands keep working, or pause becomes a trap the operator
	// cannot escape from the thread they set it in.
	CmdPause
	// CmdResume clears CmdPause.
	CmdResume
)

func (c Command) String() string {
	switch c {
	case CmdReview:
		return "review"
	case CmdFullReview:
		return "full review"
	case CmdPause:
		return "pause"
	case CmdResume:
		return "resume"
	default:
		return "none"
	}
}

// commandRe matches the mention followed by one of the verbs.
//
// The verb must FOLLOW the mention, which is why this is a single anchored
// pattern rather than two independent "contains" checks. "reviewing @vornik"
// mentions the bot and contains the word review, but it is not asking for one;
// treating it as a command would run a review — real model spend — off a
// sentence that never requested it.
//
// `full review` is listed before `review` because Go's regexp alternation is
// leftmost-first: with the order reversed, "full review" would match the
// `review` arm and silently downgrade an explicit full-review request to an
// incremental one.
var commandRe = regexp.MustCompile(`(?i)@vornik\s+(full\s+review|review|pause|resume)\b`)

// ParseCommand extracts the command from a comment body.
//
// Case-insensitive and whitespace-tolerant because this is prose typed by a
// human in a web form, not a CLI.
func ParseCommand(body string) Command {
	m := commandRe.FindStringSubmatch(body)
	if m == nil {
		return CmdNone
	}
	verb := strings.ToLower(strings.Join(strings.Fields(m[1]), " "))
	switch verb {
	case "full review":
		return CmdFullReview
	case "review":
		return CmdReview
	case "pause":
		return CmdPause
	case "resume":
		return CmdResume
	default:
		return CmdNone
	}
}
