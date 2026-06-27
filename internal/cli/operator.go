package cli

// Operator-profile CLI — surfaces the per-operator memory
// store the dispatcher's update_operator_profile tool writes
// into. Four verbs:
//
//   vornikctl operator list   [--json]
//   vornikctl operator show   <id> [--json]
//   vornikctl operator set    <id> --key K --value V --rationale R
//   vornikctl operator forget <id> [--reason R] --yes
//
// `set` mirrors the dispatcher tool's allow-list + rationale-
// required contract (server-side enforced regardless). `forget`
// requires --yes because privacy revocations are irreversible.
//
// Calls go through ClientFromEnv → /api/v1/operators (no
// admin scope required; per-operator data isn't admin-only).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var (
	operatorCmd = &cobra.Command{
		Use:   "operator",
		Short: "Inspect + manage per-operator memory profiles",
		Long: `The operator profile is the per-user memory the dispatcher injects
into the system prompt on every chat turn. Stored as a small set of
allow-listed keys (tone, verbosity, time_zone, communication_style,
preferred_channel) plus a free-form notes column.

The dispatcher writes via the update_operator_profile tool; this
CLI provides the same read + write surface for operators / scripts
without going through the chat channel.

Operator IDs are channel-scoped: telegram:<chat_id>, webchat:<token>, etc.`,
	}

	operatorListCmd = &cobra.Command{
		Use:   "list",
		Short: "List operator profiles (newest-updated first)",
		RunE:  runOperatorList,
	}

	operatorShowCmd = &cobra.Command{
		Use:   "show <operator-id>",
		Short: "Show one operator's profile",
		Args:  cobra.ExactArgs(1),
		RunE:  runOperatorShow,
	}

	operatorSetCmd = &cobra.Command{
		Use:   "set <operator-id>",
		Short: "Set or remove one allow-listed key (empty --value removes)",
		Long: `Sets one allow-listed key on the operator's profile, or removes it
when --value is empty. Allow-listed keys: tone, verbosity, time_zone,
communication_style, preferred_channel, notes. --rationale is
required and recorded in the admin audit log.`,
		Args: cobra.ExactArgs(1),
		RunE: runOperatorSet,
	}

	operatorForgetCmd = &cobra.Command{
		Use:   "forget <operator-id>",
		Short: "Delete an operator profile (privacy revocation; requires --yes)",
		Long: `Removes the operator's profile row entirely. Use for GDPR-style
"forget me" requests. The deletion is recorded in the admin audit
log. --yes is required so an accidental invocation can't wipe
state silently.`,
		Args: cobra.ExactArgs(1),
		RunE: runOperatorForget,
	}

	operatorListJSON     bool
	operatorListLimit    int
	operatorShowJSON     bool
	operatorSetKey       string
	operatorSetValue     string
	operatorSetRationale string
	operatorSetJSON      bool
	operatorForgetReason string
	operatorForgetYes    bool
)

func init() {
	operatorListCmd.Flags().BoolVar(&operatorListJSON, "json", false, "Output JSON instead of table")
	operatorListCmd.Flags().IntVarP(&operatorListLimit, "limit", "n", 200, "Maximum rows (1-500)")

	operatorShowCmd.Flags().BoolVar(&operatorShowJSON, "json", false, "Output JSON instead of human-readable")

	operatorSetCmd.Flags().StringVar(&operatorSetKey, "key", "", "Allow-listed key (tone, verbosity, time_zone, communication_style, preferred_channel, notes)")
	operatorSetCmd.Flags().StringVar(&operatorSetValue, "value", "", "New value; empty string removes the key")
	operatorSetCmd.Flags().StringVar(&operatorSetRationale, "rationale", "", "Why this change is being made (required; recorded in audit log)")
	operatorSetCmd.Flags().BoolVar(&operatorSetJSON, "json", false, "Output JSON instead of human-readable")

	operatorForgetCmd.Flags().StringVar(&operatorForgetReason, "reason", "", "Why the profile is being deleted (recorded in audit log)")
	operatorForgetCmd.Flags().BoolVar(&operatorForgetYes, "yes", false, "Required confirmation: privacy revocations are irreversible")

	operatorCmd.AddCommand(operatorListCmd)
	operatorCmd.AddCommand(operatorShowCmd)
	operatorCmd.AddCommand(operatorSetCmd)
	operatorCmd.AddCommand(operatorForgetCmd)
	rootCmd.AddCommand(operatorCmd)
}

