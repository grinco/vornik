package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// fakeMCPSource is a deterministic MCPRegistrySource for tests.
// Either returns the seeded servers or the seeded error so each
// test can pin one behaviour at a time.
type fakeMCPSource struct {
	servers []MCPRegistryServer
	err     error
	calls   int
}

func (f *fakeMCPSource) Servers(_ context.Context) ([]MCPRegistryServer, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.servers, nil
}

// seededMCPSource builds a fakeMCPSource with one server +
// two tools so tests have a predictable surface to assert on.
func seededMCPSource() *fakeMCPSource {
	return &fakeMCPSource{
		servers: []MCPRegistryServer{
			{
				Name:      "scraper",
				Transport: "sse",
				URL:       "http://scraper.local/sse",
				Reachable: true,
				Tools: []MCPRegistryTool{
					{Name: "web_fetch", Description: "Fetch a URL"},
					{Name: "html_extract", Description: "Extract content"},
				},
			},
		},
	}
}

// TestPopulateMCPSection_NoSourceNoBanner — when no source is
// wired AND the project has no MCP block, the form renders the
// banner explaining the missing registry. Project-driven custom
// rows still surface (here: none).
func TestPopulateMCPSection_NilSourceBanner(t *testing.T) {
	var data ProjectConfigFormData
	populateMCPSection(&data, nil, &registry.Project{})

	assert.True(t, data.MCPRegistryUnavailable)
	assert.Contains(t, data.MCPRegistryError, "no daemon-level MCP registry")
	assert.Empty(t, data.MCPRegistryRows)
	assert.Empty(t, data.MCPCustomRows)
}

// TestPopulateMCPSection_RegistryError — source errors degrade
// gracefully: banner fires with the upstream error, custom rows
// still render.
func TestPopulateMCPSection_RegistryError(t *testing.T) {
	src := &fakeMCPSource{err: errors.New("daemon unreachable")}
	proj := &registry.Project{
		MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{
			{Name: "custom-only", Transport: "http", URL: "http://x/y", AllowedTools: []string{"a"}},
		}},
	}
	var data ProjectConfigFormData
	populateMCPSection(&data, src, proj)

	assert.True(t, data.MCPRegistryUnavailable)
	assert.Contains(t, data.MCPRegistryError, "daemon unreachable")
	assert.Empty(t, data.MCPRegistryRows)
	require.Len(t, data.MCPCustomRows, 1)
	assert.Equal(t, "custom-only", data.MCPCustomRows[0].Name)
	assert.Equal(t, "http", data.MCPCustomRows[0].Transport)
	assert.Equal(t, "a", data.MCPCustomRows[0].AllowedTools)
}

// TestPopulateMCPSection_SubscribedAllTools — a project that
// subscribes to a registry server with no allowed_tools narrowing
// renders AllowAllTools=true on its row.
func TestPopulateMCPSection_SubscribedAllTools(t *testing.T) {
	src := seededMCPSource()
	proj := &registry.Project{
		MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{
			{Name: "scraper", Transport: "sse"},
		}},
	}
	var data ProjectConfigFormData
	populateMCPSection(&data, src, proj)

	require.Len(t, data.MCPRegistryRows, 1)
	row := data.MCPRegistryRows[0]
	assert.True(t, row.Subscribed)
	assert.True(t, row.AllowAllTools)
	assert.Empty(t, row.AllowedTools)
}

// TestPopulateMCPSection_SubscribedWithNarrowing — when the
// project lists allowed_tools, the row drops AllowAllTools and
// pre-ticks the named tools.
func TestPopulateMCPSection_SubscribedWithNarrowing(t *testing.T) {
	src := seededMCPSource()
	proj := &registry.Project{
		MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{
			{Name: "scraper", Transport: "sse", AllowedTools: []string{"web_fetch"}},
		}},
	}
	var data ProjectConfigFormData
	populateMCPSection(&data, src, proj)

	require.Len(t, data.MCPRegistryRows, 1)
	row := data.MCPRegistryRows[0]
	assert.True(t, row.Subscribed)
	assert.False(t, row.AllowAllTools)
	assert.True(t, row.AllowedTools["web_fetch"])
	assert.False(t, row.AllowedTools["html_extract"])
}

