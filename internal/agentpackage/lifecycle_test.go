package agentpackage

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func env(payload map[string]string, deployed map[string]bool, claims map[string]string) Environment {
	return Environment{
		ReadPayload: func(p string) ([]byte, error) {
			b, ok := payload[p]
			if !ok {
				return nil, fmt.Errorf("no such file in package: %s", p)
			}
			return []byte(b), nil
		},
		DeployedExists: func(p string) bool { return deployed[p] },
		ClaimedBy: func(k Kind, id string) (string, bool) {
			pkg, ok := claims[string(k)+"/"+id]
			return pkg, ok
		},
	}
}

func sample() Manifest {
	return Manifest{
		Package: "acme-incident-response",
		Version: "1.2.0",
		Contributes: Contributions{
			Workflows: []string{"incident-triage.md"},
			Roles:     []string{"incident-lead.md"},
		},
	}
}

func TestPlanInstallTargetsTheDeployedTree(t *testing.T) {
	plan, err := PlanInstall(sample(), env(map[string]string{
		"incident-triage.md": "# triage\n",
		"incident-lead.md":   "# lead\n",
	}, nil, nil))
	if err != nil {
		t.Fatalf("PlanInstall() = %v", err)
	}
	if len(plan.Items) != 2 {
		t.Fatalf("plan has %d items, want 2", len(plan.Items))
	}

	byKind := map[Kind]PlannedItem{}
	for _, it := range plan.Items {
		byKind[it.Kind] = it
	}
	// An installer that writes the SOURCE tree installs nothing, because
	// the daemon reads only the deployed copy.
	if got := byKind[KindWorkflow].TargetPath; got != "workflows/incident-triage.md" {
		t.Fatalf("workflow target = %q", got)
	}
	if got := byKind[KindRole].TargetPath; got != "role-library/incident-lead.md" {
		t.Fatalf("role target = %q", got)
	}
	if byKind[KindWorkflow].RowID != "incident-triage" {
		t.Fatalf("row id = %q, want the basename without its extension", byKind[KindWorkflow].RowID)
	}
}