// operatorEntry mirrors api.OperatorEntryJSON. Local shape so
// the CLI doesn't import the api package.
type operatorEntry struct {
	OperatorID string `json:"operator_id"`
	Channel    string `json:"channel,omitempty"`
	Structured string `json:"structured"`
	Notes      string `json:"notes,omitempty"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

type operatorListResp struct {
	Entries []operatorEntry `json:"entries"`
}

type operatorShowResp struct {
	Entry operatorEntry `json:"entry"`
}

func runOperatorList(_ *cobra.Command, _ []string) error {
	q := url.Values{}
	if operatorListLimit > 0 {
		q.Set("limit", fmt.Sprintf("%d", operatorListLimit))
	}
	path := "/api/v1/operators"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	client := ClientFromEnv()
	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("operator list: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	var out operatorListResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("operator list: decode failed: %w", err)
	}
	if operatorListJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if len(out.Entries) == 0 {
		fmt.Println("No operator profiles yet.")
		fmt.Println("Profiles are created when the dispatcher's update_operator_profile tool fires on a chat turn,")
		fmt.Println("or when you set a key explicitly with `vornikctl operator set <id> --key … --value … --rationale …`.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()
	if _, err := fmt.Fprintln(tw, "OPERATOR\tCHANNEL\tKEYS\tNOTES\tUPDATED"); err != nil {
		return err
	}
	for _, e := range out.Entries {
		keys := summariseStructuredKeys(e.Structured)
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			truncate(e.OperatorID, 30),
			truncate(e.Channel, 12),
			truncate(keys, 40),
			truncate(e.Notes, 30),
			truncate(e.UpdatedAt, 19)); err != nil {
			return err
		}
	}
	return nil
}

func runOperatorShow(_ *cobra.Command, args []string) error {
	id := args[0]
	client := ClientFromEnv()
	resp, err := client.Get("/api/v1/operators/" + url.PathEscape(id))
	if err != nil {
		return fmt.Errorf("operator show: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	var out operatorShowResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("operator show: decode failed: %w", err)
	}
	if operatorShowJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out.Entry)
	}
	printOperatorRow(out.Entry)
	return nil
}

func runOperatorSet(_ *cobra.Command, args []string) error {
	id := args[0]
	key := strings.TrimSpace(operatorSetKey)
	rationale := strings.TrimSpace(operatorSetRationale)
	if key == "" {
		return fmt.Errorf("--key is required (e.g. --key tone)")
	}
	if rationale == "" {
		return fmt.Errorf("--rationale is required — every profile change is recorded in the audit log")
	}
	body := map[string]string{
		"key":       key,
		"value":     operatorSetValue,
		"rationale": rationale,
	}
	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/operators/"+url.PathEscape(id), body)
	if err != nil {
		return fmt.Errorf("operator set: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	var out operatorShowResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("operator set: decode failed: %w", err)
	}
	if operatorSetJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out.Entry)
	}
	if operatorSetValue == "" {
		fmt.Printf("Removed %s.%s\n", id, key)
	} else {
		fmt.Printf("Set %s.%s = %s\n", id, key, operatorSetValue)
	}
	printOperatorRow(out.Entry)
	return nil
}

func runOperatorForget(_ *cobra.Command, args []string) error {
	id := args[0]
	if !operatorForgetYes {
		return fmt.Errorf("refusing to delete %s: pass --yes to confirm; this is irreversible", id)
	}
	reason := strings.TrimSpace(operatorForgetReason)
	if reason == "" {
		reason = "operator removal via vornikctl"
	}
	body := map[string]string{"rationale": reason}
	client := ClientFromEnv()
	resp, err := client.Do(http.MethodDelete, "/api/v1/operators/"+url.PathEscape(id), body)
	if err != nil {
		return fmt.Errorf("operator forget: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	fmt.Printf("Forgotten %s.\n", id)
	return nil
}

// summariseStructuredKeys renders the JSON object as a compact
// "k=v, k=v" string for the list view. Falls back to the raw
// string if it's not parseable JSON.
func summariseStructuredKeys(raw string) string {
	if raw == "" || raw == "{}" {
		return "—"
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return raw
	}
	if len(m) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, ", ")
}

func printOperatorRow(e operatorEntry) {
	fmt.Printf("Operator:   %s\n", e.OperatorID)
	if e.Channel != "" {
		fmt.Printf("Channel:    %s\n", e.Channel)
	}
	fmt.Printf("Structured: %s\n", e.Structured)
	if e.Notes != "" {
		fmt.Printf("Notes:      %s\n", e.Notes)
	}
	fmt.Printf("Created:    %s\n", e.CreatedAt)
	fmt.Printf("Updated:    %s\n", e.UpdatedAt)
}