// TestPopulateMCPSection_UnsubscribedDefaultsAllowAll — a server
// the project hasn't subscribed to defaults to AllowAllTools=true
// so the "selected only" checkbox group starts empty (operator
// has to explicitly opt in to narrowing).
func TestPopulateMCPSection_UnsubscribedDefaultsAllowAll(t *testing.T) {
	src := seededMCPSource()
	var data ProjectConfigFormData
	populateMCPSection(&data, src, &registry.Project{})

	require.Len(t, data.MCPRegistryRows, 1)
	row := data.MCPRegistryRows[0]
	assert.False(t, row.Subscribed)
	assert.True(t, row.AllowAllTools, "default for a fresh row is full catalog")
}

// TestPopulateMCPSection_CustomMergedBelowRegistry — a project
// MCP entry whose name isn't in the daemon registry renders as a
// custom row, NOT as a missing registry row.
func TestPopulateMCPSection_CustomMergedBelowRegistry(t *testing.T) {
	src := seededMCPSource()
	proj := &registry.Project{
		MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{
			{Name: "scraper", Transport: "sse"},
			{Name: "secret-experiment", Transport: "stdio", AllowedTools: []string{"do_thing"}},
		}},
	}
	var data ProjectConfigFormData
	populateMCPSection(&data, src, proj)

	require.Len(t, data.MCPRegistryRows, 1)
	assert.Equal(t, "scraper", data.MCPRegistryRows[0].Server.Name)
	require.Len(t, data.MCPCustomRows, 1)
	assert.Equal(t, "secret-experiment", data.MCPCustomRows[0].Name)
	assert.Equal(t, "stdio", data.MCPCustomRows[0].Transport)
	assert.Equal(t, "do_thing", data.MCPCustomRows[0].AllowedTools)
}

// TestOverlayMCPSection_RebuildsFromForm — POST values overwrite
// the pre-populated data so the failure-fallback render reflects
// what the operator just submitted.
func TestOverlayMCPSection_RebuildsFromForm(t *testing.T) {
	data := ProjectConfigFormData{
		MCPRegistryRows: []MCPRegistryRow{{
			Server:        MCPRegistryServer{Name: "scraper", Tools: []MCPRegistryTool{{Name: "web_fetch"}, {Name: "html_extract"}}},
			Subscribed:    false,
			AllowAllTools: true,
			AllowedTools:  map[string]bool{},
		}},
	}
	form := url.Values{}
	form.Set("mcpSubscribe_scraper", "on")
	form.Set("mcpAllowedToolsMode_scraper", "selected")
	form.Set("mcpAllowedTool_scraper_web_fetch", "on")
	form.Set("mcpCustomCount", "1")
	form.Set("mcpCustom_0_name", "extra")
	form.Set("mcpCustom_0_transport", "http")
	form.Set("mcpCustom_0_url", "http://e/x")
	form.Set("mcpCustom_0_allowedTools", "tool_a\ntool_b")
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	require.NoError(t, req.ParseForm())

	overlayMCPSection(&data, req)

	row := data.MCPRegistryRows[0]
	assert.True(t, row.Subscribed)
	assert.False(t, row.AllowAllTools)
	assert.True(t, row.AllowedTools["web_fetch"])
	assert.False(t, row.AllowedTools["html_extract"])

	require.Len(t, data.MCPCustomRows, 1)
	assert.Equal(t, "extra", data.MCPCustomRows[0].Name)
	assert.Equal(t, "http", data.MCPCustomRows[0].Transport)
	assert.Equal(t, "http://e/x", data.MCPCustomRows[0].URL)
	assert.Equal(t, "tool_a\ntool_b", data.MCPCustomRows[0].AllowedTools)
}

