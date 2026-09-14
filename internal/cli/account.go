package cli

// `vornikctl account {list,show,create,grant,revoke,disable,enable,unlink}`
// — the COMMUNITY operator shell for user accounts (2026-09-13 identity
// work, config-assistant review R1). Calls /api/v1/operator/accounts*,
// which needs operator scope PLUS an explicit capability (an API key on
// admin.allowed_keys, or an admin session); a plain unrestricted key is
// refused with OPERATOR_CAPABILITY_REQUIRED.
//
// This is the CE bootstrap path for humans: an admin-keyed operator
// creates the account, the person links a channel with `/account link`,
// and from then on every door resolves them to the same account.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

type accountWire struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"displayName"`
	Disabled    bool     `json:"disabled"`
	Role        string   `json:"role"`
	Projects    []string `json:"projects"`
	Identities  []struct {
		Channel    string `json:"channel"`
		ExternalID string `json:"externalId"`
		Display    string `json:"display"`
	} `json:"identities"`
	ActiveSessions int    `json:"activeSessions"`
	CreatedAt      string `json:"createdAt"`
}

var (
	accountCmd = &cobra.Command{
		Use:   "account",
		Short: "Manage user accounts (the CE identity core)",
		Long: `Accounts are what every door resolves to: a web session, a linked
Telegram or Slack sender, an owned API key. Create one here, grant it
'user' access scoped to projects or instance-wide 'admin', and link
channels with '/account link <code>' in the chat. Revoking an account
revokes every door at once; disabling it does so immediately.

Needs an explicit operator capability: an API key listed in
admin.allowed_keys (with admin.enabled: true) or an admin session.`,
	}
	accountListCmd   = &cobra.Command{Use: "list", Short: "List accounts", RunE: runAccountList}
	accountShowCmd   = &cobra.Command{Use: "show <id>", Short: "Show one account", Args: cobra.ExactArgs(1), RunE: runAccountShow}
	accountCreateCmd = &cobra.Command{
		Use:   "create <display-name>",
		Short: "Create an account (awaiting access unless --role is given)",
		Args:  cobra.ExactArgs(1),
		RunE:  runAccountCreate,
	}
	accountGrantCmd = &cobra.Command{
		Use:   "grant <id>",
		Short: "Grant --role admin, or --role user with --project (repeatable; '*' = all)",
		Args:  cobra.ExactArgs(1),
		RunE:  runAccountGrant,
	}
	accountRevokeCmd  = &cobra.Command{Use: "revoke <id>", Short: "Return an account to awaiting access (deliberate revocation)", Args: cobra.ExactArgs(1), RunE: accountAction("revoke")}
	accountDisableCmd = &cobra.Command{Use: "disable <id>", Short: "Disable an account (every door, immediately)", Args: cobra.ExactArgs(1), RunE: accountAction("disable")}
	accountEnableCmd  = &cobra.Command{Use: "enable <id>", Short: "Re-enable a disabled account", Args: cobra.ExactArgs(1), RunE: accountAction("enable")}
	accountUnlinkCmd  = &cobra.Command{
		Use:   "unlink <id> <channel> <external-id>",
		Short: "Unlink one channel binding from the account",
		Args:  cobra.ExactArgs(3),
		RunE:  runAccountUnlink,
	}

	accountJSONOut  bool
	accountRole     string
	accountProjects []string
)

func init() {
	accountListCmd.Flags().BoolVar(&accountJSONOut, "json", false, "print JSON")
	accountShowCmd.Flags().BoolVar(&accountJSONOut, "json", false, "print JSON")
	accountCreateCmd.Flags().StringVar(&accountRole, "role", "", "admin|user (empty = awaiting access)")
	accountCreateCmd.Flags().StringArrayVar(&accountProjects, "project", nil, "project id for --role user (repeatable; '*' = all)")
	accountGrantCmd.Flags().StringVar(&accountRole, "role", "", "admin|user")
	accountGrantCmd.Flags().StringArrayVar(&accountProjects, "project", nil, "project id for --role user (repeatable; '*' = all)")
	accountCmd.AddCommand(accountListCmd, accountShowCmd, accountCreateCmd, accountGrantCmd, accountRevokeCmd, accountDisableCmd, accountEnableCmd, accountUnlinkCmd)
	rootCmd.AddCommand(accountCmd)
}

func decodeAccountEnvelope(resp *http.Response) (*accountWire, error) {
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Account accountWire `json:"account"`
		Warning string      `json:"warning"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if out.Warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", out.Warning)
	}
	return &out.Account, nil
}

func printAccount(a *accountWire) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	role := a.Role
	if role == "" {
		role = "awaiting"
	}
	if _, err := fmt.Fprintf(tw, "id\t%s\nname\t%s\nrole\t%s\nprojects\t%s\ndisabled\t%v\nsessions\t%d\ncreated\t%s\n",
		a.ID, a.DisplayName, role, strings.Join(a.Projects, ","), a.Disabled, a.ActiveSessions, a.CreatedAt); err != nil {
		return err
	}
	for _, i := range a.Identities {
		if _, err := fmt.Fprintf(tw, "identity\t%s:%s\n", i.Channel, i.ExternalID); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func runAccountList(_ *cobra.Command, _ []string) error {
	resp, err := ClientFromEnv().Get("/api/v1/operator/accounts")
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Accounts []accountWire `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if accountJSONOut {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tNAME\tROLE\tPROJECTS\tIDENTITIES\tDISABLED"); err != nil {
		return err
	}
	for _, a := range out.Accounts {
		role := a.Role
		if role == "" {
			role = "awaiting"
		}
		ids := make([]string, 0, len(a.Identities))
		for _, i := range a.Identities {
			ids = append(ids, i.Channel+":"+i.ExternalID)
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%v\n", a.ID, a.DisplayName, role, strings.Join(a.Projects, ","), strings.Join(ids, " "), a.Disabled); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func runAccountShow(_ *cobra.Command, args []string) error {
	resp, err := ClientFromEnv().Get("/api/v1/operator/accounts/" + args[0])
	if err != nil {
		return fmt.Errorf("show: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	a, err := decodeAccountEnvelope(resp)
	if err != nil {
		return err
	}
	if accountJSONOut {
		return json.NewEncoder(os.Stdout).Encode(a)
	}
	return printAccount(a)
}

func runAccountCreate(_ *cobra.Command, args []string) error {
	resp, err := ClientFromEnv().Post("/api/v1/operator/accounts", map[string]any{
		"displayName": args[0], "role": accountRole, "projects": accountProjects,
	})
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return ParseAPIError(resp)
	}
	a, err := decodeAccountEnvelope(resp)
	if err != nil {
		return err
	}
	return printAccount(a)
}

func runAccountGrant(_ *cobra.Command, args []string) error {
	if accountRole == "" {
		return fmt.Errorf("--role admin|user is required")
	}
	return accountPost(args[0], "grant", map[string]any{"role": accountRole, "projects": accountProjects})
}

func runAccountUnlink(_ *cobra.Command, args []string) error {
	return accountPost(args[0], "unlink", map[string]any{"channel": args[1], "externalId": args[2]})
}

func accountAction(action string) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, args []string) error {
		return accountPost(args[0], action, map[string]any{})
	}
}

func accountPost(id, action string, body map[string]any) error {
	resp, err := ClientFromEnv().Post("/api/v1/operator/accounts/"+id+"/"+action, body)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	a, err := decodeAccountEnvelope(resp)
	if err != nil {
		return err
	}
	return printAccount(a)
}
