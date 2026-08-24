package api

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// roleAllowsMCPTool fails OPEN on every resolution gap, by design (Finding B2):
// no taskID, no execution/workflow/registry wired, role not found, role declares
// no allowedTools. The project gate stays in force, so this is permitted rather
// than wrong — but it was also INVISIBLE. A deployment running every MCP call
// unrestricted looked exactly like one whose roles all resolve, and the backlog
// asked for the gaps to be logged so the state is visible rather than merely
// permitted (https://docs.vornik.io, MCP tool advertisement section, 2026-08-13).
//
// These tests pin the counter, not a refusal: the fail-open behaviour is
// deliberate and unchanged here. What changes is that it can be seen.

func counterValue(t *testing.T, m *MCPGateMetrics, path, reason string) float64 {
	t.Helper()
	c, err := m.UnrestrictedTotal.GetMetricWithLabelValues(path, reason)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q,%q): %v", path, reason, err)
	}
	var out dto.Metric
	if err := c.(prometheus.Metric).Write(&out); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return out.GetCounter().GetValue()
}

func TestRoleAllowsMCPTool_CountsTheNoTaskIDGap(t *testing.T) {
	m := NewMCPGateMetrics(prometheus.NewRegistry())
	s := &Server{mcpGateMetrics: m}

	if !s.roleAllowsMCPTool(context.Background(), "", "mcp__srv__tool") {
		t.Fatal("the gap must still fail OPEN — this change makes it visible, not strict")
	}
	if got := counterValue(t, m, "call", string(mcpGapNoTaskID)); got != 1 {
		t.Errorf("no-taskID gap not counted: %v", got)
	}
}

func TestRoleAllowsMCPTool_CountsUnwiredDependencies(t *testing.T) {
	m := NewMCPGateMetrics(prometheus.NewRegistry())
	s := &Server{mcpGateMetrics: m}

	if !s.roleAllowsMCPTool(context.Background(), "task-1", "mcp__srv__tool") {
		t.Fatal("unwired deps must still fail open")
	}
	if got := counterValue(t, m, "call", string(mcpGapDepsUnwired)); got != 1 {
		t.Errorf("deps-unwired gap not counted: %v", got)
	}
}

// The two gaps must be DISTINGUISHABLE. Collapsing them into one "unresolved"
// count would reproduce the defect at the observability layer: an operator
// cannot tell a dev deployment with nothing wired from a production one whose
// roles fail to resolve.
func TestMCPGapReasons_AreDistinct(t *testing.T) {
	seen := map[mcpGapReason]bool{}
	for _, r := range allMCPGapReasons() {
		if r == "" {
			t.Error("an empty reason label would aggregate every gap into one bucket")
		}
		if seen[r] {
			t.Errorf("duplicate gap reason %q", r)
		}
		seen[r] = true
	}
	if len(seen) < 4 {
		t.Errorf("the gaps are meant to be enumerated, got %d", len(seen))
	}
}

// Nil metrics must stay a no-op: CE and test paths build a Server without them.
func TestRoleAllowsMCPTool_NilMetricsIsSafe(t *testing.T) {
	s := &Server{}
	if !s.roleAllowsMCPTool(context.Background(), "", "mcp__srv__tool") {
		t.Fatal("unmetered gate must behave identically")
	}
}

// The help text has to say the counter is a fail-open census, or an operator
// reads a rising number as attacks rather than as unresolved roles.
func TestMCPGateMetrics_HelpNamesTheFailOpen(t *testing.T) {
	m := NewMCPGateMetrics(prometheus.NewRegistry())
	ch := make(chan *prometheus.Desc, 1)
	m.UnrestrictedTotal.Describe(ch)
	desc := (<-ch).String()

	if !strings.Contains(desc, "fail-open") {
		t.Errorf("help must name the fail-open: %s", desc)
	}
}

// A resolved allowlist must count NOTHING. A census that also counts healthy
// resolutions cannot answer the question it exists for.
func TestRoleAllowsMCPTool_ResolvedAllowlistIsNotCounted(t *testing.T) {
	m := NewMCPGateMetrics(prometheus.NewRegistry())
	s := &Server{mcpGateMetrics: m}

	// mcpGapNone is what a successful resolve reports; recordMCPGap must ignore it.
	s.recordMCPGap("call", mcpGapNone)

	for _, r := range allMCPGapReasons() {
		if got := counterValue(t, m, "call", string(r)); got != 0 {
			t.Errorf("a healthy resolve incremented %q: %v", r, got)
		}
	}
}

// The advertise path shares the same resolution, and a gap there costs a wide
// PROMPT rather than a wide grant — so it is counted under its own label.
func TestRecordMCPGap_SeparatesTheTwoPaths(t *testing.T) {
	m := NewMCPGateMetrics(prometheus.NewRegistry())
	s := &Server{mcpGateMetrics: m}

	s.recordMCPGap("advertise", mcpGapRoleNotFound)

	if got := counterValue(t, m, "advertise", string(mcpGapRoleNotFound)); got != 1 {
		t.Errorf("advertise-path gap not counted: %v", got)
	}
	if got := counterValue(t, m, "call", string(mcpGapRoleNotFound)); got != 0 {
		t.Errorf("advertise gap leaked into the call path: %v", got)
	}
}

// role_declares_none is an operator DECISION, not a failure, and must be
// countable apart from the resolution failures. Merging them would make the
// number unactionable: a swarm that deliberately leaves a dispatcher
// unrestricted would look identical to one whose roles cannot be found.
func TestMCPGapReasons_SeparateDecisionFromFailure(t *testing.T) {
	if mcpGapRoleDeclaresNone == mcpGapRoleNotFound {
		t.Fatal("an operator decision and a misconfiguration must not share a label")
	}
	var found bool
	for _, r := range allMCPGapReasons() {
		if r == mcpGapRoleDeclaresNone {
			found = true
		}
	}
	if !found {
		t.Error("role_declares_none must be in the enumeration an alert is written against")
	}
}
