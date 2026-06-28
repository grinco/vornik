package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// `vornikctl companion {grant,keys}` — operator surface for the
// vornik-companion plugin contract (LLD 21). Mints + lists per-session
// bearer keys scoped to one project, a workflow allowlist, and a
// budget cap. Revocation reuses `vornikctl key revoke <id> --project=X`
// since a companion key is just an api_keys row with extra columns.
//
// The init wizard (`vornikctl companion init`) is intentionally deferred
// — it combines template instantiation with key minting, and lives
// next to the existing init-project flow. Phase-1 ships grant + list
// only; operators stitch them together via the existing
// `vornikctl init project --template companion` command followed by
// `vornikctl companion grant`.

type companionGrantOutput struct {
	ID               string     `json:"id"`
	ProjectID        string     `json:"projectId"`
	SessionLabel     string     `json:"sessionLabel,omitempty"`
	ClientKind       string     `json:"clientKind"`
	Secret           string     `json:"secret"`
	KeyPrefix        string     `json:"keyPrefix"`
	AllowedWorkflows []string   `json:"allowedWorkflows,omitempty"`
	BudgetCapUSD     *float64   `json:"budgetCapUsd,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
	MemoryRead       bool       `json:"memoryRead,omitempty"`
	MemoryWrite      bool       `json:"memoryWrite,omitempty"`
	DefaultRepoScope string     `json:"defaultRepoScope,omitempty"`
}

type companionKeyOutput struct {
	ID               string     `json:"id"`
	ProjectID        string     `json:"projectId"`
	SessionLabel     string     `json:"sessionLabel,omitempty"`
	ClientKind       string     `json:"clientKind"`
	KeyPrefix        string     `json:"keyPrefix"`
	AllowedWorkflows []string   `json:"allowedWorkflows,omitempty"`
	BudgetCapUSD     *float64   `json:"budgetCapUsd,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	LastUsedAt       *time.Time `json:"lastUsedAt,omitempty"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
	RevokedAt        *time.Time `json:"revokedAt,omitempty"`
	CreatedBy        string     `json:"createdBy,omitempty"`
}

type companionKeyListOutput struct {
	Keys []companionKeyOutput `json:"keys"`
}

var companionCmd = &cobra.Command{
	Use:   "companion",
	Short: "Manage vornik-companion plugin sessions (LLD 21)",
	Long: `Mint and list per-session bearer keys for host-LLM clients
(Claude Code today; Codex / Gemini CLI / opencode tomorrow).

A companion key is bound to ONE project, optionally narrowed to a
workflow allowlist and a USD budget cap. The host plugin presents the
key as a normal sk-vornik-* bearer; the daemon's auth layer reads the
scope columns on every call.

Revocation: use 'vornikctl key revoke <id> --project=X'. There is no
separate companion-revoke — the underlying api_keys row revokes the
same way regardless of how it was minted.`,
}

var (
	companionGrantProject      string
	companionGrantClient       string
	companionGrantLabel        string
	companionGrantWorkflowsCSV string
	companionGrantBudgetStr    string
	companionGrantExpires      string
	companionGrantRepoScope    string
	companionGrantMemoryRead   bool
	companionGrantMemoryWrite  bool
	companionGrantMemoryAll    bool
	companionGrantJSON         bool

	companionKeysProject string
	companionKeysJSON    bool
)

var companionGrantCmd = &cobra.Command{
	Use:   "grant",
	Short: "Mint a new companion key (raw secret shown ONCE)",
	Long: `Mint a per-session bearer key for a companion project. The raw
secret is printed exactly once — capture it and feed it to your host
LLM client's plugin store. The daemon stores only the sha256 hash.`,
	RunE: runCompanionGrant,
}

var companionKeysCmd = &cobra.Command{
	Use:   "keys",
	Short: "List companion keys for a project (no secrets shown)",
	RunE:  runCompanionKeysList,
}

