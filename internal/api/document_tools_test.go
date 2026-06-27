// Tests for the document_* MCP tool surface. Each case pins the
// contract a worker agent depends on: tool names, project-scope
// gate, response shape, pagination, and the "no extraction yet"
// error path.
package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// stubDocRepo is the minimum ExtractedDocumentRepository surface
// the document tools call into.
type stubDocRepo struct {
	docs map[string]*persistence.ExtractedDocument // keyed on source_artifact_id
}

func (s *stubDocRepo) Upsert(context.Context, *persistence.ExtractedDocument) error { return nil }
func (s *stubDocRepo) Get(context.Context, string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}
func (s *stubDocRepo) GetByArtifact(_ context.Context, id string) (*persistence.ExtractedDocument, error) {
	return s.docs[id], nil
}
func (s *stubDocRepo) ListByProject(context.Context, string, int) ([]*persistence.ExtractedDocument, error) {
	return nil, nil
}
func (s *stubDocRepo) Delete(context.Context, string) error { return nil }

type stubArtifactRepoForTools struct {
	arts map[string]*persistence.Artifact
}

func (s *stubArtifactRepoForTools) Create(context.Context, *persistence.Artifact) error { return nil }
func (s *stubArtifactRepoForTools) Get(_ context.Context, id string) (*persistence.Artifact, error) {
	return s.arts[id], nil
}
func (s *stubArtifactRepoForTools) GetByHash(context.Context, string) (*persistence.Artifact, error) {
	return nil, nil
}
func (s *stubArtifactRepoForTools) List(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	return nil, nil
}
func (s *stubArtifactRepoForTools) Delete(context.Context, string) error              { return nil }
func (s *stubArtifactRepoForTools) DeleteByExecutionID(context.Context, string) error { return nil }
func (s *stubArtifactRepoForTools) UpdateTaskID(context.Context, string, string) error {
	return nil
}

