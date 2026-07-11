package projectdoctor

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/registry"
)

type fakeSnap []mcp.ServerSnapshot

func (f fakeSnap) Snapshot(_ context.Context) []mcp.ServerSnapshot { return f }

type fakePinger struct{ err error }

func (f fakePinger) Ping(_ context.Context) error { return f.err }

func projWithMCP(names ...string) *registry.Project {
	p := &registry.Project{}
	for _, n := range names {
		p.MCP.Servers = append(p.MCP.Servers, registry.MCPServerConfig{Name: n})
	}
	return p
}

func TestCheckMCP(t *testing.T) {
	ctx := context.Background()
	// No MCP servers => neutral, not required.
	d := New(Deps{MCP: fakeSnap{}})
	if got := d.checkMCP(ctx, &registry.Project{}); got.Status != StatusNeutral || got.Required {
		t.Fatalf("no servers: got %+v", got)
	}
	// FixHref deep-links to the control-plane hub's MCP tab regardless of
	// outcome — the canonical daemon MCP-catalog management surface since
	// the Integrations Hub MCP kind's 2026-07-10 removal.
	if got := d.checkMCP(ctx, &registry.Project{}); got.FixHref != "/ui/admin/control-plane?section=mcp" {
		t.Fatalf("FixHref = %q, want /ui/admin/control-plane?section=mcp", got.FixHref)
	}
	// Configured + reachable => green.
	d = New(Deps{MCP: fakeSnap{{Name: "slack", Reachable: true}}})
	if got := d.checkMCP(ctx, projWithMCP("slack")); got.Status != StatusGreen {
		t.Fatalf("reachable: got %+v", got)
	}
	// Configured but probe failed => yellow (transient, not blocking).
	d = New(Deps{MCP: fakeSnap{{Name: "slack", Reachable: false, Error: "dial refused"}}})
	got := d.checkMCP(ctx, projWithMCP("slack"))
	if got.Status != StatusYellow {
		t.Fatalf("unreachable: got %+v", got)
	}
	// Not on daemon at all => red (hard misconfiguration).
	d = New(Deps{MCP: fakeSnap{{Name: "other", Reachable: true}}})
	if got := d.checkMCP(ctx, projWithMCP("slack")); got.Status != StatusRed {
		t.Fatalf("not configured: got %+v", got)
	}
	// Mixed: one red one green => red wins.
	d = New(Deps{MCP: fakeSnap{{Name: "slack", Reachable: true}}})
	if got := d.checkMCP(ctx, projWithMCP("slack", "ghost")); got.Status != StatusRed {
		t.Fatalf("mixed: got %+v", got)
	}
}

func TestCheckModel(t *testing.T) {
	ctx := context.Background()
	// Ping OK => green.
	d := New(Deps{Model: fakePinger{}})
	if got := d.checkModel(ctx); got.Status != StatusGreen || !got.Required {
		t.Fatalf("ping ok: got %+v", got)
	}
	// Ping fails => red.
	d = New(Deps{Model: fakePinger{err: errors.New("401 unauthorized")}})
	if got := d.checkModel(ctx); got.Status != StatusRed {
		t.Fatalf("ping fail: got %+v", got)
	}
	// Nil pinger => non-blocking neutral (NOT unknown). Regression
	// for the final-review finding: an unprobeable/disabled backend
	// must not false-block a project's completeness. The check drops
	// its Required flag in this branch.
	d = New(Deps{})
	got := d.checkModel(ctx)
	if got.Status != StatusNeutral {
		t.Fatalf("nil pinger status: got %+v, want neutral", got)
	}
	if got.Required {
		t.Fatalf("nil pinger must be non-required so it can't block completeness: got %+v", got)
	}
}
