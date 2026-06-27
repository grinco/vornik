package cli

// `vornikctl skill validate <file-or-dir>` — operator-facing
// wrapper around registry.ValidateSwarmSkillMarkdown.
//
// Output format mirrors `vornikctl workflow validate` (which mirrors
// `go vet` shape) so operators don't have to learn a new layout:
//
//	path/to/file.md: 2 error(s), 1 warning(s)
//	  [ERROR] name_shape: name — must be lowercase ...
//	  ...
//
// Exit code 0 on clean / warning-only; 1 on any ERROR finding.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/registry"
)

var (
	skillValidateFix  bool
	skillValidateJSON bool

	skillValidateCmd = &cobra.Command{
		Use:   "validate <path>",
		Short: "Validate a SWARM-SKILL.md file or directory of files",
		Long: `Validate a SWARM-SKILL.md file or every *.md immediate child of a directory.

Enforces the agentskills.io / SKILL.md frontmatter shape plus the
vornik payload consistency rules:
  - name (required, lowercase-hyphens, ≤64 chars)
  - description (required, ≤1024 chars)
  - version (required, semver shape)
  - author / license (recommended; warnings only)
  - metadata.vornik.schema_version must be 1 (when present)
  - every workflow step has a prompt
  - every step's role exists in metadata.vornik.roles
  - file size ≤100k chars; warns over 15k

Exit code 0 on clean or warnings-only; 1 on any ERROR finding.`,
		Args: cobra.ExactArgs(1),
		RunE: runSkillValidate,
	}
)

func init() {
	skillValidateCmd.Flags().BoolVar(&skillValidateFix, "fix", false, "Print suggested replacements for findings that have a mechanical hint")
	skillValidateCmd.Flags().BoolVar(&skillValidateJSON, "json", false, "Output the validation report as JSON")
	skillCmd.AddCommand(skillValidateCmd)
}

func runSkillValidate(cmd *cobra.Command, args []string) error {
	target := args[0]
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat %s: %w", target, err)
	}
	files, err := collectSkillFiles(target, info.IsDir())
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no *.md files found at %s", target)
	}

	type fileReport struct {
		Path   string                               `json:"path"`
		Report *registry.SwarmSkillValidationReport `json:"report"`
	}
	reports := make([]fileReport, 0, len(files))
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		reports = append(reports, fileReport{
			Path:   path,
			Report: registry.ValidateSwarmSkillMarkdown(data, filepath.Base(path)),
		})
	}

	if skillValidateJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]interface{}{"files": reports})
	}

	anyError := false
	totalFiles, cleanFiles := 0, 0
	for _, r := range reports {
		totalFiles++
		printSkillReport(cmd.OutOrStdout(), r.Path, r.Report, skillValidateFix)
		if r.Report.HasErrors() {
			anyError = true
		} else if !r.Report.HasWarnings() {
			cleanFiles++
		}
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\n%d file(s) checked, %d clean\n", totalFiles, cleanFiles)

	if anyError {
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		return fmt.Errorf("validation failed")
	}
	return nil
}

func collectSkillFiles(target string, isDir bool) ([]string, error) {
	if !isDir {
		return []string{target}, nil
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", target, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		out = append(out, filepath.Join(target, name))
	}
	sort.Strings(out)
	return out, nil
}

func printSkillReport(w interface{ Write([]byte) (int, error) }, path string, report *registry.SwarmSkillValidationReport, showHints bool) {
	if len(report.Findings) == 0 {
		_, _ = fmt.Fprintf(w, "%s: OK\n", path)
		return
	}
	errs, warns := 0, 0
	for _, f := range report.Findings {
		switch f.Severity {
		case registry.SeverityError:
			errs++
		case registry.SeverityWarning:
			warns++
		}
	}
	_, _ = fmt.Fprintf(w, "%s: %d error(s), %d warning(s)\n", path, errs, warns)
	for _, f := range report.Findings {
		_, _ = fmt.Fprintf(w, "  %s\n", f)
		if showHints && f.Hint != "" {
			for _, hl := range strings.Split(f.Hint, "\n") {
				_, _ = fmt.Fprintf(w, "      | %s\n", hl)
			}
		}
	}
}
