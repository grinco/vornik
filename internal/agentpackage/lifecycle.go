package agentpackage

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Deployed subtrees each contribution kind writes into. They are the tree the
// DAEMON reads — an installer that writes the source tree installs nothing,
// which is the single most repeated operator-facing defect in this
// repository's history (design §3).
const (
	WorkflowsDir   = "workflows"
	RoleLibraryDir = "role-library"
)

// Contribution is one provenance row: what a package put where, and the hash
// of what it put.
type Contribution struct {
	Package string
	Kind    Kind
	RowID   string
	// Path is relative to the deployed configs dir, so a provenance row
	// survives the tree moving.
	Path                 string
	ContentHashAtInstall string
}

// PlannedItem is one file an install would write.
type PlannedItem struct {
	Kind       Kind
	RowID      string
	SourcePath string
	TargetPath string
	Bytes      []byte
	Hash       string
}

// InstallPlan is the whole install, computed before anything is written.
type InstallPlan struct {
	Manifest Manifest
	Items    []PlannedItem
}

// Conflict is a refusal to install.
type Conflict struct {
	Kind   Kind
	RowID  string
	Target string
	Reason string
}

func (c Conflict) String() string {
	return fmt.Sprintf("%s %q → %s: %s", c.Kind, c.RowID, c.Target, c.Reason)
}

// ConflictError carries every conflict, not just the first: an operator
// resolving them one refusal per attempt is an operator who stops upgrading.
type ConflictError struct {
	Conflicts []Conflict
}

func (e *ConflictError) Error() string {
	parts := make([]string, 0, len(e.Conflicts))
	for _, c := range e.Conflicts {
		parts = append(parts, c.String())
	}
	return fmt.Sprintf("%d conflict(s): %s", len(e.Conflicts), strings.Join(parts, "; "))
}

// Environment is the install's view of the world. Seams rather than direct
// filesystem calls, so the plan is a pure function of what it is told.
type Environment struct {
	// ReadPayload reads a file from the package's payload tree.
	ReadPayload func(relPath string) ([]byte, error)
	// DeployedExists reports whether a path (relative to the configs dir)
	// is already present in the DEPLOYED tree.
	DeployedExists func(relPath string) bool
	// ClaimedBy returns the package that already owns a contribution, if
	// any. It is the cross-check that keeps one file from having two
	// owners.
	ClaimedBy func(kind Kind, rowID string) (pkg string, claimed bool)
}

// PlanInstall resolves a manifest into the files it would write, or refuses.
//
// Nothing is written here, and the conflict set is complete: the caller either
// gets a plan it can apply or every reason it cannot.
func PlanInstall(m Manifest, env Environment) (*InstallPlan, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}

	plan := &InstallPlan{Manifest: m}
	var conflicts []Conflict

	add := func(kind Kind, dir string, sources []string) error {
		for _, src := range sources {
			rowID := contributionRowID(src)
			body, err := env.ReadPayload(src)
			if err != nil {
				return fmt.Errorf("read %s %q from the package: %w", kind, src, err)
			}
			target := filepath.Join(dir, rowID+".md")

			if owner, claimed := env.ClaimedBy(kind, rowID); claimed {
				conflicts = append(conflicts, Conflict{
					Kind: kind, RowID: rowID, Target: target,
					Reason: fmt.Sprintf("already contributed by package %q — two packages contributing one id is an operator decision, not something an installer resolves by ordering", owner),
				})
				continue
			}
			if env.DeployedExists(target) {
				conflicts = append(conflicts, Conflict{
					Kind: kind, RowID: rowID, Target: target,
					Reason: "a file is already deployed there and no package claims it; remove or rename it first rather than have an install overwrite an operator's own config",
				})
				continue
			}

			plan.Items = append(plan.Items, PlannedItem{
				Kind: kind, RowID: rowID, SourcePath: src, TargetPath: target,
				Bytes: body, Hash: ContentHash(body),
			})
		}
		return nil
	}

	if err := add(KindWorkflow, WorkflowsDir, m.Contributes.Workflows); err != nil {
		return nil, err
	}
	if err := add(KindRole, RoleLibraryDir, m.Contributes.Roles); err != nil {
		return nil, err
	}

	if len(conflicts) > 0 {
		sort.Slice(conflicts, func(i, j int) bool {
			if conflicts[i].Kind != conflicts[j].Kind {
				return conflicts[i].Kind < conflicts[j].Kind
			}
			return conflicts[i].RowID < conflicts[j].RowID
		})
		return nil, &ConflictError{Conflicts: conflicts}
	}
	return plan, nil
}

