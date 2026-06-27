package cli

// Memory Firewall CLI — operator surface for the Policy-Aware
// Memory Firewall (LLD:
// https://docs.vornik.io).
//
//   vornikctl memory firewall mode                                — show daemon mode
//   vornikctl memory firewall evaluations --project P [--decision D] [--since DATE] [--limit N] [--json]
//
// Admin-scoped (server-side gate enforces). v1 scope mirrors
// the Phase C REST surface: two read-only verbs to surface the
// audit data + current mode. Per-chunk policy edits land
// alongside the Phase D follow-on YAML config.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	memoryFirewallCmd = &cobra.Command{
		Use:   "firewall",
		Short: "Policy-Aware Memory Firewall — inspect evaluations + mode",
		Long: `Read-only operator surface for the Policy-Aware Memory Firewall.

The firewall attaches policy metadata (provenance / sensitivity /
expiry / tenant / role / purpose) to every memory chunk and
emits an audit row per retrieval decision. These verbs surface
the audit trail + the daemon's current enforcement mode.

Requires an admin-scoped API key.`,
	}

	memoryFirewallModeCmd = &cobra.Command{
		Use:   "mode",
		Short: "Print the daemon's current firewall enforcement mode",
		RunE:  runMemoryFirewallMode,
	}

	memoryFirewallSetPolicyCmd = &cobra.Command{
		Use:   "set-policy <chunk_id>",
		Short: "Mutate per-chunk firewall policy (admin-only)",
		Long: `Edit one chunk's firewall policy. Only the flags you pass are
applied — others stay at their stored values. The policy_digest is
recomputed server-side; the response carries the new value so you
can verify the round-trip.

Examples:

  vornikctl memory firewall set-policy c1 --sensitivity restricted
  vornikctl memory firewall set-policy c1 --permitted-roles coder,analyst
  vornikctl memory firewall set-policy c1 --permitted-roles ''           # clear (deny-all)
  vornikctl memory firewall set-policy c1 --allowed-purposes operational,audit_review
  vornikctl memory firewall set-policy c1 --expires-at 2026-12-31T23:59:59Z
  vornikctl memory firewall set-policy c1 --tenant-id tenant-a`,
		Args: cobra.ExactArgs(1),
		RunE: runMemoryFirewallSetPolicy,
	}

	memoryFirewallEvalsCmd = &cobra.Command{
		Use:   "evaluations",
		Short: "List recent firewall evaluations for a project",
		Long: `Page through memory_policy_evaluations rows, newest first.

Filter by decision class to surface a specific compliance pattern:

  vornikctl memory firewall evaluations --project p1 --decision block_role_not_permitted
  vornikctl memory firewall evaluations --project p1 --decision block_expired --since 2026-05-01
  vornikctl memory firewall evaluations --project p1 --decision allow --limit 200`,
		RunE: runMemoryFirewallEvals,
	}

	mfEvalsProject  string
	mfEvalsDecision string
	mfEvalsSince    string
	mfEvalsLimit    int
	mfEvalsJSON     bool
	mfModeJSON      bool

	mfSetPolicySensitivity     string
	mfSetPolicyTenantID        string
	mfSetPolicyExpiresAt       string
	mfSetPolicyPermittedRoles  string
	mfSetPolicyAllowedPurposes string
	mfSetPolicyJSON            bool
	// Sentinels to distinguish "flag not set" from "set to empty"
	// since cobra doesn't expose IsSet at the flag value level
	// for string flags. Set by the PreRunE hook.
	mfSetPolicySensitivitySet     bool
	mfSetPolicyTenantIDSet        bool
	mfSetPolicyExpiresAtSet       bool
	mfSetPolicyPermittedRolesSet  bool
	mfSetPolicyAllowedPurposesSet bool

	mfEvalsCSV bool
)

