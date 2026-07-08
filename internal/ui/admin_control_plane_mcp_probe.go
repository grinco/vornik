package ui

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/mcp"
)

// Control-plane hub — MCP-servers tab: pre-commit PROBE (backlog #4
// "onboarding of third-party MCP servers"). Before an operator files an add
// proposal they can "Test" a candidate endpoint: the daemon connects to it,
// runs tools/list, and reports reachability + the advertised tool set inline.
// This de-risks onboarding a Home-Assistant / PageDrop / cluster MCP server —
// no more typing transport/URL blind and only learning it's wrong after the
// proposal is applied. It does NOT write anything (probe-only, admin-gated).

// mcpProbeConn is the slice of *mcp.Client the probe needs. Tests inject a
// fake so probing does no network.
type mcpProbeConn interface {
	Tools() []mcp.Tool
	Close() error
}

// mcpProbeConnect connects to a candidate MCP server for a dry-run probe.
// Package var so tests can substitute a fake connector.
var mcpProbeConnect = func(ctx context.Context, cfg mcp.ServerConfig, logger zerolog.Logger) (mcpProbeConn, error) {
	c, err := mcp.Connect(ctx, cfg, logger)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// mcpProbeTimeout bounds a candidate probe. Generous enough for a slow
// initialize + tools/list, tight enough not to hang the admin request.
const mcpProbeTimeout = 15 * time.Second

// AdminControlPlaneMCPProbe handles POST /ui/admin/control-plane/mcp/probe.
// Connects to the operator-supplied candidate endpoint, enumerates its tools,
// and returns an HTML fragment (htmx target) with the result. Never mutates
// config — onboarding is still ledger-gated via the add form.
func (s *Server) AdminControlPlaneMCPProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	transport := strings.TrimSpace(r.FormValue("transport"))
	url := strings.TrimSpace(r.FormValue("url"))
	command := strings.TrimSpace(r.FormValue("command"))

	if err := validMCPEndpoint(transport, url, command); err != nil {
		s.renderMCPProbe(w, false, "Invalid endpoint: "+err.Error(), nil)
		return
	}
	// Same secret-literal guard as the add form — don't invite pasting a raw
	// token into the probe. ${ENV} placeholders pass (and expand at connect
	// where the daemon env provides them).
	for _, v := range []string{url, command} {
		if s.hasSecretLiteral(v) {
			s.renderMCPProbe(w, false, "Use a ${ENV_VAR} placeholder, not a literal secret, in the URL/command.", nil)
			return
		}
	}

	cfg := mcp.ServerConfig{
		Name:      "probe",
		Transport: transport,
		URL:       url,
		Command:   command,
	}
	ctx, cancel := context.WithTimeout(r.Context(), mcpProbeTimeout)
	defer cancel()

	logger := s.logger.With().Str("component", "mcp-probe").Logger()
	client, err := mcpProbeConnect(ctx, cfg, logger)
	if err != nil {
		// Reachability failure (down, wrong URL, auth required, TLS, …). Still
		// useful signal — distinguishes "unreachable" from "reachable".
		s.renderMCPProbe(w, false, shortenProbeError(err.Error()), nil)
		return
	}
	tools := client.Tools()
	_ = client.Close()

	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	s.renderMCPProbe(w, true, "", names)
}

// renderMCPProbe writes the inline result fragment for the Test button.
func (s *Server) renderMCPProbe(w http.ResponseWriter, reachable bool, errMsg string, tools []string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var b strings.Builder
	if reachable {
		fmt.Fprintf(&b, `<div class="text-xs text-emerald-400">✓ reachable — %d tool(s)</div>`, len(tools))
		if len(tools) > 0 {
			b.WriteString(`<div class="mt-1 flex flex-wrap gap-1">`)
			for _, name := range tools {
				fmt.Fprintf(&b, `<span class="px-1.5 py-0.5 text-[10px] rounded bg-dark-800 text-gray-300 font-mono">%s</span>`, html.EscapeString(name))
			}
			b.WriteString(`</div>`)
		} else {
			b.WriteString(`<div class="text-[11px] text-gray-500 mt-1">Connected, but the server advertised no tools.</div>`)
		}
	} else {
		fmt.Fprintf(&b, `<div class="text-xs text-rose-400">✗ not reachable — %s</div>`, html.EscapeString(errMsg))
	}
	_, _ = w.Write([]byte(b.String()))
}

// shortenProbeError caps a probe error so a hostile/verbose server can't bloat
// the fragment.
func shortenProbeError(s string) string {
	const maxLen = 300
	s = strings.TrimSpace(s)
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}