// TestOverlayMCPSection_DropsBlankNameCustomRow — an empty name
// in a custom row treats it as a deletion (operator cleared the
// field). Keeps the form simple — no separate "remove" button.
func TestOverlayMCPSection_DropsBlankNameCustomRow(t *testing.T) {
	data := ProjectConfigFormData{}
	form := url.Values{}
	form.Set("mcpCustomCount", "2")
	form.Set("mcpCustom_0_name", "")
	form.Set("mcpCustom_0_transport", "sse")
	form.Set("mcpCustom_1_name", "keep-me")
	form.Set("mcpCustom_1_transport", "stdio")
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	require.NoError(t, req.ParseForm())

	overlayMCPSection(&data, req)

	require.Len(t, data.MCPCustomRows, 1)
	assert.Equal(t, "keep-me", data.MCPCustomRows[0].Name)
}

// TestOverlayMCPSection_ClampsCustomCount — a malformed count
// (negative or hostile) doesn't trigger an unbounded loop. Pin
// the safety net so a fuzz POST can't tar-pit the handler.
func TestOverlayMCPSection_ClampsCustomCount(t *testing.T) {
	data := ProjectConfigFormData{}
	form := url.Values{}
	form.Set("mcpCustomCount", "999999")
	form.Set("mcpCustom_0_name", "x")
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	require.NoError(t, req.ParseForm())

	overlayMCPSection(&data, req)

	// Clamp at 256 (per parseFormUint contract). The 0th row's
	// name="x" is the only thing we asserted exists; the rest
	// are blank and skipped, so len == 1.
	require.Len(t, data.MCPCustomRows, 1)
}

// TestBuildMCPServersValue_NoSubscriptions_NoCustoms — zero
// subscribed + zero custom → empty value, empty=true. Caller
// uses this to delete the whole mcp block.
func TestBuildMCPServersValue_Empty(t *testing.T) {
	data := &ProjectConfigFormData{
		MCPRegistryRows: []MCPRegistryRow{{
			Server:     MCPRegistryServer{Name: "scraper"},
			Subscribed: false,
		}},
	}
	value, empty := buildMCPServersValue(data)
	assert.True(t, empty)
	assert.Empty(t, value)
}

// TestBuildMCPServersValue_SubscribedAllTools — a subscribed row
// with AllowAllTools=true emits an entry WITHOUT allowed_tools.
// Matches the loader's "empty allowed_tools = inherit catalog"
// semantics.
func TestBuildMCPServersValue_SubscribedAllTools(t *testing.T) {
	data := &ProjectConfigFormData{
		MCPRegistryRows: []MCPRegistryRow{{
			Server:        MCPRegistryServer{Name: "scraper", Transport: "sse", URL: "http://x/sse", Tools: []MCPRegistryTool{{Name: "web_fetch"}}},
			Subscribed:    true,
			AllowAllTools: true,
			AllowedTools:  map[string]bool{"web_fetch": true}, // ignored when AllowAllTools=true
		}},
	}
	value, empty := buildMCPServersValue(data)
	assert.False(t, empty)
	require.Len(t, value, 1)
	assert.Equal(t, "scraper", value[0]["name"])
	assert.Equal(t, "sse", value[0]["transport"])
	assert.Equal(t, "http://x/sse", value[0]["url"])
	_, hasTools := value[0]["allowed_tools"]
	assert.False(t, hasTools, "AllowAllTools=true must omit allowed_tools")
}

// TestBuildMCPServersValue_SubscribedWithNarrowing — selected
// tools surface in the value's allowed_tools slice.
func TestBuildMCPServersValue_SubscribedWithNarrowing(t *testing.T) {
	data := &ProjectConfigFormData{
		MCPRegistryRows: []MCPRegistryRow{{
			Server:        MCPRegistryServer{Name: "scraper", Transport: "sse", Tools: []MCPRegistryTool{{Name: "web_fetch"}, {Name: "html_extract"}}},
			Subscribed:    true,
			AllowAllTools: false,
			AllowedTools:  map[string]bool{"web_fetch": true},
		}},
	}
	value, _ := buildMCPServersValue(data)
	require.Len(t, value, 1)
	tools, ok := value[0]["allowed_tools"].([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"web_fetch"}, tools)
}

