package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// scraper_write_client.go is the daemon→scraper half of Task 10 (LLD
// 2026-07-21-supervised-web-write-actions-phase1): the concrete
// [ScraperWriteClient] that fulfils the preview/submit seam declared in
// tool_web_submit.go by calling the scraper MCP tool `mcp__scraper__web_submit`
// (services/scraper/src/{index,submit}.ts). It is the transport adapter ONLY —
// the resume-signal routing that moves the owning task to AWAITING_APPROVAL and
// hands the approval capability back to the run is the OTHER half of Task 10 and
// lands separately (LLD Components.5 / Open-Q I5).
//
// Arg / result field names match the scraper's zod schema + return JSON EXACTLY
// (see services/scraper/src/index.ts dispatchWebSubmit).
//
// Preview → PreviewResult mapping (the scraper returns an enumerated form, NOT a
// stored row) — this client DERIVES the two JSONB blobs the daemon persists:
//   - Payload:          {name→value} of every NON-volatile field_table row (the
//                       approved set the submit body is later asserted against).
//   - SelectorBindings: the caller's own field bindings (req.Fields), i.e. the
//                       token-bound selectors submit re-fills from.
//   - FieldTable:       the scraper's field_table verbatim.
//   - Volatile:         volatile_fields verbatim.
//   - ScreenshotRef:    the scraper's screenshot (a data URI; stored as-is).
//   - BlockReason:      block_reason, normalised ("none" → ""). unbound_fields,
//                       which the interface has no field for, is folded into
//                       BlockReason so the daemon fails CLOSED (a write whose
//                       intended field can't be placed must never proceed —
//                       submit.ts refillForSubmit would fail it anyway; blocking
//                       at preview is safer + clearer). See the deviation note.
//
// Submit → SubmitResult: the daemon passes the STORED executable state in
// (SubmitReq); this client forwards it under the scraper's submit arg names and
// parses status/confirmation/sent_body/divergence_reason back.

// scraperWebSubmitTool is the fully-qualified MCP tool name. The scraper server
// is registered per project under the name "scraper" (same as web_fetch), so the
// call is routed through the per-project MCP catalog.
const scraperWebSubmitTool = "mcp__scraper__web_submit"

// scraperMCPExecutor is the narrow MCP execute seam this client needs — just the
// project-scoped tool-call entry point. Both *mcp.Manager and the dispatcher's
// own MCPExecutor satisfy it, so the service container wires
// NewMCPScraperWriteClient(c.mcpManager) into WithScraperWriteClient. Kept narrow
// so a fake stands in under test with no real MCP/scraper.
type scraperMCPExecutor interface {
	Execute(ctx context.Context, projectID, qualifiedName, argsJSON string) (string, error)
}

// mcpScraperWriteClient implements [ScraperWriteClient] over an MCP executor.
type mcpScraperWriteClient struct {
	exec scraperMCPExecutor
	// submitSecret is the daemon↔scraper web_submit capability secret (shared C1
	// contract). It is attached as `daemon_auth` on BOTH the preview and submit
	// MCP calls; the scraper rejects any web_submit whose daemon_auth does not
	// match its SCRAPER_WEB_SUBMIT_SECRET env. This is what prevents an agent
	// from calling mcp__scraper__web_submit directly and bypassing the human
	// approval gate — only the daemon holds the secret. May be empty when web
	// writes are off (the resulting calls the scraper rejects — fail-closed).
	submitSecret string
}

// NewMCPScraperWriteClient builds the production ScraperWriteClient. exec is the
// per-project MCP execute seam (the service container passes its *mcp.Manager);
// submitSecret is the daemon↔scraper web_submit capability secret attached as
// daemon_auth on every call (shared C1 contract, config web.submit_secret).
func NewMCPScraperWriteClient(exec scraperMCPExecutor, submitSecret string) ScraperWriteClient {
	return &mcpScraperWriteClient{exec: exec, submitSecret: submitSecret}
}

// Compile-time assertion that the concrete type fulfils the seam.
var _ ScraperWriteClient = (*mcpScraperWriteClient)(nil)

// scraperWriteProjectKey carries the active project into Submit. SubmitReq
// (tool_web_submit.go) has no project_id field — only PreviewReq does — yet the
// scraper's web_submit requires project_id (min 1) and MCP routing is
// project-scoped. Rather than mutate the frozen interface, the AWAITING_APPROVAL
// resume routing (the other half of Task 10, LLD Open-Q I5) attaches the owning
// run's project here via WithScraperWriteProject before it drives the submit.
// FLAGGED: until that wiring lands, Submit returns a clear error rather than
// guess a project (fail-closed). The proper long-term fix is a project_id on
// SubmitReq, which this file is not permitted to add.
type scraperWriteProjectKey struct{}

// WithScraperWriteProject stashes the project id used to route a submit MCP call.
func WithScraperWriteProject(ctx context.Context, projectID string) context.Context {
	return context.WithValue(ctx, scraperWriteProjectKey{}, projectID)
}

