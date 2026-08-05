package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/mcpauth"
)

// fakeProbeConn is a fake mcpProbeConn returning canned tools.
type fakeProbeConn struct{ tools []mcp.Tool }

func (f *fakeProbeConn) Tools() []mcp.Tool { return f.tools }
func (f *fakeProbeConn) Close() error      { return nil }

func probeRequest(form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/ui/admin/control-plane/mcp/probe", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// TestMCPProbe_ReachableListsTools — a reachable candidate returns the
// advertised tool set inline (backlog #4 onboarding probe).
func TestMCPProbe_ReachableListsTools(t *testing.T) {
	orig := mcpProbeConnect
	defer func() { mcpProbeConnect = orig }()
	mcpProbeConnect = func(_ context.Context, cfg mcp.ServerConfig, _ zerolog.Logger) (mcpProbeConn, error) {
		if cfg.URL != "https://ha.local/mcp" {
			t.Errorf("probe cfg URL = %q, want the candidate URL", cfg.URL)
		}
		return &fakeProbeConn{tools: []mcp.Tool{{Name: "turn_on_light"}, {Name: "get_state"}}}, nil
	}
	srv := NewServer()
	rec := httptest.NewRecorder()
	srv.AdminControlPlaneMCPProbe(rec, probeRequest(url.Values{
		"transport": {"streamable-http"}, "url": {"https://ha.local/mcp"},
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "reachable") || !strings.Contains(body, "2 tool") {
		t.Errorf("expected reachable + tool count; got: %s", body)
	}
	if !strings.Contains(body, "turn_on_light") || !strings.Contains(body, "get_state") {
		t.Errorf("expected tool names in fragment; got: %s", body)
	}
}

// TestMCPProbe_UnreachableReportsError — a connect failure is reported as
// not-reachable with the error (still useful onboarding signal).
func TestMCPProbe_UnreachableReportsError(t *testing.T) {
	orig := mcpProbeConnect
	defer func() { mcpProbeConnect = orig }()
	mcpProbeConnect = func(_ context.Context, _ mcp.ServerConfig, _ zerolog.Logger) (mcpProbeConn, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	srv := NewServer()
	rec := httptest.NewRecorder()
	srv.AdminControlPlaneMCPProbe(rec, probeRequest(url.Values{
		"transport": {"sse"}, "url": {"http://down.local/mcp"},
	}))

	body := rec.Body.String()
	if !strings.Contains(body, "not reachable") || !strings.Contains(body, "connection refused") {
		t.Errorf("expected not-reachable + error; got: %s", body)
	}
}

// TestMCPProbe_RejectsBadEndpointAndSecret — validation mirrors the add form:
// bad transport / non-http URL, and a literal secret, are rejected without
// attempting a connect.
func TestMCPProbe_RejectsBadEndpointAndSecret(t *testing.T) {
	orig := mcpProbeConnect
	defer func() { mcpProbeConnect = orig }()
	connectCalled := false
	mcpProbeConnect = func(_ context.Context, _ mcp.ServerConfig, _ zerolog.Logger) (mcpProbeConn, error) {
		connectCalled = true
		return &fakeProbeConn{}, nil
	}
	srv := NewServer()

	// Non-http URL for an http transport.
	rec := httptest.NewRecorder()
	srv.AdminControlPlaneMCPProbe(rec, probeRequest(url.Values{
		"transport": {"streamable-http"}, "url": {"ftp://nope"},
	}))
	if !strings.Contains(rec.Body.String(), "Invalid endpoint") {
		t.Errorf("expected invalid-endpoint message; got: %s", rec.Body.String())
	}

	// Literal secret in the stdio command (a bare high-entropy token with no
	// space / colon / slash trips the secret-literal guard).
	rec = httptest.NewRecorder()
	srv.AdminControlPlaneMCPProbe(rec, probeRequest(url.Values{
		"transport": {"stdio"}, "command": {strings.Repeat("A", 40)},
	}))
	if !strings.Contains(rec.Body.String(), "placeholder") {
		t.Errorf("expected secret-literal rejection; got: %s", rec.Body.String())
	}
	if connectCalled {
		t.Error("connect must not be attempted for invalid/secret input")
	}
}

// TestMCPProbe_AuthChallengeIsReachableNotAFailure is the regression test for
// the false negative reported 2026-08-05 while adding the Atlassian server.
//
// The probe connects with NO credentials, so any oauth-mode server refuses it.
// That refusal was rendered "✗ not reachable — mcp initialize failed for
// probe: streamable-http server returned 401", which reads as a broken config
// — and the operator sees it immediately before Connect, the step that would
// have worked. Per the MCP-auth survey, 17 of 18 remote vendors challenge on
// initialize, so the button was a false negative for essentially all of them.
//
// A 401 carrying a Bearer challenge is the FIRST STEP of RFC 9728 discovery.
// It proves more about reachability than a 200 would.
func TestMCPProbe_AuthChallengeIsReachableNotAFailure(t *testing.T) {
	origConnect, origReach := mcpProbeConnect, mcpProbeReachability
	defer func() { mcpProbeConnect, mcpProbeReachability = origConnect, origReach }()

	mcpProbeConnect = func(context.Context, mcp.ServerConfig, zerolog.Logger) (mcpProbeConn, error) {
		return nil, errors.New("mcp initialize failed for probe: streamable-http server returned 401")
	}
	mcpProbeReachability = func(context.Context, string) (mcpauth.ReachabilityVerdict, error) {
		return mcpauth.ReachabilityAuthRequired, nil
	}

	srv := NewServer()
	rec := httptest.NewRecorder()
	srv.AdminControlPlaneMCPProbe(rec, probeRequest(url.Values{
		"transport": {"streamable-http"},
		"url":       {"https://mcp.atlassian.com/v1/mcp/authv2"},
	}))

	body := rec.Body.String()
	if strings.Contains(body, "not reachable") {
		t.Errorf("an OAuth challenge must NOT read as unreachable: %s", body)
	}
	if !strings.Contains(body, "authentication required") {
		t.Errorf("the operator must be told auth is the next step: %s", body)
	}
	if !strings.Contains(body, "Connect") {
		t.Errorf("the message must name the action that resolves it: %s", body)
	}
}

// TestMCPProbe_RefusedWithoutAChallengeStillFails — the F3 case must keep
// reading as a failure. A WAF answering 403 with no WWW-Authenticate is a
// genuine problem and is materially different from an auth requirement;
// collapsing the two would trade one misleading verdict for another.
func TestMCPProbe_RefusedWithoutAChallengeStillFails(t *testing.T) {
	origConnect, origReach := mcpProbeConnect, mcpProbeReachability
	defer func() { mcpProbeConnect, mcpProbeReachability = origConnect, origReach }()

	mcpProbeConnect = func(context.Context, mcp.ServerConfig, zerolog.Logger) (mcpProbeConn, error) {
		return nil, errors.New("streamable-http server returned 403")
	}
	mcpProbeReachability = func(context.Context, string) (mcpauth.ReachabilityVerdict, error) {
		return mcpauth.ReachabilityRefused, nil
	}

	srv := NewServer()
	rec := httptest.NewRecorder()
	srv.AdminControlPlaneMCPProbe(rec, probeRequest(url.Values{
		"transport": {"streamable-http"},
		"url":       {"https://waf.example.com/mcp"},
	}))

	if body := rec.Body.String(); !strings.Contains(body, "not reachable") {
		t.Errorf("a refusal with no challenge must still report failure: %s", body)
	}
}