func init() {
	memoryFirewallModeCmd.Flags().BoolVar(&mfModeJSON, "json", false, "Output JSON instead of human-readable")

	memoryFirewallEvalsCmd.Flags().StringVar(&mfEvalsProject, "project", "", "Project ID (required)")
	memoryFirewallEvalsCmd.Flags().StringVar(&mfEvalsDecision, "decision", "", "Filter by decision class (allow | block_expired | block_tenant_mismatch | block_role_not_permitted | block_purpose_not_allowed | block_sensitivity_tier)")
	memoryFirewallEvalsCmd.Flags().StringVar(&mfEvalsSince, "since", "", "Lower bound timestamp (YYYY-MM-DD or RFC3339); default last 7 days")
	memoryFirewallEvalsCmd.Flags().IntVarP(&mfEvalsLimit, "limit", "n", 50, "Maximum rows (1-500)")
	memoryFirewallEvalsCmd.Flags().BoolVar(&mfEvalsJSON, "json", false, "Output JSON instead of table")
	memoryFirewallEvalsCmd.Flags().BoolVar(&mfEvalsCSV, "csv", false, "Stream RFC 4180 CSV instead of table (default 30-day window for compliance exports)")
	_ = memoryFirewallEvalsCmd.MarkFlagRequired("project")

	memoryFirewallSetPolicyCmd.Flags().StringVar(&mfSetPolicySensitivity, "sensitivity", "", "Sensitivity tier (public|internal|confidential|restricted)")
	memoryFirewallSetPolicyCmd.Flags().StringVar(&mfSetPolicyTenantID, "tenant-id", "", "Tenant ID (empty = clear)")
	memoryFirewallSetPolicyCmd.Flags().StringVar(&mfSetPolicyExpiresAt, "expires-at", "", "RFC3339 timestamp; empty = clear")
	memoryFirewallSetPolicyCmd.Flags().StringVar(&mfSetPolicyPermittedRoles, "permitted-roles", "", "Comma-separated list of allowed roles; empty = deny-all")
	memoryFirewallSetPolicyCmd.Flags().StringVar(&mfSetPolicyAllowedPurposes, "allowed-purposes", "", "Comma-separated list (operational|training_data|audit_review|compliance_export); empty = deny-all")
	memoryFirewallSetPolicyCmd.Flags().BoolVar(&mfSetPolicyJSON, "json", false, "Output JSON instead of human-readable")
	// PreRun snapshots which flags were explicitly set so the
	// handler can distinguish "leave untouched" from "set to empty".
	memoryFirewallSetPolicyCmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		mfSetPolicySensitivitySet = cmd.Flags().Changed("sensitivity")
		mfSetPolicyTenantIDSet = cmd.Flags().Changed("tenant-id")
		mfSetPolicyExpiresAtSet = cmd.Flags().Changed("expires-at")
		mfSetPolicyPermittedRolesSet = cmd.Flags().Changed("permitted-roles")
		mfSetPolicyAllowedPurposesSet = cmd.Flags().Changed("allowed-purposes")
		return nil
	}

	memoryFirewallCmd.AddCommand(memoryFirewallModeCmd)
	memoryFirewallCmd.AddCommand(memoryFirewallSetPolicyCmd)
	memoryFirewallCmd.AddCommand(memoryFirewallEvalsCmd)
	// Wire under the existing `memory` parent if present;
	// otherwise top-level. The memory parent lands in
	// memory_audit.go which uses init() too — to avoid cross-
	// init ordering issues we register top-level here and the
	// memory_audit.go side will pick this up via the existing
	// `memory` command structure if it exists.
	if memoryCmd != nil {
		memoryCmd.AddCommand(memoryFirewallCmd)
	} else {
		rootCmd.AddCommand(memoryFirewallCmd)
	}
}

type firewallModeResp struct {
	Mode              string            `json:"mode"`
	DescriptionByMode map[string]string `json:"description_by_mode"`
	Note              string            `json:"note"`
}

type firewallEvalsResp struct {
	Evaluations []firewallEvalRow `json:"evaluations"`
	Count       int               `json:"count"`
	Filters     map[string]any    `json:"filters"`
}

