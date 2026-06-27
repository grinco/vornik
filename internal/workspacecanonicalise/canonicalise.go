// Package workspacecanonicalise migrates project workspaces from the
// legacy autonomy/ layout to the canonical .autonomy/ convention.
// Companion to internal/executor's canonical-context pre-load (LLD
// §2 Deferred → Workspace canonicalisation) — once every workspace
// is on .autonomy/, the "mixed" / "plain_autonomy" source labels in
// telemetry stop firing and the dual-convention support in the
// resolver can sunset.
//
// Pure file operations; no daemon dependency. The CLI
// (vornikctl workspace canonicalise) calls this directly; the doctor
// check uses the same Scan walker to enumerate legacy workspaces.
package workspacecanonicalise

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// LegacyDir + CanonicalDir name the two conventions. Capitalised
// for re-use by the doctor check + tests.
const (
	LegacyDir    = "autonomy"
	CanonicalDir = ".autonomy"
)

// Outcome reports the per-workspace decision.
type Outcome string

const (
	// OutcomeMigrated — autonomy/ was renamed to .autonomy/.
	// Set on the dry-run path too when a real run would have
	// renamed.
	OutcomeMigrated Outcome = "migrated"

	// OutcomeAlreadyCanonical — .autonomy/ exists, no autonomy/
	// directory found. No action needed.
	OutcomeAlreadyCanonical Outcome = "already_canonical"

	// OutcomeNoConvention — neither directory exists. Project
	// doesn't use the autonomy-context convention.
	OutcomeNoConvention Outcome = "no_convention"

	// OutcomeMixed — both directories exist. Operator must
	// decide manually (we don't merge; risk of clobbering).
	OutcomeMixed Outcome = "mixed"

	// OutcomeError — an unexpected error during rename / stat.
	// The error string lives on Result.Error.
	OutcomeError Outcome = "error"
)

// Result is the outcome for one project workspace.
type Result struct {
	ProjectID    string
	WorkspaceDir string
	Outcome      Outcome
	Error        string // populated when Outcome == OutcomeError
}

// IsLegacy reports whether the workspace still has the legacy
// layout (autonomy/ present, .autonomy/ absent) — i.e. would be
// migrated by a non-dry-run pass. The doctor check uses this to
// surface the operator backlog.
func (r Result) IsLegacy() bool {
	return r.Outcome == OutcomeMigrated
}

// Scan walks the workspaces root and returns a Result per
// per-project subdirectory, WITHOUT performing any rename. Used by
// the doctor check + the dry-run mode of the CLI. Same code path
// CanonicaliseAll calls before deciding whether to rename, so the
// summary lines up perfectly with what a real run would do.
func Scan(workspacesRoot string) ([]Result, error) {
	return walk(workspacesRoot, true /* dryRun */)
}

// CanonicaliseAll walks every project workspace under
// workspacesRoot and renames autonomy/ → .autonomy/ where the
// migration applies. Returns the per-project results so the
// operator-facing CLI can print a summary.
//
// Skipped cases:
//   - mixed: both directories present, the operator must decide
//   - already canonical: no-op
//   - no convention: no autonomy/ in the workspace at all
func CanonicaliseAll(workspacesRoot string) ([]Result, error) {
	return walk(workspacesRoot, false)
}

// CanonicaliseOne migrates a single project's workspace. Useful
// for the CLI's --project <id> mode.
func CanonicaliseOne(workspacesRoot, projectID string, dryRun bool) (Result, error) {
	if workspacesRoot == "" {
		return Result{}, errors.New("workspaces root not configured")
	}
	if projectID == "" {
		return Result{}, errors.New("project id required")
	}
	workspaceDir := filepath.Join(workspacesRoot, projectID)
	if info, err := os.Stat(workspaceDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{ProjectID: projectID, WorkspaceDir: workspaceDir, Outcome: OutcomeNoConvention}, nil
		}
		return Result{ProjectID: projectID, WorkspaceDir: workspaceDir, Outcome: OutcomeError, Error: err.Error()}, nil
	} else if !info.IsDir() {
		return Result{ProjectID: projectID, WorkspaceDir: workspaceDir, Outcome: OutcomeError, Error: "workspace path is not a directory"}, nil
	}
	return canonicaliseDir(workspaceDir, projectID, dryRun), nil
}

