package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// `vornikctl key {create,list,rotate,revoke}` — operator surface for
// DB-backed bearer tokens. The secret returned by create / rotate is
// printed exactly once to stdout; everything else (list, revoke)
// reveals only the prefix.

type keyListEntry struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	KeyPrefix        string   `json:"key_prefix"`
	CreatedAt        string   `json:"created_at"`
	LastUsedAt       *string  `json:"last_used_at,omitempty"`
	ExpiresAt        *string  `json:"expires_at,omitempty"`
	RevokedAt        *string  `json:"revoked_at,omitempty"`
	CreatedBy        string   `json:"created_by,omitempty"`
	AllowedWorkflows []string `json:"allowed_workflows,omitempty"`
	AllowPush        bool     `json:"allow_push,omitempty"`
}

type keyListResponse struct {
	Keys []keyListEntry `json:"keys"`
}

type keyCreatedResponse struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	ProjectID string  `json:"project_id"`
	Secret    string  `json:"secret"`
	KeyPrefix string  `json:"key_prefix"`
	CreatedAt string  `json:"created_at"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

var keyCmd = &cobra.Command{
	Use:   "key",
	Short: "Manage per-project API keys",
	Long: `Create, list, rotate, and revoke DB-backed bearer tokens scoped to a
single project. The token's bound project is the authoritative cost-row
target — X-Vornik-Project-ID header overrides are IGNORED for DB-backed
keys, closing the cross-project billing leak that the legacy static-key
path allowed.

The secret returned by 'create' and 'rotate' is shown ONCE on stdout.
Capture it now; the daemon stores only a sha256 hash and cannot recover
the raw key later.`,
}

var (
	keyProject  string
	keyName     string
	keyExpires  string
	keyJSONFlag bool
)

var keyCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Mint a new API key for a project",
	Long:  "Generate a new sk-vornik-<project>.<random> token. The raw secret is printed ONCE.",
	RunE:  runKeyCreate,
}

var keyListCmd = &cobra.Command{
	Use:   "list",
	Short: "List API keys for a project (no secrets returned)",
	RunE:  runKeyList,
}

var keyRotateCmd = &cobra.Command{
	Use:   "rotate <keyID>",
	Short: "Mint a new key with the same name + expiry; revoke the old",
	Args:  cobra.ExactArgs(1),
	RunE:  runKeyRotate,
}

var keyRevokeCmd = &cobra.Command{
	Use:   "revoke <keyID>",
	Short: "Soft-delete an API key (idempotent)",
	Args:  cobra.ExactArgs(1),
	RunE:  runKeyRevoke,
}

var (
	keyUpdateAddWorkflows    []string
	keyUpdateRemoveWorkflows []string
	keyUpdateSetWorkflows    []string
	keyUpdateAllowPush       bool
	keyUpdateDisallowPush    bool
)

var keyUpdateCmd = &cobra.Command{
	Use:   "update <keyID>",
	Short: "Update an existing key's allowed_workflows list or push capability",
	Long: `Add, remove, or replace workflows on a key's allowed_workflows
list without minting a new secret. Three modes, mutually exclusive
per invocation:

  --add-workflow X[,Y]      append X (and Y) to the current list
  --remove-workflow X[,Y]   drop X (and Y) from the current list
  --set-workflows X,Y,Z     replace the list wholesale; pass '' to
                            mean "every workflow the project permits"

To flip git-push access on a key use one of the mutually exclusive flags:

  --allow-push              grant git-push access over HTTPS
  --disallow-push           revoke git-push access

Add / remove modes fetch the current list, mutate, and PUT the
result — last writer wins on concurrent edits. Set mode is the
raw PUT.