type firewallEvalRow struct {
	ID              string `json:"ID"`
	ProjectID       string `json:"ProjectID"`
	TenantID        string `json:"TenantID"`
	ChunkID         string `json:"ChunkID"`
	RequestRole     string `json:"RequestRole"`
	RequestPurpose  string `json:"RequestPurpose"`
	RequestOperator string `json:"RequestOperator"`
	TraceID         string `json:"TraceID"`
	Decision        string `json:"Decision"`
	PolicyDigest    string `json:"PolicyDigest"`
	ReasonDetail    string `json:"ReasonDetail"`
	EvaluatedAt     string `json:"EvaluatedAt"`
}

func runMemoryFirewallMode(_ *cobra.Command, _ []string) error {
	client := ClientFromEnv()
	resp, err := client.Get("/api/v1/admin/memory/policy/mode")
	if err != nil {
		return fmt.Errorf("memory firewall mode: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	if mfModeJSON {
		_, err := os.Stdout.ReadFrom(resp.Body)
		return err
	}
	var m firewallModeResp
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return fmt.Errorf("memory firewall mode: decode: %w", err)
	}
	fmt.Printf("Daemon firewall mode: %s\n\n", m.Mode)
	if desc, ok := m.DescriptionByMode[m.Mode]; ok {
		fmt.Printf("  %s\n\n", desc)
	}
	if m.Note != "" {
		fmt.Printf("Note: %s\n", m.Note)
	}
	return nil
}

func runMemoryFirewallEvals(_ *cobra.Command, _ []string) error {
	q := url.Values{}
	q.Set("project_id", mfEvalsProject)
	if mfEvalsDecision != "" {
		q.Set("decision", mfEvalsDecision)
	}
	if mfEvalsSince != "" {
		q.Set("since", mfEvalsSince)
	}
	if mfEvalsLimit > 0 {
		q.Set("limit", strconv.Itoa(mfEvalsLimit))
	}
	// CSV path hits the .csv endpoint variant (default 30-day
	// window vs 7 for JSON). Body streamed verbatim so the
	// RFC-4180 line endings + escaping survive shell
	// redirection (vornikctl ... --csv > compliance.csv).
	base := "/api/v1/admin/memory/policy/evaluations"
	if mfEvalsCSV {
		base += ".csv"
	}
	path := base + "?" + q.Encode()
	client := ClientFromEnv()
	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("memory firewall evaluations: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	if mfEvalsCSV {
		_, err := os.Stdout.ReadFrom(resp.Body)
		return err
	}
	if mfEvalsJSON {
		_, err := os.Stdout.ReadFrom(resp.Body)
		return err
	}
	var e firewallEvalsResp
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		return fmt.Errorf("memory firewall evaluations: decode: %w", err)
	}
	if e.Count == 0 {
		fmt.Println("No evaluations found for the supplied filter.")
		return nil
	}
	fmt.Printf("Recent firewall evaluations (%d row%s):\n\n", e.Count, plural(e.Count))
	for _, row := range e.Evaluations {
		fmt.Printf("  %s  %-32s  %s\n", row.EvaluatedAt, row.Decision, row.ChunkID)
		if row.ReasonDetail != "" {
			fmt.Printf("       reason: %s\n", row.ReasonDetail)
		}
		if row.RequestRole != "" || row.RequestPurpose != "" {
			fmt.Printf("       request: role=%q purpose=%q\n", row.RequestRole, row.RequestPurpose)
		}
	}
	return nil
}

// chunkPolicyUpdateRequest mirrors the API's request shape;
// nil pointers = "leave untouched", non-nil = "apply".
type chunkPolicyUpdateRequest struct {
	TenantID          *string    `json:"tenant_id,omitempty"`
	SensitivityTier   *string    `json:"sensitivity_tier,omitempty"`
	FirewallExpiresAt *time.Time `json:"firewall_expires_at,omitempty"`
	PermittedRoles    *[]string  `json:"permitted_roles,omitempty"`
	AllowedPurposes   *[]string  `json:"allowed_purposes,omitempty"`
}

