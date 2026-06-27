package cli

// `vornikctl workflow validate <path>` — the operator-facing
// surface for the WORKFLOW.md phase-2 validator
// (internal/registry/workflow_md_validate.go).
//
// Two invocation shapes, picked by what `<path>` points at:
//
//   - A file: validate that single file. Exit code 0 if clean
//     or warnings only; 1 if any ERROR finding fires.
//
//   - A directory: validate every `*.md` immediate child.
//     Exit code 0 only if every file passes; the report shows
//     per-file findings so the operator can fix them together
//     rather than fix-one, re-run, fix-next.
//
// `--fix` prints the suggested replacement (the validator's
// `Hint` field) inline. Writing the fix back is intentionally
// out of scope — the change always needs human review, and a
// safe-write path that handles partial edits / merge conflicts
// would be larger than the rest of the validator combined.

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
	workflowValidateFix  bool
	workflowValidateJSON bool

	workflowValidateCmd = &cobra.Command{
		Use:   "validate <path>",
		Short: "Validate a WORKFLOW.md file or a directory of workflows against the SKILL.md shape",
		Long: `Validate a WORKFLOW.md file or every *.md immediate child of a directory.

Enforces the agentskills.io / SKILL.md frontmatter shape:
  - name (required, lowercase-hyphens, ≤64 chars)
  - description (required, ≤1024 chars)
  - version (required, semver shape)
  - author / license (recommended; warnings only)
  - metadata.related_skills (optional list of name-shaped entries)
  - file size ≤100k chars; warns over 15k
  - body must have a '## Prompts' section when frontmatter
    declares agent steps with no inline 'prompt:'

Exit code 0 on clean or warnings-only; 1 on any ERROR finding.

The --fix flag prints suggested replacements inline; writing
them back is out of scope and remains a manual edit.`,
		Args: cobra.ExactArgs(1),
		RunE: runWorkflowValidate,
	}
)

func init() {
	workflowValidateCmd.Flags().BoolVar(&workflowValidateFix, "fix", false, "Print suggested fixes for findings that have a mechanical hint")
	workflowValidateCmd.Flags().BoolVar(&workflowValidateJSON, "json", false, "Output the validation report as JSON")
	workflowCmd.AddCommand(workflowValidateCmd)
}

func runWorkflowValidate(cmd *cobra.Command, args []string) error {
	target := args[0]
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat %s: %w", target, err)
	}

	files, err := collectWorkflowFiles(target, info.IsDir())
	if err != nil {
		return err
	}
	if len(files) == 0 {
		// Returning a real error means a non-zero exit. The
		// operator typed `validate <dir>` against a directory
		// that has no .md children — that's almost certainly
		// the wrong directory, not "0 files == OK".
		return fmt.Errorf("no *.md files found at %s", target)
	}

	type fileReport struct {
		Path   string                               `json:"path"`
		Report *registry.WorkflowMDValidationReport `json:"report"`
	}
	reports := make([]fileReport, 0, len(files))
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		report := registry.ValidateWorkflowMarkdown(data, filepath.Base(path))
		reports = append(reports, fileReport{Path: path, Report: report})
	}

	if workflowValidateJSON {
		return emitJSONReports(reports)
	}

	anyError := false
	totalFiles := 0
	cleanFiles := 0
	for _, r := range reports {
		totalFiles++
		printFileReport(cmd.OutOrStdout(), r.Path, r.Report, workflowValidateFix)
		if r.Report.HasErrors() {
			anyError = true
		} else if !r.Report.HasWarnings() {
			cleanFiles++
		}
	}
	// Trailing summary keeps the multi-file path scannable.
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\n%d file(s) checked, %d clean\n", totalFiles, cleanFiles)

	if anyError {
		// Non-nil error → cobra returns exit code 1, without
		// printing the error's text because SilenceErrors is
		// the package default for our root command. The summary
		// above already tells the operator what to fix.
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		return fmt.Errorf("validation failed")
	}
	return nil
}

// collectWorkflowFiles returns the list of *.md files the
// validator should run against. For a directory we sort by
// filename so the output order is reproducible (the doctor
// adapter relies on the same ordering for its `Items` list).
func collectWorkflowFiles(target string, isDir bool) ([]string, error) {
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

// printFileReport writes a human-readable summary for one file
// to w. The format is intentionally similar to what `golint` /
// `go vet` produce so operators don't have to learn a new
// shape.
func printFileReport(w interface{ Write([]byte) (int, error) }, path string, report *registry.WorkflowMDValidationReport, showHints bool) {
	header := path
	if len(report.Findings) == 0 {
		_, _ = fmt.Fprintf(w, "%s: OK\n", header)
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
	_, _ = fmt.Fprintf(w, "%s: %d error(s), %d warning(s)\n", header, errs, warns)
	for _, f := range report.Findings {
		_, _ = fmt.Fprintf(w, "  %s\n", f)
		if showHints && f.Hint != "" {
			// Indent every hint line so a multi-line hint
			// stays visually nested under its finding.
			for _, hl := range strings.Split(f.Hint, "\n") {
				_, _ = fmt.Fprintf(w, "      | %s\n", hl)
			}
		}
	}
}

// emitJSONReports prints the validation reports as a single
// JSON envelope. Wrapping in {"files": [...]} keeps the shape
// extensible (we can add aggregate fields without breaking
// scripts that parse the array).
func emitJSONReports(reports interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]interface{}{"files": reports})
}
