package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
)

// `vornikctl knowledge {set-global,set-project}` — operator surface for
// the knowledge-skill store's cross-project reach (LLD 2026-07-07-cross-
// project-global-skills-design).
//
// A GLOBAL skill injects into EVERY project's roles, not just its home
// project; a project-only skill injects only into its home project.
// Flipping reach does NOT change maturity — an approved skill stays
// approved. Both leaves POST /api/v1/skills/{id}/global, an operator-
// scoped (NOT admin) route so it works in Community Edition.
//
// This is the knowledge-skill store (instructional SKILL.md know-how),
// DISTINCT from `vornikctl skill` (portable SWARM-SKILL.md capability
// files) — different primitive, different command tree.
var knowledgeCmd = &cobra.Command{
	Use:   "knowledge",
	Short: "Manage knowledge skills (cross-project reach)",
	Long: `Operator surface for the knowledge-skill store.

A knowledge skill authored in one project can be promoted to GLOBAL so it
injects into every project's roles — e.g. a skill captured from a Claude
companion session becomes available to the janka and assistant autonomy
roles. Promotion does not change a skill's maturity; an approved skill
stays approved.`,
}

var knowledgeSetGlobalCmd = &cobra.Command{
	Use:   "set-global <skill-id>",
	Short: "Promote a knowledge skill to GLOBAL (injects into ALL projects)",
	Args:  cobra.ExactArgs(1),
	RunE:  func(_ *cobra.Command, args []string) error { return runKnowledgeSetGlobal(args[0], true) },
}

var knowledgeSetProjectCmd = &cobra.Command{
	Use:   "set-project <skill-id>",
	Short: "Demote a knowledge skill to project-only (its home project)",
	Args:  cobra.ExactArgs(1),
	RunE:  func(_ *cobra.Command, args []string) error { return runKnowledgeSetGlobal(args[0], false) },
}

func init() {
	knowledgeCmd.AddCommand(knowledgeSetGlobalCmd, knowledgeSetProjectCmd)
	rootCmd.AddCommand(knowledgeCmd)
}

type knowledgeGlobalOutput struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsGlobal bool   `json:"is_global"`
}

func runKnowledgeSetGlobal(id string, global bool) error {
	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/skills/"+id+"/global", map[string]any{"global": global})
	if err != nil {
		return fmt.Errorf("set reach: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()

	var out knowledgeGlobalOutput
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	reach := "project-only (home project)"
	if out.IsGlobal {
		reach = "GLOBAL (injects into ALL projects' roles)"
	}
	_, _ = fmt.Fprintf(os.Stdout, "Skill %q (%s) is now %s\n", out.Name, out.ID, reach)
	return nil
}
