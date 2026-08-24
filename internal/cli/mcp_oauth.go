package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// errMCPConnectIncomplete is returned when the CLI has already explained the outcome on stderr.
// Cobra would otherwise print "Error: …" a second time in different words.
var errMCPConnectIncomplete = errors.New("mcp connect did not complete")

// `vornikctl mcp connect` / `disconnect` / `oauth-status` — the operator path for granting an MCP
// server access, per mcp-server-authentication-design.md §7.2a.
//
// The CLI is NOT a second OAuth client. It asks the daemon to start a flow, prints the URL for the
// operator to open, and waits for the DAEMON's callback to complete the exchange. One client, one
// redirect URI, one callback — registering a loopback redirect here would double the registrations
// at the authorization server, and several servers reject loopback redirects for confidential
// clients outright.
//
// It is also a VERIFIER, not a success light (review round-2 N1). The CLI never sees the token, so
// "a row appeared" is not evidence the operator got what they consented to: a confused or tampered
// daemon could record a different resource or wider scopes. So it prints the ask BEFORE the
// operator consents, and compares the recorded grant against it afterwards.

const (
	// mcpConnectPollInterval / mcpConnectTimeout sit just past typical
	// authorization-code lifetimes (§7.2a, review round-2 suggestion 2).
	mcpConnectPollInterval = 2 * time.Second
	mcpConnectTimeout      = 5 * time.Minute
)

var (
	mcpConnectCmd = &cobra.Command{
		Use:   "connect <server>",
		Short: "Grant an MCP server access via OAuth (prints a URL and waits)",
		Long: `Start the OAuth consent flow for one MCP server.

Prints the resource and scopes being requested, then an authorization URL to
open in a browser. The daemon's own callback completes the exchange; this
command waits for it and then verifies that the recorded grant matches what was
requested.

Omit --project for a daemon-level server (config.yaml's mcp.servers). NOTE that
a daemon-level grant is reachable from EVERY project on the daemon.

Examples:
  vornikctl mcp connect atlassian -p my-project
  vornikctl mcp connect n8n`,
		Args: cobra.ExactArgs(1),
		RunE: runMCPConnect,
	}
	mcpDisconnectCmd = &cobra.Command{
		Use:   "disconnect <server>",
		Short: "Revoke Vornik's stored OAuth grant for an MCP server",
		Long: `Delete the stored OAuth grant for one MCP server.

The server's auth: block in config is untouched, so reconnecting needs no config
change. This does NOT revoke the grant at the vendor — do that in their console
if you want the authorization itself withdrawn.`,
		Args: cobra.ExactArgs(1),
		RunE: runMCPDisconnect,
	}
	mcpOAuthStatusCmd = &cobra.Command{
		Use:   "oauth-status <server>",
		Short: "Show the stored OAuth grant for an MCP server",
		Args:  cobra.ExactArgs(1),
		RunE:  runMCPOAuthStatus,
	}
)

func init() {
	for _, c := range []*cobra.Command{mcpConnectCmd, mcpDisconnectCmd, mcpOAuthStatusCmd} {
		c.Flags().StringVarP(&mcpProject, "project", "p", "",
			"Project ID (omit for a daemon-level server)")
	}
	mcpOAuthStatusCmd.Flags().BoolVar(&mcpJSON, "json", false, "JSON output")
	mcpCmd.AddCommand(mcpConnectCmd, mcpDisconnectCmd, mcpOAuthStatusCmd)
}

type mcpOAuthBeginResp struct {
	AuthorizationURL string   `json:"authorization_url"`
	Resource         string   `json:"resource"`
	Scopes           []string `json:"scopes"`
	RedirectURI      string   `json:"redirect_uri"`
	DroppedScopes    []string `json:"dropped_scopes,omitempty"`
}

type mcpOAuthStatusResp struct {
	Connected      bool     `json:"connected"`
	Resource       string   `json:"resource"`
	Scopes         []string `json:"scopes"`
	ConnectedBy    string   `json:"connected_by"`
	ConnectedAt    string   `json:"connected_at"`
	ExpiresAt      string   `json:"expires_at"`
	NeedsReconnect bool     `json:"needs_reconnect"`
	InheritedFrom  string   `json:"inherited_from"`
}

