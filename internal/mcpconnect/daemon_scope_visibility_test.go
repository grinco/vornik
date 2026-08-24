package mcpconnect

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/mcpauth"
)

// A daemon-scope OAuth connection silently drops every write scope, and the
// operator has no way to find out.
//
// Begin() replaces the PRM's advertised scopes with readOnlyScopes() whenever a
// DAEMON-scope server declares no explicit auth.scopes (§12.2 — a daemon-scope
// token is reachable from every project, so write there must be named in config
// rather than inherited). The rule is right. What was missing is that it happens
// invisibly: the CLI prints the FINAL scope list, so an operator sees a
// plausible read-only ask and cannot tell that a broader set existed and was
// removed, nor what to do about it.
//
// Measured cost, 2026-08-22: the operator connected Atlassian through the normal
// control-plane flow and got a read-only grant
// (read:jira-work, read:account, read:me, read:*:compass). Every write tool was
// enabled server-side, so mcp.atlassian.com advertised 16 read tools and no
// createJiraIssue — which reads as "the vendor does not offer it", not as "we
// declined to ask". Diagnosing that took a code read; it should have been one
// line of CLI output.

func TestBegin_DaemonScopeReportsTheScopesItDropped(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:jira-work", "write:jira-work", "manage:everything", "offline_access")
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://vornik.example.com")

	// ProjectID "" = daemon scope, and no explicit Auth.Scopes → the downgrade.
	begun, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "", ServerName: "atlassian", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "operator@example.com")
	require.NoError(t, err)

	// The behaviour itself is unchanged: still read-only.
	assert.NotContains(t, begun.Scopes, "write:jira-work", "the downgrade must still apply")
	assert.Contains(t, begun.Scopes, "read:jira-work")

	// ...but what it removed is now reportable.
	assert.ElementsMatch(t, []string{"write:jira-work", "manage:everything"}, begun.DroppedScopes,
		"the operator must be able to see which scopes were withheld")
	assert.True(t, begun.ScopesDowngraded(), "a downgraded ask must announce itself")
}

// A daemon-scope server that names its scopes explicitly is an operator decision
// already made — nothing is dropped and nothing is announced.
func TestBegin_ExplicitDaemonScopesAreNotDowngraded(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:jira-work", "write:jira-work", "offline_access")
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://vornik.example.com")

	begun, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "", ServerName: "atlassian", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth, Scopes: []string{"read:jira-work", "write:jira-work"}},
	}, "operator@example.com")
	require.NoError(t, err)

	assert.Contains(t, begun.Scopes, "write:jira-work", "an explicit ask is honoured verbatim")
	assert.Empty(t, begun.DroppedScopes)
	assert.False(t, begun.ScopesDowngraded())
}

// PROJECT-scoped is the escape hatch, and the rule must not fire there — that is
// what makes "connect per-project" a real answer to a stripped write scope.
func TestBegin_ProjectScopeKeepsWriteScopes(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:jira-work", "write:jira-work", "offline_access")
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://vornik.example.com")

	begun, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "vornik-marketing", ServerName: "atlassian", URL: vendor.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth},
	}, "operator@example.com")
	require.NoError(t, err)

	assert.Contains(t, begun.Scopes, "write:jira-work",
		"a project-scoped grant is confined to one project, so it keeps the advertised set")
	assert.Empty(t, begun.DroppedScopes)
}

// The remedy has to travel with the finding. An operator who reads "2 scopes
// withheld" and is not told the two ways out has been informed, not helped.
func TestDroppedScopeNotice_NamesBothRemedies(t *testing.T) {
	notice := DroppedScopeNotice("atlassian", []string{"write:jira-work"})

	require.NotEmpty(t, notice)
	assert.Contains(t, notice, "write:jira-work", "say what was withheld")
	assert.Contains(t, notice, "auth.scopes", "remedy 1: name them in config")
	assert.True(t, strings.Contains(notice, "--project") || strings.Contains(notice, "-p "),
		"remedy 2: connect per-project")
	assert.Empty(t, DroppedScopeNotice("atlassian", nil), "nothing dropped, nothing to say")
}
