package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/outputguard"
)

// sendEmail implements the send_email tool. Composes a fresh
// outbound email via the active project's email channel — separate
// from the inbound-reply path that ChannelReceiver.sendReply
// already handles. See [EmailSender] for the wiring contract.
//
// Validation order is intentional: cheapest checks first so a
// missing field doesn't cost an EmailSender round-trip, the
// project-scope check beats the JSON-arg check so an unauthorized
// caller can't enumerate which fields the tool accepts. Errors
// are surfaced verbatim to the LLM so it can self-correct on the
// retry — the LLM contract is "human-readable error text in
// ToolResult.Content."
func (te *ToolExecutor) sendEmail(ctx context.Context, argsJSON, activeProject string, allowedProjects []string) ToolResult {
	if te.emailSender == nil {
		return ToolResult{Content: "Email sending is not configured on this daemon. The project may be missing the `email.smtp_*` fields or no email channel is wired."}
	}
	if strings.TrimSpace(activeProject) == "" {
		return ToolResult{Content: "send_email requires an active project — call switch_project first or attach project_id to your session."}
	}
	if !projectAllowed(activeProject, allowedProjects) {
		return ToolResult{Content: fmt.Sprintf("Access to project '%s' is not permitted for this session.", activeProject)}
	}

	var args struct {
		To        string `json:"to"`
		Subject   string `json:"subject"`
		Body      string `json:"body"`
		InReplyTo string `json:"in_reply_to"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}
	args.To = strings.TrimSpace(args.To)
	args.Subject = strings.TrimSpace(args.Subject)
	args.Body = strings.TrimSpace(args.Body)
	if args.To == "" {
		return ToolResult{Content: "to is required."}
	}
	if args.Subject == "" {
		return ToolResult{Content: "subject is required."}
	}
	if args.Body == "" {
		return ToolResult{Content: "body is required."}
	}

	// Recipient gating. The EmailSender interface's doc promises the
	// adapter checks the project's Email.SenderAllowlist against the
	// To: address ("closed-loop assistant" trust model), but no layer
	// actually enforced it — the LLM could send to ANY address from
	// the project's trusted From:, an exfiltration/phishing vector on
	// a LIVE trading deployment. Enforce it here at the tool boundary
	// before spending an EmailSender round-trip. Empty allowlist =
	// permit-all is the documented dev-mode pass-through (mirrors the
	// inbound senderAllowlist); operators on the LIVE deployment MUST
	// configure a non-empty email.sender_allowlist to bound egress.
	if te.registry != nil {
		if p := te.registry.GetProject(activeProject); p != nil {
			if !recipientAllowed(p.Email.SenderAllowlist, args.To) {
				return ToolResult{Content: fmt.Sprintf(
					"Recipient '%s' is not on this project's email allowlist. Sending is restricted to the configured recipients; ask the operator to add this address to email.sender_allowlist if it should be reachable.",
					args.To)}
			}
		}
	}

	msgID, err := te.emailSender.SendEmail(ctx, activeProject, EmailSendRequest{
		To:        args.To,
		Subject:   args.Subject,
		Body:      args.Body,
		InReplyTo: strings.TrimSpace(args.InReplyTo),
	})
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Email send failed: %v", err)}
	}
	return ToolResult{Content: fmt.Sprintf("Email sent to %s. Message-ID: %s", args.To, msgID), Provenance: outputguard.ProvenanceFirstParty}
}

// recipientAllowed reports whether the To: address is admitted by the
// project's email allowlist. It mirrors email.senderAllowlist.allows
// (which is unexported, so the matching logic is replicated here):
//
//   - empty/whitespace-only list => permit-all (dev-mode pass-through);
//   - entries containing "@" match the full address case-insensitively;
//   - bare entries match the recipient's domain case-insensitively.
//
// Keeping the same semantics on the outbound path makes the documented
// "same trust model both ways" control real without a second config
// surface. If asymmetric scoping is ever needed, slice 2's
// recipient_allowlist would replace this argument.
func recipientAllowed(allowlist []string, to string) bool {
	addresses := map[string]struct{}{}
	domains := map[string]struct{}{}
	any := false
	for _, e := range allowlist {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		any = true
		if strings.Contains(e, "@") {
			addresses[e] = struct{}{}
		} else {
			domains[e] = struct{}{}
		}
	}
	if !any {
		// No allowlist configured: permit-all dev-mode posture.
		return true
	}
	addr := strings.ToLower(strings.TrimSpace(to))
	if addr == "" {
		return false
	}
	if _, ok := addresses[addr]; ok {
		return true
	}
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return false
	}
	_, ok := domains[addr[at+1:]]
	return ok
}