func runMCPConnect(_ *cobra.Command, args []string) error {
	server := args[0]

	raw, err := postJSON("/api/v1/mcp/oauth/begin", map[string]string{
		"project_id": mcpProject,
		"server":     server,
	})
	if err != nil {
		return err
	}
	var begun mcpOAuthBeginResp
	if err := json.Unmarshal(raw, &begun); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	// Step 1 of §7.2a: show the ask BEFORE the operator consents, so they can
	// compare it with what they intended rather than discovering it afterwards.
	fmt.Printf("Connecting MCP server %q for %s\n\n", server, mcpScopeLabel())
	fmt.Printf("  Resource:     %s\n", begun.Resource)
	fmt.Printf("  Scopes:       %s\n", mcpScopeList(begun.Scopes))
	fmt.Printf("  Redirect URI: %s\n", begun.RedirectURI)
	if len(begun.DroppedScopes) > 0 {
		// The ask was NARROWED. Printing only the final list is what let a
		// read-only Atlassian grant read as "the vendor has no write tools"
		// (2026-08-22) — the consent screen can only offer what we requested.
		fmt.Printf("\n  WITHHELD:     %s\n", mcpScopeList(begun.DroppedScopes))
		fmt.Printf("                The server advertises these; this request does NOT ask for them,\n")
		fmt.Printf("                because a daemon-scope grant may not inherit write access.\n")
		fmt.Printf("                To grant them, either name them in auth.scopes for %q in\n", server)
		fmt.Printf("                config.yaml (all projects), or re-run with --project <id>\n")
		fmt.Printf("                to confine the grant to one project.\n")
	}
	if mcpProject == "" {
		fmt.Printf("\n  NOTE: a daemon-level grant is usable by EVERY project on this daemon.\n")
	}
	fmt.Printf("\nOpen this URL to consent:\n\n  %s\n\n", begun.AuthorizationURL)
	fmt.Printf("Waiting for the callback (up to %s)…\n", mcpConnectTimeout)

	got, err := waitForMCPGrant(server)
	if err != nil {
		return err
	}

	// Steps 2 and 3: compare the RECORDED grant with what was requested. The
	// CLI cannot prevent daemon-side tampering — nothing CLI-side can — but it
	// can turn an invisible discrepancy into a loud one.
	if mismatch := mcpGrantMismatch(begun, got); mismatch != "" {
		fmt.Fprintf(os.Stderr, "\nconsent completed, but the recorded grant does not match what was requested — inspect the ledger\n  %s\n", mismatch)
		return errMCPConnectIncomplete
	}

	fmt.Printf("\nConnected. Recorded grant:\n")
	fmt.Printf("  Resource: %s\n", got.Resource)
	fmt.Printf("  Scopes:   %s\n", mcpScopeList(got.Scopes))
	if got.ExpiresAt != "" {
		fmt.Printf("  Expires:  %s (refreshed automatically)\n", got.ExpiresAt)
	}
	return nil
}

// waitForMCPGrant polls until the daemon's callback has recorded a grant.
//
// A timeout ends the CLI's WAIT, not the flow: a consent completed afterwards still lands via the
// daemon callback, which is why the message says so instead of implying failure.
func waitForMCPGrant(server string) (mcpOAuthStatusResp, error) {
	deadline := time.Now().Add(mcpConnectTimeout)
	// Ignore a PRE-EXISTING grant: reconnecting an already-connected server
	// must wait for the NEW consent, not return the old row instantly.
	before, _ := fetchMCPGrant(server)

	for time.Now().Before(deadline) {
		time.Sleep(mcpConnectPollInterval)
		got, err := fetchMCPGrant(server)
		if err != nil {
			// A transient read failure must not abandon a consent the
			// operator may already have given.
			continue
		}
		if got.Connected && (!before.Connected || got.ConnectedAt != before.ConnectedAt) {
			return got, nil
		}
	}
	fmt.Fprintf(os.Stderr, "\nconsent not completed — you can still finish in the browser; the connection will appear in the control plane once you do\n")
	return mcpOAuthStatusResp{}, errMCPConnectIncomplete
}

func fetchMCPGrant(server string) (mcpOAuthStatusResp, error) {
	path := "/api/v1/mcp/oauth/status?server=" + url.QueryEscape(server)
	if mcpProject != "" {
		path += "&project_id=" + url.QueryEscape(mcpProject)
	}
	raw, err := fetchJSON(path)
	if err != nil {
		return mcpOAuthStatusResp{}, err
	}
	var out mcpOAuthStatusResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return mcpOAuthStatusResp{}, fmt.Errorf("parse response: %w", err)
	}
	return out, nil
}