func scraperWriteProjectFromContext(ctx context.Context) string {
	v, _ := ctx.Value(scraperWriteProjectKey{}).(string)
	return v
}

// scraperPreviewArgs is the mcp__scraper__web_submit arg shape for mode=preview.
// DaemonAuth is the C1 capability secret the scraper validates before doing any
// work — attached to preview too (not only submit) so an agent cannot probe the
// preview path either.
type scraperPreviewArgs struct {
	Mode       string     `json:"mode"`
	URL        string     `json:"url"`
	Fields     []WebField `json:"fields"`
	ProjectID  string     `json:"project_id"`
	Profile    string     `json:"profile,omitempty"`
	DaemonAuth string     `json:"daemon_auth"`
}

// scraperSubmitArgs is the arg shape for mode=submit. Payload / SelectorBindings ride as
// pre-marshalled JSON (the daemon stored them as JSONB); omitempty keeps a
// nil/"null" blob from becoming a JSON `null` the scraper's zod record/array
// would reject — the scraper then defaults them to {} / [].
type scraperSubmitArgs struct {
	Mode             string          `json:"mode"`
	SubmissionID     string          `json:"submission_id"`
	URL              string          `json:"url"`
	Payload          json.RawMessage `json:"payload,omitempty"`
	SelectorBindings json.RawMessage `json:"selector_bindings,omitempty"`
	Volatile         []string        `json:"volatile,omitempty"`
	WritesMode       string          `json:"writes_mode"`
	WriteAllowlist   []string        `json:"write_allowlist,omitempty"`
	ProjectID        string          `json:"project_id"`
	// DaemonAuth is the C1 capability secret the scraper validates before it
	// performs the actual (irreversible) submit.
	DaemonAuth string `json:"daemon_auth"`
}

// previewWire mirrors the scraper's preview return JSON.
type previewWire struct {
	SubmissionID   string          `json:"submission_id"`
	Screenshot     string          `json:"screenshot"`
	FieldTable     json.RawMessage `json:"field_table"`
	VolatileFields []string        `json:"volatile_fields"`
	BlockReason    string          `json:"block_reason"`
	BlockDetail    string          `json:"block_detail"`
	UnboundFields  []string        `json:"unbound_fields"`
}

// submitWire mirrors the scraper's submit return JSON.
type submitWire struct {
	Status                 string          `json:"status"`
	ConfirmationScreenshot string          `json:"confirmation_screenshot"`
	ConfirmationText       string          `json:"confirmation_text"`
	SentBody               json.RawMessage `json:"sent_body"`
	DivergenceReason       string          `json:"divergence_reason"`
}

// fieldRow is the minimal projection of a field_table row needed to derive the
// approved-set payload (name→value).
type fieldRow struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Preview drives mode=preview and derives the persisted approved-set payload +
// selector bindings from the returned field table.
// derivePreviewPayload builds the approved-set payload (non-volatile name→value)
// from the scraper's returned field table. A gate/block path returns an empty
// field_table → empty payload, and the daemon refuses on BlockReason before it
// reads the payload anyway. Split out of Preview to keep that method focused.
func derivePreviewPayload(wire previewWire) ([]byte, error) {
	var rows []fieldRow
	if len(wire.FieldTable) > 0 && string(wire.FieldTable) != "null" {
		if err := json.Unmarshal(wire.FieldTable, &rows); err != nil {
			return nil, fmt.Errorf("scraper web_submit preview: malformed field_table: %w", err)
		}
	}
	volatile := make(map[string]struct{}, len(wire.VolatileFields))
	for _, n := range wire.VolatileFields {
		volatile[n] = struct{}{}
	}
	payloadMap := make(map[string]string, len(rows))
	for _, r := range rows {
		if _, isVol := volatile[r.Name]; isVol {
			continue
		}
		payloadMap[r.Name] = r.Value
	}
	payloadJSON, err := json.Marshal(payloadMap)
	if err != nil {
		return nil, fmt.Errorf("scraper web_submit preview: marshal payload: %w", err)
	}
	return payloadJSON, nil
}