// TestBuildMCPServersValue_CustomRowAppended — custom rows
// follow registry rows in the emitted value. Each carries
// name + transport + url + (optional) allowed_tools.
func TestBuildMCPServersValue_CustomRowAppended(t *testing.T) {
	data := &ProjectConfigFormData{
		MCPCustomRows: []MCPCustomRow{{
			Name:         "extra",
			Transport:    "http",
			URL:          "http://e/x",
			AllowedTools: "tool_a\ntool_b",
		}},
	}
	value, empty := buildMCPServersValue(data)
	assert.False(t, empty)
	require.Len(t, value, 1)
	assert.Equal(t, "extra", value[0]["name"])
	assert.Equal(t, "http", value[0]["transport"])
	assert.Equal(t, "http://e/x", value[0]["url"])
	tools, ok := value[0]["allowed_tools"].([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"tool_a", "tool_b"}, tools)
}

// TestBuildMCPPatches_DeletesWhenEmpty — zero subscriptions →
// the patch list is a single RemoveIfEmpty delete on mcp.servers.
// Lets the existing patcher clear the block from the YAML.
func TestBuildMCPPatches_DeletesWhenEmpty(t *testing.T) {
	patches := buildMCPPatches(&ProjectConfigFormData{})
	require.Len(t, patches, 1)
	assert.Equal(t, []string{"mcp", "servers"}, patches[0].Path)
	assert.True(t, patches[0].RemoveIfEmpty)
}

// TestParseFormUint — clamps to max, rejects negatives, defaults
// blank/junk to 0. Pins the safety contract used by the custom-
// row count parser.
func TestParseFormUint(t *testing.T) {
	assert.Equal(t, 0, parseFormUint("", 256))
	assert.Equal(t, 0, parseFormUint("junk", 256))
	assert.Equal(t, 0, parseFormUint("-5", 256))
	assert.Equal(t, 5, parseFormUint("5", 256))
	assert.Equal(t, 256, parseFormUint("999999", 256))
}

// TestProjectConfigFormEdit_RendersMCPSection — the form's MCP
// section surfaces one subscribe checkbox per daemon-known
// server + the pre-ticked allowed-tools rows for any project the
// operator already subscribed.
func TestProjectConfigFormEdit_RendersMCPSection(t *testing.T) {
	root := writeFormFixture(t)
	// Subscribe the project to scraper with a narrowed allowed_tools list.
	path := filepath.Join(root, "projects", "form-demo.yaml")
	existing, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(existing, []byte(`
mcp:
  servers:
    - name: scraper
      transport: sse
      url: "http://scraper.local/sse"
      allowed_tools:
        - web_fetch
`)...), 0o600))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(
		WithProjectRegistry(reg),
		WithConfigReloader(reloader),
		WithMCPFormRegistrySource(seededMCPSource()),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	// Section heading + the subscribe / per-tool field names.
	assert.Contains(t, body, "MCP servers")
	assert.Contains(t, body, `name="mcpSubscribe_scraper"`)
	assert.Contains(t, body, `name="mcpAllowedToolsMode_scraper"`)
	assert.Contains(t, body, `name="mcpAllowedTool_scraper_web_fetch"`)
	assert.Contains(t, body, `name="mcpAllowedTool_scraper_html_extract"`)
	// web_fetch is pre-ticked (the project's current narrowing).
	idx := strings.Index(body, `name="mcpAllowedTool_scraper_web_fetch"`)
	require.GreaterOrEqual(t, idx, 0)
	// `checked` should appear in the same input — within ~120
	// chars of the name attribute.
	window := body[idx:min(idx+200, len(body))]
	assert.Contains(t, window, "checked", "pre-ticked tool must surface as checked")
	// The "selected only" radio is ticked because allowed_tools is non-empty.
	assert.Contains(t, body, `value="selected" checked`)
	// And the subscribe checkbox itself is checked.
	idxSub := strings.Index(body, `name="mcpSubscribe_scraper"`)
	require.GreaterOrEqual(t, idxSub, 0)
	assert.Contains(t, body[idxSub:min(idxSub+200, len(body))], "checked")
}

