package dispatcher

import (
	"context"
	"strings"
)

// TurnStatusReporter receives coarse progress for an in-flight turn so the
// originating channel can show the human that work is happening.
//
// Implemented in the service container, which resolves the channel by name and
// forwards to it when it is a conversation.TurnStatusChannel. The dispatcher
// deliberately does not know about channels.
//
// Every call is best-effort and must not block the turn: this fires from inside
// the tool loop, and a slow status update would make the very latency it is
// reporting worse.
type TurnStatusReporter interface {
	ReportTurnStatus(ctx context.Context, channel, sessionID, status string)
}

// SetTurnStatusReporter binds the status sink. Late-bound like the other
// channel-facing seams, because channels are constructed after the agent.
func (a *Agent) SetTurnStatusReporter(r TurnStatusReporter) {
	a.turnStatusMu.Lock()
	a.turnStatus = r
	a.turnStatusMu.Unlock()
}

// reportTurnStatus forwards one status line for the turn described by req.
// No-op when nothing is bound or the request has no originating channel — API
// and A2A callers have no surface to show it on.
func (a *Agent) reportTurnStatus(ctx context.Context, req Request, status string) {
	if a == nil || req.OriginatingChannel == "" || req.OriginatingSessionID == "" {
		return
	}
	a.turnStatusMu.RLock()
	reporter := a.turnStatus
	a.turnStatusMu.RUnlock()
	if reporter == nil {
		return
	}
	reporter.ReportTurnStatus(ctx, req.OriginatingChannel, req.OriginatingSessionID, status)
}

// toolStatusLine renders a tool call as something a non-engineer can read.
//
// The tool NAME is the only thing surfaced, never its arguments: a status line
// is visible to everyone in a shared channel, and arguments routinely carry the
// substance of the request (file paths, search terms, a person's name).
func toolStatusLine(toolName string) string {
	name := strings.TrimSpace(toolName)
	if name == "" {
		return "working on it…"
	}
	if phrase, ok := friendlyToolPhrases[name]; ok {
		return phrase
	}
	// Unknown tools (MCP servers register their own at runtime) degrade to the
	// bare name rather than to something invented.
	return "running `" + name + "`…"
}

// friendlyToolPhrases covers the built-in dispatcher tools. An unlisted tool is
// not a bug — MCP catalogues are per-deployment — it just reads less warmly.
var friendlyToolPhrases = map[string]string{
	"memory_search":           "searching memory…",
	"memory_forget":           "forgetting a memory chunk…",
	"create_task":             "scheduling the job…",
	"list_tasks":              "checking your tasks…",
	"get_task_status":         "checking the job status…",
	"wait_for_task":           "waiting for the job…",
	"cancel_task":             "cancelling the job…",
	"retry_task":              "retrying the job…",
	"list_artifacts":          "looking through the results…",
	"read_artifact":           "reading the result…",
	"send_artifact":           "sending the file…",
	"send_email":              "sending the email…",
	"render_document":         "rendering the document…",
	"set_reminder":            "setting the reminder…",
	"cancel_reminder":         "cancelling the reminder…",
	"list_projects":           "looking at your projects…",
	"switch_project":          "switching project…",
	"query_api":               "calling an external service…",
	"list_apis":               "checking what services are available…",
	"web_submit":              "submitting the form…",
	"update_operator_profile": "updating your profile…",
	"report_problem":          "filing the bug report…",
}
