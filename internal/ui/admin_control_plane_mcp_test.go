package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// mcpBaseConfig mirrors the REAL daemon schema: the MCP catalog is the LIST at
// mcp.servers (config.MCPConfig.Servers, yaml `mcp.servers`), not a
// mcp_servers.<name> map. The hub edits must target this shape.
const mcpBaseConfig = "# vornik config\nmcp:\n  servers:\n    - name: existing\n      transport: sse\n      url: http://existing\nserver:\n  address: :8080\n"

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
	// MCP add/remove is non-disruptive to in-flight tasks → live-apply so the
	// daemon-scope busy gate (never opens in prod) is skipped. This is the fix
	// for the 2026-07-08 "homeassistant apply keeps failing" report.
	if !p.LiveApply {
		t.Error("MCP-hub proposal must be LiveApply=true (skips the busy gate)")
	}
	// Shape: the server must be an mcp.servers LIST item, NOT the dead
	// top-level mcp_servers.<name> map (the 2026-07-08 invisible-server bug).
	if !strings.Contains(p.ApplyContent, "name: homeassistant") {
		t.Errorf("new server must be an mcp.servers list item: %s", p.ApplyContent)
	}
	if strings.Contains(p.ApplyContent, "mcp_servers:") {
		t.Errorf("must NOT reintroduce the dead top-level mcp_servers map: %s", p.ApplyContent)
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

// TestUnifiedish_PairsChangesByPosition is the regression test for the
// 2026-08-05 "scary diff" report: adding the atlassian MCP server round-
// tripped config.yaml through a full yaml.v3 re-encode, which reformatted
// dozens of unrelated lines (flow-map spacing, comment spacing) elsewhere in
// the file. The old SET-based unifiedish bucketed every removed line before
// every added line, so the operator's proposal-review pane showed what
// looked like a wall of unrelated deletions with the matching (reformatted)
// additions scrolled far below — indistinguishable from real data loss.
//
// This pins the fix: two independent single-line reformats, separated by an
// unchanged line, must render as adjacent -/+ pairs in file order — "- foo"
// immediately followed by "+ FOO" — not all removals first. The old
// implementation fails this exact assertion (it emits "- foo\n- bar\n+
// FOO\n+ BAR\n").
func TestUnifiedish_PairsChangesByPosition(t *testing.T) {
	old := "a\nfoo\nb\nbar\nc\n"
	updated := "a\nFOO\nb\nBAR\nc\n"
	got := unifiedish(old, updated)
	want := "- foo\n+ FOO\n- bar\n+ BAR\n"
	if got != want {
		t.Fatalf("unifiedish must pair changes by position, not bucket them:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestUnifiedish_NoChangeSentinel(t *testing.T) {
	if got := unifiedish("same\n", "same\n"); got != "(no line-level change)" {
		t.Fatalf("identical input must report no change, got %q", got)
	}
}

// TestUnifiedish_LargeInputUsesLinearPositionalDiff pins the memory backstop:
// an input whose CHANGED WINDOW still exceeds maxUnifiedishLCSCells after
// prefix/suffix trimming must fall through to the linear positionalDiff
// rather than allocate an unbounded quadratic table.
//
// The window must be genuinely large to reach that path — an earlier version
// of this test used 500 mostly-identical lines, which trimming collapses to a
// 1-line window that the LCS handles comfortably, so it passed without ever
// exercising the fallback. Every line differs here, defeating trimming.
func TestUnifiedish_LargeInputUsesLinearPositionalDiff(t *testing.T) {
	const lineCount = 2100 // (2100+1)² ≈ 4.4M cells > maxUnifiedishLCSCells.
	if !lcsTableTooLarge(lineCount, lineCount) {
		t.Fatalf("fixture must exceed the LCS cap, or this test proves nothing")
	}
	oldLines := make([]string, lineCount)
	newLines := make([]string, lineCount)
	for i := range oldLines {
		oldLines[i] = fmt.Sprintf("old %d", i)
		newLines[i] = fmt.Sprintf("new %d", i)
	}
	got := unifiedish(strings.Join(oldLines, "\n"), strings.Join(newLines, "\n"))

	// The degrade must announce itself — an operator cannot otherwise tell the
	// fallback's inflated churn from a genuinely large change.
	if !strings.HasPrefix(got, "(") || !strings.Contains(got, "positional comparison") {
		t.Fatalf("degraded diff must lead with a notice, got:\n%.200s", got)
	}
	// Pairing is the property that must survive the degrade: each position's
	// removal is immediately followed by its replacement.
	if !strings.Contains(got, "- old 0\n+ new 0\n- old 1\n+ new 1\n") {
		t.Fatalf("fallback must keep adjacent -/+ pairs in file order, got:\n%.200s", got)
	}
	if n := strings.Count(got, "\n"); n != lineCount*2+1 { // +1 for the notice
		t.Fatalf("expected %d diff lines + notice, got %d", lineCount*2, n)
	}
}

// TestUnifiedish_CapBoundary pins both sides of the LCS/positional decision at
// the threshold itself, so a future change to maxUnifiedishLCSCells cannot
// silently move which path real input takes (review suggestion 5).
func TestUnifiedish_CapBoundary(t *testing.T) {
	// Square windows: n*n cells, compared as (n+1)*(n+1) with the terminal
	// row/column. Pick sizes either side of the cap.
	under := 1900 // 1901² ≈ 3.61M  < 4M
	over := 2100  // 2101² ≈ 4.41M  > 4M
	if lcsTableTooLarge(under, under) {
		t.Errorf("%d×%d must stay on the minimal LCS path", under, under)
	}
	if !lcsTableTooLarge(over, over) {
		t.Errorf("%d×%d must degrade to the positional path", over, over)
	}
	// A degenerate 1-line window must never be judged too large.
	if lcsTableTooLarge(0, 0) || lcsTableTooLarge(1, 1) {
		t.Errorf("tiny windows must never degrade")
	}
}

// TestUnifiedish_EdgeCases covers the empty / single-line / strict-subset
// inputs the companion review flagged as untested (suggestion 2). These are
// the shapes where the prefix+suffix re-slicing could go out of bounds.
func TestUnifiedish_EdgeCases(t *testing.T) {
	for _, tc := range []struct{ name, old, updated, want string }{
		{"both empty", "", "", "(no line-level change)"},
		{"empty to one line", "", "a\n", "+ a\n"},
		{"one line to empty", "a\n", "", "- a\n"},
		{"single line changed", "a\n", "b\n", "- a\n+ b\n"},
		{"strict subset, tail removed", "a\nb\n", "a\n", "- b\n"},
		{"strict subset, tail added", "a\n", "a\nb\n", "+ b\n"},
		{"strict subset, head removed", "a\nb\n", "b\n", "- a\n"},
		{"identical single line", "a\n", "a\n", "(no line-level change)"},
		{"trailing newline is not a change", "a", "a\n", "(no line-level change)"},
		{"all lines replaced", "a\nb\n", "c\nd\n", "- a\n- b\n+ c\n+ d\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unifiedish(tc.old, tc.updated); got != tc.want {
				t.Errorf("unifiedish(%q, %q) = %q, want %q", tc.old, tc.updated, got, tc.want)
			}
		})
	}
}

// TestUnifiedish_RealisticConfigSizeStaysMinimal pins the SIZE half of the
// 2026-08-05 scary-diff fix. Bounding the quadratic LCS table is correct —
// config.yaml is operator-owned and unbounded — but the bound must not be so
// tight that the REAL deployment falls through to positionalDiff.
//
// Measured against the live 1,468-line config.yaml + the actual mcp.servers
// add: the minimal diff is +15/-18, while the positional fallback reports
// +164/-167, because inserting 3 lines shifts every later line by 3 and every
// shifted line renders as a remove/add pair. That fallback output reads as the
// atlassian add DELETING the admin-surface comment block — the same
// unreviewable wall the fix exists to remove, in a different shape.
//
// This models that exact shape: a config-sized file, scattered single-line
// reformats, and an insert partway through.
func TestUnifiedish_RealisticConfigSizeStaysMinimal(t *testing.T) {
	const lineCount = 1500
	oldLines := make([]string, 0, lineCount)
	for i := 0; i < lineCount; i++ {
		oldLines = append(oldLines, fmt.Sprintf("key_%d: value                # comment", i))
	}
	newLines := append([]string(nil), oldLines...)
	// Scattered reformats, as the yaml.v3 round-trip produces.
	for _, i := range []int{300, 470, 1320, 1400} {
		newLines[i] = fmt.Sprintf("key_%d: value # comment", i)
	}
	// An insert partway through — the shift that wrecks a positional diff.
	inserted := []string{"  - name: atlassian", "    transport: streamable-http"}
	newLines = append(newLines[:1290], append(inserted, newLines[1290:]...)...)

	got := unifiedish(strings.Join(oldLines, "\n"), strings.Join(newLines, "\n"))

	added, removed := 0, 0
	for _, l := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "+"):
			added++
		case strings.HasPrefix(l, "-"):
			removed++
		}
	}
	// Minimal: 4 reformats (4 -/+ pairs) + 2 pure inserts = 6 added, 4 removed.
	if added != 6 || removed != 4 {
		t.Fatalf("realistic config-sized edit must produce the MINIMAL diff, got +%d/-%d\n%s", added, removed, got)
	}
	// The inserted lines must appear as pure additions, never paired against
	// unrelated content that merely shifted position.
	if !strings.Contains(got, "+   - name: atlassian") {
		t.Errorf("inserted server line must render as a pure addition:\n%s", got)
	}
	if strings.Contains(got, "- key_1291:") || strings.Contains(got, "- key_1400: value # comment") {
		t.Errorf("shifted-but-unchanged lines must NOT appear as removals:\n%s", got)
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
