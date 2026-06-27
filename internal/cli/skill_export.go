package cli

// `vornikctl skill export <project>/<workflow>` — pull a workflow +
// the roles its steps reference out of the running daemon and emit
// a portable SWARM-SKILL.md.
//
// The three reads (project / workflow / swarm) all hit existing
// read-only endpoints; nothing new on the daemon side. The CLI is
// the assembly point.
//
// Flags follow the design contract:
//
//	-o / --output     write to file instead of stdout
//	--standard        drop metadata.vornik.* (agentskills.io-canonical)
//	--author <name>   override the canonical author field
//	--license <id>    override the canonical license field (SPDX)
//	--version <ver>   override the canonical version field

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/registry"
)

var (
	skillExportOutput   string
	skillExportStandard bool
	skillExportAuthor   string
	skillExportLicense  string
	skillExportVersion  string

	skillExportCmd = &cobra.Command{
		Use:   "export <project>/<workflow>",
		Short: "Export a workflow + its roles as a portable SWARM-SKILL.md",
		Long: `Export packages the named workflow and the roles its steps reference
into a single SWARM-SKILL.md file with agentskills.io-shaped frontmatter.

Argument is always <project>/<workflow>:
  - <project> resolves to that project's swarm (where the roles live);
  - <workflow> selects which workflow in the registry to bundle.

The standard flag drops the metadata.vornik.* block so the resulting
file is consumable by non-vornik SKILL.md tools. Standard files are
one-way: they cannot be re-imported.`,
		Args: cobra.ExactArgs(1),
		RunE: runSkillExport,
	}
)

func init() {
	skillExportCmd.Flags().StringVarP(&skillExportOutput, "output", "o", "", "Write to file instead of stdout")
	skillExportCmd.Flags().BoolVar(&skillExportStandard, "standard", false, "Drop metadata.vornik.* — produce a clean agentskills.io SKILL.md")
	skillExportCmd.Flags().StringVar(&skillExportAuthor, "author", "", "Set the canonical author field")
	skillExportCmd.Flags().StringVar(&skillExportLicense, "license", "", "Set the canonical license field (SPDX identifier)")
	skillExportCmd.Flags().StringVar(&skillExportVersion, "version", "", "Override the canonical version field")
	skillCmd.AddCommand(skillExportCmd)
}

func runSkillExport(cmd *cobra.Command, args []string) error {
	projectID, workflowID, err := parseProjectWorkflowArg(args[0])
	if err != nil {
		return err
	}

	client := ClientFromEnv()
	skill, err := assembleSkillFromDaemon(client, projectID, workflowID)
	if err != nil {
		return err
	}

	// CLI overrides — applied after the daemon read so the operator's
	// intent wins over whatever happened to be in the registry. Author
	// and license commonly aren't carried on existing workflows; without
	// these flags the file would publish without attribution.
	if skillExportAuthor != "" {
		skill.Author = skillExportAuthor
	}
	if skillExportLicense != "" {
		skill.License = skillExportLicense
	}
	if skillExportVersion != "" {
		skill.Version = skillExportVersion
		if skill.Workflow != nil {
			skill.Workflow.Version = skillExportVersion
		}
	}

	bytes, err := registry.MarshalSwarmSkill(skill, registry.MarshalSwarmSkillOpts{Standard: skillExportStandard})
	if err != nil {
		return fmt.Errorf("marshal SWARM-SKILL.md: %w", err)
	}

	if skillExportOutput == "" {
		_, err := cmd.OutOrStdout().Write(bytes)
		return err
	}
	// 0o644 — the file is for publishing, no secret material; same
	// mode any operator-authored config would land at.
	if err := os.WriteFile(skillExportOutput, bytes, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", skillExportOutput, err)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s (%d bytes)\n", skillExportOutput, len(bytes))
	return nil
}

// parseProjectWorkflowArg splits "<project>/<workflow>" into the
// two halves and surfaces a precise error for the common typo
// (missing slash, empty side).
func parseProjectWorkflowArg(arg string) (project, workflow string, err error) {
	parts := strings.SplitN(arg, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("argument must be <project>/<workflow> (got %q)", arg)
	}
	project, workflow = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if project == "" || workflow == "" {
		return "", "", fmt.Errorf("argument <project>/<workflow> requires both halves (got %q)", arg)
	}
	return project, workflow, nil
}

