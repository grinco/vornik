package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// §5.5 key and link-code verbs. They hit the CE operator shell under
// /api/v1/operator/, which is where identity's operator routes live —
// /api/v1/admin/ answers 501 EDITION_UNSUPPORTED in Community, and identity
// is a Community feature.

// claimSecretEnv is where `claim-key` reads the key secret from.
//
// NOT a command-line argument, deliberately. An argument lands in shell
// history and is visible in `ps` output to every user on the box for the life
// of the process — so a verb whose whole purpose is proving possession of a
// credential would be the thing that leaks it. The env var is read once and
// never echoed.
const claimSecretEnv = "VORNIK_KEY_SECRET"

var (
	accountLinkCodeCmd = &cobra.Command{
		Use:   "link-code <account-id>",
		Short: "Issue a one-time code to link a chat channel to an account (operator)",
		Long: "Issues an 8-character code, valid for 10 minutes. The holder sends\n" +
			"`/link <code>` from Telegram, or `<your Slack slash command> link <code>`\n" +
			"from Slack, in the channel they want bound. The code is shown once here\n" +
			"and stored only as a sha256 — it cannot be re-read.\n\n" +
			"This is the OPERATOR verb: it takes an account id and needs operator\n" +
			"capability. To issue a code for yourself, use the \"My account\" page —\n" +
			"vornikctl authenticates with an API key and has no session, so it\n" +
			"cannot reach the self route.",
		Args: cobra.ExactArgs(1),
		RunE: runAccountLinkCode,
	}
	accountClaimKeyCmd = &cobra.Command{
		Use:   "claim-key <account-id> <key-id>",
		Short: "Claim an API key for an account by proving possession of its secret",
		Long: "Maps an API key to an account so the key's recorded history — every\n" +
			"task, token and dollar — attributes to that person.\n\n" +
			"The secret is read from $" + claimSecretEnv + ", never from the command line:\n" +
			"an argument would land in shell history and in `ps` output.\n\n" +
			"A refusal is deliberately identical whether the key does not exist,\n" +
			"the secret is wrong, or someone else already claimed it.\n\n" +
			"This verb needs OPERATOR capability as well as the secret: vornikctl\n" +
			"authenticates with an API key and has no session, so it cannot use the\n" +
			"session-derived self route. A non-operator claims their key from the\n" +
			"\"My account\" panel in the web UI instead.",
		Args: cobra.ExactArgs(2),
		RunE: runAccountClaimKey,
	}
	accountGrantKeyCmd = &cobra.Command{
		Use:   "grant-key <account-id> <key-id>",
		Short: "Assign an API key to an account on operator authority (no secret)",
		Long: "The operator path: correct a wrong mapping, or attribute the key of\n" +
			"someone who has left. Self-claim cannot do either.",
		Args: cobra.ExactArgs(2),
		RunE: runAccountGrantKey,
	}
	accountRevokeKeyCmd = &cobra.Command{
		Use:   "revoke-key <account-id> <key-id>",
		Short: "Remove a key's mapping, returning it to unattributed",
		Args:  cobra.ExactArgs(2),
		RunE:  runAccountRevokeKey,
	}
)

func init() {
	accountCmd.AddCommand(accountLinkCodeCmd, accountClaimKeyCmd, accountGrantKeyCmd, accountRevokeKeyCmd)
}

func runAccountLinkCode(_ *cobra.Command, args []string) error {
	resp, err := ClientFromEnv().Post("/api/v1/operator/accounts/"+args[0]+"/link-code", map[string]any{})
	if err != nil {
		return fmt.Errorf("link-code: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Code      string `json:"code"`
		ExpiresIn int    `json:"expiresIn"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	// Printed once, and said to be once, because it genuinely cannot be
	// re-read: only the sha256 is stored.
	fmt.Printf("Link code: %s\n", out.Code)
	fmt.Printf("Valid for %d minutes. Send `/link %s` from the channel to bind it.\n", out.ExpiresIn/60, out.Code)
	fmt.Println("This code is shown once and is not recoverable.")
	return nil
}

func runAccountClaimKey(_ *cobra.Command, args []string) error {
	secret := strings.TrimSpace(os.Getenv(claimSecretEnv))
	if secret == "" {
		return errors.New("no key secret: set $" + claimSecretEnv + " to the key's secret " +
			"(it is read from the environment, never from the command line, so it does not reach shell history)")
	}
	return accountPost(args[0], "claim-key", map[string]any{"keyId": args[1], "keySecret": secret})
}

func runAccountGrantKey(_ *cobra.Command, args []string) error {
	return accountPost(args[0], "assign-key", map[string]any{"keyId": args[1]})
}

func runAccountRevokeKey(_ *cobra.Command, args []string) error {
	return accountPost(args[0], "unassign-key", map[string]any{"keyId": args[1]})
}
