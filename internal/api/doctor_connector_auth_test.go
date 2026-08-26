package api

import (
	"strings"
	"testing"
	"time"
)

func ptrTime(t time.Time) *time.Time { return &t }

// THE check that would have caught the 2026-08-25 P0 on its first run.
//
// The stored grant reads perfectly healthy — connected, unexpired, not flagged
// — while the vendor rejects every call. That combination is not a warning: it
// is the exact shape of a connector that has silently died, and the whole
// reason a stored-state-only check was not enough.
func TestConnectorAuthFlagsFailuresDespiteAHealthyGrant(t *testing.T) {
	now := time.Now().UTC()
	grants := []connectorGrantState{{
		projectID: "vornik-marketing", server: "atlassian",
		needsReconnect: false, expiresAt: ptrTime(now.Add(8 * time.Hour)), hasRefresh: true,
	}}
	failures := []connectorAuthFailure{{
		projectID: "vornik-marketing", server: "atlassian", count: 14, last: now.Add(-2 * time.Minute),
	}}

	items, worst := evaluateConnectorAuth(grants, failures, now)
	if worst != "ERROR" {
		t.Fatalf("a connector rejecting every call must be an ERROR, got %q", worst)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 finding, got %d: %v", len(items), items)
	}
	for _, want := range []string{"atlassian", "vornik-marketing", "14", "vornikctl mcp connect"} {
		if !strings.Contains(items[0], want) {
			t.Errorf("finding %q does not mention %q", items[0], want)
		}
	}
}

// A healthy connector with no recent failures produces nothing.
func TestConnectorAuthSilentWhenHealthy(t *testing.T) {
	now := time.Now().UTC()
	grants := []connectorGrantState{{
		projectID: "p", server: "atlassian",
		expiresAt: ptrTime(now.Add(8 * time.Hour)), hasRefresh: true,
	}}
	items, worst := evaluateConnectorAuth(grants, nil, now)
	if worst != "OK" || len(items) != 0 {
		t.Fatalf("a healthy connector must produce no findings: %q %v", worst, items)
	}
}

func TestConnectorAuthFlagsNeedsReconnect(t *testing.T) {
	now := time.Now().UTC()
	grants := []connectorGrantState{{projectID: "p", server: "atlassian", needsReconnect: true}}
	items, worst := evaluateConnectorAuth(grants, nil, now)
	if worst != "ERROR" {
		t.Fatalf("needs_reconnect must be an ERROR, got %q", worst)
	}
	if len(items) != 1 || !strings.Contains(items[0], "needs_reconnect") {
		t.Fatalf("unexpected findings: %v", items)
	}
}

// A grant that cannot renew itself and is nearly expired is a SCHEDULED
// outage. Warning, with enough notice to act on.
func TestConnectorAuthWarnsOnUnrenewableExpiry(t *testing.T) {
	now := time.Now().UTC()
	grants := []connectorGrantState{{
		projectID: "p", server: "intercom",
		expiresAt: ptrTime(now.Add(3 * time.Hour)), hasRefresh: false,
	}}
	items, worst := evaluateConnectorAuth(grants, nil, now)
	if worst != "WARNING" {
		t.Fatalf("want WARNING, got %q (%v)", worst, items)
	}
	if len(items) != 1 || !strings.Contains(items[0], "cannot renew itself") {
		t.Fatalf("unexpected findings: %v", items)
	}
}

// A grant WITH a refresh token approaching expiry is not a finding — renewing
// it is exactly what the daemon now does per call.
func TestConnectorAuthQuietForRefreshableExpiry(t *testing.T) {
	now := time.Now().UTC()
	grants := []connectorGrantState{{
		projectID: "p", server: "atlassian",
		expiresAt: ptrTime(now.Add(5 * time.Minute)), hasRefresh: true,
	}}
	items, worst := evaluateConnectorAuth(grants, nil, now)
	if worst != "OK" || len(items) != 0 {
		t.Fatalf("a refreshable grant near expiry is routine: %q %v", worst, items)
	}
}

func TestConnectorAuthFlagsExpiredUnrenewable(t *testing.T) {
	now := time.Now().UTC()
	grants := []connectorGrantState{{
		projectID: "p", server: "intercom",
		expiresAt: ptrTime(now.Add(-time.Minute)), hasRefresh: false,
	}}
	_, worst := evaluateConnectorAuth(grants, nil, now)
	if worst != "ERROR" {
		t.Fatalf("an expired unrenewable grant must be an ERROR, got %q", worst)
	}
}

// Failures against a server nobody connected get their own line — a different
// problem from a grant that went stale.
func TestConnectorAuthFlagsFailuresWithNoGrant(t *testing.T) {
	now := time.Now().UTC()
	failures := []connectorAuthFailure{{projectID: "p", server: "ghost", count: 3, last: now}}
	items, worst := evaluateConnectorAuth(nil, failures, now)
	if worst != "ERROR" {
		t.Fatalf("want ERROR, got %q", worst)
	}
	if len(items) != 1 || !strings.Contains(items[0], "NO stored grant") {
		t.Fatalf("unexpected findings: %v", items)
	}
}

// A daemon-scope grant lives at project "" and is shared by every project that
// subscribes by name. Say so rather than printing an empty field.
func TestConnectorLabelNamesDaemonScope(t *testing.T) {
	if got := connectorLabel("", "atlassian"); !strings.Contains(got, "daemon scope") {
		t.Errorf("got %q", got)
	}
	if got := connectorLabel("p", "atlassian"); !strings.Contains(got, "project p") {
		t.Errorf("got %q", got)
	}
}

func TestConnectorFixHintOmitsProjectFlagAtDaemonScope(t *testing.T) {
	if got := connectorFixHint("", "atlassian"); strings.Contains(got, "-p ") {
		t.Errorf("a daemon-scope grant takes no -p flag: %q", got)
	}
	if got := connectorFixHint("mktg", "atlassian"); !strings.Contains(got, "-p mktg") {
		t.Errorf("got %q", got)
	}
}

func TestServerFromQualifiedTool(t *testing.T) {
	cases := map[string]string{
		"mcp__atlassian__searchJiraIssuesUsingJql": "atlassian",
		"mcp__google-workspace__gmail_send":        "google-workspace",
		"file_read":                                "",
		"mcp__":                                    "",
		"mcp__noseparator":                         "",
	}
	for in, want := range cases {
		if got := serverFromQualifiedTool(in); got != want {
			t.Errorf("serverFromQualifiedTool(%q) = %q, want %q", in, got, want)
		}
	}
}

// An observed failure outranks stored state: the vendor is authoritative about
// its own credential, and our row is only a belief about it.
func TestObservedFailureOutranksStoredState(t *testing.T) {
	now := time.Now().UTC()
	grants := []connectorGrantState{{
		projectID: "p", server: "atlassian",
		needsReconnect: true, expiresAt: ptrTime(now.Add(-time.Hour)), hasRefresh: false,
	}}
	failures := []connectorAuthFailure{{projectID: "p", server: "atlassian", count: 9, last: now}}
	items, _ := evaluateConnectorAuth(grants, failures, now)
	if len(items) != 1 {
		t.Fatalf("one connector must produce one finding, got %d: %v", len(items), items)
	}
	if !strings.Contains(items[0], "rejected with 401/403") {
		t.Errorf("the observed failure should be the reported diagnosis, got %q", items[0])
	}
}
