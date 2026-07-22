package dispatcher

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/outputguard"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/web/urlguard"
)

// web_submit is the daemon-side security core of the supervised web-write
// feature (LLD 2026-07-21-supervised-web-write-actions). It is modeled on
// tool_send_email.go's boundary-gate shape: cheapest-first gates, every failure
// surfaced as human-readable ToolResult.Content (never a Go error), and a narrow
// injected seam (here [ScraperWriteClient]) that a fake stands in for under test.
//
// The tool is two-phase:
//
//   - preview: fill + screenshot + enumerate the form on the scraper, persist a
//     pending web_write_action row, and park the owning task at AWAITING_APPROVAL
//     for the operator to approve in /inbox.
//   - submit: its ONLY inputs are submission_id + approval_token; all executable
//     state is read from the stored row. It verifies the whole-row-bound token,
//     atomically CASes approved→submitting (single-winner double-submit guard),
//     drives the scraper's intercepting submit, and finalizes the row.
//
// It is side-effecting and MUST stay OFF DefaultReplaySafeTools (sideeffects.go).

// ScraperWriteClient is the seam onto the scraper's web_submit tool. The real
// impl (MCP client over internal/mcp) arrives in a later task; here it is
// injected via [WithScraperWriteClient] and a fake stands in for it under test.
type ScraperWriteClient interface {
	// Preview fills the agent's bound fields on an autofill-disabled context,
	// enumerates every field the form would submit, screenshots, and returns —
	// WITHOUT submitting.
	Preview(ctx context.Context, req PreviewReq) (PreviewResult, error)
	// Submit re-opens the stored form, re-fills from the stored selectors, and
	// submits while intercepting the outgoing request to assert the sent body/URL
	// equal the approved set. The daemon passes WritesMode + the resolved
	// WriteAllowlist so the scraper can enforce the per-hop host check (Task 9)
	// without a daemon round-trip.
	Submit(ctx context.Context, req SubmitReq) (SubmitResult, error)
}

// WebField is one agent-supplied field binding for a preview fill.
type WebField struct {
	Selector string `json:"selector,omitempty"`
	Label    string `json:"label,omitempty"`
	Value    string `json:"value"`
}

// PreviewReq is the daemon→scraper preview call. Profile selects the project's
// persistent browser context ("submission bits" the LLD refers to).
type PreviewReq struct {
	URL       string     `json:"url"`
	Fields    []WebField `json:"fields"`
	ProjectID string     `json:"project_id"`
	Profile   string     `json:"profile,omitempty"`
}

// PreviewResult is what the scraper returns from a preview. The JSON-shaped
// blobs are opaque []byte the daemon persists verbatim into the row. BlockReason
// (captcha/login/paywall) is the no-evasion signal: non-empty ⇒ the daemon
// refuses rather than proceeds.
type PreviewResult struct {
	// Payload is the approved-set field values (non-volatile name→value), JSONB.
	Payload []byte `json:"payload"`
	// SelectorBindings is the token-bound field→selector map, JSONB.
	SelectorBindings []byte `json:"selector_bindings"`
	// FieldTable is the full enumerated field table incl. provenance, JSONB.
	FieldTable []byte `json:"field_table"`
	// Volatile is the set of volatile (server-issued, value-exempt) field names.
	Volatile []string `json:"volatile"`
	// ScreenshotRef is the artifact ref for the preview screenshot.
	ScreenshotRef string `json:"screenshot_ref"`
	// BlockReason, when non-empty, means the page presented an anti-bot / login /
	// payment gate — the daemon refuses (no-evasion rule).
	BlockReason string `json:"block_reason"`
}

// SubmitReq is the daemon→scraper submit call. The daemon reads all executable
// state from the stored row and passes it in; the scraper never reads it from
// the agent. WritesMode + WriteAllowlist ride along so the scraper enforces the
// per-hop host check itself.
type SubmitReq struct {
	SubmissionID     string   `json:"submission_id"`
	URL              string   `json:"url"`
	Payload          []byte   `json:"payload"`
	SelectorBindings []byte   `json:"selector_bindings"`
	Volatile         []string `json:"volatile"`
	WritesMode       string   `json:"writes_mode"`
	WriteAllowlist   []string `json:"write_allowlist"`
}

