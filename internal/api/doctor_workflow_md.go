package api

// Doctor adapter for the WORKFLOW.md phase-2 validator
// (internal/registry/workflow_md_validate.go).
//
// Severity contract:
//
//   - Any ERROR finding in any workflow → check status ERROR
//     and `vornikctl doctor` exits with a non-OK summary.
//   - At least one WARNING finding (and no errors) → WARNING.
//     The check still passes but the operator sees the gaps.
//   - Empty findings across every file → OK.
//
// Implementation notes:
//
//   - We walk `<configDir>/workflows/*.md` directly rather than
//     going through registry.Load() — Load() runs the workflow
//     schema validation first, and a workflow that fails *that*
//     validation would be excluded from the staged set, so it
//     would never reach this check. The SKILL.md shape is
//     orthogonal: a file can pass schema validation but fail
//     `description_missing`, and the operator should still hear
//     about it.
//
//   - `Items` is sorted (filename, then finding code) so the
//     diff between two doctor runs is stable. Without this an
//     operator's "fix one finding, run doctor, diff" workflow
//     would be churn-heavy.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// checkWorkflowMDShape validates every WORKFLOW.md under the
// configured config directory's `workflows/` subdir against the
// agentskills.io / SKILL.md frontmatter shape. Phase 2 of the
// WORKFLOW.md feature (see https://docs.vornik.io and
// internal/registry/workflow_md_validate.go).
func (h *DoctorHandlers) checkWorkflowMDShape() DoctorCheck {
	const name = "workflow_md_shape"
	if h.configDir == "" {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: "no config directory configured; skipping",
		}
	}
	dir := filepath.Join(h.configDir, "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return DoctorCheck{
				Name:    name,
				Status:  "OK",
				Message: "no workflows directory; nothing to validate",
			}
		}
		return DoctorCheck{
			Name:    name,
			Status:  "ERROR",
			Message: fmt.Sprintf("read workflows dir: %v", err),
		}
	}

	var (
		errs     []workflowMDFileFinding
		warns    []workflowMDFileFinding
		checked  int
		readErrs []string
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			readErrs = append(readErrs, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		report := registry.ValidateWorkflowMarkdown(data, e.Name())
		checked++
		for _, f := range report.Findings {
			switch f.Severity {
			case registry.SeverityError:
				errs = append(errs, workflowMDFileFinding{filename: e.Name(), finding: f})
			case registry.SeverityWarning:
				warns = append(warns, workflowMDFileFinding{filename: e.Name(), finding: f})
			}
		}
	}

	if len(readErrs) > 0 && checked == 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "ERROR",
			Message: "no workflow files were readable",
			Items:   readErrs,
		}
	}

	// Sort both buckets so the diff between two runs is stable.
	sortFileFindings := func(in []workflowMDFileFinding) {
		sort.SliceStable(in, func(i, j int) bool {
			if in[i].filename != in[j].filename {
				return in[i].filename < in[j].filename
			}
			return in[i].finding.Code < in[j].finding.Code
		})
	}
	sortFileFindings(errs)
	sortFileFindings(warns)

	formatItems := func(in []workflowMDFileFinding) []string {
		out := make([]string, 0, len(in))
		for _, ff := range in {
			out = append(out, fmt.Sprintf("%s: %s", ff.filename, ff.finding))
		}
		return out
	}

	if len(errs) > 0 {
		items := formatItems(errs)
		// Append read errors at the tail so they're visible
		// but never bury the validation findings.
		items = append(items, readErrs...)
		return DoctorCheck{
			Name:    name,
			Status:  "ERROR",
			Message: fmt.Sprintf("%d workflow shape error(s) across %d file(s)", len(errs), countDistinctFiles(errs)),
			Items:   items,
		}
	}
	if len(warns) > 0 {
		items := formatItems(warns)
		items = append(items, readErrs...)
		return DoctorCheck{
			Name:    name,
			Status:  "WARNING",
			Message: fmt.Sprintf("%d recommended-field warning(s) across %d file(s)", len(warns), countDistinctFiles(warns)),
			Items:   items,
		}
	}
	msg := fmt.Sprintf("all %d workflow file(s) pass the SKILL.md shape", checked)
	if len(readErrs) > 0 {
		// We checked some, couldn't read others — still OK
		// overall (no findings against what we read) but
		// surface the read errors so they're not silent.
		return DoctorCheck{Name: name, Status: "WARNING", Message: msg, Items: readErrs}
	}
	return DoctorCheck{Name: name, Status: "OK", Message: msg}
}

// workflowMDFileFinding pairs a finding with the file it came
// from. Used so we can sort the doctor `Items` deterministically
// and present per-file context in each list entry.
type workflowMDFileFinding struct {
	filename string
	finding  registry.WorkflowMDFinding
}

// countDistinctFiles counts unique filenames in the slice. Used
// so the doctor message reads "3 errors across 2 files" rather
// than the misleading "3 errors across 3 files" when one file
// trips multiple rules.
func countDistinctFiles(in []workflowMDFileFinding) int {
	seen := map[string]struct{}{}
	for _, ff := range in {
		seen[ff.filename] = struct{}{}
	}
	return len(seen)
}
