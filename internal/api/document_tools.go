// Package api — document_* tools exposed to worker agents via MCP.
//
// The Phase 2 surface from document-extraction-design.md §6.
// Worker agents call these instead of raw file_read on a binary
// attachment; that's how a researcher role on an 8K-context model
// navigates a 600-page textbook without blowing context.
//
// The tools are registered as a built-in MCP "server" named
// "vornik" so they share the existing namespacing convention
// (mcp__vornik__document_*). The agent's mcp-bridge picks them up
// from the daemon's /mcp/tools catalog with no entrypoint.sh
// changes — that's the load-bearing reason for the prefix.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/persistence"
)

// Built-in MCP "server" name. Tools are qualified as
// mcp__vornik__<tool> when surfaced to agents. Distinct from any
// real MCP server name so a project mis-configuring an external
// server called "vornik" doesn't shadow these.
const builtinMCPServer = "vornik"

// documentToolNames is the canonical list — used by Tools(),
// Execute()'s dispatch, and the doctor_prompt_lint allowlist so
// drift between catalogues is impossible.
var documentToolNames = []string{
	"document_get_metadata",
	"document_get_outline",
	"document_read_section",
}

// DocumentToolDeps bundles every dependency the document_* tools
// reach for. Construction with nil-pointer fields disables the
// tool surface gracefully — MCP tools simply don't appear in the
// catalog. See container_http.go for production wiring.
type DocumentToolDeps struct {
	Repo         persistence.ExtractedDocumentRepository
	ArtifactRepo persistence.ArtifactRepository
}

// DocumentToolProvider implements the built-in MCP tool catalog.
// Tools() advertises the four tools; Execute() routes a qualified
// name to the matching handler. Concurrency-safe: each call works
// off the repos handed at construction time without per-call
// mutation.
type DocumentToolProvider struct {
	deps DocumentToolDeps
}

// NewDocumentToolProvider builds the provider. Returns nil when
// any required dep is missing — callers can use that as the gate
// for whether to wire the surface at all.
func NewDocumentToolProvider(deps DocumentToolDeps) *DocumentToolProvider {
	if deps.Repo == nil || deps.ArtifactRepo == nil {
		return nil
	}
	return &DocumentToolProvider{deps: deps}
}

// Tools returns the four document_* tools as chat.Tool entries.
// Project ID is accepted for parity with the MCPExecutor interface
// but isn't used today — the tools are project-scoped at the data
// layer via the artifact's project_id, not at the catalog layer.
//
// Returns nil when the provider is nil so the composed executor
// can skip the merge step cheaply.
func (p *DocumentToolProvider) Tools(_ string) []chat.Tool {
	if p == nil {
		return nil
	}
	return []chat.Tool{
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        qualifiedName("document_get_metadata"),
				Description: "[vornik] Return the operator-visible metadata for an attached document (title, author, publisher, ISBN, language, section count, MIME type, extractor version). Returns a small JSON blob — use BEFORE reading sections so you know the structure exists.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"artifact_id":{"type":"string","description":"Source artifact ID (typically email-att-... from the [Attached files] block)."}
					},
					"required":["artifact_id"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        qualifiedName("document_get_outline"),
				Description: "[vornik] Return the document's table of contents — a flat list of {section_id, title, depth, text_bytes} entries in reading order. Use this to decide which sections to read in detail instead of dumping the whole document into context.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"artifact_id":{"type":"string","description":"Source artifact ID."}
					},
					"required":["artifact_id"]
				}`),
			},
		},
		{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        qualifiedName("document_read_section"),
				Description: "[vornik] Read one section's text content (markdown). Bounded by limit_chars (default 8000). For very large sections, page through by passing the next_offset returned in the previous response.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"artifact_id":{"type":"string","description":"Source artifact ID."},
						"section_id":{"type":"string","description":"Section identifier returned by document_get_outline."},
						"offset_chars":{"type":"integer","description":"Character offset to start reading from. Default 0.","default":0},
						"limit_chars":{"type":"integer","description":"Maximum characters to return. Default 8000.","default":8000}
					},
					"required":["artifact_id","section_id"]
				}`),
			},
		},
	}
}

