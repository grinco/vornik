package dispatcher

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/registry"
)

// fakeMCPExecutor returns a fixed tool catalog so allTools tests can
// exercise the MCP-merge branch without standing up a real MCP server.
type fakeMCPExecutor struct {
	tools map[string][]chat.Tool
}

func (f *fakeMCPExecutor) Tools(projectID string) []chat.Tool {
	if f == nil || f.tools == nil {
		return nil
	}
	return f.tools[projectID]
}

func (f *fakeMCPExecutor) Execute(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}

func TestAllTools_BaseDispatcherToolsOnly(t *testing.T) {
	a := &Agent{}
	tools := a.allTools("", 0, chat.TierPeak)
	// Built-ins surface even when nothing else is wired.
	assert.NotEmpty(t, tools)
	// builtin "list_projects" must be present.
	found := false
	for _, tl := range tools {
		if tl.Function.Name == "list_projects" {
			found = true
			break
		}
	}
	assert.True(t, found, "list_projects must be in builtin catalog")
}

func TestAllTools_MCPMergedWhenProjectIDSet(t *testing.T) {
	mcp := &fakeMCPExecutor{tools: map[string][]chat.Tool{
		"p1": {
			{Type: "function", Function: chat.ToolFunction{Name: "mcp__server__op1"}},
			{Type: "function", Function: chat.ToolFunction{Name: "mcp__server__op2"}},
		},
	}}
	a := &Agent{mcpManager: mcp}
	tools := a.allTools("p1", 0, chat.TierPeak)
	names := map[string]struct{}{}
	for _, tl := range tools {
		names[tl.Function.Name] = struct{}{}
	}
	assert.Contains(t, names, "mcp__server__op1")
	assert.Contains(t, names, "mcp__server__op2")
}

func TestAllTools_EmptyProjectIDSkipsMCP(t *testing.T) {
	mcp := &fakeMCPExecutor{tools: map[string][]chat.Tool{
		"p1": {{Type: "function", Function: chat.ToolFunction{Name: "mcp__server__op1"}}},
	}}
	a := &Agent{mcpManager: mcp}
	tools := a.allTools("", 0, chat.TierPeak)
	for _, tl := range tools {
		assert.NotEqual(t, "mcp__server__op1", tl.Function.Name)
	}
}

func TestAllTools_ChatIDZeroBypassesDeferredLoading(t *testing.T) {
	// Even with an expanded store wired, chatID=0 forces the legacy
	// "everything visible" path — important for sub-agent contexts.
	a := &Agent{toolExecutor: &ToolExecutor{expanded: newExpandedToolStore()}}
	tools := a.allTools("p1", 0, chat.TierPeak)
	assert.NotEmpty(t, tools)
}

// TestExtractURLsFromText covers the URL extraction contract:
// JSON-embedded URLs, multiple URLs, trailing punctuation removed,
// no-URL input returns empty slice.
func TestExtractURLsFromText(t *testing.T) {
	t.Run("no URLs returns empty", func(t *testing.T) {
		assert.Empty(t, extractURLsFromText("just some text, no links"))
	})
	t.Run("single URL", func(t *testing.T) {
		got := extractURLsFromText("see https://example.com for details.")
		assert.Equal(t, []string{"https://example.com"}, got)
	})
	t.Run("multiple URLs separated by whitespace", func(t *testing.T) {
		got := extractURLsFromText("a https://a.example and b http://b.example then end")
		assert.Equal(t, []string{"https://a.example", "http://b.example"}, got)
	})
	t.Run("JSON-embedded URL extracted", func(t *testing.T) {
		// The regex strips trailing punctuation including " — so the
		// quote and surrounding JSON syntax don't bleed in.
		got := extractURLsFromText(`{"url":"https://json.example/path?x=1"}`)
		require.Len(t, got, 1)
		assert.Equal(t, "https://json.example/path?x=1", got[0])
	})
	t.Run("trailing punctuation stripped", func(t *testing.T) {
		got := extractURLsFromText("see https://example.com, then https://other.example.")
		assert.Equal(t, []string{"https://example.com", "https://other.example"}, got)
	})
	t.Run("URL with semicolon delimiter", func(t *testing.T) {
		got := extractURLsFromText("https://x.example;")
		assert.Equal(t, []string{"https://x.example"}, got)
	})
}

func TestProjectIDsFromRegistry_AllBranches(t *testing.T) {
	t.Run("nil input returns nil", func(t *testing.T) {
		assert.Nil(t, projectIDsFromRegistry(nil))
	})
	t.Run("empty input returns nil", func(t *testing.T) {
		assert.Nil(t, projectIDsFromRegistry([]*registry.Project{}))
	})
	t.Run("nil project skipped, others kept", func(t *testing.T) {
		projs := []*registry.Project{
			{ID: "p1"},
			nil,
			{ID: "p2"},
		}
		got := projectIDsFromRegistry(projs)
		assert.Equal(t, []string{"p1", "p2"}, got)
	})
	t.Run("single project returned", func(t *testing.T) {
		assert.Equal(t, []string{"only"}, projectIDsFromRegistry([]*registry.Project{{ID: "only"}}))
	})
}
