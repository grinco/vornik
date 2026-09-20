package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/registry"
)

// `vornikctl config keys` — the upgrade surface, per loader-validator
// agreement design §13.3.
//
// It publishes HERE and to a companion skill, and deliberately NOT to an
// UPGRADE.md or a section of AGENTS.md. Either would become the thing customers
// rely on, and both go stale in the one direction that matters: a published
// upgrade note that is wrong about the current release is worse than no note,
// because it is trusted. This binary cannot be stale about its own schema.
var (
	configKeysCmd = &cobra.Command{
		Use:   "keys",
		Short: "List the project config keys this binary accepts, and which release introduced each",
		Long: `Print every project config key THIS BINARY accepts.

This answers two different upgrade questions, and they are not the same:

  "would my config load under this binary?"  — the full list, always correct,
      because it is derived from the schema rather than maintained by hand.

  "what is new since the release I am running?" — pass --since <release>. Keys
      that predate per-key provenance are reported as older than tracking
      rather than guessed at.

A key your config uses that is NOT in this list is why a project went dark:
deploy the binary that knows the key first, then the config.`,
		RunE: runConfigKeys,
	}

	configKeysSince string
)

func init() {
	configKeysCmd.Flags().StringVar(&configKeysSince, "since", "",
		"Only keys introduced after this release (e.g. 2026.9.4)")
	configKeysCmd.Flags().BoolVar(&configJSON, "json", false, "JSON output instead of the table")
	configCmd.AddCommand(configKeysCmd)
}

func runConfigKeys(cmd *cobra.Command, _ []string) error {
	keys := registry.ProjectConfigKeysSince()
	if configKeysSince != "" {
		keys = registry.ProjectKeysNewerThan(configKeysSince)
	}

	if configJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(keys)
	}

	if len(keys) == 0 && configKeysSince != "" {
		// SAY WHY IT IS EMPTY. "No keys" and "no keys carry provenance yet" are
		// different answers, and an operator planning an upgrade must not read
		// the second as the first.
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"No project config key declares a release after %s.\n"+
				"Note that keys older than per-key provenance declare no release at all, so this\n"+
				"is NOT a statement that nothing changed — run without --since for the full list\n"+
				"this binary accepts.\n", configKeysSince)
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "KEY\tSINCE")
	untagged := 0
	for _, k := range keys {
		since := k.Since
		if since == "" {
			since = "(predates tracking)"
			untagged++
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\n", k.Key, since)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\n%d key(s)", len(keys))
	if untagged > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), ", %d predating per-key release provenance", untagged)
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), ".")
	return nil
}
