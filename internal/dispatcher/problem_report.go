package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/report"
	"vornik.io/vornik/internal/version"
)

// ProblemReportBuilder turns a user's description of a problem into an
// ANONYMISED public-safe issue body plus a prefilled GitHub issue URL.
//
// OPERATOR REQUEST 2026-07-30: "customers expect to be able to submit the bug
// report via the chat channels (slack/telegram/email)". `vornikctl report` has
// done this from a terminal since it shipped; a customer talking to the bot in
// Slack had no way in.
//
// Implemented in the service container, which is where the version, edition and
// daemon state live. Nil leaves the tool out of the catalogue entirely rather
// than exposing one that fails — the lead is told the capability is
// unavailable, matching how channelThreads behaves.
type ProblemReportBuilder interface {
	// BuildProblemReport returns (issueURL, anonymisedBody, error). It must
	// NOT submit anything: see the tool's own comment on why the human clicking
	// the link is load-bearing rather than a convenience.
	BuildProblemReport(ctx context.Context, symptom string) (issueURL, body string, err error)

	// Edition reports the build's edition (version.EditionCommunity /
	// EditionEnterprise) so the bundle guidance can avoid naming an
	// Enterprise-only command to a Community reporter. The dispatcher has no
	// build metadata of its own; the service container that implements this
	// interface already holds it.
	Edition() string
}

// SetProblemReportBuilder wires the bug-report path. Late-bound for the same
// reason as the other channel-facing seams.
//
// It MUST write through to the tool executor as well. NewAgent builds
// a.toolExecutor once, copying the agent's fields at that moment, so a setter
// that only assigned a.problemReports would leave the executor holding nil and
// the tool reporting itself unavailable forever — which is exactly what happened
// on first deploy (the bot answered "filing a bug report isn't available on this
// deployment" while the daemon logged the path as wired). Same shape as
// SetChannelThreadReader.
func (a *Agent) SetProblemReportBuilder(b ProblemReportBuilder) {
	if a == nil {
		return
	}
	a.problemReports = b
	if a.toolExecutor != nil {
		a.toolExecutor.problemReports = b
	}
}

// reportProblemArgs is the tool's argument shape.
type reportProblemArgs struct {
	Summary string `json:"summary"`
}

// reportProblem builds an anonymised report and hands back the review link.
//
// IT DELIBERATELY DOES NOT SUBMIT. The report goes to a PUBLIC repository
// (grinco/vornik), and anonymisation is a two-tier best effort over free text
// and diagnostics — it cannot prove the user's own words carry no customer
// name, hostname or credential. So the last step stays with a human: the tool
// returns a prefilled issue URL, the person reads the body GitHub shows them,
// and they press submit. That is the same gate `vornikctl report` and the
// report-problem skill enforce, and the reason neither auto-posts either.
//
// Anything that made this auto-submit would be publishing a customer's words to
// the internet on the strength of a regex.
func (te *ToolExecutor) reportProblem(ctx context.Context, args string) ToolResult {
	if te.problemReports == nil {
		return ToolResult{Content: "Filing a bug report is not available on this deployment. " +
			"Run `vornikctl report --summary \"...\"` on the host instead."}
	}
	var in reportProblemArgs
	if strings.TrimSpace(args) != "" {
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			return ToolResult{Content: fmt.Sprintf("report_problem: could not parse arguments: %v", err)}
		}
	}
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		return ToolResult{Content: "report_problem needs a `summary`: a short description of what went wrong."}
	}

	url, body, err := te.problemReports.BuildProblemReport(ctx, summary)
	if err != nil {
		// Anonymisation failing closed is the designed behaviour, not an
		// outage: it means we could not guarantee the body was public-safe.
		return ToolResult{Content: "I could not prepare a public-safe bug report just now, so I have " +
			"not created one. Nothing was sent anywhere."}
	}

	// Render the link in the originating channel's own syntax. A prefilled GitHub issue
	// URL is over a thousand characters of percent-encoding; pasted raw it fills the
	// screen and reads as noise (operator, 2026-07-30). The channel comes from the
	// context withOriginatingChannel already stashes, so the tool needs no new argument.
	channelName, _ := originatingChannelFromContext(ctx)
	linked := conversation.Link(channelName, url, "open the report and submit it")

	var sb strings.Builder
	sb.WriteString(":memo: *Prepared an anonymised bug report — nothing has been submitted yet.*\n\n")
	sb.WriteString(linked + "\n\n")
	sb.WriteString("Relay that link to the user EXACTLY as written, including its formatting, so " +
		"they get a clickable link rather than a wall of URL.\n\n")
	sb.WriteString("This is what it will say:\n\n")
	sb.WriteString(body)
	sb.WriteString("\nHostnames, paths, IPs, emails and anything that looks like a credential are " +
		"removed automatically, but the report goes to a PUBLIC repository — check your own " +
		"wording for customer names or anything confidential before you submit.\n\n")
	// The body carries the build identity and the reporter's words — and deliberately no
	// collected diagnostics: running the doctor or tailing the journal from a chat-triggered
	// path would let a chat message spawn processes on the host, which this product does not
	// allow (operator decision 2026-08-03; see the SECURITY INVARIANT in
	// service/container_problem_report.go). So the logs, the task timeline and the Black Box
	// trace come from an OPERATOR-run support bundle, and this is the only place a
	// shell-less reporter learns it exists, where it lands, and that they attach it
	// themselves after reading it.
	edition := te.problemReports.Edition()
	sb.WriteString(report.BundleGuidance("--task <the task id>   # or: --since 2h", edition))
	// The "don't run it for them" instruction is about a command that only
	// exists in Enterprise. Repeating it on Community would reintroduce the
	// dead end BundleGuidance just removed — naming a command the reporter
	// cannot run, to a reporter who has no terminal to see the error in.
	if version.NormalizeEdition(edition) == version.EditionEnterprise {
		sb.WriteString("\n\nRelay the steps above too if they ask for the logs — do NOT run " +
			"support-report for them and do NOT paste its contents here: it is theirs to " +
			"inspect and attach.")
	} else {
		sb.WriteString("\n\nRelay the note above too if they ask for the logs, and do NOT collect " +
			"or paste diagnostics for them: anything that reaches the issue is theirs to " +
			"review and attach.")
	}
	return ToolResult{Content: sb.String()}
}