Examples:
  vornikctl key update -p my-project --add-workflow=rag-ingest <keyID>
  vornikctl key update -p my-project --remove-workflow=doc-review <keyID>
  vornikctl key update -p my-project --set-workflows=research-gather,rag-ingest <keyID>
  vornikctl key update -p my-project --set-workflows='' <keyID>   # = every project workflow
  vornikctl key update -p my-project --allow-push <keyID>
  vornikctl key update -p my-project --disallow-push <keyID>`,
	Args: cobra.ExactArgs(1),
	RunE: runKeyUpdate,
}

func init() {
	keyCreateCmd.Flags().StringVarP(&keyProject, "project", "p", "", "Project ID (required)")
	keyCreateCmd.Flags().StringVar(&keyName, "name", "", "Operator-friendly label (required)")
	keyCreateCmd.Flags().StringVar(&keyExpires, "expires", "",
		"Expiration (RFC3339 or duration like 30d, 6m, 1y). Empty = never.")
	keyCreateCmd.Flags().BoolVar(&keyJSONFlag, "json", false, "Emit JSON instead of human text")
	_ = keyCreateCmd.MarkFlagRequired("project")
	_ = keyCreateCmd.MarkFlagRequired("name")

	keyListCmd.Flags().StringVarP(&keyProject, "project", "p", "", "Project ID (required)")
	keyListCmd.Flags().BoolVar(&keyJSONFlag, "json", false, "Emit JSON instead of a table")
	_ = keyListCmd.MarkFlagRequired("project")

	keyRotateCmd.Flags().StringVarP(&keyProject, "project", "p", "", "Project ID (required)")
	keyRotateCmd.Flags().BoolVar(&keyJSONFlag, "json", false, "Emit JSON instead of human text")
	_ = keyRotateCmd.MarkFlagRequired("project")

	keyRevokeCmd.Flags().StringVarP(&keyProject, "project", "p", "", "Project ID (required)")
	_ = keyRevokeCmd.MarkFlagRequired("project")

	keyUpdateCmd.Flags().StringVarP(&keyProject, "project", "p", "", "Project ID (required)")
	keyUpdateCmd.Flags().StringSliceVar(&keyUpdateAddWorkflows, "add-workflow", nil,
		"Workflow ID(s) to append to allowed_workflows. Repeatable / comma-separated.")
	keyUpdateCmd.Flags().StringSliceVar(&keyUpdateRemoveWorkflows, "remove-workflow", nil,
		"Workflow ID(s) to drop from allowed_workflows. Repeatable / comma-separated.")
	keyUpdateCmd.Flags().StringSliceVar(&keyUpdateSetWorkflows, "set-workflows", nil,
		"Workflow ID(s) replacing allowed_workflows. Empty value (--set-workflows='') clears the list = every project workflow.")
	keyUpdateCmd.Flags().BoolVar(&keyUpdateAllowPush, "allow-push", false,
		"Grant git-push access over HTTPS to this key.")
	keyUpdateCmd.Flags().BoolVar(&keyUpdateDisallowPush, "disallow-push", false,
		"Revoke git-push access over HTTPS from this key.")
	keyUpdateCmd.Flags().BoolVar(&keyJSONFlag, "json", false, "Emit JSON instead of human text")
	_ = keyUpdateCmd.MarkFlagRequired("project")

	keyCmd.AddCommand(keyCreateCmd, keyListCmd, keyRotateCmd, keyRevokeCmd, keyUpdateCmd)
	rootCmd.AddCommand(keyCmd)
}

func runKeyCreate(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	body := map[string]any{"name": keyName}
	if keyExpires != "" {
		ts, err := parseExpiry(keyExpires)
		if err != nil {
			return fmt.Errorf("invalid --expires: %w", err)
		}
		body["expires_at"] = ts.UTC().Format(time.RFC3339)
	}
	resp, err := client.Post(
		fmt.Sprintf("/api/v1/projects/%s/keys", keyProject), body)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var out keyCreatedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if keyJSONFlag {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	// Human-readable. The secret is what the operator NEEDS to
	// capture; print it on its own line with a clear warning so
	// it can't be missed in a scroll-back.
	fmt.Printf("Created API key for project %q:\n\n", out.ProjectID)
	fmt.Printf("  id:         %s\n", out.ID)
	fmt.Printf("  name:       %s\n", out.Name)
	fmt.Printf("  prefix:     %s\n", out.KeyPrefix)
	fmt.Printf("  created_at: %s\n", out.CreatedAt)
	if out.ExpiresAt != nil {
		fmt.Printf("  expires_at: %s\n", *out.ExpiresAt)
	}
	fmt.Println()
	fmt.Println("Secret (shown ONCE — copy it now):")
	fmt.Println()
	fmt.Println("  " + out.Secret)
	fmt.Println()
	fmt.Println("Pass this as Authorization: Bearer <secret> or X-API-Key: <secret>.")
	return nil
}

func runKeyList(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	resp, err := client.Get(fmt.Sprintf("/api/v1/projects/%s/keys", keyProject))
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	var out keyListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if keyJSONFlag {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if len(out.Keys) == 0 {
		fmt.Printf("No keys for project %q.\n", keyProject)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tNAME\tPREFIX\tCREATED\tLAST USED\tEXPIRES\tSTATUS\tPUSH")
	for _, k := range out.Keys {
		status := "active"
		if k.RevokedAt != nil && *k.RevokedAt != "" {
			status = "revoked"
		}
		lastUsed := "—"
		if k.LastUsedAt != nil && *k.LastUsedAt != "" {
			lastUsed = *k.LastUsedAt
		}
		expires := "—"
		if k.ExpiresAt != nil && *k.ExpiresAt != "" {
			expires = *k.ExpiresAt
		}
		push := "no"
		if k.AllowPush {
			push = "yes"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			k.ID, k.Name, k.KeyPrefix, k.CreatedAt, lastUsed, expires, status, push)
	}
	return w.Flush()
}

func runKeyRotate(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	resp, err := client.Post(
		fmt.Sprintf("/api/v1/projects/%s/keys/%s/rotate", keyProject, args[0]),
		nil,
	)
	if err != nil {
		return fmt.Errorf("rotate: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var out keyCreatedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if keyJSONFlag {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	fmt.Printf("Rotated key (old revoked). New key for project %q:\n\n", out.ProjectID)
	fmt.Printf("  id:     %s\n", out.ID)
	fmt.Printf("  name:   %s\n", out.Name)
	fmt.Printf("  prefix: %s\n\n", out.KeyPrefix)
	fmt.Println("Secret (shown ONCE — copy it now):")
	fmt.Println()
	fmt.Println("  " + out.Secret)
	return nil
}

// keyWithWorkflowsResponse mirrors the shape returned by
// PUT /api/v1/projects/{id}/keys/{kid}/workflows — apiKeyListEntry +
// allowed_workflows. Decoupled from keyListEntry so the CLI doesn't
// have to share the wire type with the daemon's internal package.
type keyWithWorkflowsResponse struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	KeyPrefix        string   `json:"key_prefix"`
	CreatedAt        string   `json:"created_at"`
	LastUsedAt       *string  `json:"last_used_at,omitempty"`
	ExpiresAt        *string  `json:"expires_at,omitempty"`
	RevokedAt        *string  `json:"revoked_at,omitempty"`
	CreatedBy        string   `json:"created_by,omitempty"`
	AllowedWorkflows []string `json:"allowed_workflows"`
}

// keyWithAllowPushResponse mirrors the shape returned by
// PUT /api/v1/projects/{id}/keys/{kid}/allow-push — apiKeyListEntry
// with AllowPush reflected. Decoupled from keyListEntry for the same
// reason as keyWithWorkflowsResponse.
type keyWithAllowPushResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by,omitempty"`
	AllowPush bool   `json:"allow_push,omitempty"`
}

