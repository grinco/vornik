package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// Control-plane hub — MCP-servers tab (LLD 2026-07-08-control-plane-hub-design
// §4). Lists the daemon's MCP servers (cached snapshot + reachability badge)
// and adds/removes them through the LEDGER: an add/remove computes a
// comment-preserving config.yaml edit (config.SetYAMLKey / DeleteYAMLKey) and
// files a daemon-scope `config` proposal (ProposedBy=operator-ui, BaseHash for
// optimistic concurrency). Nothing is applied here — the operator reviews the
// diff + applies on the Proposals tab. Test-reachability is read-only.

// AdminCPMCPRow is one MCP server on the tab.
type AdminCPMCPRow struct {
	Name      string
	Transport string
	Endpoint  string // URL or command
	Reachable bool
	Error     string
	ToolCount int
	LastCheck string
}

// mcpServerNameRe bounds a server name to a safe config key (dotted-path safe).
var mcpServerNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// validMCPTransports is the closed transport enum (hub-design §4).
var validMCPTransports = map[string]bool{"stdio": true, "sse": true, "streamable-http": true}

// validMCPEndpoint checks the transport enum + the transport-appropriate
// endpoint field (http(s) URL for sse/streamable-http, non-empty command for
// stdio). Shared by the add-form edit and the pre-commit probe so both agree
// on what a well-formed candidate looks like.
func validMCPEndpoint(transport, url, command string) error {
	if !validMCPTransports[transport] {
		return errMCPBadTransport
	}
	switch transport {
	case "sse", "streamable-http":
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return errMCPBadEndpoint
		}
	case "stdio":
		if command == "" {
			return errMCPBadEndpoint
		}
	}
	return nil
}

// buildCPMCP fills the MCP tab's list from the cached registry snapshot (no
// live probe on load — the badge is last-known status).
func (s *Server) buildCPMCP(ctx context.Context, data *AdminControlPlaneData) {
	data.MCPWritable = s.cpConfigPath != "" && s.proposalStore != nil
	if s.mcpRegistry == nil {
		return
	}
	for _, srv := range s.mcpRegistry.Snapshot(ctx) {
		endpoint := srv.URL
		if endpoint == "" {
			endpoint = srv.Command
		}
		last := ""
		if !srv.LastCheckedAt.IsZero() {
			last = srv.LastCheckedAt.Format(time.RFC3339)
		}
		data.MCPRows = append(data.MCPRows, AdminCPMCPRow{
			Name: srv.Name, Transport: srv.Transport, Endpoint: endpoint,
			Reachable: srv.Reachable, Error: srv.Error, ToolCount: len(srv.Tools), LastCheck: last,
		})
	}
}