// SubmitResult is the scraper's submit outcome. Status is the terminal status
// for the row (submitted|failed|unknown). On divergence the scraper aborts the
// send and returns Status=failed + DivergenceReason.
type SubmitResult struct {
	Status           string `json:"status"`
	Confirmation     string `json:"confirmation"`
	SentBody         []byte `json:"sent_body"`
	DivergenceReason string `json:"divergence_reason"`
}

// WebWriteApprovalHook is the seam Task 6/10 wires to move the owning task to
// AWAITING_APPROVAL and route the approval capability back to the owning
// agent_run_id + submission_id. At Task-5 scope the dispatcher tool has no
// owning task_id/agent_run_id in its call context (the resume path is Task 10),
// so preview persists the pending row and calls this hook if present; when nil,
// it logs a clearly-marked TODO and returns the submission_id anyway (the row is
// visible via the repo / inbox query). It is NOT faked here.
type WebWriteApprovalHook func(ctx context.Context, action *persistence.WebWriteAction) error

// webSubmitArgs is the LLM-facing arg shape. The schema description NEVER names
// credentials. submit reads ONLY submission_id + approval_token; any executable
// arg (url/fields) present on a submit call is rejected.
type webSubmitArgs struct {
	Mode          string     `json:"mode"`
	URL           string     `json:"url"`
	Fields        []WebField `json:"fields"`
	Profile       string     `json:"profile"`
	SubmissionID  string     `json:"submission_id"`
	ApprovalToken string     `json:"approval_token"`
}

// webWriteTokenTTL bounds how long an approval token stays valid after the
// operator approves (LLD Open-Q 1 proposes 24h). Package var so tests can shrink
// it.
var webWriteTokenTTL = 24 * time.Hour

// webSubmit is the dispatcher handler. Gate order is cheapest-first and shared
// by both phases at the front (LLD §"Gate order (submit)" + §"Write
// authorization" preview-gate-order): nil-wiring (HARD) → active project →
// projectAllowed → web.writes != off. Only then are the args parsed and the
// per-phase URL/host gates run (the URL comes from args for preview, from the
// stored row for submit).
func (te *ToolExecutor) webSubmit(ctx context.Context, argsJSON, activeProject string, allowedProjects []string) ToolResult {
	// nil-wiring is a HARD gate (LLD Components.2 / N2): without both the scraper
	// client and the pending-write store there is no safe way to run the flow.
	if te.scraperWriteClient == nil || te.webWriteRepo == nil {
		return ToolResult{Content: "Web writes are not configured on this daemon. The scraper web_submit tool and/or the web-write store are not wired."}
	}
	if strings.TrimSpace(activeProject) == "" {
		return ToolResult{Content: "web_submit requires an active project — call switch_project first or attach project_id to your session."}
	}
	if !projectAllowed(activeProject, allowedProjects) {
		return ToolResult{Content: fmt.Sprintf("Access to project '%s' is not permitted for this session.", activeProject)}
	}

	// Daemon toggle. WritesMode validates the enum + insecure co-flag at startup;
	// here an invalid config resolves to "" and we treat it as off (fail-closed).
	mode, _ := te.webWrites.WritesMode()
	if mode == "off" || mode == "" {
		return ToolResult{Content: "Web writes are disabled on this daemon. Ask the operator to set web.writes to `on` (per-project write_allowlist) to enable them."}
	}

	var args webSubmitArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ToolResult{Content: fmt.Sprintf("Invalid arguments: %v", err)}
	}

	switch strings.TrimSpace(args.Mode) {
	case "preview":
		return te.webSubmitPreview(ctx, activeProject, mode, args)
	case "submit":
		return te.webSubmitSubmit(ctx, activeProject, mode, args)
	default:
		return ToolResult{Content: "mode is required and must be either 'preview' or 'submit'."}
	}
}

