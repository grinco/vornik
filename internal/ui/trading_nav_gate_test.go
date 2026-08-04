package ui

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Regression (2026-08-03, customer reports): an Enterprise deployment that
// never configured trading still rendered the Insight → Trading destination.
// The edition gate (WithTradingEnabled) only asks "does this build have the
// capability", never "does this deployment actually trade", so every EE
// operator saw a nav entry leading to a page whose only content was "No
// projects have trading enabled". The nav gate must additionally require at
// least one project with a `broker` MCP server — the same predicate the
// /trading dropdown uses (trading-dashboard-design.md §6) — evaluated per
// render so it follows a config reload.

// writeNavProject seeds one project YAML under root/projects. When broker is
// true the project carries a `broker` MCP server (= trading-enabled).
func writeNavProject(t *testing.T, root, id string, broker bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	yaml := "projectId: " + id + "\ndisplayName: " + id + "\nswarmId: swarm-1\ndefaultWorkflowId: wf-1\n"
	if broker {
		yaml += "mcp:\n  servers:\n    - name: broker\n      transport: sse\n      url: http://127.0.0.1:1\n"
	}
	if err := os.WriteFile(filepath.Join(root, "projects", id+".yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write project: %v", err)
	}
}

// navTradingRegistry returns a registry rooted at dir holding a single
// project, trading-enabled or not.
func navTradingRegistry(t *testing.T, dir string, broker bool) *registry.Registry {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "swarms"), 0o755); err != nil {
		t.Fatalf("mkdir swarms: %v", err)
	}
	swarm := "---\nswarmId: swarm-1\nroles:\n  - name: worker\n    runtime:\n      image: fake-agent\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "swarms", "swarm.md"), []byte(swarm), 0o644); err != nil {
		t.Fatalf("write swarm: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o755); err != nil {
		t.Fatalf("mkdir workflows: %v", err)
	}
	wf := "---\nworkflowId: wf-1\nentrypoint: run\nsteps:\n  run:\n    type: agent\n    prompt: \"do work\"\n    role: worker\n    on_success: done\nterminals:\n  done:\n    status: COMPLETED\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "workflows", "wf.md"), []byte(wf), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	writeNavProject(t, dir, "p1", broker)
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	return reg
}

// navHTML renders the production nav partial through s's own template
// registry, so the assertion covers NewServer's FuncMap wiring and not just
// the nav model in isolation.
func navHTML(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.render(rec, "nav", navFixture{CurrentPage: "spend", IsAdmin: true, IsSession: true})
	return rec.Body.String()
}

func TestTradingNav_HiddenWhenNoProjectHasBroker(t *testing.T) {
	s := NewServer(WithTradingEnabled(), WithProjectRegistry(navTradingRegistry(t, t.TempDir(), false)))
	if html := navHTML(t, s); strings.Contains(html, "/ui/trading") {
		t.Error("nav must omit Trading when no project has a broker MCP server configured")
	}
}

func TestTradingNav_ShownWhenAProjectHasBroker(t *testing.T) {
	s := NewServer(WithTradingEnabled(), WithProjectRegistry(navTradingRegistry(t, t.TempDir(), true)))
	html := navHTML(t, s)
	if !strings.Contains(html, "/ui/trading") {
		t.Error("nav must keep Trading when a project has a broker MCP server configured")
	}
	if !strings.Contains(html, "/ui/spend") {
		t.Errorf("sibling Insight dests must survive: %q", html)
	}
}

func TestTradingNav_HiddenWhenNoRegistryWired(t *testing.T) {
	s := NewServer(WithTradingEnabled())
	if html := navHTML(t, s); strings.Contains(html, "/ui/trading") {
		t.Error("nav must omit Trading when no project registry is wired")
	}
}

// Community stays hidden even with a trading-enabled project: the /trading
// route 404s there, so the edition gate still wins.
func TestTradingNav_HiddenOnCommunityDespiteBrokerProject(t *testing.T) {
	s := NewServer(WithProjectRegistry(navTradingRegistry(t, t.TempDir(), true)))
	if html := navHTML(t, s); strings.Contains(html, "/ui/trading") {
		t.Error("Community nav must omit Trading regardless of project config")
	}
}

// The gate is evaluated per render, not frozen at NewServer: adding a
// trading project and reloading the registry must reveal the nav entry
// without a daemon restart.
func TestTradingNav_FollowsConfigReload(t *testing.T) {
	dir := t.TempDir()
	reg := navTradingRegistry(t, dir, false)
	s := NewServer(WithTradingEnabled(), WithProjectRegistry(reg))
	if html := navHTML(t, s); strings.Contains(html, "/ui/trading") {
		t.Fatal("precondition: Trading must be hidden before a broker project exists")
	}

	writeNavProject(t, dir, "trader", true)
	if err := reg.Load(dir); err != nil {
		t.Fatalf("registry reload: %v", err)
	}
	if html := navHTML(t, s); !strings.Contains(html, "/ui/trading") {
		t.Error("Trading must appear after a broker project is added and config reloaded")
	}
}

// hasTradingProject is the predicate itself — unit-level, so a future
// refactor of the nav plumbing keeps a direct test of the rule.
func TestHasTradingProject(t *testing.T) {
	if (&Server{}).hasTradingProject() {
		t.Error("no registry: want false")
	}
	if NewServer(WithProjectRegistry(navTradingRegistry(t, t.TempDir(), false))).hasTradingProject() {
		t.Error("project without broker: want false")
	}
	if !NewServer(WithProjectRegistry(navTradingRegistry(t, t.TempDir(), true))).hasTradingProject() {
		t.Error("project with broker: want true")
	}
}