func init() {
	companionGrantCmd.Flags().StringVarP(&companionGrantProject, "project", "p", "", "Project ID (required) — typically companion-<user>")
	companionGrantCmd.Flags().StringVar(&companionGrantClient, "client", "", "Host LLM client: claude-code | codex | gemini-cli | opencode (required)")
	companionGrantCmd.Flags().StringVar(&companionGrantLabel, "label", "", "Operator-friendly session label (e.g. 'vadim/laptop')")
	companionGrantCmd.Flags().StringVar(&companionGrantWorkflowsCSV, "workflows", "",
		"Comma-separated workflow allowlist (omit for 'all project workflows')")
	companionGrantCmd.Flags().StringVar(&companionGrantBudgetStr, "budget-usd", "",
		"Lifetime USD budget cap for this key (omit for uncapped)")
	companionGrantCmd.Flags().StringVar(&companionGrantExpires, "expires", "",
		"Expiration (RFC3339 or duration like 30d, 6m). Empty = never expires.")
	companionGrantCmd.Flags().StringVar(&companionGrantRepoScope, "repo-scope", "",
		"Default repo_scope stamped on memory calls that omit it (e.g. github.com/grinco/vornik). "+
			"Recommended for clients without a SessionStart scope injector (Codex) so deposits can't land NULL-scoped. "+
			"An explicit per-call repo_scope still overrides it.")
	// LLD 22: companion RAG capabilities. Off by default — existing
	// scripts that don't pass these flags continue to mint keys that
	// can delegate but can't reach memory.
	companionGrantCmd.Flags().BoolVar(&companionGrantMemoryRead, "memory-read", false,
		"Allow this key to call recall() against the project's RAG store.")
	companionGrantCmd.Flags().BoolVar(&companionGrantMemoryWrite, "memory-write", false,
		"Allow this key to call remember() to deposit notes into the project's RAG store. Implies --memory-read.")
	companionGrantCmd.Flags().BoolVar(&companionGrantMemoryAll, "memory-all", false,
		"Shorthand for --memory-read --memory-write.")
	companionGrantCmd.Flags().BoolVar(&companionGrantJSON, "json", false, "Emit JSON instead of human text")
	_ = companionGrantCmd.MarkFlagRequired("project")
	_ = companionGrantCmd.MarkFlagRequired("client")

	companionKeysCmd.Flags().StringVarP(&companionKeysProject, "project", "p", "", "Project ID (required)")
	companionKeysCmd.Flags().BoolVar(&companionKeysJSON, "json", false, "Emit JSON instead of a table")
	_ = companionKeysCmd.MarkFlagRequired("project")

	companionCmd.AddCommand(companionGrantCmd, companionKeysCmd)
	rootCmd.AddCommand(companionCmd)
}