// fetchKeyAllowedWorkflows reads the current allowed_workflows slice
// for a single key from the daemon's list endpoint. Necessary for
// the add / remove modes: the daemon's PUT replaces wholesale, so
// the CLI mutates client-side. ListAPIKeys surfaces allowed_workflows
// per row (apiKeyListEntry.AllowedWorkflows, omitempty for
// non-companion keys); the GET → mutate → PUT path is a small race
// window — last writer wins on concurrent edits, which is the same
// guarantee the daemon's UPDATE gives.
func fetchKeyAllowedWorkflows(project, keyID string) ([]string, error) {
	client := ClientFromEnv()
	resp, err := client.Get(fmt.Sprintf("/api/v1/projects/%s/keys", project))
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, ParseAPIError(resp)
	}
	// The list endpoint returns apiKeyListEntry shapes without
	// allowed_workflows. To read the current slice we round-trip
	// through the workflows PUT with the body the daemon currently
	// stores — but we don't know that body yet. Compromise: when
	// the operator uses --add / --remove and the list response
	// doesn't include allowed_workflows, ask the operator to use
	// --set-workflows instead. This is a one-time UX paper cut
	// until the list endpoint grows allowed_workflows (small
	// follow-on; tracked alongside this CLI surface).
	var listResp keyListResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return nil, fmt.Errorf("decode list response: %w", err)
	}
	for _, k := range listResp.Keys {
		if k.ID == keyID {
			return k.AllowedWorkflows, nil
		}
	}
	return nil, fmt.Errorf("key %s not found in project %s", keyID, project)
}

