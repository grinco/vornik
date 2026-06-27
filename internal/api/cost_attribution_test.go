package api

import (
	"context"
	"net/http/httptest"
	"testing"
)

// TestProjectForCostAttribution_DBKeyWinsOverHeader — the headline
// security property. An attacker who supplies a valid DB-backed key
// for project A and an X-Vornik-Project-ID header naming project B
// must bill A, never B. Pre-fix the header would have won.
func TestProjectForCostAttribution_DBKeyWinsOverHeader(t *testing.T) {
	ctx := context.WithValue(context.Background(), projectIDFromKeyKey, "project-a")
	req := httptest.NewRequest("POST", "/api/chat", nil)
	req.Header.Set("X-Vornik-Project-ID", "project-b")

	pid, src := projectForCostAttribution(ctx, req, "fallback-project")
	if pid != "project-a" {
		t.Errorf("projectID = %q, want project-a (DB-bound wins)", pid)
	}
	if src != AttributionFromDBKey {
		t.Errorf("source = %q, want %q", src, AttributionFromDBKey)
	}
}

// TestProjectForCostAttribution_ScopedAgentKeyIgnoresSpoofedHeader —
// Finding B4 regression. Once B1 lands, the agent container carries a
// project-SCOPED key (DB-bound project), so the X-Vornik-Project-ID
// header can no longer redirect the agent's LLM spend to another
// project. This pins that the DB-bound project wins even when a
// prompt-injected agent sets a conflicting header to a victim project.
func TestProjectForCostAttribution_ScopedAgentKeyIgnoresSpoofedHeader(t *testing.T) {
	// The agent's per-task key resolves to its own project; middleware
	// stamps projectIDFromKeyKey with it.
	ctx := context.WithValue(context.Background(), projectIDFromKeyKey, "agent-own-project")
	req := httptest.NewRequest("POST", "/api/v1/chat/completions", nil)
	// Attacker-controlled header naming the trading/broker project.
	req.Header.Set("X-Vornik-Project-ID", "victim-broker-project")

	pid, src := projectForCostAttribution(ctx, req, "fallback")
	if pid != "agent-own-project" {
		t.Errorf("projectID = %q, want agent-own-project (scoped key wins, header ignored)", pid)
	}
	if src != AttributionFromDBKey {
		t.Errorf("source = %q, want %q (key-bound, not header)", src, AttributionFromDBKey)
	}
}

// TestProjectForCostAttribution_HeaderWhenNoDBKey — legacy
// static-key callers must keep working: when no DB-bound project is
// in context, the header is honoured.
func TestProjectForCostAttribution_HeaderWhenNoDBKey(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/chat", nil)
	req.Header.Set("X-Vornik-Project-ID", "project-b")

	pid, src := projectForCostAttribution(context.Background(), req, "fallback-project")
	if pid != "project-b" || src != AttributionFromHeader {
		t.Errorf("got (%q, %q), want (project-b, %q)", pid, src, AttributionFromHeader)
	}
}

// TestProjectForCostAttribution_FallbackPinsDaemonWide — neither
// context nor header — the operator-configured fallback claims the
// row. Same row would have been "_external" without the fallback.
func TestProjectForCostAttribution_FallbackPinsDaemonWide(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/chat", nil)
	pid, src := projectForCostAttribution(context.Background(), req, "billing-project")
	if pid != "billing-project" || src != AttributionFromFallback {
		t.Errorf("got (%q, %q), want (billing-project, %q)", pid, src, AttributionFromFallback)
	}
}

// TestProjectForCostAttribution_AnonymousSentinel — last-resort
// bucket. The literal "_external" is what the chat-proxy used pre-
// design; we keep it to satisfy the NOT NULL constraint on the
// usage row, and tag the source as "anonymous" so operators can
// grep for rows that escaped attribution entirely.
func TestProjectForCostAttribution_AnonymousSentinel(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/chat", nil)
	pid, src := projectForCostAttribution(context.Background(), req, "")
	if pid != "_external" || src != AttributionAnonymous {
		t.Errorf("got (%q, %q), want (_external, %q)", pid, src, AttributionAnonymous)
	}
}

// TestProjectForCostAttribution_NilRequestSafe — defensive: the
// helper is callable from paths that don't have an http.Request
// (e.g. internal cost ledger writers). Nil request must not panic;
// fallback / sentinel still applies.
func TestProjectForCostAttribution_NilRequestSafe(t *testing.T) {
	pid, src := projectForCostAttribution(context.Background(), nil, "fallback")
	if pid != "fallback" || src != AttributionFromFallback {
		t.Errorf("got (%q, %q), want (fallback, %q)", pid, src, AttributionFromFallback)
	}

	pid, src = projectForCostAttribution(context.Background(), nil, "")
	if pid != "_external" || src != AttributionAnonymous {
		t.Errorf("got (%q, %q), want (_external, %q)", pid, src, AttributionAnonymous)
	}
}