// assembleSkillFromDaemon performs the three reads
// (project config → swarm id → workflow + swarm) and assembles the
// SwarmSkill payload. Returns precise per-read errors so the operator
// can tell which lookup failed.
func assembleSkillFromDaemon(client *Client, projectID, workflowID string) (*registry.SwarmSkill, error) {
	projectDetail, err := fetchProjectConfig(client, projectID)
	if err != nil {
		return nil, err
	}
	if projectDetail.SwarmID == "" {
		return nil, fmt.Errorf("project %q has no swarm assigned; cannot resolve roles", projectID)
	}

	workflow, err := fetchWorkflow(client, workflowID)
	if err != nil {
		return nil, err
	}

	swarm, err := fetchSwarm(client, projectDetail.SwarmID)
	if err != nil {
		return nil, err
	}

	roles := filterRolesUsedByWorkflow(workflow, swarm.Roles)
	if len(roles) == 0 {
		return nil, fmt.Errorf("workflow %q has no agent steps referencing roles in swarm %q (nothing to export)", workflowID, projectDetail.SwarmID)
	}

	name := sluggifyExportName(workflow)
	desc := workflow.Description
	if desc == "" {
		desc = fmt.Sprintf("Workflow %q exported from project %q.", workflow.ID, projectID)
	}
	version := workflow.Version
	if version == "" {
		version = "0.1.0"
	}

	return &registry.SwarmSkill{
		Name:        name,
		Description: desc,
		Version:     version,
		Workflow:    workflow,
		Roles:       roles,
	}, nil
}

// sluggifyExportName picks a SKILL.md-conforming name for the
// exported bundle. Workflow.ID is the canonical identifier on the
// daemon side; we lowercase + hyphenate to meet the
// `[a-z0-9](-[a-z0-9])*` shape the validator enforces.
func sluggifyExportName(wf *registry.Workflow) string {
	if wf == nil {
		return "skill"
	}
	if slug := registry.SluggifySkillName(wf.ID); slug != "" {
		return slug
	}
	return "skill"
}

// filterRolesUsedByWorkflow returns the subset of swarmRoles
// referenced by any agent step in wf, preserving the original order.
// The order matters for byte-stable export → import → export
// round-trips.
func filterRolesUsedByWorkflow(wf *registry.Workflow, swarmRoles []registry.SwarmRole) []registry.SwarmRole {
	if wf == nil {
		return nil
	}
	used := make(map[string]bool, len(swarmRoles))
	for _, step := range wf.Steps {
		if step.Role != "" {
			used[step.Role] = true
		}
	}
	out := make([]registry.SwarmRole, 0, len(used))
	for _, r := range swarmRoles {
		if used[r.Name] {
			out = append(out, r)
		}
	}
	return out
}

// --- HTTP read helpers ----------------------------------------------------

type cliProjectSummary struct {
	ProjectID         string `json:"projectId"`
	DisplayName       string `json:"displayName"`
	SwarmID           string `json:"swarmId"`
	DefaultWorkflowID string `json:"defaultWorkflowId"`
}

func fetchProjectConfig(client *Client, projectID string) (*cliProjectSummary, error) {
	resp, err := client.Get("/api/v1/projects/" + projectID + "/config")
	if err != nil {
		return nil, fmt.Errorf("project %s: %w", projectID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, ParseAPIError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("project %s: read body: %w", projectID, err)
	}
	var detail cliProjectSummary
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("project %s: parse response: %w", projectID, err)
	}
	return &detail, nil
}

func fetchWorkflow(client *Client, workflowID string) (*registry.Workflow, error) {
	resp, err := client.Get("/api/v1/workflows/" + workflowID)
	if err != nil {
		return nil, fmt.Errorf("workflow %s: %w", workflowID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, ParseAPIError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("workflow %s: read body: %w", workflowID, err)
	}
	var wf registry.Workflow
	if err := json.Unmarshal(body, &wf); err != nil {
		return nil, fmt.Errorf("workflow %s: parse response: %w", workflowID, err)
	}
	return &wf, nil
}

func fetchSwarm(client *Client, swarmID string) (*registry.Swarm, error) {
	resp, err := client.Get("/api/v1/swarms/" + swarmID)
	if err != nil {
		return nil, fmt.Errorf("swarm %s: %w", swarmID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, ParseAPIError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("swarm %s: read body: %w", swarmID, err)
	}
	var sw registry.Swarm
	if err := json.Unmarshal(body, &sw); err != nil {
		return nil, fmt.Errorf("swarm %s: parse response: %w", swarmID, err)
	}
	return &sw, nil
}