// enforceWriteHost runs the SSRF pre-gate and the per-project write-eligibility
// check for a resolved target URL. It returns the validated host, a non-empty
// human-readable refusal on any failure, and whether this write bypassed the
// allowlist under web.writes=insecure. Shared by both phases so a config change
// between preview and submit is re-validated identically.
func (te *ToolExecutor) enforceWriteHost(mode, rawURL, projectID string) (host, refusal string, insecureBypass bool) {
	u, err := urlguard.ValidateTargetURL(rawURL)
	if err != nil {
		return "", fmt.Sprintf("Target URL rejected: %v", err), false
	}
	host = u.Hostname()

	switch mode {
	case "insecure":
		// web.writes=insecure bypasses ONLY the allowlist (dev/testing). Every
		// other gate (human approval, SSRF above, request interception, no-evasion)
		// still applies; the write is audited insecure_bypass=true and warn-logged.
		te.logger.Warn().
			Str("project", projectID).
			Str("host", host).
			Msg("dispatcher: web_submit proceeding under web.writes=insecure — WriteAllowlist bypassed (insecure_bypass=true)")
		return host, "", true
	case "on":
		var allow []string
		if te.registry != nil {
			if p := te.registry.GetProject(projectID); p != nil {
				allow = p.Web.WriteAllowlist
			}
		}
		// Deny-by-default: an empty allowlist yields HostAllowed=false ⇒ refuse.
		ok, herr := urlguard.HostAllowed(host, allow)
		if herr != nil || !ok {
			return host, fmt.Sprintf(
				"Host %q is not write-eligible for project '%s'. Ask the operator to add it to this project's web.write_allowlist (exact host, or a *.domain wildcard).",
				host, projectID), false
		}
		return host, "", false
	default:
		// Unreachable: the caller already filtered off/"".
		return host, "Web writes are disabled on this daemon.", false
	}
}

// resolvedAllowlist returns the project's write allowlist (nil-safe).
func (te *ToolExecutor) resolvedAllowlist(projectID string) []string {
	if te.registry == nil {
		return nil
	}
	if p := te.registry.GetProject(projectID); p != nil {
		return p.Web.WriteAllowlist
	}
	return nil
}

// webSubmitPreview handles mode=preview: gate the URL, drive the scraper's
// preview, persist the pending row, and park for approval.
func (te *ToolExecutor) webSubmitPreview(ctx context.Context, activeProject, mode string, args webSubmitArgs) ToolResult {
	if strings.TrimSpace(args.URL) == "" {
		return ToolResult{Content: "url is required for a preview."}
	}
	if len(args.Fields) == 0 {
		return ToolResult{Content: "fields is required for a preview: supply at least one {selector|label, value} binding."}
	}

	host, refusal, insecureBypass := te.enforceWriteHost(mode, args.URL, activeProject)
	if refusal != "" {
		return ToolResult{Content: refusal}
	}

	res, err := te.scraperWriteClient.Preview(ctx, PreviewReq{
		URL:       args.URL,
		Fields:    args.Fields,
		ProjectID: activeProject,
		Profile:   args.Profile,
	})
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Web preview failed: %v", err)}
	}
	// No-evasion rule (LLD Non-Goals / Goals 6): any block_reason → refuse.
	if strings.TrimSpace(res.BlockReason) != "" {
		return ToolResult{Content: fmt.Sprintf(
			"Cannot proceed: the page presented a gate (%s). Anti-bot / login / payment gates are never bypassed — hand this to a human.",
			res.BlockReason)}
	}

	volatileJSON, err := marshalVolatile(res.Volatile)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Preview post-processing failed: %v", err)}
	}

	action := &persistence.WebWriteAction{
		ProjectID: activeProject,
		// TaskID / AgentRunID are the owning run's identity. The dispatcher tool
		// call context does not carry them at Task-5 scope; the resume-capability
		// routing that binds them is Task 10 (LLD Components.5). Left empty here;
		// the approval hook seam below is where that wiring lands.
		TargetURL:        args.URL,
		TargetHost:       host,
		PayloadJSON:      nonNilJSON(res.Payload),
		SelectorBindings: nonNilJSON(res.SelectorBindings),
		FieldTableJSON:   nonNilJSON(res.FieldTable),
		VolatileFields:   volatileJSON,
		ScreenshotRef:    res.ScreenshotRef,
		Status:           "pending",
		InsecureBypass:   insecureBypass,
	}
	if err := te.webWriteRepo.Create(ctx, action); err != nil {
		return ToolResult{Content: fmt.Sprintf("Failed to persist the pending web-write action: %v", err)}
	}

	// Move the owning task to AWAITING_APPROVAL + route the approval capability.
	// This is a SEAM: the existing dispatcher tool-call context carries no owning
	// task_id/agent_run_id (the AWAITING_APPROVAL resume path is Task 10), so the
	// transition is delegated to an injected hook. When unwired we log a
	// clearly-marked TODO rather than fake the transition (per task instruction).
	if te.webApprovalHook != nil {
		if err := te.webApprovalHook(ctx, action); err != nil {
			te.logger.Warn().Err(err).
				Str("submission_id", action.SubmissionID).
				Msg("dispatcher: web_submit approval hook failed — row persisted but task not parked")
		}
	} else {
		// TODO(web-write Task 10): wire WithWebWriteApprovalHook to move the owning
		// task to AWAITING_APPROVAL and route {submission_id, agent_run_id,
		// approval_token} to the paused run. Until then the pending row is visible
		// via the inbox/repo query but no task is auto-parked.
		te.logger.Info().
			Str("submission_id", action.SubmissionID).
			Msg("dispatcher: web_submit preview stored a pending action but no approval hook is wired (TODO: Task 10 resume routing)")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Web-write preview stored for approval. submission_id: %s\n", action.SubmissionID)
	fmt.Fprintf(&b, "Target: %s (host %s)\n", args.URL, host)
	fmt.Fprintf(&b, "Fields bound: %d; volatile (value-exempt): %d\n", len(args.Fields), len(res.Volatile))
	if len(res.Volatile) > 0 {
		fmt.Fprintf(&b, "Volatile fields (server-issued, not pinned): %s\n", strings.Join(res.Volatile, ", "))
	}
	if res.ScreenshotRef != "" {
		fmt.Fprintf(&b, "Preview screenshot: %s\n", res.ScreenshotRef)
	}
	if insecureBypass {
		b.WriteString("WARNING: web.writes=insecure — the per-project write allowlist was bypassed for this action.\n")
	}
	b.WriteString("Awaiting operator approval in /inbox. Once approved, call web_submit again with mode=submit, this submission_id, and the minted approval_token.")
	return ToolResult{Content: b.String(), Provenance: outputguard.ProvenanceFirstParty}
}