// AdminControlPlaneMCPWrite handles POST /ui/admin/control-plane/mcp — the MCP
// Add/Remove forms. It NEVER writes config directly: it computes the edit and
// files a daemon-scope config proposal for review on the Proposals tab.
func (s *Server) AdminControlPlaneMCPWrite(w http.ResponseWriter, r *http.Request) {
	base := "/ui/admin/control-plane?section=mcp"
	redirect := func(done string) {
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		http.Redirect(w, r, base+sep+"done="+done, http.StatusSeeOther)
	}
	if r.Method != http.MethodPost {
		http.Redirect(w, r, base, http.StatusSeeOther)
		return
	}
	if s.proposalStore == nil || s.cpConfigPath == "" {
		redirect("error")
		return
	}
	_ = r.ParseForm()
	action := r.FormValue("action")
	name := strings.TrimSpace(r.FormValue("name"))
	if !mcpServerNameRe.MatchString(name) {
		redirect("mcp-bad-name")
		return
	}

	current, err := os.ReadFile(s.cpConfigPath) //nolint:gosec // operator-configured daemon path
	if err != nil {
		redirect("error")
		return
	}
	baseHash := sha256Hex(current)

	var edited []byte
	var title string
	switch action {
	case "add":
		edited, title, err = s.mcpAddEdit(current, name, r)
	case "remove":
		edited, title, err = mcpRemoveEdit(current, name)
	default:
		redirect("error")
		return
	}
	if err != nil {
		redirect(mcpErrToken(err))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	actor := adminPrincipal(r)
	evidence, _ := json.Marshal(map[string]string{"base_hash": baseHash, "actor": actor})
	p := &persistence.ControlPlaneProposal{
		ID:           persistence.GenerateID("cpp"),
		Kind:         persistence.ProposalKindConfig,
		BlastRadius:  persistence.ProposalScopeDaemon, // MCP catalog is daemon-scope
		Title:        title,
		Diff:         unifiedish(string(current), string(edited)),
		Rationale:    fmt.Sprintf("MCP server %q %s via the control-plane hub by %s. Review the diff, then apply (daemon-scope — affects every project).", name, action, actor),
		Evidence:     string(evidence),
		Status:       persistence.ProposalStatusDraft,
		ProposedBy:   "operator-ui", // reserved principal — server-stamped, never from input
		ApplyTarget:  "config.yaml",
		ApplyContent: string(edited),
	}
	if err := s.proposalStore.Create(ctx, p); err != nil {
		redirect("error")
		return
	}
	redirect("mcp-proposed")
}

// mcpAddEdit builds the config.yaml content adding/replacing one mcp_servers
// entry from the form, using the comment-preserving SetYAMLKey. Rejects a bad
// transport or a secret literal in a field.
func (s *Server) mcpAddEdit(current []byte, name string, r *http.Request) ([]byte, string, error) {
	transport := strings.TrimSpace(r.FormValue("transport"))
	if !validMCPTransports[transport] {
		return nil, "", errMCPBadTransport
	}
	url := strings.TrimSpace(r.FormValue("url"))
	command := strings.TrimSpace(r.FormValue("command"))
	if err := validMCPEndpoint(transport, url, command); err != nil {
		return nil, "", err
	}
	// Secret-literal guard: fields take ${ENV} placeholders, never a raw secret.
	for _, v := range []string{url, command} {
		if s.hasSecretLiteral(v) {
			return nil, "", errMCPSecretLiteral
		}
	}
	prefix := "mcp_servers." + name + "."
	out := current
	var err error
	set := func(key string, val any) {
		if err != nil {
			return
		}
		out, _, err = config.SetYAMLKey(out, prefix+key, val)
	}
	set("transport", transport)
	if url != "" {
		set("url", url)
	}
	if command != "" {
		set("command", command)
	}
	if err != nil {
		return nil, "", err
	}
	return out, fmt.Sprintf("MCP: add server %q (%s)", name, transport), nil
}

// mcpRemoveEdit deletes the mcp_servers.<name> node (comment-preserving).
func mcpRemoveEdit(current []byte, name string) ([]byte, string, error) {
	out, removed, err := config.DeleteYAMLKey(current, "mcp_servers."+name)
	if err != nil {
		return nil, "", err
	}
	if !removed {
		return nil, "", errMCPNotFound
	}
	return out, fmt.Sprintf("MCP: remove server %q", name), nil
}

// hasSecretLiteral is a cheap guard: a value that looks like a bare secret
// (long high-entropy-ish token) rather than an ${ENV} placeholder. Conservative
// — only flags obvious literals so the ${VAR} path is unobstructed.
func (s *Server) hasSecretLiteral(v string) bool {
	if v == "" || strings.Contains(v, "${") {
		return false
	}
	// A bare token that looks like a key (>=24 chars, no spaces, not a URL path).
	if len(v) >= 24 && !strings.ContainsAny(v, " /:") {
		return true
	}
	return false
}

var (
	errMCPBadTransport  = fmt.Errorf("bad transport")
	errMCPBadEndpoint   = fmt.Errorf("bad endpoint")
	errMCPSecretLiteral = fmt.Errorf("secret literal")
	errMCPNotFound      = fmt.Errorf("not found")
)

func mcpErrToken(err error) string {
	switch err {
	case errMCPBadTransport:
		return "mcp-bad-transport"
	case errMCPBadEndpoint:
		return "mcp-bad-endpoint"
	case errMCPSecretLiteral:
		return "mcp-secret"
	case errMCPNotFound:
		return "mcp-not-found"
	default:
		return "error"
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// unifiedish renders a minimal old→new line diff for the proposal review pane.
// Not a true unified diff — a compact changed-lines summary is enough for the
// operator to see what the edit does before applying.
func unifiedish(oldStr, newStr string) string {
	oldLines := strings.Split(oldStr, "\n")
	newLines := strings.Split(newStr, "\n")
	oldSet := map[string]bool{}
	for _, l := range oldLines {
		oldSet[l] = true
	}
	newSet := map[string]bool{}
	for _, l := range newLines {
		newSet[l] = true
	}
	var b strings.Builder
	for _, l := range oldLines {
		if !newSet[l] {
			fmt.Fprintf(&b, "- %s\n", l)
		}
	}
	for _, l := range newLines {
		if !oldSet[l] {
			fmt.Fprintf(&b, "+ %s\n", l)
		}
	}
	if b.Len() == 0 {
		return "(no line-level change)"
	}
	return b.String()
}