// mcpGrantMismatch describes the first material difference between what was requested and what was
// recorded, or "" when they agree.
//
// A NARROWER granted scope set is a mismatch worth reporting, not an error: an authorization server
// is free to narrow, and the operator should know it happened rather than discover it later as a
// puzzling 403. A WIDER set is the more serious direction and is reported the same way.
func mcpGrantMismatch(requested mcpOAuthBeginResp, recorded mcpOAuthStatusResp) string {
	if !recorded.Connected {
		return "no grant was recorded"
	}
	if requested.Resource != "" && recorded.Resource != requested.Resource {
		return fmt.Sprintf("resource: requested %q, recorded %q", requested.Resource, recorded.Resource)
	}
	want := normalizedScopeSet(requested.Scopes)
	have := normalizedScopeSet(recorded.Scopes)
	if len(want) == 0 {
		return ""
	}
	var extra, missing []string
	for s := range have {
		if _, ok := want[s]; !ok {
			extra = append(extra, s)
		}
	}
	for s := range want {
		if _, ok := have[s]; !ok {
			missing = append(missing, s)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	switch {
	case len(extra) > 0 && len(missing) > 0:
		return fmt.Sprintf("scopes: granted %s that were not requested, and did not grant %s",
			strings.Join(extra, ", "), strings.Join(missing, ", "))
	case len(extra) > 0:
		return fmt.Sprintf("scopes: granted %s, which was not requested", strings.Join(extra, ", "))
	case len(missing) > 0:
		return fmt.Sprintf("scopes: %s was requested but not granted", strings.Join(missing, ", "))
	}
	return ""
}

func normalizedScopeSet(scopes []string) map[string]struct{} {
	out := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = struct{}{}
		}
	}
	return out
}

func runMCPDisconnect(_ *cobra.Command, args []string) error {
	if _, err := postJSON("/api/v1/mcp/oauth/disconnect", map[string]string{
		"project_id": mcpProject,
		"server":     args[0],
	}); err != nil {
		return err
	}
	fmt.Printf("Disconnected %q for %s. The auth: block in config is unchanged, so you can reconnect without editing it.\n",
		args[0], mcpScopeLabel())
	fmt.Printf("This did NOT revoke the authorization at the vendor — do that in their console if you want it withdrawn there too.\n")
	return nil
}

func runMCPOAuthStatus(_ *cobra.Command, args []string) error {
	got, err := fetchMCPGrant(args[0])
	if err != nil {
		return err
	}
	if mcpJSON {
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	}
	if !got.Connected {
		fmt.Printf("%q is not connected for %s.\n", args[0], mcpScopeLabel())
		return nil
	}
	if got.InheritedFrom != "" {
		// The grant is the daemon's, shared by every project that does not
		// override it. Saying only "connected for project X" would hide where
		// the credential lives — which is where a reconnect or a revoke has to
		// happen, and which projects a revoke would affect.
		fmt.Printf("%q is connected for project %s via the daemon-scope grant.\n", args[0], got.InheritedFrom)
		fmt.Printf("  This project subscribes by name only, so it shares the daemon's credential\n")
		fmt.Printf("  with every other project that does not declare its own. Manage it with\n")
		fmt.Printf("  `vornikctl mcp connect/disconnect %s` (no -p flag).\n", args[0])
	} else {
		fmt.Printf("%q is connected for %s.\n", args[0], mcpScopeLabel())
	}
	fmt.Printf("  Resource:     %s\n", got.Resource)
	fmt.Printf("  Scopes:       %s\n", mcpScopeList(got.Scopes))
	fmt.Printf("  Connected by: %s\n", got.ConnectedBy)
	fmt.Printf("  Connected at: %s\n", got.ConnectedAt)
	if got.ExpiresAt != "" {
		fmt.Printf("  Expires:      %s\n", got.ExpiresAt)
	}
	if got.NeedsReconnect {
		fmt.Printf("\n  NEEDS RECONNECT: the stored grant can no longer be refreshed.\n")
		fmt.Printf("  Run: vornikctl mcp connect %s%s\n", args[0], mcpProjectFlagSuffix())
	}
	return nil
}

func mcpScopeLabel() string {
	if mcpProject == "" {
		return "the daemon scope"
	}
	return "project " + mcpProject
}

func mcpProjectFlagSuffix() string {
	if mcpProject == "" {
		return ""
	}
	return " -p " + mcpProject
}

func mcpScopeList(scopes []string) string {
	if len(scopes) == 0 {
		return "(none — the server advertised none and none were configured)"
	}
	return strings.Join(scopes, " ")
}