// walk is the shared scanner / canonicaliser core. dryRun=true
// means "report what would happen"; dryRun=false performs renames.
func walk(workspacesRoot string, dryRun bool) ([]Result, error) {
	if workspacesRoot == "" {
		return nil, errors.New("workspaces root not configured")
	}
	entries, err := os.ReadDir(workspacesRoot)
	if err != nil {
		return nil, fmt.Errorf("read workspaces root %q: %w", workspacesRoot, err)
	}
	var out []Result
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip dotted top-level dirs (e.g. `.tmp/` scratch dirs
		// the runtime sometimes leaves around). A real project
		// workspace is always a bare ID.
		if len(e.Name()) > 0 && e.Name()[0] == '.' {
			continue
		}
		workspaceDir := filepath.Join(workspacesRoot, e.Name())
		out = append(out, canonicaliseDir(workspaceDir, e.Name(), dryRun))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectID < out[j].ProjectID })
	return out, nil
}

// canonicaliseDir runs the decision tree for one workspace.
// Pure function modulo the rename — the dryRun path doesn't
// touch the filesystem.
func canonicaliseDir(workspaceDir, projectID string, dryRun bool) Result {
	legacy := filepath.Join(workspaceDir, LegacyDir)
	canonical := filepath.Join(workspaceDir, CanonicalDir)

	legacyExists, _ := isDir(legacy)
	canonicalExists, _ := isDir(canonical)

	res := Result{ProjectID: projectID, WorkspaceDir: workspaceDir}
	switch {
	case legacyExists && canonicalExists:
		res.Outcome = OutcomeMixed
	case canonicalExists:
		res.Outcome = OutcomeAlreadyCanonical
	case legacyExists:
		res.Outcome = OutcomeMigrated
		if !dryRun {
			if err := os.Rename(legacy, canonical); err != nil {
				res.Outcome = OutcomeError
				res.Error = err.Error()
			}
		}
	default:
		res.Outcome = OutcomeNoConvention
	}
	return res
}

// isDir reports whether path exists AND is a directory. Symlinks
// are followed; the canonical-context resolver rejects symlinks
// inside the convention dir for safety reasons, but the convention
// dir itself can be a symlink (e.g. ops-mounted shared spec).
func isDir(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return info.IsDir(), nil
}

// CountLegacy returns the number of results whose Outcome is
// OutcomeMigrated. Used by the doctor check to surface the
// migration backlog without re-walking the directory.
func CountLegacy(results []Result) int {
	n := 0
	for _, r := range results {
		if r.IsLegacy() {
			n++
		}
	}
	return n
}

// CountMixed returns the number of workspaces with both
// directories present — those need manual operator attention.
func CountMixed(results []Result) int {
	n := 0
	for _, r := range results {
		if r.Outcome == OutcomeMixed {
			n++
		}
	}
	return n
}

// LegacyProjects returns the IDs of every workspace still on
// the legacy autonomy/ layout. Sorted for stable output.
func LegacyProjects(results []Result) []string {
	var out []string
	for _, r := range results {
		if r.IsLegacy() {
			out = append(out, r.ProjectID)
		}
	}
	sort.Strings(out)
	return out
}

// MixedProjects returns the IDs of workspaces with both
// directories present — operator must resolve manually.
func MixedProjects(results []Result) []string {
	var out []string
	for _, r := range results {
		if r.Outcome == OutcomeMixed {
			out = append(out, r.ProjectID)
		}
	}
	sort.Strings(out)
	return out
}
