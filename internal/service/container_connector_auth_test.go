package service

import "testing"

func TestConnectorServerFromTool(t *testing.T) {
	cases := map[string]string{
		"mcp__atlassian__searchJiraIssuesUsingJql": "atlassian",
		"mcp__google-workspace__gmail_send":        "google-workspace",
		// Container-local tools have no connector to reconnect, so they must
		// never produce a finding naming one.
		"file_read":        "",
		"shell":            "",
		"mcp__":            "",
		"mcp__noseparator": "",
	}
	for in, want := range cases {
		if got := connectorServerFromTool(in); got != want {
			t.Errorf("connectorServerFromTool(%q) = %q, want %q", in, got, want)
		}
	}
}

// The alert window must be at least the scan interval, or a failure can fall
// between two ticks and never be alerted on.
func TestAlertWindowCoversTheScanInterval(t *testing.T) {
	// The worker's default interval is 5m; the window is 15m.
	if connectorAuthAlertWindow < 5*60*1e9 {
		t.Fatalf("window %s is shorter than the worker's scan interval", connectorAuthAlertWindow)
	}
}

// A nil source is a no-op, not a panic — the daemon may start before the DB
// handle is wired.
func TestNilConnectorAuthSourceIsSafe(t *testing.T) {
	var s *dbConnectorAuthSource
	got, err := s.RecentAuthFailures(nil) //nolint:staticcheck // nil ctx is the point
	if err != nil || got != nil {
		t.Fatalf("nil source must be a quiet no-op, got %v %v", got, err)
	}
}