// fixtureProvider builds a DocumentToolProvider over an in-memory
// (artifact, extracted-doc, section content) fixture. tmpDir is
// returned so the caller can also write section files when
// exercising document_read_section.
func fixtureProvider(t *testing.T) (*DocumentToolProvider, string) {
	t.Helper()
	storageDir := t.TempDir()

	// Write two section files so document_read_section has real
	// content to slice.
	sectionsDir := storageDir + "/sections"
	if err := writeFile(sectionsDir+"/001-intro.md", "Hello world. This is the introduction."); err != nil {
		t.Fatalf("write section 1: %v", err)
	}
	bigBody := strings.Repeat("payload-", 2000) // 16000 chars — exercises pagination
	if err := writeFile(sectionsDir+"/002-body.md", bigBody); err != nil {
		t.Fatalf("write section 2: %v", err)
	}

	outline := `[
		{"section_id":"001-intro","title":"Introduction","depth":0,"text_bytes":40},
		{"section_id":"002-body","title":"Body","depth":0,"text_bytes":16000}
	]`
	metadata := `{"title":"Test Book","author":"Test Author","language":"en","isbn":"978-1234567890"}`

	doc := &persistence.ExtractedDocument{
		ID:               "extdoc_t1",
		ProjectID:        "proj-test",
		SourceArtifactID: "art-1",
		ExtractorName:    "vornik-extract-epub",
		ExtractorVersion: "0.1.0",
		MimeType:         "application/epub+zip",
		StoragePath:      storageDir,
		MetadataBlob:     []byte(metadata),
		OutlineBlob:      []byte(outline),
		SectionCount:     2,
		Status:           persistence.ExtractedDocumentStatusOK,
	}
	art := &persistence.Artifact{
		ID:        "art-1",
		ProjectID: "proj-test",
		Name:      "book.epub",
	}
	p := NewDocumentToolProvider(DocumentToolDeps{
		Repo:         &stubDocRepo{docs: map[string]*persistence.ExtractedDocument{"art-1": doc}},
		ArtifactRepo: &stubArtifactRepoForTools{arts: map[string]*persistence.Artifact{"art-1": art}},
	})
	if p == nil {
		t.Fatal("provider construction returned nil with valid deps")
	}
	return p, storageDir
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

// TestDocumentTools_Catalog pins the four-tool catalog so a future
// rename or removal trips the test. The qualified names matter:
// worker agents reference them as "mcp__vornik__document_*" in
// their tool-call output.
func TestDocumentTools_Catalog(t *testing.T) {
	p, _ := fixtureProvider(t)
	tools := p.Tools("proj-test")
	if len(tools) != len(documentToolNames) {
		t.Fatalf("Tools returned %d entries; want %d", len(tools), len(documentToolNames))
	}
	got := make(map[string]bool)
	for _, tool := range tools {
		got[tool.Function.Name] = true
		if tool.Type != "function" {
			t.Errorf("tool %q has type %q; want function", tool.Function.Name, tool.Type)
		}
		if len(tool.Function.Parameters) == 0 {
			t.Errorf("tool %q has no parameters schema", tool.Function.Name)
		}
		// Description should reference the [vornik] tag so operators
		// can spot built-in tools in the catalog.
		if !strings.HasPrefix(tool.Function.Description, "[vornik]") {
			t.Errorf("tool %q description must lead with [vornik] tag; got %q",
				tool.Function.Name, tool.Function.Description)
		}
	}
	for _, name := range documentToolNames {
		qualified := "mcp__vornik__" + name
		if !got[qualified] {
			t.Errorf("catalog missing %q", qualified)
		}
	}
}

func TestDocumentTools_NilProvider_NoCatalogNoExecute(t *testing.T) {
	var p *DocumentToolProvider // nil
	if got := p.Tools("any"); got != nil {
		t.Errorf("nil provider Tools = %v; want nil", got)
	}
	if got := p.Owns("mcp__vornik__document_get_outline"); got {
		t.Errorf("nil provider should not own any tool")
	}
	_, err := p.Execute(context.Background(), "any", "mcp__vornik__document_get_outline", "{}")
	if err == nil {
		t.Error("nil provider Execute must error")
	}
}

func TestDocumentTools_NewProvider_MissingDeps_ReturnsNil(t *testing.T) {
	if NewDocumentToolProvider(DocumentToolDeps{}) != nil {
		t.Error("missing repos must yield nil provider")
	}
	if NewDocumentToolProvider(DocumentToolDeps{Repo: &stubDocRepo{}}) != nil {
		t.Error("missing artifact repo must yield nil provider")
	}
	if NewDocumentToolProvider(DocumentToolDeps{ArtifactRepo: &stubArtifactRepoForTools{}}) != nil {
		t.Error("missing extracted-doc repo must yield nil provider")
	}
}

func TestDocumentTools_GetMetadata_HappyPath(t *testing.T) {
	p, _ := fixtureProvider(t)
	got, err := p.Execute(context.Background(), "proj-test",
		"mcp__vornik__document_get_metadata", `{"artifact_id":"art-1"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var resp metadataResponse
	if err := json.Unmarshal([]byte(got), &resp); err != nil {
		t.Fatalf("decode: %v\nresponse: %s", err, got)
	}
	if resp.Title != "Test Book" || resp.Author != "Test Author" {
		t.Errorf("metadata round-trip lost fields: %+v", resp)
	}
	if resp.ISBN != "978-1234567890" {
		t.Errorf("ISBN = %q", resp.ISBN)
	}
	if resp.SectionCount != 2 {
		t.Errorf("SectionCount = %d", resp.SectionCount)
	}
	if resp.ExtractorName != "vornik-extract-epub" {
		t.Errorf("ExtractorName = %q", resp.ExtractorName)
	}
	if resp.Status != "OK" {
		t.Errorf("Status = %q", resp.Status)
	}
}

func TestDocumentTools_GetOutline_HappyPath(t *testing.T) {
	p, _ := fixtureProvider(t)
	got, err := p.Execute(context.Background(), "proj-test",
		"mcp__vornik__document_get_outline", `{"artifact_id":"art-1"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var resp outlineResponse
	if err := json.Unmarshal([]byte(got), &resp); err != nil {
		t.Fatalf("decode: %v\nresponse: %s", err, got)
	}
	if len(resp.Outline) != 2 {
		t.Fatalf("outline length = %d; want 2", len(resp.Outline))
	}
	if resp.Outline[0].SectionID != "001-intro" || resp.Outline[0].Title != "Introduction" {
		t.Errorf("outline[0] = %+v", resp.Outline[0])
	}
	if resp.Outline[1].SectionID != "002-body" {
		t.Errorf("outline[1].SectionID = %q", resp.Outline[1].SectionID)
	}
}

func TestDocumentTools_ReadSection_HappyPath(t *testing.T) {
	p, _ := fixtureProvider(t)
	got, err := p.Execute(context.Background(), "proj-test",
		"mcp__vornik__document_read_section",
		`{"artifact_id":"art-1","section_id":"001-intro"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var resp readSectionResponse
	if err := json.Unmarshal([]byte(got), &resp); err != nil {
		t.Fatalf("decode: %v\nresponse: %s", err, got)
	}
	if !strings.Contains(resp.Content, "introduction") {
		t.Errorf("content missing expected text: %q", resp.Content)
	}
	if resp.HasMore {
		t.Errorf("small section must not signal has_more; got %+v", resp)
	}
	if resp.Title != "Introduction" {
		t.Errorf("Title pulled from outline = %q", resp.Title)
	}
}

func TestDocumentTools_ReadSection_Pagination(t *testing.T) {
	p, _ := fixtureProvider(t)
	got, err := p.Execute(context.Background(), "proj-test",
		"mcp__vornik__document_read_section",
		`{"artifact_id":"art-1","section_id":"002-body","limit_chars":5000}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var resp readSectionResponse
	if err := json.Unmarshal([]byte(got), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Content) != 5000 {
		t.Errorf("page 1 length = %d; want 5000", len(resp.Content))
	}
	if !resp.HasMore || resp.NextOffset != 5000 {
		t.Errorf("expected has_more + next_offset=5000; got has_more=%v next=%d",
			resp.HasMore, resp.NextOffset)
	}

	// Second page picks up where the first left off.
	got2, _ := p.Execute(context.Background(), "proj-test",
		"mcp__vornik__document_read_section",
		`{"artifact_id":"art-1","section_id":"002-body","limit_chars":5000,"offset_chars":5000}`)
	var resp2 readSectionResponse
	if err := json.Unmarshal([]byte(got2), &resp2); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}
	if len(resp2.Content) != 5000 {
		t.Errorf("page 2 length = %d", len(resp2.Content))
	}
}

func TestDocumentTools_ReadSection_OffsetsAreRunes(t *testing.T) {
	p, storageDir := fixtureProvider(t)
	if err := writeFile(filepath.Join(storageDir, "sections", "003-unicode.md"), "Ahoj světe"); err != nil {
		t.Fatalf("write unicode section: %v", err)
	}
	got, err := p.Execute(context.Background(), "proj-test",
		"mcp__vornik__document_read_section",
		`{"artifact_id":"art-1","section_id":"003-unicode","offset_chars":5,"limit_chars":3}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var resp readSectionResponse
	if err := json.Unmarshal([]byte(got), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !utf8.ValidString(resp.Content) {
		t.Fatalf("content is invalid UTF-8: %q", resp.Content)
	}
	if resp.Content != "svě" {
		t.Fatalf("content = %q; want rune-sliced %q", resp.Content, "svě")
	}
	if resp.TotalChars != 10 {
		t.Fatalf("TotalChars = %d; want 10", resp.TotalChars)
	}
}

func TestDocumentTools_LimitClampToMax(t *testing.T) {
	p, _ := fixtureProvider(t)
	got, err := p.Execute(context.Background(), "proj-test",
		"mcp__vornik__document_read_section",
		`{"artifact_id":"art-1","section_id":"002-body","limit_chars":999999}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var resp readSectionResponse
	_ = json.Unmarshal([]byte(got), &resp)
	if len(resp.Content) > maxSectionReadLimit {
		t.Errorf("limit not clamped: %d > %d", len(resp.Content), maxSectionReadLimit)
	}
}

func TestDocumentTools_CrossProject_ReturnsError(t *testing.T) {
	// The artifact belongs to proj-test; calling from proj-other
	// must not leak content. Lookup must surface a clear error,
	// not a 200 with someone else's text.
	p, _ := fixtureProvider(t)
	got, err := p.Execute(context.Background(), "proj-other",
		"mcp__vornik__document_get_metadata", `{"artifact_id":"art-1"}`)
	if err != nil {
		t.Fatalf("Execute should return wrapped JSON error, not Go error: %v", err)
	}
	if !strings.Contains(got, "error") || strings.Contains(got, "Test Book") {
		t.Errorf("cross-project lookup must NOT return content; got %s", got)
	}
}

func TestDocumentTools_UnknownArtifact_ReturnsError(t *testing.T) {
	p, _ := fixtureProvider(t)
	got, _ := p.Execute(context.Background(), "proj-test",
		"mcp__vornik__document_get_metadata", `{"artifact_id":"missing"}`)
	if !strings.Contains(got, "error") {
		t.Errorf("unknown artifact must return error JSON; got %s", got)
	}
}

func TestDocumentTools_ExtractionNotRun_GuidanceInError(t *testing.T) {
	// An artifact that exists but has no extracted_documents row
	// returns an actionable error so the LLM (or operator) knows
	// what to do — explicitly the /extract endpoint URL.
	p := NewDocumentToolProvider(DocumentToolDeps{
		Repo: &stubDocRepo{}, // no docs
		ArtifactRepo: &stubArtifactRepoForTools{arts: map[string]*persistence.Artifact{
			"art-1": {ID: "art-1", ProjectID: "proj-test", Name: "book.epub"},
		}},
	})
	got, _ := p.Execute(context.Background(), "proj-test",
		"mcp__vornik__document_get_metadata", `{"artifact_id":"art-1"}`)
	if !strings.Contains(got, "no extracted document") {
		t.Errorf("error must explain why; got %s", got)
	}
	if !strings.Contains(got, "/extract") {
		t.Errorf("error must point at the /extract endpoint; got %s", got)
	}
}

func TestDocumentTools_UnknownToolName(t *testing.T) {
	p, _ := fixtureProvider(t)
	_, err := p.Execute(context.Background(), "proj-test",
		"mcp__vornik__document_nonexistent", `{"artifact_id":"art-1"}`)
	if err == nil {
		t.Error("unknown tool name must surface a Go error (not in catalog)")
	}
	if !p.Owns("mcp__vornik__document_nonexistent") &&
		p.Owns("mcp__vornik__document_get_metadata") != true {
		t.Errorf("Owns() must distinguish real tool names")
	}
}

func TestDocumentTools_Owns_RejectsOtherServers(t *testing.T) {
	p, _ := fixtureProvider(t)
	// Real external MCP tools must NOT be claimed by Owns even if
	// the bare tool name happens to match.
	if p.Owns("mcp__broker__document_get_metadata") {
		t.Error("must not claim foreign server's tool name")
	}
	if p.Owns("memory_search") {
		t.Error("must not claim bare-name tools")
	}
	if !p.Owns("mcp__vornik__document_get_outline") {
		t.Error("must claim its own tools")
	}
}

// TestComposedExecutor_DispatchPreferenceBuiltinOverExternal pins
// the "vornik" server name is reserved for built-in tools — if an
// external MCP server registered the same name (defensive case)
// the built-in still wins for tool calls that match the document
// surface.
func TestComposedExecutor_DispatchPreferenceBuiltinOverExternal(t *testing.T) {
	p, _ := fixtureProvider(t)
	external := &mockMCPExecutor{
		executed: func(_ context.Context, _, name, _ string) (string, error) {
			return "external-served-" + name, nil
		},
	}
	composed := &ComposedMCPExecutor{
		External: external,
		Builtin:  p,
	}

	// Built-in tool — must go to the document provider.
	got, err := composed.Execute(context.Background(), "proj-test",
		"mcp__vornik__document_get_metadata", `{"artifact_id":"art-1"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.HasPrefix(got, "external-served") {
		t.Errorf("built-in tool incorrectly routed to external server: %q", got)
	}

	// External tool — must NOT match the built-in prefix and falls
	// through to the external executor.
	got, err = composed.Execute(context.Background(), "proj-test",
		"mcp__broker__get_account_summary", `{}`)
	if err != nil {
		t.Fatalf("external Execute: %v", err)
	}
	if got != "external-served-mcp__broker__get_account_summary" {
		t.Errorf("external dispatch failed: %q", got)
	}
}

func TestComposedExecutor_NilFields(t *testing.T) {
	// Tools returns nil slice when both sides are nil — the
	// /mcp/tools endpoint then surfaces an empty catalog rather
	// than 500ing.
	c := &ComposedMCPExecutor{}
	if got := c.Tools("p"); len(got) != 0 {
		t.Errorf("empty composed returned %d tools; want 0", len(got))
	}
	// Execute on a tool with no backends returns a typed error.
	_, err := c.Execute(context.Background(), "p", "mcp__broker__x", "{}")
	if err == nil {
		t.Error("expected error when no executor wired")
	}
}

func TestHasBuiltinPrefix(t *testing.T) {
	if !HasBuiltinPrefix("mcp__vornik__document_get_metadata") {
		t.Error("must recognise built-in prefix")
	}
	if HasBuiltinPrefix("mcp__broker__anything") {
		t.Error("must not match external server")
	}
	if HasBuiltinPrefix("memory_search") {
		t.Error("must not match bare names")
	}
}

// mockMCPExecutor lets the composed-executor test exercise the
// external-dispatch path without dragging in mcp.Manager.
type mockMCPExecutor struct {
	executed func(context.Context, string, string, string) (string, error)
}

func (m *mockMCPExecutor) Tools(string) []chat.Tool { return nil }
func (m *mockMCPExecutor) Execute(ctx context.Context, projectID, name, args string) (string, error) {
	if m.executed != nil {
		return m.executed(ctx, projectID, name, args)
	}
	return "", nil
}
