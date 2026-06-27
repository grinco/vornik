package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// evalSuiteShape mirrors the cli.evalSuite JSON shape. Duplicated here
// so the API package doesn't import cli (which would re-enter this
// package through rootCmd wiring). Small struct — keeping it local
// costs less than the import-graph surgery.
type evalSuiteShape struct {
	ProjectID  string          `json:"project_id"`
	WorkflowID string          `json:"workflow_id,omitempty"`
	Cases      []evalSuiteCase `json:"cases"`
}

type evalSuiteCase struct {
	Name       string `json:"name"`
	Prompt     string `json:"prompt"`
	WorkflowID string `json:"workflow_id,omitempty"`
}

// checkEvalSuiteLint parses every configs/evals/*.json file and flags
// suites that reference a project / workflow / swarm combination that
// isn't actually compatible. Complements workflow_swarm_compat: that
// check operates on registered projects × workflows; this one catches
// eval-specific drift (you renamed a swarm and forgot to update the
// eval suite that referenced it).
//
// Severity WARNING — eval suites aren't runtime-critical, but a broken
// one silently fails every time someone runs `vornikctl eval run`.
func (h *DoctorHandlers) checkEvalSuiteLint() DoctorCheck {
	const name = "eval_suite_lint"

	// Eval suites live at {configDir}/../evals/*.json in the canonical
	// layout (a sibling of projects/ / swarms/ / workflows/). Some
	// deployments drop them under configs/evals/ directly; probe both.
	candidates := []string{}
	if h.configDir != "" {
		candidates = append(candidates,
			filepath.Join(filepath.Dir(h.configDir), "evals"),
			filepath.Join(h.configDir, "evals"),
		)
	}
	candidates = append(candidates, "configs/evals")

	var evalsDir string
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			evalsDir = c
			break
		}
	}
	if evalsDir == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "no eval suites directory found, skipping"}
	}

	entries, err := os.ReadDir(evalsDir)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf("could not read %s: %v", evalsDir, err)}
	}

	// Load the registry once — we'll cross-reference every eval suite
	// against it. Use a fresh registry instance so the check is
	// independent of any live daemon state.
	reg := registry.New()
	if h.configDir != "" {
		_ = reg.Load(h.configDir)
	}
	projects := map[string]*registry.Project{}
	for _, p := range reg.ListProjects() {
		if p != nil {
			projects[p.ID] = p
		}
	}
	workflows := map[string]*registry.Workflow{}
	for _, w := range reg.ListWorkflows() {
		if w != nil {
			workflows[w.ID] = w
		}
	}
	swarms := map[string]*registry.Swarm{}
	for _, sw := range reg.ListSwarms() {
		if sw != nil {
			swarms[sw.ID] = sw
		}
	}

	var items []string
	var suitesChecked int
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		suitesChecked++
		suitePath := filepath.Join(evalsDir, entry.Name())
		for _, finding := range lintEvalSuite(suitePath, projects, workflows, swarms) {
			items = append(items, fmt.Sprintf("%s: %s", entry.Name(), finding))
		}
	}

	if len(items) == 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("all %d eval suite(s) reference valid projects/workflows/swarms", suitesChecked),
		}
	}
	sort.Strings(items)
	return DoctorCheck{
		Name:    name,
		Status:  "WARNING",
		Message: fmt.Sprintf("%d eval suite lint finding(s) across %d suite(s)", len(items), suitesChecked),
		Items:   items,
	}
}

// lintEvalSuite returns zero or more lint findings for one eval suite.
// Deliberately conservative — findings drive operator attention, and
// an over-eager linter teaches operators to ignore it.
func lintEvalSuite(
	path string,
	projects map[string]*registry.Project,
	workflows map[string]*registry.Workflow,
	swarms map[string]*registry.Swarm,
) []string {
	var out []string

	body, err := os.ReadFile(path)
	if err != nil {
		out = append(out, fmt.Sprintf("read failed: %v", err))
		return out
	}
	var suite evalSuiteShape
	if err := json.Unmarshal(body, &suite); err != nil {
		out = append(out, fmt.Sprintf("parse failed: %v", err))
		return out
	}
	if len(suite.Cases) == 0 {
		out = append(out, "suite has no cases")
		// Other checks still run — a suite can be malformed AND
		// reference a missing project.
	}
	if suite.ProjectID == "" {
		out = append(out, "project_id is required")
		return out
	}

	project, ok := projects[suite.ProjectID]
	if !ok {
		out = append(out, fmt.Sprintf("project %q is not loaded in the registry", suite.ProjectID))
		return out
	}
	swarm, hasSwarm := swarms[project.SwarmID]
	if !hasSwarm {
		out = append(out, fmt.Sprintf("project %q references swarm %q which is not loaded", project.ID, project.SwarmID))
	}

	// Collect every workflow ID the suite might dispatch. The
	// suite-level workflow_id is the default; per-case workflow_id
	// overrides. An empty suite-level default + empty case-level
	// means "use project default".
	seen := map[string]bool{}
	if suite.WorkflowID != "" {
		seen[suite.WorkflowID] = true
	}
	for _, c := range suite.Cases {
		if c.WorkflowID != "" {
			seen[c.WorkflowID] = true
		}
	}
	if len(seen) == 0 {
		seen[project.DefaultWorkflowID] = true
	}

	for wfID := range seen {
		wf, ok := workflows[wfID]
		if !ok {
			out = append(out, fmt.Sprintf("workflow %q referenced by project %q is not loaded", wfID, project.ID))
			continue
		}
		if !hasSwarm {
			continue
		}
		roles := make(map[string]bool, len(swarm.Roles))
		for _, r := range swarm.Roles {
			roles[r.Name] = true
		}
		missing := missingRolesForWorkflow(wf, roles)
		if len(missing) > 0 {
			out = append(out, fmt.Sprintf(
				"workflow %q requires role(s) %s not present in swarm %q (project %q)",
				wf.ID, strings.Join(missing, ", "), swarm.ID, project.ID,
			))
		}
	}

	return out
}