// TestProjectConfigFormEdit_MCPRegistryUnavailable — when no
// source is wired the form still renders, with a banner
// explaining the missing registry. Custom rows still surface.
func TestProjectConfigFormEdit_MCPRegistryUnavailable(t *testing.T) {
	root := writeFormFixture(t)
	path := filepath.Join(root, "projects", "form-demo.yaml")
	existing, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(existing, []byte(`
mcp:
  servers:
    - name: custom-only
      transport: http
      url: "http://x/y"
`)...), 0o600))
	server, _ := formServer(t, root) // no MCPRegistrySource

	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	assert.Contains(t, body, "Daemon MCP registry is unavailable")
	// Custom row still surfaces.
	assert.Contains(t, body, `name="mcpCustom_0_name"`)
	assert.Contains(t, body, "custom-only")
}

// TestProjectConfigFormEdit_MCPRegistryErrorBanner — a source
// that returns an error renders the banner with the error message.
func TestProjectConfigFormEdit_MCPRegistryErrorBanner(t *testing.T) {
	root := writeFormFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	server := NewServer(
		WithProjectRegistry(reg),
		WithMCPFormRegistrySource(&fakeMCPSource{err: errors.New("503 unreachable")}),
	)

	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Daemon MCP registry is unavailable")
	assert.Contains(t, body, "503 unreachable")
}

// TestProjectConfigFormSave_MCPSubscribeRoundTrip — POST a
// subscribe + selected tools, assert the YAML on disk has the
// matching mcp.servers entry and the registry round-trips it.
func TestProjectConfigFormSave_MCPSubscribeRoundTrip(t *testing.T) {
	root := writeFormFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(
		WithProjectRegistry(reg),
		WithConfigReloader(reloader),
		WithMCPFormRegistrySource(seededMCPSource()),
	)

	form := baselineFormValues()
	form.Set("mcpSubscribe_scraper", "on")
	form.Set("mcpAllowedToolsMode_scraper", "selected")
	form.Set("mcpAllowedTool_scraper_web_fetch", "on")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, reloader.calls)

	proj := reg.GetProject("form-demo")
	require.NotNil(t, proj)
	require.Len(t, proj.MCP.Servers, 1)
	srv := proj.MCP.Servers[0]
	assert.Equal(t, "scraper", srv.Name)
	assert.Equal(t, "sse", srv.Transport)
	assert.Equal(t, "http://scraper.local/sse", srv.URL)
	assert.Equal(t, []string{"web_fetch"}, srv.AllowedTools)
}

// TestProjectConfigFormSave_MCPAllToolsOmitsAllowedTools — when
// the operator picks "all", the saved YAML must NOT include an
// allowed_tools field (inherit full catalog).
func TestProjectConfigFormSave_MCPAllToolsOmitsAllowedTools(t *testing.T) {
	root := writeFormFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(
		WithProjectRegistry(reg),
		WithConfigReloader(reloader),
		WithMCPFormRegistrySource(seededMCPSource()),
	)
	path := filepath.Join(root, "projects", "form-demo.yaml")

	form := baselineFormValues()
	form.Set("mcpSubscribe_scraper", "on")
	form.Set("mcpAllowedToolsMode_scraper", "all")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(got), "name: \"scraper\"")
	assert.NotContains(t, string(got), "allowed_tools", "AllowAll mode must omit allowed_tools key")
}