func runCompanionGrant(cmd *cobra.Command, args []string) error {
	body := map[string]any{
		"projectId":  companionGrantProject,
		"clientKind": companionGrantClient,
	}
	if companionGrantLabel != "" {
		body["sessionLabel"] = companionGrantLabel
	}
	if csv := strings.TrimSpace(companionGrantWorkflowsCSV); csv != "" {
		parts := strings.Split(csv, ",")
		// Trim each so "wf-a, wf-b" works as well as "wf-a,wf-b".
		// Empty entries (from a stray trailing comma) are dropped
		// here — the server-side validator would reject them with
		// VALIDATION_ERROR but the CLI cleaning is more
		// operator-friendly.
		clean := make([]string, 0, len(parts))
		for _, p := range parts {
			if t := strings.TrimSpace(p); t != "" {
				clean = append(clean, t)
			}
		}
		if len(clean) == 0 {
			return fmt.Errorf("invalid --workflows: only commas / whitespace")
		}
		body["allowedWorkflows"] = clean
	}
	if companionGrantBudgetStr != "" {
		v, err := strconv.ParseFloat(companionGrantBudgetStr, 64)
		if err != nil {
			return fmt.Errorf("invalid --budget-usd %q: %w", companionGrantBudgetStr, err)
		}
		body["budgetCapUsd"] = v
	}
	if companionGrantExpires != "" {
		ts, err := parseExpiry(companionGrantExpires)
		if err != nil {
			return fmt.Errorf("invalid --expires: %w", err)
		}
		body["expiresAt"] = ts.UTC().Format(time.RFC3339)
	}
	if rs := strings.TrimSpace(companionGrantRepoScope); rs != "" {
		body["defaultRepoScope"] = rs
	}
	memRead := companionGrantMemoryRead || companionGrantMemoryAll || companionGrantMemoryWrite // write implies read
	memWrite := companionGrantMemoryWrite || companionGrantMemoryAll
	if memRead {
		body["memoryRead"] = true
	}
	if memWrite {
		body["memoryWrite"] = true
	}

	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/admin/companion/grant", body)
	if err != nil {
		return fmt.Errorf("grant: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()

	var out companionGrantOutput
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if companionGrantJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}

	fmt.Printf("Minted companion key for project %q (client=%s):\n\n", out.ProjectID, out.ClientKind)
	fmt.Printf("  id:         %s\n", out.ID)
	if out.SessionLabel != "" {
		fmt.Printf("  label:      %s\n", out.SessionLabel)
	}
	fmt.Printf("  prefix:     %s\n", out.KeyPrefix)
	if len(out.AllowedWorkflows) > 0 {
		fmt.Printf("  workflows:  %s\n", strings.Join(out.AllowedWorkflows, ", "))
	} else {
		fmt.Printf("  workflows:  (all project workflows)\n")
	}
	if out.BudgetCapUSD != nil {
		fmt.Printf("  budget:     $%.2f USD\n", *out.BudgetCapUSD)
	} else {
		fmt.Printf("  budget:     uncapped\n")
	}
	// LLD 22 capabilities: only print when granted, to keep the
	// "delegate-only" key's output unchanged from pre-LLD-22 days.
	switch {
	case out.MemoryRead && out.MemoryWrite:
		fmt.Printf("  memory:     read + write\n")
	case out.MemoryRead:
		fmt.Printf("  memory:     read only\n")
	}
	if out.DefaultRepoScope != "" {
		fmt.Printf("  repo_scope: %s (default; stamped on memory calls that omit repo_scope)\n", out.DefaultRepoScope)
	}
	fmt.Printf("  created_at: %s\n", out.CreatedAt.UTC().Format(time.RFC3339))
	if out.ExpiresAt != nil {
		fmt.Printf("  expires_at: %s\n", out.ExpiresAt.UTC().Format(time.RFC3339))
	}
	fmt.Println()
	fmt.Println("Secret (shown ONCE — copy it now and paste it into your plugin's .mcp.json):")
	fmt.Println()
	fmt.Println("  " + out.Secret)
	fmt.Println()
	fmt.Println("Auth header: Authorization: Bearer <secret>")
	return nil
}

func runCompanionKeysList(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	resp, err := client.Get("/api/v1/admin/companion/keys?projectId=" + companionKeysProject)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()

	var out companionKeyListOutput
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if companionKeysJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}

	if len(out.Keys) == 0 {
		fmt.Printf("No companion keys for project %q.\n", companionKeysProject)
		fmt.Println("Mint one with:  vornikctl companion grant --project=" + companionKeysProject + " --client=claude-code")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tCLIENT\tLABEL\tPREFIX\tWORKFLOWS\tBUDGET\tCREATED\tSTATUS")
	for _, k := range out.Keys {
		wfs := "(all)"
		if len(k.AllowedWorkflows) > 0 {
			wfs = strings.Join(k.AllowedWorkflows, ",")
			if len(wfs) > 40 {
				wfs = wfs[:37] + "..."
			}
		}
		budget := "uncapped"
		if k.BudgetCapUSD != nil {
			budget = fmt.Sprintf("$%.2f", *k.BudgetCapUSD)
		}
		status := "active"
		if k.RevokedAt != nil {
			status = "revoked"
		} else if k.ExpiresAt != nil && k.ExpiresAt.Before(time.Now()) {
			status = "expired"
		}
		label := k.SessionLabel
		if label == "" {
			label = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			k.ID, k.ClientKind, label, k.KeyPrefix, wfs, budget,
			k.CreatedAt.UTC().Format("2006-01-02"), status)
	}
	return tw.Flush()
}