// webSubmitSubmit handles mode=submit. Inputs are ONLY submission_id +
// approval_token; any executable arg present is rejected. It re-validates the
// stored URL/host, verifies the whole-row-bound token, CASes to submitting
// (single-winner), drives the scraper submit, and finalizes.
func (te *ToolExecutor) webSubmitSubmit(ctx context.Context, activeProject, mode string, args webSubmitArgs) ToolResult {
	// submit reads ONLY submission_id + approval_token. Any executable state on
	// the call is a red flag — a submit must never carry a fresh URL/fields
	// (I1: all executable state comes from the stored, token-bound row).
	if strings.TrimSpace(args.URL) != "" || len(args.Fields) > 0 || strings.TrimSpace(args.Profile) != "" {
		return ToolResult{Content: "submit takes only submission_id and approval_token. Do not pass url, fields, or profile on a submit — all executable state is read from the approved submission."}
	}
	if strings.TrimSpace(args.SubmissionID) == "" {
		return ToolResult{Content: "submission_id is required for a submit."}
	}

	// Resolve the approval token. Operator-chat-driven v1 (LLD Components.5):
	// the operator-chat assistant calls submit WITHOUT holding the token — the
	// raw token was minted + delivered daemon-side by the authenticated inbox
	// approval into the single-use token store, keyed by submission_id. So when
	// the arg is empty we Take it from the store. When the caller DID supply a
	// token (future autonomous mode), that value is honoured as-is and the store
	// is not consulted. Either path runs the SAME whole-row hash verification
	// below — this only changes how the token is OBTAINED, never how it is
	// verified.
	token := strings.TrimSpace(args.ApprovalToken)
	if token == "" && te.webWriteTokenStore != nil {
		if stored, ok := te.webWriteTokenStore.Take(args.SubmissionID); ok {
			token = stored
		}
	}
	if token == "" {
		return ToolResult{Content: "approval_token is required for a submit. Approve this submission in /inbox first — the operator-chat assistant then submits without holding the token."}
	}

	row, err := te.webWriteRepo.Get(ctx, args.SubmissionID)
	if err != nil || row == nil {
		return ToolResult{Content: fmt.Sprintf("No web-write submission found for id %q.", args.SubmissionID)}
	}
	// Session-scope guard: the submission must belong to the active project.
	if row.ProjectID != activeProject {
		return ToolResult{Content: fmt.Sprintf("Submission %q does not belong to project '%s'.", args.SubmissionID, activeProject)}
	}

	// Re-validate the STORED target URL + host under the CURRENT config so a
	// narrowed allowlist between preview and submit denies (no bypass on widening).
	_, refusal, insecureBypass := te.enforceWriteHost(mode, row.TargetURL, activeProject)
	if refusal != "" {
		return ToolResult{Content: refusal}
	}

	// Token check: status must be approved, unexpired, and the presented token
	// must hash to the whole-row-bound approval hash (target_url+host+payload+
	// selector_bindings+volatile). Any mutation of a bound field invalidates the
	// token (I1).
	if row.Status != "approved" {
		return ToolResult{Content: fmt.Sprintf("Submission %q is not approved (status: %s). It must be approved in /inbox before submit.", args.SubmissionID, row.Status)}
	}
	if expired, why := webWriteTokenExpired(row); expired {
		return ToolResult{Content: fmt.Sprintf("The approval for submission %q has expired (%s). Re-run preview to obtain a fresh approval.", args.SubmissionID, why)}
	}
	if row.ApprovalTokenHash == "" || !tokenMatchesRow(token, row) {
		return ToolResult{Content: "The approval_token is invalid for this submission (token mismatch or the approved content changed). Re-run preview and obtain a fresh approval."}
	}

	// Single-winner CAS approved→submitting consumes the token. The irreversible
	// submit happens only after this commits (C3 double-submit guard).
	won, err := te.webWriteRepo.CASToSubmitting(ctx, args.SubmissionID)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Failed to transition submission %q to submitting: %v", args.SubmissionID, err)}
	}
	if !won {
		return ToolResult{Content: fmt.Sprintf("Submission %q is already submitting or has been submitted — refusing to submit again.", args.SubmissionID)}
	}

	return te.runAndFinalizeWebSubmit(ctx, activeProject, mode, row, insecureBypass)
}

