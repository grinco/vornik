package forgereview

import (
	"regexp"
	"strings"
	"sync"
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

// commandVerbs is the alternation of recognised verbs.
//
// `full review` precedes `review` because Go regexp is leftmost-first: with the
// order reversed, "full review" matches the `review` arm and silently downgrades
// an explicit full-review request to an incremental one.
const commandVerbs = `(full\s+review|review|pause|resume)\b`

// DefaultHandle is used when a deployment has not configured one.
//
// NOT "vornik": github.com/vornik is a real user account registered in 2013, so
// that handle notified a stranger on every command and made any legitimate
// mention of them look like an instruction to us.
//
// This is the App's own slug, which is the only handle guaranteed not to belong
// to somebody else — GitHub reserves it for the App. Other deployments configure
// their own, because a CE customer installs their own GitHub App under their own
// name and this default will not match it.
const DefaultHandle = "vornik-companion"

var (
	handleReMu sync.Mutex
	handleRe   = map[string]*regexp.Regexp{}
)

// commandReFor builds (and caches) the matcher for one handle.
//
// The handle is regexp-QUOTED: it comes from configuration, and a dot or bracket
// in it must be a literal character rather than pattern syntax.
func commandReFor(handle string) *regexp.Regexp {
	handleReMu.Lock()
	defer handleReMu.Unlock()
	if re, ok := handleRe[handle]; ok {
		return re
	}
	// \b after the handle so a handle is not matched inside a LONGER one —
	// "@vornik" must not fire on "@vornik-development-companion".
	re := regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(handle) + `\b\s+` + commandVerbs)
	handleRe[handle] = re
	return re
}

// ParseCommandFor extracts the command addressed to a specific handle.
//
// The verb must FOLLOW the mention, in one anchored pattern rather than two
// independent "contains" checks: "reviewing @handle" mentions the bot and
// contains the word review, but it is not asking for one, and treating it as a
// command would spend real money on a sentence that never requested it.
//
// An empty handle never matches. Falling back to some default there would act on
// a name the operator did not choose — which is the entire bug this exists to
// fix.
func ParseCommandFor(handle, body string) Command {
	handle = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(handle), "@"))
	if handle == "" {
		return CmdNone
	}
	m := commandReFor(strings.ToLower(handle)).FindStringSubmatch(body)
	if m == nil {
		return CmdNone
	}
	switch strings.ToLower(strings.Join(strings.Fields(m[1]), " ")) {
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

// IsTrustedAssociation reports whether a commenter may spend the project's
// review budget, from GitHub's own `author_association` on the comment.
//
// Only people with standing in the repository qualify. CONTRIBUTOR is
// deliberately EXCLUDED: on GitHub it means "has had a pull request merged
// here", which is a statement about the past, not permission to trigger model
// spend at will. Anything unrecognised or absent is untrusted, so a payload
// shape we do not know about fails closed rather than open.
//
// IT LIVES HERE, not in a provider package, because this project has two forge
// ingresses and the rule must be the same one for both. It was written inside
// internal/forge/github when the generic webhook path got its gate (9ecedb2c),
// which is exactly why the GitHub App channel — dispatching the identical
// command grammar — shipped with no author gate at all until the 2026-09-03
// audit. A rule that only one ingress can reach is not a rule.
func IsTrustedAssociation(assoc string) bool {
	switch strings.ToUpper(strings.TrimSpace(assoc)) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	default:
		return false
	}
}
