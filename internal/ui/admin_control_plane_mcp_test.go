package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

const mcpBaseConfig = "# vornik config\nmcp_servers:\n  existing:\n    transport: sse\n    url: http://existing\nserver:\n  address: :8080\n"

func mcpTestServer(t *testing.T) (*Server, persistence.ProposalRepository) {
	t.Helper()
	repo := newProposalRepoUI(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(mcpBaseConfig), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	s := NewServer(WithProposalStore(repo), WithControlPlaneConfigPath(path))
	return s, repo
}

func postMCP(t *testing.T, s *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/control-plane/mcp", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminControlPlaneMCPWrite(rec, req)
	return rec
}

func onlyProposal(t *testing.T, repo persistence.ProposalRepository) *persistence.ControlPlaneProposal {
	t.Helper()
	ps, _ := repo.List(context.Background(), persistence.ProposalListFilter{})
	if len(ps) != 1 {
		t.Fatalf("expected exactly 1 proposal, got %d", len(ps))
	}
	return ps[0]
}

func TestMCPAdd_FilesDaemonScopeProposal(t *testing.T) {
	s, repo := mcpTestServer(t)
	form := url.Values{"action": {"add"}, "name": {"homeassistant"}, "transport": {"streamable-http"}, "url": {"http://homeassistant.local:8123/mcp"}}
	rec := postMCP(t, s, form)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "done=mcp-proposed") {
		t.Fatalf("add: want 303 mcp-proposed, got %d %s", rec.Code, rec.Header().Get("Location"))
	}
	p := onlyProposal(t, repo)
	if p.BlastRadius != persistence.ProposalScopeDaemon || p.ProposedBy != "operator-ui" || p.ApplyTarget != "config.yaml" {
		t.Fatalf("unexpected proposal shape: %+v", p)
	}
	// The edited content adds the new server + keeps the existing one + comments.
	if !strings.Contains(p.ApplyContent, "homeassistant") || !strings.Contains(p.ApplyContent, "existing") || !strings.Contains(p.ApplyContent, "# vornik config") {
		t.Errorf("edited config must add new + preserve existing/comments: %s", p.ApplyContent)
	}
	// Base hash recorded for optimistic concurrency.
	if !strings.Contains(p.Evidence, "base_hash") {
		t.Errorf("expected base_hash in evidence: %s", p.Evidence)
	}
}

func TestMCPAdd_RejectsBadTransport(t *testing.T) {
	s, repo := mcpTestServer(t)
	rec := postMCP(t, s, url.Values{"action": {"add"}, "name": {"x"}, "transport": {"telepathy"}})
	if !strings.Contains(rec.Header().Get("Location"), "mcp-bad-transport") {
		t.Fatalf("bad transport must be rejected, got %s", rec.Header().Get("Location"))
	}
	if n := draftCountUI(t, repo); n != 0 {
		t.Fatalf("no proposal on bad transport, got %d", n)
	}
}

func TestMCPAdd_RejectsSecretLiteral(t *testing.T) {
	s, repo := mcpTestServer(t)
	// A long bare token in the URL position → looks like a literal secret.
	rec := postMCP(t, s, url.Values{"action": {"add"}, "name": {"x"}, "transport": {"stdio"}, "command": {"sk-abcdefghijklmnopqrstuvwxyz012345"}})
	if !strings.Contains(rec.Header().Get("Location"), "mcp-secret") {
		t.Fatalf("secret literal must be rejected, got %s", rec.Header().Get("Location"))
	}
	if n := draftCountUI(t, repo); n != 0 {
		t.Fatalf("no proposal on secret literal, got %d", n)
	}
}

func TestMCPAdd_RejectsBadName(t *testing.T) {
	s, _ := mcpTestServer(t)
	rec := postMCP(t, s, url.Values{"action": {"add"}, "name": {"bad name!"}, "transport": {"sse"}, "url": {"http://x"}})
	if !strings.Contains(rec.Header().Get("Location"), "mcp-bad-name") {
		t.Fatalf("bad name must be rejected, got %s", rec.Header().Get("Location"))
	}
}

func TestMCPRemove_FilesProposal(t *testing.T) {
	s, repo := mcpTestServer(t)
	rec := postMCP(t, s, url.Values{"action": {"remove"}, "name": {"existing"}})
	if !strings.Contains(rec.Header().Get("Location"), "done=mcp-proposed") {
		t.Fatalf("remove: want mcp-proposed, got %s", rec.Header().Get("Location"))
	}
	p := onlyProposal(t, repo)
	if strings.Contains(p.ApplyContent, "existing") {
		t.Errorf("removed server must be gone from edited config: %s", p.ApplyContent)
	}
}

func TestMCPRemove_NotFound(t *testing.T) {
	s, repo := mcpTestServer(t)
	rec := postMCP(t, s, url.Values{"action": {"remove"}, "name": {"ghost"}})
	if !strings.Contains(rec.Header().Get("Location"), "mcp-not-found") {
		t.Fatalf("removing an absent server must report not-found, got %s", rec.Header().Get("Location"))
	}
	if n := draftCountUI(t, repo); n != 0 {
		t.Fatalf("no proposal when nothing removed, got %d", n)
	}
}

func draftCountUI(t *testing.T, repo persistence.ProposalRepository) int {
	t.Helper()
	ps, _ := repo.List(context.Background(), persistence.ProposalListFilter{})
	return len(ps)
}