// chunkPolicyUpdateResponse mirrors the API's response shape.
// Only fields the human-readable mode prints are decoded; extras
// are ignored.
type chunkPolicyUpdateResponse struct {
	ChunkID      string `json:"chunk_id"`
	PolicyDigest string `json:"policy_digest"`
	AuditEntry   string `json:"audit_entry_id,omitempty"`
	Policy       struct {
		SensitivityTier   string     `json:"SensitivityTier"`
		TenantID          string     `json:"TenantID"`
		FirewallExpiresAt *time.Time `json:"FirewallExpiresAt"`
		PermittedRoles    []string   `json:"PermittedRoles"`
		AllowedPurposes   []string   `json:"AllowedPurposes"`
	} `json:"policy"`
}

func runMemoryFirewallSetPolicy(_ *cobra.Command, args []string) error {
	chunkID := args[0]
	req := chunkPolicyUpdateRequest{}
	if mfSetPolicySensitivitySet {
		v := mfSetPolicySensitivity
		req.SensitivityTier = &v
	}
	if mfSetPolicyTenantIDSet {
		v := mfSetPolicyTenantID
		req.TenantID = &v
	}
	if mfSetPolicyExpiresAtSet {
		if mfSetPolicyExpiresAt == "" {
			// Empty = clear; encode as zero-value Time pointer
			// → server applies as NULL.
			z := time.Time{}
			req.FirewallExpiresAt = &z
		} else {
			t, err := time.Parse(time.RFC3339, mfSetPolicyExpiresAt)
			if err != nil {
				return fmt.Errorf("--expires-at: must be RFC3339: %w", err)
			}
			req.FirewallExpiresAt = &t
		}
	}
	if mfSetPolicyPermittedRolesSet {
		roles := splitCSVNonEmpty(mfSetPolicyPermittedRoles)
		req.PermittedRoles = &roles
	}
	if mfSetPolicyAllowedPurposesSet {
		purposes := splitCSVNonEmpty(mfSetPolicyAllowedPurposes)
		req.AllowedPurposes = &purposes
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("memory firewall set-policy: marshal: %w", err)
	}
	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/admin/memory/policy/chunks/"+url.PathEscape(chunkID), json.RawMessage(body))
	if err != nil {
		return fmt.Errorf("memory firewall set-policy: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	if mfSetPolicyJSON {
		_, err := os.Stdout.ReadFrom(resp.Body)
		return err
	}
	var out chunkPolicyUpdateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("memory firewall set-policy: decode: %w", err)
	}
	fmt.Printf("Chunk policy updated.\n\n")
	fmt.Printf("  Chunk:        %s\n", out.ChunkID)
	fmt.Printf("  New digest:   %s\n", out.PolicyDigest)
	if out.AuditEntry != "" {
		fmt.Printf("  Audit entry:  %s\n", out.AuditEntry)
	}
	fmt.Printf("\nMerged policy:\n")
	fmt.Printf("  sensitivity:       %s\n", out.Policy.SensitivityTier)
	fmt.Printf("  tenant_id:         %s\n", out.Policy.TenantID)
	if out.Policy.FirewallExpiresAt != nil && !out.Policy.FirewallExpiresAt.IsZero() {
		fmt.Printf("  expires_at:        %s\n", out.Policy.FirewallExpiresAt.Format(time.RFC3339))
	} else {
		fmt.Printf("  expires_at:        (none)\n")
	}
	fmt.Printf("  permitted_roles:   %v\n", out.Policy.PermittedRoles)
	fmt.Printf("  allowed_purposes:  %v\n", out.Policy.AllowedPurposes)
	return nil
}

// splitCSVNonEmpty turns "a,b,c" into []string{"a","b","c"};
// "" returns an empty slice (used to signal "clear the field"
// downstream, since the API distinguishes nil from []).
func splitCSVNonEmpty(s string) []string {
	if s == "" {
		return []string{}
	}
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