// TestProjectConfigFormSave_MCPUnsubscribeRemovesEntry — saving
// the form without a subscribe checkbox for a previously-
// subscribed server removes the entry from disk.
func TestProjectConfigFormSave_MCPUnsubscribeRemovesEntry(t *testing.T) {
	root := writeFormFixture(t)
	path := filepath.Join(root, "projects", "form-demo.yaml")
	existing, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(existing, []byte(`
mcp:
  servers:
    - name: scraper
      transport: sse
      allowed_tools:
        - web_fetch
`)...), 0o600))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(
		WithProjectRegistry(reg),
		WithConfigReloader(reloader),
		WithMCPFormRegistrySource(seededMCPSource()),
	)

	// Form with NO subscribe checkbox for scraper → unsubscribe.
	form := baselineFormValues()
	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	proj := reg.GetProject("form-demo")
	require.NotNil(t, proj)
	assert.Empty(t, proj.MCP.Servers, "scraper entry must be removed when subscribe checkbox is missing")
	// YAML on disk should not have an mcp.servers entry either.
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(got), "name: \"scraper\"")
}

// TestProjectConfigFormSave_MCPAddCustomServer — POST with new
// custom-server fields → YAML gets a new server entry; registry
// round-trips it.
func TestProjectConfigFormSave_MCPAddCustomServer(t *testing.T) {
	root := writeFormFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(
		WithProjectRegistry(reg),
		WithConfigReloader(reloader),
		WithMCPFormRegistrySource(&fakeMCPSource{}),
	)

	form := baselineFormValues()
	form.Set("mcpCustomCount", "1")
	form.Set("mcpCustom_0_name", "secret-experiment")
	form.Set("mcpCustom_0_transport", "stdio")
	form.Set("mcpCustom_0_url", "")
	form.Set("mcpCustom_0_allowedTools", "do_thing\ndo_other")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	proj := reg.GetProject("form-demo")
	require.NotNil(t, proj)
	require.Len(t, proj.MCP.Servers, 1)
	srv := proj.MCP.Servers[0]
	assert.Equal(t, "secret-experiment", srv.Name)
	assert.Equal(t, "stdio", srv.Transport)
	assert.Equal(t, "", srv.URL)
	assert.Equal(t, []string{"do_thing", "do_other"}, srv.AllowedTools)
}

// TestProjectConfigFormSave_MCPRegistryUnavailablePreservesExistingSubscriptions
// — when the daemon registry is unavailable, a project's existing
// mcp.servers entries are rendered as "custom" rows in the form
// and round-trip through a save unchanged. Pins the fail-safe so
// a transient registry outage doesn't silently wipe operator
// subscriptions on the next save.
func TestProjectConfigFormSave_MCPRegistryUnavailablePreservesExistingSubscriptions(t *testing.T) {
	root := writeFormFixture(t)
	path := filepath.Join(root, "projects", "form-demo.yaml")
	existing, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(existing, []byte(`
mcp:
  servers:
    - name: scraper
      transport: sse
      url: "http://scraper.local/sse"
      allowed_tools:
        - web_fetch
`)...), 0o600))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &reloadingReloader{reg: reg, root: root}
	// Registry source errors — operator can't see daemon servers
	// but the project's existing subscription must survive.
	server := NewServer(
		WithProjectRegistry(reg),
		WithConfigReloader(reloader),
		WithMCPFormRegistrySource(&fakeMCPSource{err: errors.New("503")}),
	)

	// Initial GET — the scraper subscription shows up as a custom
	// row (because the daemon registry is unavailable to merge it).
	getReq := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	getRec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(getRec, getReq, "form-demo")
	require.Equal(t, http.StatusOK, getRec.Code)
	body := getRec.Body.String()
	assert.Contains(t, body, `name="mcpCustom_0_name"`)
	assert.Contains(t, body, `value="scraper"`, "existing subscription must surface as a custom row")

	// POST — round-trip the custom row exactly as the browser would.
	form := baselineFormValues()
	form.Set("mcpCustomCount", "1")
	form.Set("mcpCustom_0_name", "scraper")
	form.Set("mcpCustom_0_transport", "sse")
	form.Set("mcpCustom_0_url", "http://scraper.local/sse")
	form.Set("mcpCustom_0_allowedTools", "web_fetch")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	proj := reg.GetProject("form-demo")
	require.NotNil(t, proj)
	require.Len(t, proj.MCP.Servers, 1)
	srv := proj.MCP.Servers[0]
	assert.Equal(t, "scraper", srv.Name)
	assert.Equal(t, "sse", srv.Transport)
	assert.Equal(t, "http://scraper.local/sse", srv.URL)
	assert.Equal(t, []string{"web_fetch"}, srv.AllowedTools)
}