// contributionRowID is the id a contributed file registers under: its
// basename without the extension, which is how both the workflow registry and
// the role library already key their trees.
func contributionRowID(payloadPath string) string {
	base := filepath.Base(filepath.Clean(payloadPath))
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// Contributions converts a plan into the provenance rows an install records.
func (p *InstallPlan) Contributions() []Contribution {
	out := make([]Contribution, 0, len(p.Items))
	for _, it := range p.Items {
		out = append(out, Contribution{
			Package:              p.Manifest.Package,
			Kind:                 it.Kind,
			RowID:                it.RowID,
			Path:                 it.TargetPath,
			ContentHashAtInstall: it.Hash,
		})
	}
	return out
}

// UninstallVerdict is what uninstall does with one recorded contribution.
type UninstallVerdict string

const (
	// UninstallRemove — the file is still byte-for-byte what the package
	// put there.
	UninstallRemove UninstallVerdict = "remove"
	// UninstallRefuseEdited — the operator changed it. An operator who
	// tuned a contributed workflow must not lose the tuning to a package
	// lifecycle, and a package that silently overwrote it would be worse
	// than one that refuses.
	UninstallRefuseEdited UninstallVerdict = "refuse-edited"
	// UninstallCleanEntry — the operator deleted it. Not an error: the end
	// state uninstall wants is already true.
	UninstallCleanEntry UninstallVerdict = "clean-entry"
)

// ErrEdited is returned by Uninstall when a contributed file was edited.
var ErrEdited = errors.New("a contributed file was edited since install")

// DecideUninstall is the three-outcome table of design §3, as a pure function
// of what is on disk now.
func DecideUninstall(c Contribution, current []byte, exists bool) (UninstallVerdict, string) {
	if !exists {
		return UninstallCleanEntry, fmt.Sprintf("%s %q (%s) is already gone; clearing its provenance row", c.Kind, c.RowID, c.Path)
	}
	if got := ContentHash(current); got != c.ContentHashAtInstall {
		return UninstallRefuseEdited, fmt.Sprintf("%s %q (%s) has been edited since %s installed it — remove the edit, or delete the file yourself, then uninstall again", c.Kind, c.RowID, c.Path, c.Package)
	}
	return UninstallRemove, fmt.Sprintf("%s %q (%s) is unchanged since install; removing", c.Kind, c.RowID, c.Path)
}

// UninstallPlan is the decision for every recorded contribution.
type UninstallPlan struct {
	Package  string
	Remove   []Contribution
	Clean    []Contribution
	Edited   []Contribution
	Messages []string
}

// PlanUninstall decides every row, and REFUSES THE WHOLE UNINSTALL if any file
// was edited.
//
// Whole-set rather than per-file, because a partial uninstall leaves a package
// half-present with a provenance table that claims otherwise — and the
// operator's next `install` of the new version then conflicts on exactly the
// rows the partial uninstall could not remove, with no record of why.
func PlanUninstall(pkg string, rows []Contribution, read func(relPath string) ([]byte, bool)) (*UninstallPlan, error) {
	plan := &UninstallPlan{Package: pkg}
	for _, row := range rows {
		body, exists := read(row.Path)
		verdict, msg := DecideUninstall(row, body, exists)
		plan.Messages = append(plan.Messages, msg)
		switch verdict {
		case UninstallRemove:
			plan.Remove = append(plan.Remove, row)
		case UninstallCleanEntry:
			plan.Clean = append(plan.Clean, row)
		case UninstallRefuseEdited:
			plan.Edited = append(plan.Edited, row)
		}
	}
	if len(plan.Edited) > 0 {
		names := make([]string, 0, len(plan.Edited))
		for _, e := range plan.Edited {
			names = append(names, e.Path)
		}
		return plan, fmt.Errorf("%w: %s — uninstall of %q changed nothing", ErrEdited, strings.Join(names, ", "), pkg)
	}
	return plan, nil
}
