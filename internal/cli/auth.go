package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// vornikctl auth — manage the API key in the OS keychain instead of a
// plaintext VORNIK_API_KEY env var (security LLD review batch 3).
//
//   - auth login   — read a key from stdin and store it in the keychain
//   - auth logout  — remove the stored key
//   - auth status  — report which source vornikctl would use (no secret)
var (
	authCmd = &cobra.Command{
		Use:   "auth",
		Short: "Manage the vornikctl API key in the OS keychain",
		Long: "Store the vornikctl API key in the OS keychain (libsecret / " +
			"macOS Keychain / Windows Credential Manager) instead of a " +
			"plaintext VORNIK_API_KEY env var. VORNIK_API_KEY still works " +
			"and takes precedence when set.",
	}

	authLoginCmd = &cobra.Command{
		Use:   "login",
		Short: "Store the API key in the OS keychain (reads the key from stdin)",
		Long: "Reads the API key from stdin and stores it in the OS keychain.\n" +
			"Pipe the key so it never lands in shell history or argv:\n\n" +
			"    printf '%s' \"$KEY\" | vornikctl auth login\n",
		RunE: runAuthLogin,
	}

	authLogoutCmd = &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored API key from the OS keychain",
		RunE:  runAuthLogout,
	}

	authStatusCmd = &cobra.Command{
		Use:   "status",
		Short: "Show which API-key source vornikctl would use (no secret printed)",
		RunE:  runAuthStatus,
	}
)

func runAuthLogin(cmd *cobra.Command, _ []string) error {
	// If stdin is a terminal, nudge the operator toward piping so the key
	// doesn't get echoed; still accept interactive input.
	if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Enter API key (input is echoed; prefer piping the key in to keep it out of shell history):")
	}
	data, err := io.ReadAll(bufio.NewReader(cmd.InOrStdin()))
	if err != nil {
		return fmt.Errorf("read key from stdin: %w", err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return fmt.Errorf("no API key provided on stdin")
	}
	if err := StoreAPIKey(key); err != nil {
		return fmt.Errorf("store key in OS keychain (is a secret service available?): %w", err)
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "API key stored in the OS keychain. You can now unset VORNIK_API_KEY.")
	return nil
}

func runAuthLogout(cmd *cobra.Command, _ []string) error {
	if err := DeleteStoredAPIKey(); err != nil {
		return fmt.Errorf("remove key from OS keychain: %w", err)
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "API key removed from the OS keychain.")
	return nil
}

func runAuthStatus(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	if os.Getenv("VORNIK_API_KEY") != "" {
		_, _ = fmt.Fprintln(out, "API key source: VORNIK_API_KEY environment variable (takes precedence)")
		if _, err := LoadStoredAPIKey(); err == nil {
			_, _ = fmt.Fprintln(out, "  (a key is also stored in the OS keychain; the env var wins)")
		}
		return nil
	}
	_, err := LoadStoredAPIKey()
	switch {
	case err == nil:
		_, _ = fmt.Fprintln(out, "API key source: OS keychain")
	case errors.Is(err, ErrNoStoredKey):
		_, _ = fmt.Fprintln(out, "API key source: built-in default (no env var, no keychain entry)")
		_, _ = fmt.Fprintln(out, "  Run 'vornikctl auth login' or set VORNIK_API_KEY.")
	default:
		_, _ = fmt.Fprintf(out, "API key source: built-in default (OS keychain unavailable: %v)\n", err)
	}
	return nil
}

func init() {
	authCmd.AddCommand(authLoginCmd)
	authCmd.AddCommand(authLogoutCmd)
	authCmd.AddCommand(authStatusCmd)
	rootCmd.AddCommand(authCmd)
}