// runAndFinalizeWebSubmit performs the (now token-consumed) scraper submit,
// finalizes the row's terminal status, and builds the operator-facing summary.
// Split out of webSubmitSubmit to keep each unit focused.
func (te *ToolExecutor) runAndFinalizeWebSubmit(ctx context.Context, activeProject, mode string, row *persistence.WebWriteAction, insecureBypass bool) ToolResult {
	// SubmitReq carries no project_id (the frozen interface has none), yet the
	// scraper's web_submit requires it and MCP routing is project-scoped. Attach
	// the active project to the ctx so the MCP client (scraper_write_client.go)
	// can route the submit call — mirrors the WithScraperWriteProject contract.
	ctx = WithScraperWriteProject(ctx, activeProject)
	sres, serr := te.scraperWriteClient.Submit(ctx, SubmitReq{
		SubmissionID:     row.SubmissionID,
		URL:              row.TargetURL,
		Payload:          row.PayloadJSON,
		SelectorBindings: row.SelectorBindings,
		Volatile:         unmarshalVolatile(row.VolatileFields),
		WritesMode:       mode,
		WriteAllowlist:   te.resolvedAllowlist(activeProject),
	})

	// Determine the terminal status. A transport error after the token was
	// consumed means we cannot be sure the POST landed → unknown (held for
	// operator adjudication via the recovery command). Otherwise take the
	// scraper's status, defaulting an unrecognized value to failed (fail-closed).
	var status string
	if serr != nil {
		status = "unknown"
	} else {
		switch sres.Status {
		case "submitted", "failed", "unknown":
			status = sres.Status
		default:
			status = "failed"
		}
	}
	if ferr := te.webWriteRepo.Finalize(ctx, row.SubmissionID, status); ferr != nil {
		te.logger.Warn().Err(ferr).
			Str("submission_id", row.SubmissionID).
			Str("status", status).
			Msg("dispatcher: web_submit failed to finalize row status")
	}

	var b strings.Builder
	switch status {
	case "submitted":
		fmt.Fprintf(&b, "Submission %q completed: submitted.\n", row.SubmissionID)
		if sres.Confirmation != "" {
			fmt.Fprintf(&b, "Confirmation: %s\n", sres.Confirmation)
		}
	case "failed":
		fmt.Fprintf(&b, "Submission %q failed and nothing was sent.\n", row.SubmissionID)
		if sres.DivergenceReason != "" {
			fmt.Fprintf(&b, "Reason: %s\n", sres.DivergenceReason)
		} else if serr != nil {
			fmt.Fprintf(&b, "Reason: %v\n", serr)
		}
	case "unknown":
		fmt.Fprintf(&b, "Submission %q is in an unknown state: the request may have been sent but the outcome is ambiguous.\n", row.SubmissionID)
		if serr != nil {
			fmt.Fprintf(&b, "Detail: %v\n", serr)
		}
		b.WriteString("It will NOT be re-submitted (the token is consumed). Ask the operator to verify the outcome and resolve it via `vornikctl web-write resolve`.")
	}
	if insecureBypass {
		b.WriteString("\nNote: this write ran under web.writes=insecure (allowlist bypassed).")
	}
	return ToolResult{Content: b.String(), Provenance: outputguard.ProvenanceFirstParty}
}