// TestProjectConfigFormSave_MCPRegistryStaleStillSaves — when
// the daemon registry source is unavailable, the operator can
// still save (e.g. tweaks to unrelated form fields). The form
// shows the banner; the save path doesn't crash.
func TestProjectConfigFormSave_MCPRegistryStaleStillSaves(t *testing.T) {
	root := writeFormFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(
		WithProjectRegistry(reg),
		WithConfigReloader(reloader),
		WithMCPFormRegistrySource(&fakeMCPSource{err: errors.New("daemon unreachable")}),
	)

	form := baselineFormValues()
	form.Set("displayName", "Stale Registry Demo")
	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	proj := reg.GetProject("form-demo")
	require.NotNil(t, proj)
	assert.Equal(t, "Stale Registry Demo", proj.DisplayName)
	// Banner must still surface in the post-save render.
	assert.Contains(t, rec.Body.String(), "Daemon MCP registry is unavailable")
}

// TestProjectConfigFormSave_MCPCustomDeleteByBlankName — clearing
// a custom row's name field on save removes the row.
func TestProjectConfigFormSave_MCPCustomDeleteByBlankName(t *testing.T) {
	root := writeFormFixture(t)
	path := filepath.Join(root, "projects", "form-demo.yaml")
	existing, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(existing, []byte(`
mcp:
  servers:
    - name: doomed
      transport: stdio
`)...), 0o600))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	reloader := &reloadingReloader{reg: reg, root: root}
	server := NewServer(
		WithProjectRegistry(reg),
		WithConfigReloader(reloader),
		WithMCPFormRegistrySource(&fakeMCPSource{}),
	)

	form := baselineFormValues()
	form.Set("mcpCustomCount", "1")
	form.Set("mcpCustom_0_name", "") // clear → delete
	form.Set("mcpCustom_0_transport", "stdio")

	rec := httptest.NewRecorder()
	server.ProjectConfigFormSave(rec, postForm(form), "form-demo")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	proj := reg.GetProject("form-demo")
	require.NotNil(t, proj)
	assert.Empty(t, proj.MCP.Servers)
}

// TestProjectConfigFormEdit_NoLongerMentionsMCPInFootnote — the
// regression bait: the "Other settings" footnote must no longer
// claim MCP servers live in Advanced YAML, since this slice's
// whole purpose is to bring them into the form.
func TestProjectConfigFormEdit_NoLongerMentionsMCPInFootnote(t *testing.T) {
	root := writeFormFixture(t)
	server, _ := formServer(t, root)
	req := httptest.NewRequest(http.MethodGet, "/projects/form-demo/config/form", nil)
	rec := httptest.NewRecorder()
	server.ProjectConfigFormEdit(rec, req, "form-demo")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	// Footnote is still there for webhooks + verifiers.
	assert.Contains(t, body, "Signed webhooks")
	// But MCP servers should NOT appear in the footnote (they
	// have their own section now). Spot-check by the exact phrase
	// from the old footer.
	idx := strings.Index(body, "Signed webhooks")
	require.GreaterOrEqual(t, idx, 0)
	// Inspect the ~200 chars following the footnote heading.
	window := body[idx:min(idx+300, len(body))]
	assert.NotContains(t, window, "MCP servers", "footnote must no longer list MCP servers")
}

// min is a local helper because the package compiles against
// pre-1.21 in some envs.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
