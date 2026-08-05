package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcpauth"
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
	// AuthChallenged marks a server the health check could not reach ONLY
	// because it demands authentication (401). Rendering that as "unreachable"
	// sends the operator looking for a networking or URL fault when the
	// endpoint is fine and the answer is a consent grant — reported
	// 2026-08-05 against the atlassian server, where the row said
	// "unreachable ... returned 401" while the endpoint was perfectly healthy.
	AuthChallenged bool
	Error          string
	ToolCount      int
	LastCheck      string

	// AuthMode is none | oauth | static | env (MCP server authentication
	// design §7 item 3).
	AuthMode string
	// Connected / TokenExpiresAt / ConnectedBy / NeedsReconnect describe the
	// stored OAuth grant. They exist because the reachability badge above is
	// AUTH-BLIND: a server returning 401 on every call is "reachable", and a
	// row that cannot tell reachable from authorized tells an operator
	// nothing about why their tools are failing.
	Connected      bool
	TokenExpiresAt string
	ConnectedBy    string
	NeedsReconnect bool
	// CanConnect is false when the §7.1 precondition is unmet, and
	// ConnectBlockedReason says why. Disabling the button up front is the
	// point: failing AFTER the operator consented at the vendor wastes their
	// time and strands an authorization code.
	CanConnect           bool
	ConnectBlockedReason string
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
		row := AdminCPMCPRow{
			Name: srv.Name, Transport: srv.Transport, Endpoint: endpoint,
			Reachable: srv.Reachable, Error: srv.Error, ToolCount: len(srv.Tools), LastCheck: last,
		}
		row.AuthChallenged = !row.Reachable && mcpErrorIsAuthChallenge(row.Error)
		s.decorateCPMCPAuth(ctx, &row)
		data.MCPRows = append(data.MCPRows, row)
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
		Rationale:    fmt.Sprintf("MCP server %q %s via the control-plane hub by %s. Review the diff, then apply (daemon-scope — affects every project). Applies live — no idle window needed.", name, action, actor),
		Evidence:     string(evidence),
		Status:       persistence.ProposalStatusDraft,
		ProposedBy:   "operator-ui", // reserved principal — server-stamped, never from input
		ApplyTarget:  "config.yaml",
		ApplyContent: string(edited),
		// LiveApply: adding/removing an MCP server is non-disruptive to in-flight
		// tasks — the MCP catalog is injected into agent containers at start
		// (executor writes /app/input/mcp.json once), so a running task never
		// sees a mid-flight catalog change. Skipping the all-projects busy gate
		// is what makes this daemon-scope change appliable on a busy prod daemon.
		LiveApply: true,
	}
	if err := s.proposalStore.Create(ctx, p); err != nil {
		redirect("error")
		return
	}
	redirect("mcp-proposed")
}

// mcpAddEdit builds the config.yaml content adding (or replacing) one server in
// the daemon MCP catalog. The catalog is the LIST at `mcp.servers` (a sequence
// of {name, transport, url|command, …} items) — NOT a `mcp_servers.<name>` map;
// that was the 2026-07-08 bug where hub-added servers landed under a key the
// config parser (mcp.servers) never reads, so they were invisible everywhere.
// Add-or-replace: drop any existing item with this name, then append the new
// one. Comment-preserving. Rejects a bad transport or a secret literal.
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
	fields := []config.YAMLListField{
		{Key: "name", Value: name},
		{Key: "transport", Value: transport},
	}
	if url != "" {
		fields = append(fields, config.YAMLListField{Key: "url", Value: url})
	}
	if command != "" {
		fields = append(fields, config.YAMLListField{Key: "command", Value: command})
	}
	// Add-or-replace via the shared upsert primitive (an existing entry of
	// the same name is replaced; otherwise the item is appended).
	out, err := config.UpsertYAMLListItemByField(current, "mcp.servers", "name", fields)
	if err != nil {
		return nil, "", err
	}

	// The `auth:` block is written SEPARATELY, as a surgical field edit rather
	// than a field on the upsert: the upsert replaces the whole list item, and
	// SetYAMLListItemField is the primitive that writes one nested mapping
	// without disturbing the rest. Before this the form had to REFUSE editing a
	// server carrying an auth block, because yamledit could not write a nested
	// mapping at all.
	//
	// A form that submits mode none deliberately REMOVES the block: leaving a
	// stale one behind would keep presenting a credential the operator just
	// turned off.
	authFields, err := mcpAuthFieldsFromForm(r, transport)
	if err != nil {
		return nil, "", err
	}
	if authFields != nil {
		out, err = config.SetYAMLListItemField(out, "mcp.servers", "name", name, "auth", authFields)
		if err != nil {
			return nil, "", err
		}
	}
	return out, fmt.Sprintf("MCP: add server %q (%s)", name, transport), nil
}