// Execute dispatches a qualified tool name to its handler. Returns
// the JSON-encoded result as a string (the MCP-bridge contract).
// Errors surface as plain-text "ERROR: ..." strings so the LLM
// can read and recover, matching the MCPExecutor convention used
// by external MCP servers.
func (p *DocumentToolProvider) Execute(ctx context.Context, projectID, qualifiedName, argsJSON string) (string, error) {
	if p == nil {
		return "", errors.New("document tool provider is nil")
	}
	tool, ok := parseBuiltinName(qualifiedName)
	if !ok {
		return "", fmt.Errorf("document tools: not a built-in tool name: %s", qualifiedName)
	}
	switch tool {
	case "document_get_metadata":
		return p.handleGetMetadata(ctx, projectID, argsJSON)
	case "document_get_outline":
		return p.handleGetOutline(ctx, projectID, argsJSON)
	case "document_read_section":
		return p.handleReadSection(ctx, projectID, argsJSON)
	default:
		return "", fmt.Errorf("unknown document tool: %s", tool)
	}
}

// Owns reports whether qualifiedName belongs to the document
// tool provider. Used by the composed MCP executor to route a
// call without trying every backend.
func (p *DocumentToolProvider) Owns(qualifiedName string) bool {
	if p == nil {
		return false
	}
	_, ok := parseBuiltinName(qualifiedName)
	return ok
}

// ----- handlers -----

type artifactRefArgs struct {
	ArtifactID string `json:"artifact_id"`
}

type metadataResponse struct {
	ExtractedDocumentID string `json:"extracted_document_id"`
	Title               string `json:"title,omitempty"`
	Author              string `json:"author,omitempty"`
	Publisher           string `json:"publisher,omitempty"`
	PublicationDate     string `json:"publication_date,omitempty"`
	ISBN                string `json:"isbn,omitempty"`
	Language            string `json:"language,omitempty"`
	PageCount           int    `json:"page_count,omitempty"`
	DurationSeconds     int    `json:"duration_seconds,omitempty"`
	SectionCount        int    `json:"section_count"`
	MimeType            string `json:"mime_type"`
	ExtractorName       string `json:"extractor_name"`
	ExtractorVersion    string `json:"extractor_version"`
	Status              string `json:"status"`
}

func (p *DocumentToolProvider) handleGetMetadata(ctx context.Context, projectID, argsJSON string) (string, error) {
	var args artifactRefArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return jsonErr("invalid arguments: " + err.Error()), nil
	}
	doc, err := p.lookupDoc(ctx, projectID, args.ArtifactID)
	if err != nil {
		return jsonErr(err.Error()), nil
	}
	meta := decodeMetadataBlob(doc.MetadataBlob)
	resp := metadataResponse{
		ExtractedDocumentID: doc.ID,
		Title:               meta.Title,
		Author:              meta.Author,
		Publisher:           meta.Publisher,
		PublicationDate:     meta.PublicationDate,
		ISBN:                meta.ISBN,
		Language:            meta.Language,
		PageCount:           meta.PageCount,
		DurationSeconds:     meta.DurationSeconds,
		SectionCount:        doc.SectionCount,
		MimeType:            doc.MimeType,
		ExtractorName:       doc.ExtractorName,
		ExtractorVersion:    doc.ExtractorVersion,
		Status:              doc.Status,
	}
	return mustJSON(resp), nil
}

type outlineEntryResp struct {
	SectionID         string `json:"section_id"`
	Title             string `json:"title"`
	Depth             int    `json:"depth"`
	PageStart         int    `json:"page_start,omitempty"`
	TimestampStartSec int    `json:"timestamp_start_sec,omitempty"`
	TextBytes         int    `json:"text_bytes"`
}

type outlineResponse struct {
	ExtractedDocumentID string             `json:"extracted_document_id"`
	Outline             []outlineEntryResp `json:"outline"`
}