func TestPlanInstallRecordsAHashPerContributedRow(t *testing.T) {
	plan, err := PlanInstall(sample(), env(map[string]string{
		"incident-triage.md": "# triage\n",
		"incident-lead.md":   "# lead\n",
	}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	rows := plan.Contributions()
	if len(rows) != 2 {
		t.Fatalf("%d provenance rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Package != "acme-incident-response" {
			t.Fatalf("row %+v has the wrong package", r)
		}
		if r.ContentHashAtInstall == "" {
			// Without the hash, uninstall cannot tell an untouched file
			// from an operator's tuned one — the prior art's blind spot.
			t.Fatalf("row %+v carries no content hash", r)
		}
		if r.RowID == "" || r.Path == "" {
			t.Fatalf("row %+v is incomplete", r)
		}
	}
	if rows[0].ContentHashAtInstall == rows[1].ContentHashAtInstall {
		t.Fatal("two different files recorded the same hash")
	}
}

func TestPlanInstallRefusesAConflictRatherThanOrderingIt(t *testing.T) {
	// Two packages contributing one workflow id is an operator decision,
	// not something an installer resolves by ordering.
	_, err := PlanInstall(sample(), env(
		map[string]string{"incident-triage.md": "a", "incident-lead.md": "b"},
		nil,
		map[string]string{"workflow/incident-triage": "globex-oncall"},
	))
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("PlanInstall() = %v, want a ConflictError", err)
	}
	if !strings.Contains(err.Error(), "globex-oncall") {
		t.Fatalf("the refusal must name the other package, got %q", err)
	}
}

func TestPlanInstallRefusesToOverwriteAnUnclaimedFile(t *testing.T) {
	// No package claims it, so it is the operator's own config. An
	// install that silently replaced it would be worse than one that
	// refuses.
	_, err := PlanInstall(sample(), env(
		map[string]string{"incident-triage.md": "a", "incident-lead.md": "b"},
		map[string]bool{"workflows/incident-triage.md": true},
		nil,
	))
	if err == nil {
		t.Fatal("PlanInstall() = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "no package claims it") {
		t.Fatalf("err = %q, want it to distinguish an operator's file from a package's", err)
	}
}

func TestPlanInstallReportsEveryConflictAtOnce(t *testing.T) {
	// An operator resolving conflicts one refusal per attempt is an
	// operator who stops upgrading.
	_, err := PlanInstall(sample(), env(
		map[string]string{"incident-triage.md": "a", "incident-lead.md": "b"},
		map[string]bool{"workflows/incident-triage.md": true, "role-library/incident-lead.md": true},
		nil,
	))
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("PlanInstall() = %v, want a ConflictError", err)
	}
	if len(ce.Conflicts) != 2 {
		t.Fatalf("%d conflicts reported, want both", len(ce.Conflicts))
	}
}

func TestPlanInstallSurfacesAMissingPayloadFile(t *testing.T) {
	_, err := PlanInstall(sample(), env(map[string]string{"incident-triage.md": "a"}, nil, nil))
	if err == nil || !strings.Contains(err.Error(), "incident-lead.md") {
		t.Fatalf("PlanInstall() = %v, want the missing payload named", err)
	}
}

func TestPlanInstallValidatesBeforePlanning(t *testing.T) {
	m := sample()
	m.Contributes.MCPServers = []string{"pagerduty.yaml"}
	if _, err := PlanInstall(m, env(nil, nil, nil)); err == nil {
		t.Fatal("PlanInstall() accepted a manifest Validate refuses")
	}
}

func TestDecideUninstallThreeOutcomes(t *testing.T) {
	row := Contribution{
		Package: "acme", Kind: KindWorkflow, RowID: "incident-triage",
		Path: "workflows/incident-triage.md", ContentHashAtInstall: ContentHash([]byte("# triage\n")),
	}

	t.Run("unchanged is removed", func(t *testing.T) {
		v, msg := DecideUninstall(row, []byte("# triage\n"), true)
		if v != UninstallRemove {
			t.Fatalf("verdict = %q, want remove (%s)", v, msg)
		}
	})

	t.Run("edited is refused and named", func(t *testing.T) {
		// An operator who tuned a contributed workflow must not lose the
		// tuning to a package lifecycle.
		v, msg := DecideUninstall(row, []byte("# triage\n# my tuning\n"), true)
		if v != UninstallRefuseEdited {
			t.Fatalf("verdict = %q, want refuse-edited", v)
		}
		if !strings.Contains(msg, row.Path) {
			t.Fatalf("msg = %q, want it to name the file", msg)
		}
	})

	t.Run("already gone cleans the row without an error", func(t *testing.T) {
		v, _ := DecideUninstall(row, nil, false)
		if v != UninstallCleanEntry {
			t.Fatalf("verdict = %q, want clean-entry: the end state is already true", v)
		}
	})
}

func TestPlanUninstallRefusesTheWholeSetWhenOneFileWasEdited(t *testing.T) {
	// A partial uninstall leaves a package half-present with a provenance
	// table that claims otherwise, and the operator's next install then
	// conflicts on exactly the rows it could not remove.
	rows := []Contribution{
		{Package: "acme", Kind: KindWorkflow, RowID: "a", Path: "workflows/a.md", ContentHashAtInstall: ContentHash([]byte("a"))},
		{Package: "acme", Kind: KindRole, RowID: "b", Path: "role-library/b.md", ContentHashAtInstall: ContentHash([]byte("b"))},
	}
	plan, err := PlanUninstall("acme", rows, func(p string) ([]byte, bool) {
		if p == "role-library/b.md" {
			return []byte("b -- tuned by the operator"), true
		}
		return []byte("a"), true
	})
	if !errors.Is(err, ErrEdited) {
		t.Fatalf("PlanUninstall() = %v, want ErrEdited", err)
	}
	if !strings.Contains(err.Error(), "role-library/b.md") {
		t.Fatalf("err = %q, want the edited file named", err)
	}
	if !strings.Contains(err.Error(), "changed nothing") {
		t.Fatalf("err = %q, want it to say the uninstall did not proceed", err)
	}
	// The plan is still returned so a caller can show the whole picture.
	if len(plan.Remove) != 1 || len(plan.Edited) != 1 {
		t.Fatalf("plan = %+v, want one removable and one edited", plan)
	}
}

func TestPlanUninstallCleansRowsForFilesTheOperatorDeleted(t *testing.T) {
	rows := []Contribution{
		{Package: "acme", Kind: KindWorkflow, RowID: "a", Path: "workflows/a.md", ContentHashAtInstall: ContentHash([]byte("a"))},
		{Package: "acme", Kind: KindRole, RowID: "b", Path: "role-library/b.md", ContentHashAtInstall: ContentHash([]byte("b"))},
	}
	plan, err := PlanUninstall("acme", rows, func(p string) ([]byte, bool) {
		if p == "workflows/a.md" {
			return nil, false
		}
		return []byte("b"), true
	})
	if err != nil {
		t.Fatalf("PlanUninstall() = %v, want a deleted file to be no error", err)
	}
	if len(plan.Clean) != 1 || len(plan.Remove) != 1 {
		t.Fatalf("plan = %+v, want one cleaned row and one removal", plan)
	}
	if len(plan.Messages) != 2 {
		t.Fatalf("%d messages, want one per row", len(plan.Messages))
	}
}
