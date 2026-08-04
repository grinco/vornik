package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// mcpGrantMismatch is the §7.2a N1 verifier: the CLI never sees the token, so "a row appeared" is
// not evidence the operator got what they consented to. These pin that a discrepancy is LOUD.

func TestMCPGrantMismatch_AgreementIsSilent(t *testing.T) {
	req := mcpOAuthBeginResp{
		Resource: "https://mcp.atlassian.com/v1/mcp/authv2",
		Scopes:   []string{"read:jira-work", "offline_access"},
	}
	rec := mcpOAuthStatusResp{
		Connected: true,
		Resource:  "https://mcp.atlassian.com/v1/mcp/authv2",
		// Order must not matter — an authorization server is free to
		// reorder, and a spurious mismatch would train operators to ignore
		// the check.
		Scopes: []string{"offline_access", "read:jira-work"},
	}
	assert.Empty(t, mcpGrantMismatch(req, rec))
}

func TestMCPGrantMismatch_NoGrantRecorded(t *testing.T) {
	assert.Contains(t,
		mcpGrantMismatch(mcpOAuthBeginResp{Resource: "https://r"}, mcpOAuthStatusResp{}),
		"no grant was recorded")
}

// TestMCPGrantMismatch_DifferentResourceIsTheSeriousOne — F5 makes this concrete: Trello and Jira
// share an authorization server, so a grant recorded against the wrong resource is a token that
// would be presented to the wrong audience.
func TestMCPGrantMismatch_DifferentResourceIsTheSeriousOne(t *testing.T) {
	got := mcpGrantMismatch(
		mcpOAuthBeginResp{Resource: "https://mcp.atlassian.com/v1/mcp/authv2"},
		mcpOAuthStatusResp{Connected: true, Resource: "https://mcp.trello.com/v1"},
	)
	assert.Contains(t, got, "resource")
	assert.Contains(t, got, "mcp.trello.com")
}

// TestMCPGrantMismatch_WiderScopesAreReported — the direction that matters most: the operator
// consented to less than was recorded.
func TestMCPGrantMismatch_WiderScopesAreReported(t *testing.T) {
	got := mcpGrantMismatch(
		mcpOAuthBeginResp{Resource: "https://r", Scopes: []string{"read:jira-work"}},
		mcpOAuthStatusResp{Connected: true, Resource: "https://r",
			Scopes: []string{"read:jira-work", "write:jira-work"}},
	)
	assert.Contains(t, got, "write:jira-work")
	assert.Contains(t, got, "not requested")
}

// TestMCPGrantMismatch_NarrowerScopesAreReported — an AS narrowing a grant is legitimate, but the
// operator should hear about it now rather than discover it later as a puzzling 403.
func TestMCPGrantMismatch_NarrowerScopesAreReported(t *testing.T) {
	got := mcpGrantMismatch(
		mcpOAuthBeginResp{Resource: "https://r", Scopes: []string{"read:jira-work", "offline_access"}},
		mcpOAuthStatusResp{Connected: true, Resource: "https://r", Scopes: []string{"read:jira-work"}},
	)
	assert.Contains(t, got, "offline_access")
	assert.Contains(t, got, "not granted")
}

func TestMCPGrantMismatch_BothDirectionsAtOnce(t *testing.T) {
	got := mcpGrantMismatch(
		mcpOAuthBeginResp{Resource: "https://r", Scopes: []string{"read:a"}},
		mcpOAuthStatusResp{Connected: true, Resource: "https://r", Scopes: []string{"write:b"}},
	)
	assert.Contains(t, got, "write:b")
	assert.Contains(t, got, "read:a")
}

// TestMCPGrantMismatch_UnknownRequestedScopesSkipTheComparison — when config named no scopes and
// the server advertised none, there is nothing to compare and a mismatch would be noise.
func TestMCPGrantMismatch_UnknownRequestedScopesSkipTheComparison(t *testing.T) {
	assert.Empty(t, mcpGrantMismatch(
		mcpOAuthBeginResp{Resource: "https://r"},
		mcpOAuthStatusResp{Connected: true, Resource: "https://r", Scopes: []string{"whatever"}},
	))
}

func TestMCPScopeList_EmptyIsExplained(t *testing.T) {
	assert.Contains(t, mcpScopeList(nil), "none")
	assert.Equal(t, "a b", mcpScopeList([]string{"a", "b"}))
}

func TestMCPScopeLabel(t *testing.T) {
	t.Cleanup(func() { mcpProject = "" })
	mcpProject = ""
	assert.Equal(t, "the daemon scope", mcpScopeLabel())
	mcpProject = "p1"
	assert.Equal(t, "project p1", mcpScopeLabel())
	assert.Equal(t, " -p p1", mcpProjectFlagSuffix())
}
