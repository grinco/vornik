package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DeadClassScan is the result of checking a deployed config tree for step
// error classes the running binary rejects.
//
// Lives in registry rather than in either doctor path because BOTH need it and
// neither may import the other: the HTTP doctor (internal/api) and the offline
// doctor (internal/cli) are separate check lists, and a pre-cutover preflight
// runs the offline one with the daemon down. One implementation, so the two
// cannot drift into diagnosing different things — the same reason
// finishOfflineDoctorReport is shared between its two entry points.
type DeadClassScan struct {
	// Findings name a file, its tree, and the rejected class. Non-empty means
	// the daemon will FAIL TO START on its next restart.
	Findings []string
	// Skipped names files that could not be parsed at all. Per file, never
	// per tree: one malformed workflow must not blind the scan to its
	// siblings, which is what a whole-tree load would do.
	Skipped []string
}

// ScanDeployedTreeForDeadClasses walks a config directory's workflows/ and
// project-templates/ and reports retry classes this binary would refuse.
//
// WHY IT MATTERS. The strict loader refuses an unknown class and the refusal is
// FATAL TO THE WHOLE DAEMON rather than scoped to the offending workflow:
//
//	failed to load workflows: workflow validation error in deep-research.md:
//	steps.publish.retry.on - unknown step error class "container_non_zero_exit"
//
// `container_non_zero_exit` was renamed to `unclassified` on 2026-08-26, and on
// 2026-09-17 a service started on a HEAD build against a stale tree, failed,
// and flapped on Restart=on-failure until stopped. Nothing detected it — it was
// found by a daemon failing to start.
func ScanDeployedTreeForDeadClasses(configDir string) DeadClassScan {
	var out DeadClassScan
	if strings.TrimSpace(configDir) == "" {
		return out
	}
	for _, tree := range []struct{ dir, label, suffix string }{
		// What the loader reads. A hit here is the daemon-down.
		{filepath.Join(configDir, "workflows"), "workflows", ".md"},
		// The MECHANICAL re-arm vector: creating a project from a stale
		// template writes a live tree with no human in the loop, so this is
		// the only pre-arm catch available. Backup directories are
		// deliberately NOT scanned — they re-arm only when an operator
		// restores one, and a restore lands the file in workflows/ where the
		// next scan sees it.
		{filepath.Join(configDir, "project-templates"), "project-templates", ".md.tmpl"},
	} {
		scanTree(tree.dir, tree.label, tree.suffix, &out)
	}
	sort.Strings(out.Findings)
	sort.Strings(out.Skipped)
	return out
}

func scanTree(dir, label, suffix string, out *DeadClassScan) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// An absent tree is not a finding; project-templates/ is missing on
		// most deployments.
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if label == "project-templates" {
				// One directory per template.
				scanTree(filepath.Join(dir, entry.Name()), label, suffix, out)
			}
			continue
		}
		name := entry.Name()
		// THE LOADER'S OWN PREDICATE, not an approximation. LoadWorkflows uses
		// strings.HasSuffix(name, ".md"), which matches a multi-dot basename
		// like deep-research.v2.md — a file it DOES load. An allowlist
		// anchored on a single-dot basename would skip that and make the scan
		// BLIND to an armed file, which is strictly worse than the suffix
		// denylist it replaced (and that denylist was already two shapes
		// behind on the reference deployment). Sidelined copies fall out for
		// free: none of .md.bak-…, .md.pre-T-… or .md.tmpl.bak-… ends in the
		// suffix.
		if !strings.HasSuffix(name, suffix) || name == "README.md" {
			continue
		}
		path := filepath.Join(dir, name)
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s/%s: unreadable (%v)", label, name, readErr))
			continue
		}
		out.Findings = append(out.Findings, deadClassFindings(content, name, label)...)
	}
}

// deadClassFindings filters the per-file validator's findings by CODE, and that
// filter is load-bearing twice over.
//
// It isolates the retry-class rejection from the validator's other rules — and
// it excludes the `name_shape` ERROR that every shipped template produces,
// because `workflowId: "{{.projectId}}-report"` is not lowercase-hyphens. A
// scan reporting every ERROR finding would cry wolf on every template on its
// first run, which is the failure this whole check is shaped around avoiding.
//
// Uses ValidateWorkflowMarkdown, the loader's own SINGLE-FILE entry, rather
// than LoadWorkflows: the batch loader returns ONE error for the whole tree —
// the very behaviour that makes the incident fatal — so a scan built on it
// could only report a whole-tree skip.
func deadClassFindings(content []byte, name, label string) []string {
	report := ValidateWorkflowMarkdown(content, name)
	if report == nil {
		return nil
	}
	var out []string
	for _, f := range report.Findings {
		if f.Code != "workflow_invalid" {
			continue
		}
		// workflow_invalid covers more than retry classes; only the class
		// rejection predicts the daemon-down this scan reports.
		if !strings.Contains(f.Message, "unknown step error class") {
			continue
		}
		out = append(out, fmt.Sprintf("%s/%s: %s", label, name, f.Message))
	}
	return out
}
