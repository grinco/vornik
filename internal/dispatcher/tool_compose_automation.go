package dispatcher

// compose_automation dispatcher tool — the chat front door to the NL
// Automation Composer (task 1.4; design
// https://docs.vornik.io §5.7 Phase
// 4: "The dispatcher gets a compose_automation tool that opens a
// composer session ... the plan renders as text, the graph as a
// link.").
//
// This is a THIN BRIDGE. All synthesis / guardrail / validation /
// commit logic lives in projectwizard.Wizard, reached in-process
// through the injected ComposerBridge — this file never
// reimplements or weakens any of that. The operator's free-text
// request is UNTRUSTED input; it flows straight into
// Wizard.Converse, which is already schema-constrained and
// guardrailed. v1 never commits a bundle from chat: a ready plan
// comes back as chat text plus a link to the wizard preview page,
// where the existing Plan/Graph/YAML tabs and commit button (tasks
// 1.2a/1.2b) live. Committing from chat would need its own explicit
// confirmation step layered on top of the existing schedule-
// confirmation structural gate — left for a future iteration; the
// web surface is where non-technical trust is built first (design
// §5.7).
import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/outputguard"
)

// composeAutomationName is the LLM-visible tool name.
const composeAutomationName = "compose_automation"

// composeAutomationDescriptor is the chat.Tool definition the
// dispatcher registers in DispatcherTools(). Always registered
// (mirrors send_email / set_reminder): availability is enforced in
// the handler + reflected in InventoryTools(), not by omitting the
// tool from the schema — a disabled/unwired composer returns a
// graceful message instead of executing a turn.
func composeAutomationDescriptor() chat.Tool {
	return chat.Tool{
		Type: "function",
		Function: chat.ToolFunction{
			Name:        composeAutomationName,
			Description: "Open or continue a conversation with vornik's NL Automation Composer to build a scheduled/triggered automation from a plain-language description. Call this when the operator describes something they want automated (what to do, on what schedule, who should approve what) rather than a one-off task. Returns EITHER a clarifying question (call again with the operator's reply — it continues the SAME draft) OR a full plan (numbered steps, schedule, cost estimate, human-approval points) plus a link where the operator reviews the graph/YAML and commits it. This tool NEVER commits anything itself — always relay the returned plan/question to the operator verbatim rather than paraphrasing away the approval or cost-estimate details.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"request":{"type":"string","description":"The operator's automation request, or their reply to the composer's last clarifying question, in their own words. Do not summarise or translate it — pass it through."}
				},
				"required":["request"]
			}`),
		},
	}
}

// ComposerBridge is the narrow seam the compose_automation tool uses
// to reach the NL Automation Composer's tier-3 Converse loop
// (projectwizard.Wizard) without this package depending on
// internal/projectwizard directly. Implemented by an adapter in
// internal/service wrapping the daemon's *projectwizard.Wizard — the
// SAME instance (same repos/config/metrics) the web wizard preview
// uses, so a chat-started draft is visible and committable from the
// browser under the returned previewURL.
//
// ComposeTurn opens (first call for a conversation) or continues (a
// later call for the same conversation) a composer session and runs
// exactly ONE Converse turn with message. It returns:
//   - needsInput=true: planText is the composer's clarifying
//     question / status message; previewURL is empty — there is no
//     committable plan yet.
//   - needsInput=false: planText is the synthesized plan rendered as
//     chat text (steps, schedule, cost estimate, approvals,
//     ApprovalsBypassed); previewURL links to the SAME session's
//     wizard preview page, where the operator reviews Graph/YAML and
//     commits (v1 never commits from chat — see the package doc
//     comment above).
//
// Implementations must not re-implement, loosen, or bypass any
// composer guardrail/validation logic; message is untrusted input
// that must reach Wizard.Converse unmodified.
type ComposerBridge interface {
	ComposeTurn(ctx context.Context, operatorID, conversationID, message string) (planText string, previewURL string, needsInput bool, err error)
}

// composeAutomationArgs is the parsed shape of the LLM's tool args.
type composeAutomationArgs struct {
	Request string `json:"request"`
}

// composeAutomation handles the compose_automation tool call. Thin by
// design: resolves operator + conversation identity from context,
// parses the single `request` argument, and delegates everything
// else — session lookup, the Converse turn, guardrails, plan
// rendering — to te.composer.
func (te *ToolExecutor) composeAutomation(ctx context.Context, argsJSON string, chatID int64) ToolResult {
	if te.composer == nil {
		return ToolResult{Content: "The automation composer isn't configured on this daemon."}
	}
	if !te.composerEnabled {
		return ToolResult{Content: "The automation composer isn't enabled yet on this daemon (still in its soak period). Ask the operator to try again later, or use the \"Describe it\" wizard on the web UI's projects page once it's turned on."}
	}
	operatorID, _ := operatorIDFromContext(ctx)
	if operatorID == "" {
		return ToolResult{Content: "Cannot use the automation composer: this turn was not initiated by an identified operator (synthetic / autonomy / post-mortem context have no operator_id)."}
	}

	var args composeAutomationArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: "compose_automation: invalid arguments: " + err.Error()}
	}
	args.Request = strings.TrimSpace(args.Request)
	if args.Request == "" {
		return ToolResult{Content: "compose_automation: `request` is required — describe the automation, or reply to the composer's last question."}
	}

	conversationID := composerConversationID(ctx, chatID)
	planText, previewURL, needsInput, err := te.composer.ComposeTurn(ctx, operatorID, conversationID, args.Request)
	if err != nil {
		return ToolResult{Content: "compose_automation: " + err.Error()}
	}

	content := planText
	if !needsInput && previewURL != "" {
		content = strings.TrimRight(content, "\n") + "\n\nReview the graph, YAML, and commit it here: " + previewURL
	}
	// First-party: the content is either the Wizard's own schema-
	// constrained message or this package's deterministic rendering
	// of its structured plan — never raw third-party tool output — so
	// injection-class output-guard rules can skip it, same as
	// set_reminder's confirmation text.
	return ToolResult{Content: content, Provenance: outputguard.ProvenanceFirstParty}
}

// composerConversationID derives a stable per-conversation key so a
// follow-up compose_automation call in the SAME chat continues the
// SAME composer session (open-or-continue) instead of starting a
// fresh draft on every turn. Prefers the originating channel +
// session id stashed by withOriginatingChannel (set for every real
// inbound turn — Telegram, email, future Slack); falls back to the
// legacy Telegram chatID for callers that don't thread that context
// (older direct-Execute test fixtures). Empty when neither is
// available (synthesised turns) — the bridge still functions, it
// just can't distinguish concurrent conversations for that operator.
func composerConversationID(ctx context.Context, chatID int64) string {
	channel, sessionID := originatingChannelFromContext(ctx)
	if channel != "" && sessionID != "" {
		return channel + ":" + sessionID
	}
	if chatID != 0 {
		return "telegram:" + strconv.FormatInt(chatID, 10)
	}
	return ""
}
