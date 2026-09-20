package api

import (
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/projectdeps"
)

func TestProjectDependenciesUnwiredReportsSkippedNotOK(t *testing.T) {
	// 2026-08-26-doctor-skipped-vs-ok-design: a check that cannot
	// distinguish "examined and clean" from "never examined" reports the
	// first and means the second.
	got := (&DoctorHandlers{}).checkProjectDependencies()

	if got.Status != "SKIPPED" {
		t.Fatalf("Status = %q, want SKIPPED", got.Status)
	}
	if !strings.Contains(got.Message, "NOT a statement") {
		t.Fatalf("Message = %q, want it to disclaim what it did not check", got.Message)
	}
}

func inventory(items ...ProjectDependencyStatus) DependencyInventory {
	return func() []ProjectDependencyStatus { return items }
}

func TestProjectDependenciesWithNoManifestsIsOK(t *testing.T) {
	h := &DoctorHandlers{}
	h.SetDependencyInventory(inventory())

	got := h.checkProjectDependencies()
	if got.Status != "OK" {
		t.Fatalf("Status = %q, want OK: no manifest is the documented default", got.Status)
	}
}

func TestProjectDependenciesAllMaterialisedIsOK(t *testing.T) {
	h := &DoctorHandlers{}
	h.SetDependencyInventory(inventory(ProjectDependencyStatus{
		ProjectID: "headmatch",
		Plans: []projectdeps.Plan{
			{Entry: projectdeps.Entry{Ecosystem: projectdeps.EcosystemPip}, Key: "pip-linux-amd64-abc", Materialised: true},
		},
	}))

	got := h.checkProjectDependencies()
	if got.Status != "OK" {
		t.Fatalf("Status = %q (%s), want OK", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "1 declared dependency sets") {
		t.Fatalf("Message = %q, want the count it examined", got.Message)
	}
}

func TestProjectDependenciesPendingWarnsAndNamesTheRemedy(t *testing.T) {
	// Not a defect — a fresh install, a changed lockfile, or an
	// air-gapped deployment awaiting an import. It IS a statement that
	// the reviewer case does not work right now.
	h := &DoctorHandlers{}
	h.SetDependencyInventory(inventory(ProjectDependencyStatus{
		ProjectID: "headmatch",
		Plans: []projectdeps.Plan{
			{Entry: projectdeps.Entry{Ecosystem: projectdeps.EcosystemPip}, Key: "pip-linux-amd64-abc"},
		},
	}))

	got := h.checkProjectDependencies()
	if got.Status != "WARNING" {
		t.Fatalf("Status = %q, want WARNING", got.Status)
	}
	for _, want := range []string{"headmatch", "pip-linux-amd64-abc", "vornikctl deps import"} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("Message = %q, want it to carry %q", got.Message, want)
		}
	}
}

func TestProjectDependenciesBrokenManifestIsAnError(t *testing.T) {
	h := &DoctorHandlers{}
	h.SetDependencyInventory(inventory(
		ProjectDependencyStatus{
			ProjectID: "headmatch",
			Plans: []projectdeps.Plan{
				{Entry: projectdeps.Entry{Ecosystem: projectdeps.EcosystemPip}, Problem: errors.New("requirements.lock:4: carries no --hash=")},
			},
		},
		ProjectDependencyStatus{
			ProjectID: "othersvc",
			Plans: []projectdeps.Plan{
				{Entry: projectdeps.Entry{Ecosystem: projectdeps.EcosystemPip}, Key: "pip-linux-amd64-def"},
			},
		},
	))

	got := h.checkProjectDependencies()
	if got.Status != "ERROR" {
		t.Fatalf("Status = %q, want ERROR", got.Status)
	}
	if !strings.Contains(got.Message, "carries no --hash=") {
		t.Fatalf("Message = %q, want the underlying reason", got.Message)
	}
	// The pending one must still be counted: an ERROR on one project must
	// not hide that another is also not ready.
	if !strings.Contains(got.Message, "a further 1 are sound but not yet materialised") {
		t.Fatalf("Message = %q, want the pending count kept", got.Message)
	}
}
