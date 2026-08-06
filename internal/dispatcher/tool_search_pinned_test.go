package dispatcher

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
)

// Regression tests for the 2026-07-15 "dispatcher says it has no access
// to the pagedrop MCP" incident: the assistant project's MCP catalog
// (27 tools) crossed DefaultDeferredToolThreshold when homeassistant was
// attached, so deferred loading hid every MCP tool behind tool_search —
// including mcp__pagedrop__pagedrop_protect, which the operator's chat
// system prompt documents BY NAME. The model trusted its function list
// over the prompt and refused in prose without ever calling tool_search.
// Tools the operator names in the system prompt are now pinned
// always-visible, and the threshold is operator-configurable.

func bigMCPCatalog(n int) []chat.Tool {
	out := make([]chat.Tool, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, chat.Tool{
			Type:     "function",
			Function: chat.ToolFunction{Name: fmt.Sprintf("mcp__srv__tool_%02d", i)},
		})
	}
	return out
}

func toolNameSet(tools []chat.Tool) map[string]struct{} {
	names := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		names[t.Function.Name] = struct{}{}
	}
	return names
}

func TestExtractPinnedMCPTools(t *testing.T) {
	t.Run("no mentions returns empty", func(t *testing.T) {
		assert.Empty(t, extractPinnedMCPTools("plain prose, no tools here"))
	})
	t.Run("qualified names extracted from prose", func(t *testing.T) {
		prompt := "Use mcp__pagedrop__pagedrop_protect to change or clear a page's " +
			"password, and `mcp__pagedrop__pagedrop_list` to find earlier pages."
		got := extractPinnedMCPTools(prompt)
		assert.Contains(t, got, "mcp__pagedrop__pagedrop_protect")
		assert.Contains(t, got, "mcp__pagedrop__pagedrop_list")
		assert.Len(t, got, 2)
	})
	t.Run("wildcard mentions are not pinned", func(t *testing.T) {
		// A prompt (or pasted config) saying mcp__* must not pin anything —
		// there is no concrete tool name to pin.
		assert.Empty(t, extractPinnedMCPTools("roles may hold mcp__* grants"))
	})
	t.Run("duplicates collapse", func(t *testing.T) {
		got := extractPinnedMCPTools("mcp__a__b then mcp__a__b again")
		assert.Len(t, got, 1)
	})
}

func TestApplyDeferredLoading_PinnedToolsVisibleAboveThreshold(t *testing.T) {
	builtin := DispatcherTools()
	mcp := bigMCPCatalog(25)
	pinned := map[string]struct{}{"mcp__srv__tool_07": {}}

	got := applyDeferredLoading(builtin, mcp, newExpandedToolStore(), "42", 20, pinned)
	names := toolNameSet(got)

	assert.Contains(t, names, "mcp__srv__tool_07",
		"a system-prompt-documented tool must stay visible when deferral engages")
	assert.Contains(t, names, ToolSearchName, "tool_search still surfaces for the rest of the catalog")
	assert.NotContains(t, names, "mcp__srv__tool_08", "unpinned tools stay deferred")
}

func TestApplyDeferredLoading_PinnedAndExpandedDoNotDuplicate(t *testing.T) {
	builtin := DispatcherTools()
	mcp := bigMCPCatalog(25)
	store := newExpandedToolStore()
	store.expand("42", []string{"mcp__srv__tool_07"})
	pinned := map[string]struct{}{"mcp__srv__tool_07": {}}

	got := applyDeferredLoading(builtin, mcp, store, "42", 20, pinned)
	count := 0
	for _, tl := range got {
		if tl.Function.Name == "mcp__srv__tool_07" {
			count++
		}
	}
	assert.Equal(t, 1, count, "a tool both expanded and pinned must appear exactly once")
}

func TestAllTools_SystemPromptPinnedToolSurvivesDeferral(t *testing.T) {
	mcp := &fakeMCPExecutor{tools: map[string][]chat.Tool{"assistant": bigMCPCatalog(25)}}
	a := &Agent{mcpManager: mcp, toolExecutor: &ToolExecutor{expanded: newExpandedToolStore()}}

	prompt := "PUBLISHING — use mcp__srv__tool_03 to protect a page."
	got := a.allTools("assistant", "559741208", chat.TierPeak, prompt)
	names := toolNameSet(got)

	assert.Contains(t, names, "mcp__srv__tool_03")
	assert.NotContains(t, names, "mcp__srv__tool_04")
	assert.Contains(t, names, ToolSearchName)
}

func TestAllTools_NegativeThresholdDisablesDeferral(t *testing.T) {
	mcp := &fakeMCPExecutor{tools: map[string][]chat.Tool{"p1": bigMCPCatalog(50)}}
	a := &Agent{
		mcpManager:            mcp,
		toolExecutor:          &ToolExecutor{expanded: newExpandedToolStore()},
		deferredToolThreshold: -1,
	}
	got := a.allTools("p1", "7", chat.TierPeak, "")
	names := toolNameSet(got)
	assert.Contains(t, names, "mcp__srv__tool_49", "negative threshold = never defer, full catalog visible")
	assert.NotContains(t, names, ToolSearchName)
}

func TestAllTools_CustomThresholdRespected(t *testing.T) {
	mcp := &fakeMCPExecutor{tools: map[string][]chat.Tool{"p1": bigMCPCatalog(25)}}

	// 25 tools with threshold 30: below threshold, everything visible.
	a := &Agent{
		mcpManager:            mcp,
		toolExecutor:          &ToolExecutor{expanded: newExpandedToolStore()},
		deferredToolThreshold: 30,
	}
	names := toolNameSet(a.allTools("p1", "7", chat.TierPeak, ""))
	assert.Contains(t, names, "mcp__srv__tool_24")
	assert.NotContains(t, names, ToolSearchName)

	// Same catalog with threshold 10: deferral engages.
	a.deferredToolThreshold = 10
	names = toolNameSet(a.allTools("p1", "7", chat.TierPeak, ""))
	assert.NotContains(t, names, "mcp__srv__tool_24")
	assert.Contains(t, names, ToolSearchName)
}

func TestWithDeferredToolThreshold_Option(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, nil, WithDeferredToolThreshold(-1))
	require.NotNil(t, a)
	assert.Equal(t, -1, a.deferredToolThreshold)
}