func runKeyUpdate(cmd *cobra.Command, args []string) error {
	keyID := args[0]
	addFlag := cmd.Flags().Changed("add-workflow")
	removeFlag := cmd.Flags().Changed("remove-workflow")
	setFlag := cmd.Flags().Changed("set-workflows")
	// For allow-push / disallow-push use the variable values, not Changed().
	// Bool flags default false; the user can only activate them by passing
	// the flag (setting the value to true). Changed() would also be true
	// after a test-cleanup Set("flag","false"), making it unreliable in
	// testing. Variable values are the authoritative signal.
	allowPushFlag := keyUpdateAllowPush
	disallowPushFlag := keyUpdateDisallowPush

	// allow-push / disallow-push are mutually exclusive with each
	// other and with all workflow flags.
	if allowPushFlag && disallowPushFlag {
		return fmt.Errorf("--allow-push and --disallow-push are mutually exclusive")
	}
	pushMode := allowPushFlag || disallowPushFlag

	// Push mode and workflow edits are exclusive: the push branch returns before
	// reaching the workflow logic, so combining them would silently drop the
	// workflow edit. Reject up front (before any HTTP) and tell the user to run
	// separate commands. (Final-review FIX 2.)
	if pushMode && (addFlag || removeFlag || setFlag) {
		return fmt.Errorf("--allow-push/--disallow-push cannot be combined with workflow flags; run separate `key update` commands")
	}

	if pushMode {
		// Push-capability update path: one PUT to the allow-push endpoint.
		allow := allowPushFlag
		client := ClientFromEnv()
		resp, err := client.Do(
			http.MethodPut,
			fmt.Sprintf("/api/v1/projects/%s/keys/%s/allow-push", keyProject, keyID),
			map[string]any{"allow_push": allow},
		)
		if err != nil {
			return fmt.Errorf("update allow-push: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return ParseAPIError(resp)
		}
		var out keyWithAllowPushResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		if keyJSONFlag {
			return json.NewEncoder(os.Stdout).Encode(out)
		}
		pushStr := "disabled"
		if out.AllowPush {
			pushStr = "enabled"
		}
		fmt.Printf("Updated key %s. allow_push: %s\n", out.ID, pushStr)
		return nil
	}

	switch {
	case !addFlag && !removeFlag && !setFlag:
		return fmt.Errorf("specify one of --add-workflow, --remove-workflow, --set-workflows, --allow-push, or --disallow-push")
	case (addFlag || removeFlag) && setFlag:
		return fmt.Errorf("--set-workflows is exclusive with --add-workflow / --remove-workflow")
	}

	var target []string
	if setFlag {
		// Set mode is the raw PUT. Empty slice means "all
		// project workflows"; we send [] explicitly so the
		// handler stores the neutral form.
		target = keyUpdateSetWorkflows
	} else {
		// Add / remove mode: read current list, mutate, PUT.
		// Last writer wins on concurrent edits — operators
		// running parallel updates should serialise.
		current, err := fetchKeyAllowedWorkflows(keyProject, keyID)
		if err != nil {
			return err
		}
		// Build a set for O(n) mutation; preserve insertion order
		// on the resulting slice so the operator's view of the
		// list stays predictable.
		set := make(map[string]struct{}, len(current)+len(keyUpdateAddWorkflows))
		ordered := make([]string, 0, len(current)+len(keyUpdateAddWorkflows))
		for _, wf := range current {
			if _, ok := set[wf]; ok {
				continue
			}
			set[wf] = struct{}{}
			ordered = append(ordered, wf)
		}
		if addFlag {
			for _, wf := range keyUpdateAddWorkflows {
				wf = strings.TrimSpace(wf)
				if wf == "" {
					continue
				}
				if _, ok := set[wf]; ok {
					continue
				}
				set[wf] = struct{}{}
				ordered = append(ordered, wf)
			}
		}
		if removeFlag {
			drop := make(map[string]struct{}, len(keyUpdateRemoveWorkflows))
			for _, wf := range keyUpdateRemoveWorkflows {
				drop[strings.TrimSpace(wf)] = struct{}{}
			}
			filtered := make([]string, 0, len(ordered))
			for _, wf := range ordered {
				if _, ok := drop[wf]; ok {
					continue
				}
				filtered = append(filtered, wf)
			}
			ordered = filtered
		}
		target = ordered
	}

	// Client.Do marshals the body itself; pass the typed value, not
	// a pre-marshalled io.Reader. (The Reader gets json.Marshal'd
	// to {} which the handler treats as a missing list — every
	// PUT silently cleared the column.)
	client := ClientFromEnv()
	resp, err := client.Do(
		http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/keys/%s/workflows", keyProject, keyID),
		map[string]any{"allowed_workflows": target},
	)
	if err != nil {
		return fmt.Errorf("update workflows: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	var out keyWithWorkflowsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if keyJSONFlag {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	fmt.Printf("Updated key %s. allowed_workflows now:\n", out.ID)
	if len(out.AllowedWorkflows) == 0 {
		fmt.Println("  (empty — every workflow the project permits)")
	} else {
		for _, wf := range out.AllowedWorkflows {
			fmt.Println("  - " + wf)
		}
	}
	return nil
}

func runKeyRevoke(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	resp, err := client.Do(http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/keys/%s", keyProject, args[0]),
		nil,
	)
	if err != nil {
		return fmt.Errorf("revoke: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return ParseAPIError(resp)
	}
	fmt.Printf("Revoked key %s.\n", args[0])
	return nil
}

// parseExpiry accepts either RFC3339 ("2026-12-31T00:00:00Z") or a
// shorthand duration suffix: 30d / 6m / 1y. The shorthand resolves
// relative to wall-clock now, which is what a human typing
// "give me 30 days" actually wants.
func parseExpiry(in string) (time.Time, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return time.Time{}, fmt.Errorf("empty")
	}
	if ts, err := time.Parse(time.RFC3339, in); err == nil {
		return ts, nil
	}
	// Shorthand: 30d / 6m / 1y. Numbers + a single-char unit.
	// strconv.Atoi (rather than Sscanf) is intentional — Sscanf
	// would happily eat internal whitespace, accepting "30 d" as
	// 30. Atoi rejects anything that isn't strictly digit chars.
	if len(in) < 2 {
		return time.Time{}, fmt.Errorf("unrecognised expiry %q", in)
	}
	unit := in[len(in)-1]
	nStr := in[:len(in)-1]
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 {
		return time.Time{}, fmt.Errorf("unrecognised expiry %q", in)
	}
	now := time.Now().UTC()
	switch unit {
	case 'd':
		return now.AddDate(0, 0, n), nil
	case 'm':
		return now.AddDate(0, n, 0), nil
	case 'y':
		return now.AddDate(n, 0, 0), nil
	default:
		return time.Time{}, fmt.Errorf("unrecognised unit %q (use d/m/y or RFC3339)", string(unit))
	}
}