// mcpRemoveEdit removes the `mcp.servers` list item with the given name
// (comment-preserving).
func mcpRemoveEdit(current []byte, name string) ([]byte, string, error) {
	out, removed, err := config.RemoveYAMLListItemByField(current, "mcp.servers", "name", name)
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
	// errMCPBadAuth reports a malformed auth block from the Add/Edit form.
	// Validated at the form rather than left to config load: a proposal
	// carrying an invalid block would pass review and then fail the daemon's
	// config load on apply, which is the worst place to find out.
	errMCPBadAuth = fmt.Errorf("invalid auth block")
)

// mcpErrToken maps a form error to the redirect token the tab renders a message for.
//
// errors.Is, not equality: errMCPBadAuth is WRAPPED with the specific validation failure so the
// daemon log says which rule was broken, and an equality switch silently degraded every one of
// those to the generic "error" token — caught by the control-plane auth test.
func mcpErrToken(err error) string {
	switch {
	case errors.Is(err, errMCPBadTransport):
		return "mcp-bad-transport"
	case errors.Is(err, errMCPBadEndpoint):
		return "mcp-bad-endpoint"
	case errors.Is(err, errMCPSecretLiteral):
		return "mcp-secret"
	case errors.Is(err, errMCPNotFound):
		return "mcp-not-found"
	case errors.Is(err, errMCPBadAuth):
		return "mcp-bad-auth"
	default:
		return "error"
	}
}