// webWriteRowHash is the whole-row content hash the approval token is bound to:
// target_url + target_host + payload + selector_bindings + volatile set. A NUL
// separator prevents field-boundary ambiguity. Any mutation of a bound field
// changes this hash and thus invalidates a previously minted token (I1).
func webWriteRowHash(a *persistence.WebWriteAction) string {
	h := sha256.New()
	sep := []byte{0}
	h.Write([]byte(a.TargetURL))
	h.Write(sep)
	h.Write([]byte(a.TargetHost))
	h.Write(sep)
	h.Write(a.PayloadJSON)
	h.Write(sep)
	h.Write(a.SelectorBindings)
	h.Write(sep)
	h.Write(a.VolatileFields)
	return hex.EncodeToString(h.Sum(nil))
}

// WebWriteApprovalTokenHash binds a minted approval token to the row content.
// The inbox approve path (Task 6) stores this as web_write_actions.
// approval_token_hash; submit re-derives it from the presented token + the
// current row and requires an exact match. Exported so the inbox minter and the
// dispatcher verifier share one construction.
func WebWriteApprovalTokenHash(token string, a *persistence.WebWriteAction) string {
	h := sha256.New()
	h.Write([]byte(token))
	h.Write([]byte{0})
	h.Write([]byte(webWriteRowHash(a)))
	return hex.EncodeToString(h.Sum(nil))
}

// tokenMatchesRow reports whether the presented token, bound to the row's
// current content, matches the stored approval_token_hash. Constant-time compare
// avoids leaking hash bytes via timing.
func tokenMatchesRow(token string, a *persistence.WebWriteAction) bool {
	want := a.ApprovalTokenHash
	got := WebWriteApprovalTokenHash(token, a)
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// webWriteTokenExpired reports whether the approval has lapsed the token TTL,
// measured from the decision time. A missing decision time is treated as
// expired (fail-closed).
func webWriteTokenExpired(a *persistence.WebWriteAction) (bool, string) {
	if !a.DecidedAt.Valid {
		return true, "no decision timestamp"
	}
	if age := time.Since(a.DecidedAt.Time); age > webWriteTokenTTL {
		return true, fmt.Sprintf("approved %s ago, TTL %s", age.Truncate(time.Second), webWriteTokenTTL)
	}
	return false, ""
}

// marshalVolatile renders the volatile-field name set as a JSON array, never nil
// (the column is NOT NULL DEFAULT '[]').
func marshalVolatile(names []string) ([]byte, error) {
	if len(names) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal(names)
}

// unmarshalVolatile parses the stored volatile-field JSON array back to a slice.
func unmarshalVolatile(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// nonNilJSON ensures a JSONB column is never persisted as SQL NULL: an empty
// blob becomes the JSON null literal so the NOT NULL columns stay valid.
func nonNilJSON(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}