func (p *DocumentToolProvider) handleGetOutline(ctx context.Context, projectID, argsJSON string) (string, error) {
	var args artifactRefArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return jsonErr("invalid arguments: " + err.Error()), nil
	}
	doc, err := p.lookupDoc(ctx, projectID, args.ArtifactID)
	if err != nil {
		return jsonErr(err.Error()), nil
	}
	var outline []extractor.OutlineEntry
	if len(doc.OutlineBlob) > 0 {
		if err := json.Unmarshal(doc.OutlineBlob, &outline); err != nil {
			return jsonErr("malformed outline blob: " + err.Error()), nil
		}
	}
	out := make([]outlineEntryResp, 0, len(outline))
	for _, e := range outline {
		out = append(out, outlineEntryResp{
			SectionID:         e.SectionID,
			Title:             e.Title,
			Depth:             e.Depth,
			PageStart:         e.PageStart,
			TimestampStartSec: e.TimestampStartSec,
			TextBytes:         e.TextBytes,
		})
	}
	return mustJSON(outlineResponse{
		ExtractedDocumentID: doc.ID,
		Outline:             out,
	}), nil
}

type readSectionArgs struct {
	ArtifactID  string `json:"artifact_id"`
	SectionID   string `json:"section_id"`
	OffsetChars int    `json:"offset_chars"`
	LimitChars  int    `json:"limit_chars"`
}

type readSectionResponse struct {
	SectionID  string `json:"section_id"`
	Title      string `json:"title,omitempty"`
	Content    string `json:"content"`
	TotalChars int    `json:"total_chars"`
	HasMore    bool   `json:"has_more"`
	NextOffset int    `json:"next_offset,omitempty"`
}

const (
	defaultSectionReadLimit = 8000
	// maxSectionReadLimit clamps the operator-supplied limit_chars
	// so a runaway tool call can't pull the whole 30-MB transcript
	// of an audio extraction in one shot — even with bounded
	// per-section files, the cumulative effect on context budget
	// matters. 32k is comfortably above any chapter's
	// "single coherent section" size.
	maxSectionReadLimit = 32000
)

// validSectionID rejects section ids that could escape the per-document
// sections directory or carry control bytes. Permissive on charset (binary
// extractors emit varied ids) but hard-blocks the traversal-relevant chars.
func validSectionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	if strings.Contains(id, "..") || strings.ContainsAny(id, "/\\\x00") {
		return false
	}
	for _, r := range id {
		if r < 0x20 { // control bytes
			return false
		}
	}
	return true
}

func (p *DocumentToolProvider) handleReadSection(ctx context.Context, projectID, argsJSON string) (string, error) {
	var args readSectionArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return jsonErr("invalid arguments: " + err.Error()), nil
	}
	if args.SectionID == "" {
		return jsonErr("section_id is required"), nil
	}
	// Defense-in-depth: section_id is used to build a filesystem path
	// (safepath.JoinUnder backstops traversal at the storage layer, but
	// reject obviously-malicious ids at the boundary so a binary-format
	// extractor can't persist a `..`-bearing id that we later read). Block
	// path separators, parent refs, and control bytes; cap the length.
	if !validSectionID(args.SectionID) {
		return jsonErr("invalid section_id"), nil
	}
	doc, err := p.lookupDoc(ctx, projectID, args.ArtifactID)
	if err != nil {
		return jsonErr(err.Error()), nil
	}
	content, err := extractor.ReadSection(doc, args.SectionID)
	if err != nil {
		return jsonErr("read section: " + err.Error()), nil
	}

	limit := args.LimitChars
	if limit <= 0 {
		limit = defaultSectionReadLimit
	}
	if limit > maxSectionReadLimit {
		limit = maxSectionReadLimit
	}
	offset := args.OffsetChars
	if offset < 0 {
		offset = 0
	}
	runes := []rune(content)
	if offset > len(runes) {
		offset = len(runes)
	}

	end := offset + limit
	if end > len(runes) {
		end = len(runes)
	}
	slice := string(runes[offset:end])

	// Locate section title from the outline so the response is
	// self-describing — the agent doesn't have to call
	// document_get_outline again to label the snippet.
	title := lookupSectionTitle(doc, args.SectionID)

	resp := readSectionResponse{
		SectionID:  args.SectionID,
		Title:      title,
		Content:    slice,
		TotalChars: len(runes),
		HasMore:    end < len(runes),
	}
	if resp.HasMore {
		resp.NextOffset = end
	}
	return mustJSON(resp), nil
}