// mcpErrorIsAuthChallenge reports whether a recorded health-check error is an
// authentication demand rather than a reachability fault.
//
// Matches internal/mcp's "<transport> server returned <code>" text because the
// health snapshot keeps the error as a STRING, not an error value, so
// errors.As is unavailable at this layer. That coupling is pinned by
// TestMCPErrorIsAuthChallenge_MatchesTheRealClientError, which builds the
// message from the mcp client's own formatting — change the format there and
// the test fails rather than the badge quietly going wrong again.
func mcpErrorIsAuthChallenge(msg string) bool {
	return strings.Contains(msg, "server returned 401")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// maxUnifiedishLCSCells bounds the quadratic LCS table used for a compact
// minimal diff. config.yaml is operator-owned and can be much larger than the
// workflow files computeWorkflowDiff was designed for. This caps the
// transient table at ~32 MiB on a 64-bit platform (~16 MiB where int is 32
// bits) — acceptable for a human-triggered admin action, and it refuses to
// scale with an arbitrarily large file.
//
// Sized deliberately against the real deployment rather than picked round:
// after common prefix/suffix trimming a ~1,500-line config.yaml presents a
// changed window of ~1,100 lines (~1.3M cells), so realistic configs get the
// MINIMAL diff and only pathological input degrades to positionalDiff. A
// smaller cap silently sent the live config down the linear path, which
// re-created the very bug this function was fixed for — see
// TestUnifiedish_RealisticConfigSizeStaysMinimal.
const maxUnifiedishLCSCells = 4_000_000

// unifiedish renders an old→new line diff for the proposal review pane.
// Small inputs use the minimal LCS diff shared with the workflow-proposal
// panel. Larger inputs use a linear positional diff so an MCP proposal cannot
// exhaust memory or stall the admin request on a large config.yaml.
//
// A prior version diffed by SET membership: every old line absent from the
// new line set printed under a single "-" block, followed by every new line
// absent from the old set under a single "+" block — with no positional
// pairing between them. On config.yaml, the mcp.servers add path
// round-trips the WHOLE file through a yaml.v3 Node encode (comment-
// preserving, but not blank-line- or flow-spacing-preserving), so an
// unrelated add reformats dozens of scattered lines (e.g. "{ a: 1 }" ->
// "{a: 1}"). The bucketed diff rendered that as a wall of unrelated-looking
// deletions with the matching (reformatted) additions scrolled far below —
// scary and unreviewable even though nothing was actually deleted. Pairing
// by position makes the reformat read as what it is: adjacent -/+ lines
// carrying the same content.
func unifiedish(oldStr, newStr string) string {
	oldLines := splitLinesForDiff(oldStr)
	newLines := splitLinesForDiff(newStr)

	// Trim the identical head and tail before measuring the LCS table: only
	// the changed WINDOW needs the quadratic algorithm, and unifiedish emits
	// nothing for unchanged lines anyway. On a config edit this is most of
	// the file, which is what keeps a realistic config.yaml under the cap
	// instead of degrading to positionalDiff.
	//
	// INVARIANT: the suffix is measured on the ALREADY prefix-trimmed slices,
	// so the two regions can never overlap and both re-slices stay in bounds
	// (commonSuffixLen returns at most len of the shorter remaining slice).
	pre := commonPrefixLen(oldLines, newLines)
	oldLines, newLines = oldLines[pre:], newLines[pre:]
	suf := commonSuffixLen(oldLines, newLines)
	oldLines = oldLines[:len(oldLines)-suf]
	newLines = newLines[:len(newLines)-suf]

	var lines []DiffLine
	degraded := lcsTableTooLarge(len(oldLines), len(newLines))
	if degraded {
		lines = positionalDiff(oldLines, newLines)
	} else {
		lines = computeDiffLines(oldLines, newLines)
	}
	var b strings.Builder
	// Say so when the diff is not minimal. Silent degradation would leave an
	// operator unable to tell a fallback's inflated churn from a real change,
	// which is the same "can't trust the pane" failure this function was
	// fixed for. Stated in the pane rather than the daemon log because the
	// pane is where the person making the decision is looking.
	if degraded {
		fmt.Fprintf(&b, "(%d changed lines — too large for a minimal diff; showing a positional comparison, so shifted lines may appear as changes)\n",
			len(oldLines)+len(newLines))
	}
	for _, l := range lines {
		switch l.Op {
		case diffRemove:
			fmt.Fprintf(&b, "- %s\n", l.Text)
		case diffAdd:
			fmt.Fprintf(&b, "+ %s\n", l.Text)
		}
	}
	if b.Len() == 0 {
		return "(no line-level change)"
	}
	return b.String()
}

func lcsTableTooLarge(oldLineCount, newLineCount int) bool {
	// computeDiffLines allocates an extra terminal row and column. Compared by
	// DIVISION rather than multiplying the two counts, which would overflow on
	// adversarially large input.
	return oldLineCount+1 > maxUnifiedishLCSCells/(newLineCount+1)
}

// commonPrefixLen counts the leading lines a and b share.
func commonPrefixLen(a, b []string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// commonSuffixLen counts the trailing lines a and b share.
func commonSuffixLen(a, b []string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[len(a)-1-i] != b[len(b)-1-i] {
			return i
		}
	}
	return n
}

// positionalDiff emits the changed lines at each position as an adjacent
// remove/add pair. It is not minimal, but stays O(n) in memory and time while
// retaining the review pane's important pairing property for large rewrites.
func positionalDiff(oldLines, newLines []string) []DiffLine {
	shared := len(oldLines)
	if len(newLines) < shared {
		shared = len(newLines)
	}
	lines := make([]DiffLine, 0, len(oldLines)+len(newLines))
	for i := 0; i < shared; i++ {
		if oldLines[i] == newLines[i] {
			continue
		}
		lines = append(lines, DiffLine{Op: diffRemove, Text: oldLines[i]}, DiffLine{Op: diffAdd, Text: newLines[i]})
	}
	for _, line := range oldLines[shared:] {
		lines = append(lines, DiffLine{Op: diffRemove, Text: line})
	}
	for _, line := range newLines[shared:] {
		lines = append(lines, DiffLine{Op: diffAdd, Text: line})
	}
	return lines
}

// decorateCPMCPAuth fills a row's auth columns from config and the token store.
//
// Daemon scope only: this tab is the daemon catalog (config.yaml's mcp.servers), and a
// project-scoped grant belongs on the project's own page.
func (s *Server) decorateCPMCPAuth(ctx context.Context, row *AdminCPMCPRow) {
	if s.mcpOAuthAdmin == nil {
		return
	}
	ref, ok := s.mcpOAuthAdmin.ResolveServer("", row.Name)
	if !ok {
		return
	}
	row.AuthMode = ref.Auth.EffectiveMode()
	if row.AuthMode != mcpauth.ModeOAuth {
		return
	}
	if _, err := s.mcpOAuthAdmin.RedirectURI(); err != nil {
		row.ConnectBlockedReason = "set server.public_base_url to an https:// origin first — OAuth needs a redirect URI the vendor can reach"
	} else {
		row.CanConnect = true
	}
	tok, err := s.mcpOAuthAdmin.Grant(ctx, "", row.Name)
	if err != nil || tok == nil {
		return
	}
	row.Connected = true
	row.ConnectedBy = tok.ConnectedBy
	row.NeedsReconnect = tok.NeedsReconnect
	if tok.ExpiresAt != nil {
		row.TokenExpiresAt = tok.ExpiresAt.UTC().Format(time.RFC3339)
	}
}

// mcpAuthFieldsFromForm builds the `auth:` mapping for a server from the Add/Edit form, or nil
// when the operator selected mode none.
//
// Secret VALUES never come through here. The form takes `secret://<name>` references only, which
// is what lets the resulting proposal travel through the ledger as a reviewable diff with nothing
// sensitive in it — and it is why the mode-specific validation below rejects a literal rather than
// trying to be helpful about it.
//
// Fields outside the submitted mode are IGNORED, which the switch below is the whole mechanism for.
// The form's mode toggle only hides the other fieldsets, so every auth_* input is still submitted:
// an operator who configures oauth, switches to static and saves would otherwise carry stale scopes
// — or a credential reference for a mode no longer in use — into the written block. Pinned by
// TestMcpAddEdit_IgnoresFieldsOutsideTheSubmittedMode.
func mcpAuthFieldsFromForm(r *http.Request, transport string) (map[string]any, error) {
	mode := strings.TrimSpace(r.FormValue("auth_mode"))
	if mode == "" || mode == mcpauth.ModeNone {
		return nil, nil
	}
	auth := mcpauth.Auth{Mode: mode}
	switch mode {
	case mcpauth.ModeStatic:
		auth.Header = strings.TrimSpace(r.FormValue("auth_header"))
		auth.ValueFrom = strings.TrimSpace(r.FormValue("auth_value_from"))
		auth.ValuePrefix = r.FormValue("auth_value_prefix")
	case mcpauth.ModeEnv:
		auth.EnvFrom = parseEnvFromLines(r.FormValue("auth_env_from"))
	case mcpauth.ModeOAuth:
		auth.Scopes = splitChipList(r.FormValue("auth_scopes"))
		auth.ClientID = strings.TrimSpace(r.FormValue("auth_client_id"))
		auth.ClientSecretFrom = strings.TrimSpace(r.FormValue("auth_client_secret_from"))
		auth.AuthorizationEndpoint = strings.TrimSpace(r.FormValue("auth_authorization_endpoint"))
		auth.TokenEndpoint = strings.TrimSpace(r.FormValue("auth_token_endpoint"))
	}
	// Validate HERE rather than at load: a proposal carrying an invalid block
	// would pass review and then fail the daemon's config load on apply, which
	// is the worst place to discover it.
	if err := auth.Validate(transport); err != nil {
		return nil, fmt.Errorf("%w: %s", errMCPBadAuth, err.Error())
	}
	raw, err := yaml.Marshal(auth)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// parseEnvFromLines parses "NAME=secret://ref" lines from the env-mode textarea.
func parseEnvFromLines(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, ref, ok := strings.Cut(line, "=")
		if !ok {
			// Keep the malformed entry so Validate rejects it by name
			// instead of the form silently dropping it.
			out[strings.TrimSpace(line)] = ""
			continue
		}
		out[strings.TrimSpace(name)] = strings.TrimSpace(ref)
	}
	return out
}

// AdminControlPlaneMCPConnect handles POST /ui/admin/control-plane/mcp/connect — the per-row
// Connect and Disconnect actions.
//
// Deliberately NOT a config proposal (design §7): consent is an ACTION on an already-applied
// server, not a config mutation. A proposal-shaped Connect would also be unworkable — an
// authorization code lives seconds to minutes, so gating it on a second operator's approval would
// guarantee it expired. The grant is recorded instead, which is what makes an invisible compromise
// a detectable one.
//
// Connect is refused for a server whose config is not applied yet, because its endpoint may still
// change under review — consenting against a URL that is about to be edited grants access to the
// wrong thing.
func (s *Server) AdminControlPlaneMCPConnect(w http.ResponseWriter, r *http.Request) {
	base := "/ui/admin/control-plane?section=mcp"
	redirect := func(done string) {
		http.Redirect(w, r, base+"&done="+done, http.StatusSeeOther)
	}
	if r.Method != http.MethodPost {
		http.Redirect(w, r, base, http.StatusSeeOther)
		return
	}
	if s.mcpOAuthAdmin == nil {
		redirect("error")
		return
	}
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if !mcpServerNameRe.MatchString(name) {
		redirect("mcp-bad-name")
		return
	}
	actor := adminPrincipal(r)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if r.FormValue("action") == "disconnect" {
		if err := s.mcpOAuthAdmin.Disconnect(ctx, "", name, actor); err != nil {
			s.logger.Error().Err(err).Str("server", name).Msg("mcp oauth: disconnect failed")
			redirect("error")
			return
		}
		redirect("mcp-disconnected")
		return
	}

	ref, ok := s.mcpOAuthAdmin.ResolveServer("", name)
	if !ok {
		redirect("mcp-not-found")
		return
	}
	begun, err := s.mcpOAuthAdmin.Begin(ctx, ref, actor)
	if err != nil {
		// Never surface the vendor's own words: an OAuth error body can echo
		// request parameters, which on a token request means the client secret.
		s.logger.Error().Err(err).Str("server", name).Msg("mcp oauth: connect could not start")
		redirect("mcp-connect-failed")
		return
	}
	// Hand the operator straight to the vendor's consent screen. The daemon's
	// own callback completes the exchange and lands them back on this tab.
	http.Redirect(w, r, begun.AuthorizationURL, http.StatusSeeOther)
}
