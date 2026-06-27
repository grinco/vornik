package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// playbookEntryCLI mirrors playbook.Entry. Kept private so the daemon
// owns the wire shape.
type playbookEntryCLI struct {
	Class       string   `json:"class"`
	Cause       string   `json:"cause"`
	Suggestions []string `json:"suggestions"`
	References  []string `json:"references"`
}

type playbookListResponseCLI struct {
	Entries []playbookEntryCLI `json:"entries"`
}

var playbookListJSON bool
var playbookShowJSON bool

var playbookCmd = &cobra.Command{
	Use:   "playbook",
	Short: "Operator-actionable remediations for task failure classes",
	Long: `Look up rule-based remediations for failure classes the executor
emits (TOOL_ITERATION_LIMIT, INVALID_OUTPUT, MERGE_FAILED, …). Two
forms:

  vornikctl playbook list            — print the full corpus
  vornikctl playbook show <CLASS>    — print suggestions for one class

The same content is also embedded in 'vornikctl task explain' output as
context for the LLM-generated summary.`,
}

var playbookListCmd = &cobra.Command{
	Use:   "list",
	Short: "List every known failure class and its remediation entry",
	RunE:  runPlaybookList,
}

var playbookShowCmd = &cobra.Command{
	Use:   "show <CLASS>",
	Short: "Show the playbook entry for one failure class",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlaybookShow,
}

func init() {
	playbookListCmd.Flags().BoolVar(&playbookListJSON, "json", false, "Output in JSON format")
	playbookShowCmd.Flags().BoolVar(&playbookShowJSON, "json", false, "Output in JSON format")
	playbookCmd.AddCommand(playbookListCmd)
	playbookCmd.AddCommand(playbookShowCmd)
	rootCmd.AddCommand(playbookCmd)
}

func runPlaybookList(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	resp, err := client.Get("/api/v1/playbook")
	if err != nil {
		return fmt.Errorf("failed to connect to vornik: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}
	var list playbookListResponseCLI
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if playbookListJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(list)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CLASS\tCAUSE")
	for _, e := range list.Entries {
		// Trim cause to one line for the index view; the full text
		// is available via `playbook show`.
		cause := strings.SplitN(e.Cause, "\n", 2)[0]
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", e.Class, cause)
	}
	_ = tw.Flush()
	fmt.Println("\nUse `vornikctl playbook show <CLASS>` for the full entry.")
	return nil
}

func runPlaybookShow(cmd *cobra.Command, args []string) error {
	class := strings.TrimSpace(args[0])
	if class == "" {
		return fmt.Errorf("class is required")
	}
	client := ClientFromEnv()
	resp, err := client.Get("/api/v1/playbook/" + url.PathEscape(class))
	if err != nil {
		return fmt.Errorf("failed to connect to vornik: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}
	var entry playbookEntryCLI
	if err := json.Unmarshal(body, &entry); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if playbookShowJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(entry)
	}

	fmt.Printf("CLASS: %s\n\n", entry.Class)
	fmt.Println("Cause:")
	fmt.Printf("  %s\n", entry.Cause)
	if len(entry.Suggestions) > 0 {
		fmt.Println("\nSuggestions:")
		for i, s := range entry.Suggestions {
			fmt.Printf("  %d. %s\n", i+1, s)
		}
	}
	if len(entry.References) > 0 {
		fmt.Println("\nReferences:")
		for _, r := range entry.References {
			fmt.Printf("  - %s\n", r)
		}
	}
	return nil
}