// ----- helpers -----

// lookupDoc fetches the extracted document by source artifact ID
// and enforces the project-scope gate. Returns a friendly error
// when the artifact has no matching extraction (the LLM can read
// the message and choose to call document_get_metadata for a
// different artifact_id).
func (p *DocumentToolProvider) lookupDoc(ctx context.Context, projectID, artifactID string) (*persistence.ExtractedDocument, error) {
	if artifactID == "" {
		return nil, errors.New("artifact_id is required")
	}
	// Project-scope: load the artifact first and verify it belongs
	// to the calling project. Without this an agent in project A
	// could read extracted text from project B's books.
	art, err := p.deps.ArtifactRepo.Get(ctx, artifactID)
	if err != nil {
		return nil, fmt.Errorf("artifact lookup: %w", err)
	}
	if art == nil || art.ProjectID != projectID {
		return nil, fmt.Errorf("artifact %q not found in project %q", artifactID, projectID)
	}
	doc, err := p.deps.Repo.GetByArtifact(ctx, artifactID)
	if err != nil {
		return nil, fmt.Errorf("extracted-document lookup: %w", err)
	}
	if doc == nil {
		return nil, fmt.Errorf("no extracted document for artifact %q (try POST /api/v1/projects/%s/artifacts/%s/extract first)",
			artifactID, projectID, artifactID)
	}
	if doc.ProjectID != projectID {
		// Defence in depth — should never happen given the artifact
		// project-scope check above, but the artifact row could in
		// principle be repointed across projects via a migration bug.
		return nil, fmt.Errorf("extracted document %q is scoped to a different project", doc.ID)
	}
	return doc, nil
}

func qualifiedName(tool string) string {
	return "mcp__" + builtinMCPServer + "__" + tool
}

// parseBuiltinName strips the mcp__vornik__ prefix and returns the
// bare tool name. Returns ("", false) when the qualified name
// belongs to a different MCP server.
func parseBuiltinName(qualified string) (string, bool) {
	prefix := "mcp__" + builtinMCPServer + "__"
	if !strings.HasPrefix(qualified, prefix) {
		return "", false
	}
	bare := qualified[len(prefix):]
	for _, name := range documentToolNames {
		if bare == name {
			return bare, true
		}
	}
	return "", false
}

// mustJSON marshals a value, falling back to a JSON error string
// when marshalling fails. The agent's tool-call dispatcher expects
// a valid JSON response either way; panicking would crash the
// daemon's HTTP handler and a half-formed body would be parsed
// by the LLM as an actual answer.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return jsonErr("marshal failed: " + err.Error())
	}
	return string(b)
}

// jsonErr returns a stable error shape the LLM can recognise and
// recover from. Mirrors the convention external MCP servers use.
func jsonErr(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

func decodeMetadataBlob(blob []byte) extractor.Metadata {
	var m extractor.Metadata
	if len(blob) == 0 {
		return m
	}
	_ = json.Unmarshal(blob, &m)
	return m
}

func lookupSectionTitle(doc *persistence.ExtractedDocument, sectionID string) string {
	if doc == nil || len(doc.OutlineBlob) == 0 {
		return ""
	}
	var outline []extractor.OutlineEntry
	if err := json.Unmarshal(doc.OutlineBlob, &outline); err != nil {
		return ""
	}
	for _, e := range outline {
		if e.SectionID == sectionID {
			return e.Title
		}
	}
	return ""
}
