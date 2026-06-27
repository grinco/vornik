package cli

// `vornikctl skill search / register / rate` — discovery +
// community-contribution surface for the registry.
//
// search — fetch the index, filter by query, print results.
// register — print a PR-submit snippet for the registry repo
//   (or run `gh pr create` when gh is on PATH and --gh is set).
// rate — POST {handle, skill, stars} to <registry>/rating.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// --- search ---------------------------------------------------

var (
	skillSearchRegistry string
	skillSearchJSON     bool

	skillSearchCmd = &cobra.Command{
		Use:   "search [<query>]",
		Short: "Search the registry index",
		Long: `Fetch <registry>/index.json and filter by query.

Query matches case-insensitively against handle, skill name,
description, and tag list. Empty query lists every skill in the
index. The registry URL defaults to ` + DefaultSkillRegistryURL + `;
override with --registry or VORNIK_SKILL_REGISTRY_URL.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runSkillSearch,
	}
)

func init() {
	skillSearchCmd.Flags().StringVar(&skillSearchRegistry, "registry", "", "Override registry index URL")
	skillSearchCmd.Flags().BoolVar(&skillSearchJSON, "json", false, "Output as JSON")
	skillCmd.AddCommand(skillSearchCmd)
}

func runSkillSearch(cmd *cobra.Command, args []string) error {
	query := ""
	if len(args) == 1 {
		query = args[0]
	}
	results, err := NewSkillIndexClient(skillSearchRegistry).Search(query)
	if err != nil {
		return err
	}
	if skillSearchJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"skills": results, "total": len(results)})
	}
	if len(results) == 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No skills matched %q on the registry.\n", query)
		return nil
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "HANDLE/SKILL\tDESCRIPTION\tTAGS\tINSTALLS\tRATING")
	for _, r := range results {
		rating := "—"
		if r.RatingN > 0 {
			rating = fmt.Sprintf("%.1f (%d)", r.RatingAvg, r.RatingN)
		}
		_, _ = fmt.Fprintf(tw, "%s/%s\t%s\t%s\t%d\t%s\n",
			r.Handle, r.Skill, truncateString(r.Description, 60), strings.Join(r.Tags, ","), r.InstallCount, rating)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nTotal: %d\n", len(results))
	return nil
}

// truncateString shortens s to at most n runes with an ellipsis
// when truncation happens — used for the description column in
// `vornikctl skill search`'s tabular view.
func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// --- register -------------------------------------------------

var (
	skillRegisterGitURL      string
	skillRegisterDescription string
	skillRegisterTags        []string
	skillRegisterHomepage    string
	skillRegisterRegistry    string
	skillRegisterGH          bool

	skillRegisterCmd = &cobra.Command{
		Use:   "register <handle>/<skill>",
		Short: "Print PR-submit instructions for the registry index",
		Long: `Build a skills.yaml row for <handle>/<skill> and print
either:

  - The exact YAML snippet to append to the registry repo's
    skills.yaml (default), so the operator can submit a PR
    manually; OR
  - With --gh, run "gh pr create" against the registry's
    SkillRegistryRepo (default skills.vornik.io). Requires
    the gh CLI on PATH.`,
		Args: cobra.ExactArgs(1),
		RunE: runSkillRegister,
	}
)

// SkillRegistryRepo is the default GitHub repo `--gh` will open
// PRs against. Override via VORNIK_SKILL_REGISTRY_REPO when
// hosting a self-served index.
const SkillRegistryRepo = "vornik-dev/skills.vornik.io"

func init() {
	skillRegisterCmd.Flags().StringVar(&skillRegisterGitURL, "git-url", "", "Git URL the new skill installs from (required)")
	skillRegisterCmd.Flags().StringVar(&skillRegisterDescription, "description", "", "One-line description")
	skillRegisterCmd.Flags().StringSliceVar(&skillRegisterTags, "tag", nil, "Tag (repeat for multiple)")
	skillRegisterCmd.Flags().StringVar(&skillRegisterHomepage, "homepage", "", "Project homepage URL")
	skillRegisterCmd.Flags().StringVar(&skillRegisterRegistry, "registry", "", "Override registry index URL")
	skillRegisterCmd.Flags().BoolVar(&skillRegisterGH, "gh", false, "Open a PR via gh CLI instead of printing the snippet")
	skillCmd.AddCommand(skillRegisterCmd)
}

func runSkillRegister(cmd *cobra.Command, args []string) error {
	handle, skillName, err := splitInstalledSkillRef(args[0])
	if err != nil {
		return err
	}
	if skillRegisterGitURL == "" {
		return fmt.Errorf("--git-url is required")
	}
	row := SkillIndexItem{
		Handle:      handle,
		Skill:       skillName,
		GitURL:      skillRegisterGitURL,
		Description: skillRegisterDescription,
		Tags:        skillRegisterTags,
		Homepage:    skillRegisterHomepage,
	}
	snippet := renderSkillsYAMLRow(row)

	if !skillRegisterGH {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Append this row to skills.yaml in the registry repo, then open a PR:\n\n%s\n", snippet)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Registry repo: https://github.com/%s\n", registryRepo())
		return nil
	}

	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("gh CLI not on PATH; drop --gh and submit the printed snippet manually")
	}
	body := fmt.Sprintf("Adds %s/%s to the registry index.\n\n```yaml\n%s\n```\n", handle, skillName, snippet)
	title := fmt.Sprintf("registry: add %s/%s", handle, skillName)
	pr := exec.Command("gh", "pr", "create", "--repo", registryRepo(), "--title", title, "--body", body)
	pr.Stdout = cmd.OutOrStdout()
	pr.Stderr = cmd.ErrOrStderr()
	return pr.Run()
}

func registryRepo() string {
	if env := strings.TrimSpace(os.Getenv("VORNIK_SKILL_REGISTRY_REPO")); env != "" {
		return env
	}
	return SkillRegistryRepo
}

// renderSkillsYAMLRow formats a SkillIndexItem as a single
// skills.yaml list entry. Hand-rolling beats yaml.Marshal here
// because we want a snippet the operator can copy-paste as one
// list item — not a full document.
func renderSkillsYAMLRow(item SkillIndexItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  - handle: %s\n", item.Handle)
	fmt.Fprintf(&b, "    skill: %s\n", item.Skill)
	fmt.Fprintf(&b, "    git_url: %s\n", item.GitURL)
	if item.Description != "" {
		fmt.Fprintf(&b, "    description: %s\n", yamlQuoteIfNeeded(item.Description))
	}
	if len(item.Tags) > 0 {
		fmt.Fprintf(&b, "    tags: [%s]\n", strings.Join(item.Tags, ", "))
	}
	if item.Homepage != "" {
		fmt.Fprintf(&b, "    homepage: %s\n", item.Homepage)
	}
	return b.String()
}

// yamlQuoteIfNeeded wraps a string in single quotes when it
// contains characters that would otherwise need YAML escaping.
// Keeps the snippet readable while being safe to paste verbatim.
func yamlQuoteIfNeeded(s string) string {
	for _, r := range s {
		if r == ':' || r == '#' || r == '\'' || r == '"' || r == '\n' || r == '[' || r == '{' {
			return "'" + strings.ReplaceAll(s, "'", "''") + "'"
		}
	}
	return s
}

// --- rate -----------------------------------------------------

var (
	skillRateStars    int
	skillRateRegistry string

	skillRateCmd = &cobra.Command{
		Use:   "rate <handle>/<skill>",
		Short: "Submit a rating for an installed skill",
		Long: `POST {handle, skill, stars} to <registry>/rating.

Best-effort: a 404 / network failure is reported but doesn't
return a non-zero exit (so a busted registry doesn't break
scripts).`,
		Args: cobra.ExactArgs(1),
		RunE: runSkillRate,
	}
)

func init() {
	skillRateCmd.Flags().IntVar(&skillRateStars, "stars", 0, "Stars (1-5)")
	skillRateCmd.Flags().StringVar(&skillRateRegistry, "registry", "", "Override registry URL")
	skillCmd.AddCommand(skillRateCmd)
}

func runSkillRate(cmd *cobra.Command, args []string) error {
	handle, skillName, err := splitInstalledSkillRef(args[0])
	if err != nil {
		return err
	}
	if skillRateStars < 1 || skillRateStars > 5 {
		return fmt.Errorf("--stars must be 1..5 (got %d)", skillRateStars)
	}
	base := NewSkillIndexClient(skillRateRegistry).BaseURL()
	if base == "" {
		return fmt.Errorf("no registry URL configured")
	}
	payload := map[string]any{
		"handle":      handle,
		"skill":       skillName,
		"stars":       skillRateStars,
		"instance_id": skillTelemetryInstanceID(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+"/rating", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "vornikctl-skill/1")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "rating not submitted (%v); the registry may be offline\n", err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "rating not submitted (HTTP %d); the registry may be offline\n", resp.StatusCode)
		return nil
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "rated %s/%s %d★\n", handle, skillName, skillRateStars)
	return nil
}