func (c *mcpScraperWriteClient) Preview(ctx context.Context, req PreviewReq) (PreviewResult, error) {
	if c == nil || c.exec == nil {
		return PreviewResult{}, errors.New("scraper web_submit preview: no MCP executor wired")
	}
	project := strings.TrimSpace(req.ProjectID)
	if project == "" {
		project = scraperWriteProjectFromContext(ctx)
	}
	if project == "" {
		return PreviewResult{}, errors.New("scraper web_submit preview: no project id on the request")
	}

	argsJSON, err := json.Marshal(scraperPreviewArgs{
		Mode:       "preview",
		URL:        req.URL,
		Fields:     req.Fields,
		ProjectID:  project,
		Profile:    req.Profile,
		DaemonAuth: c.submitSecret,
	})
	if err != nil {
		return PreviewResult{}, fmt.Errorf("scraper web_submit preview: marshal args: %w", err)
	}

	raw, err := c.callTool(ctx, project, argsJSON)
	if err != nil {
		return PreviewResult{}, err
	}

	var wire previewWire
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return PreviewResult{}, fmt.Errorf("scraper web_submit preview: parse result: %w", err)
	}

	// Normalise the block signal: "none" is the scraper's "no gate" sentinel.
	block := strings.TrimSpace(wire.BlockReason)
	if block == "none" {
		block = ""
	}
	// Fail-closed fold: bindings the scraper could not place block the write.
	// The interface has no UnboundFields field, so surface it via BlockReason.
	if block == "" && len(wire.UnboundFields) > 0 {
		block = "unbindable field binding(s): " + strings.Join(wire.UnboundFields, "; ")
	}

	payloadJSON, err := derivePreviewPayload(wire)
	if err != nil {
		return PreviewResult{}, err
	}

	// SelectorBindings = the caller's bindings; these are what submit re-fills
	// from. Coerce a nil slice to "[]" so the stored JSONB is a valid array.
	var selJSON []byte
	if len(req.Fields) == 0 {
		selJSON = []byte("[]")
	} else {
		selJSON, err = json.Marshal(req.Fields)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("scraper web_submit preview: marshal selector bindings: %w", err)
		}
	}

	return PreviewResult{
		Payload:          payloadJSON,
		SelectorBindings: selJSON,
		FieldTable:       cloneRaw(wire.FieldTable),
		Volatile:         wire.VolatileFields,
		ScreenshotRef:    wire.Screenshot,
		BlockReason:      block,
	}, nil
}

// Submit drives mode=submit with the daemon-supplied stored executable state.
func (c *mcpScraperWriteClient) Submit(ctx context.Context, req SubmitReq) (SubmitResult, error) {
	if c == nil || c.exec == nil {
		return SubmitResult{}, errors.New("scraper web_submit submit: no MCP executor wired")
	}
	// SubmitReq carries no project_id (see scraperWriteProjectKey). The resume
	// routing must have attached it; refuse rather than route to a wrong/empty
	// project (fail-closed).
	project := scraperWriteProjectFromContext(ctx)
	if project == "" {
		return SubmitResult{}, errors.New(
			"scraper web_submit submit: no project id in context — the AWAITING_APPROVAL resume routing must attach it via WithScraperWriteProject (SubmitReq carries no project_id)")
	}

	argsJSON, err := json.Marshal(scraperSubmitArgs{
		Mode:             "submit",
		SubmissionID:     req.SubmissionID,
		URL:              req.URL,
		Payload:          cleanRawJSON(req.Payload),
		SelectorBindings: cleanRawJSON(req.SelectorBindings),
		Volatile:         req.Volatile,
		WritesMode:       req.WritesMode,
		WriteAllowlist:   req.WriteAllowlist,
		ProjectID:        project,
		DaemonAuth:       c.submitSecret,
	})
	if err != nil {
		return SubmitResult{}, fmt.Errorf("scraper web_submit submit: marshal args: %w", err)
	}

	raw, err := c.callTool(ctx, project, argsJSON)
	if err != nil {
		return SubmitResult{}, err
	}

	var wire submitWire
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return SubmitResult{}, fmt.Errorf("scraper web_submit submit: parse result: %w", err)
	}

	return SubmitResult{
		Status:           wire.Status,
		Confirmation:     wire.ConfirmationText,
		SentBody:         cloneRaw(wire.SentBody),
		DivergenceReason: wire.DivergenceReason,
	}, nil
}

// callTool invokes the scraper web_submit MCP tool and maps its two error shapes
// to Go errors: a transport failure surfaces as a Go error from Execute; a
// tool-level error (the scraper's isError result) is returned by *mcp.Manager as
// an "MCP error: …"-prefixed string with a nil Go error, so it is detected here
// and converted. An empty result is also an error (fail-closed).
func (c *mcpScraperWriteClient) callTool(ctx context.Context, project string, argsJSON []byte) (string, error) {
	out, err := c.exec.Execute(ctx, project, scraperWebSubmitTool, string(argsJSON))
	if err != nil {
		return "", fmt.Errorf("scraper %s call failed: %w", scraperWebSubmitTool, err)
	}
	if strings.HasPrefix(out, "MCP error") {
		msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(out, "MCP error:"), "MCP error"))
		return "", fmt.Errorf("scraper %s tool error: %s", scraperWebSubmitTool, msg)
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("scraper %s returned an empty result", scraperWebSubmitTool)
	}
	return out, nil
}

// cleanRawJSON drops an empty or JSON-null blob (the daemon's nonNilJSON stores
// "null" for an empty JSONB column) so it is omitted from the args rather than
// sent as a literal `null` the scraper's zod schema would reject.
func cleanRawJSON(b []byte) json.RawMessage {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	return json.RawMessage(b)
}

// cloneRaw returns a defensive copy of a json.RawMessage so the daemon persists
// bytes it owns (the parsed wire buffer is not retained).
func cloneRaw(b json.RawMessage) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
