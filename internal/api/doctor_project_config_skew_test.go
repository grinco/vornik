package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// UPGRADE SAFETY (BACKLOG P1 2026-09-12, slice 1). The check that turns "my
// project is gone" into "the upgrade rejected a key, here is which and what to
// do" — see doctor_project_config_skew.go for the incident it comes from.

func skewDir(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(projects, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCheckProjectConfigSkew_CleanTreeReportsWhatItExamined(t *testing.T) {
	h := &DoctorHandlers{configDir: skewDir(t, map[string]string{
		"a.yaml": "projectId: a\n",
		"b.yaml": "projectId: b\n",
	})}

	got := h.checkProjectConfigSkew(false)
	if got.Status != "OK" {
		t.Fatalf("Status = %s (%s), want OK", got.Status, got.Message)
	}
	// The DENOMINATOR is the point: "clean" must say how much was looked at,
	// or it is indistinguishable from "looked at nothing".
	if !strings.Contains(got.Message, "2 project file(s) examined") {
		t.Errorf("a clean result must publish what it examined, got %q", got.Message)
	}
}

func TestCheckProjectConfigSkew_UnknownKeyIsAnErrorNamingTheProject(t *testing.T) {
	h := &DoctorHandlers{configDir: skewDir(t, map[string]string{
		"ok.yaml":        "projectId: ok\n",
		"assistant.yaml": "projectId: assistant\nautonomy:\n  feed_cadence_budget: 5\n",
	})}

	got := h.checkProjectConfigSkew(false)
	// ERROR, not WARNING: the project does not exist right now. This is an
	// outage being reported, not a risk being flagged.
	if got.Status != "ERROR" {
		t.Fatalf("Status = %s (%s), want ERROR", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "1 of 2") {
		t.Errorf("Message must scope the damage against what was examined, got %q", got.Message)
	}
	if !strings.Contains(got.Message, "NEWER release") {
		t.Errorf("an unknown key must be read as version skew in the summary, got %q", got.Message)
	}
	joined := strings.Join(got.Items, "\n")
	for _, want := range []string{"assistant", "feed_cadence_budget", "deploy the binary"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the item must carry %q:\n%s", want, joined)
		}
	}
}

// NOT CHECKED must not render as CHECKED AND CLEAN. This check exists because
// an absent signal was read as a healthy one; it must not repeat that.
func TestCheckProjectConfigSkew_UnwiredConfigDirSkipsLoudly(t *testing.T) {
	got := (&DoctorHandlers{}).checkProjectConfigSkew(false)
	if got.Status != "SKIPPED" {
		t.Fatalf("Status = %s, want SKIPPED with no config dir", got.Status)
	}
	if !strings.Contains(got.Message, "NOT a statement") {
		t.Errorf("a skip must say what it does NOT mean, got %q", got.Message)
	}
}

// A file the check cannot read is reported as not examined, and never counted
// among the ones that loaded.
func TestCheckProjectConfigSkew_UnreadableFileIsNotCountedAsClean(t *testing.T) {
	root := skewDir(t, map[string]string{"a.yaml": "projectId: a\n", "locked.yaml": "projectId: locked\n"})
	if err := os.Chmod(filepath.Join(root, "projects", "locked.yaml"), 0o000); err != nil {
		t.Skipf("cannot make a file unreadable here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "projects", "locked.yaml"), 0o644) })
	if os.Geteuid() == 0 {
		t.Skip("running as root; mode 000 is still readable")
	}

	got := (&DoctorHandlers{configDir: root}).checkProjectConfigSkew(false)
	if got.Status == "OK" {
		t.Fatalf("an unreadable project file must not produce a clean result: %+v", got)
	}
	if !strings.Contains(strings.Join(got.Items, "\n"), "NOT examined") {
		t.Errorf("the unreadable file must be named as not examined: %v", got.Items)
	}
}

// §13.4: --fix repairs a misspelt key and nothing else. Without the flag the
// file is untouched — a doctor that edits a customer's config when asked only
// to look is the failure this bound exists to prevent.
func TestCheckProjectConfigSkew_FixRepairsAMisspeltKeyOnly(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projects, "demo.yaml")
	const src = "projectId: demo\ndisplay_name: Demo\nswarmId: dev-swarm\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without --fix: reported, not touched.
	if got := (&DoctorHandlers{configDir: root}).checkProjectConfigSkew(false); got.Status != "ERROR" {
		t.Fatalf("want ERROR without --fix, got %s", got.Status)
	}
	after, _ := os.ReadFile(path)
	if string(after) != src {
		t.Fatalf("the check edited the file without --fix:\n%s", after)
	}

	// With --fix: repaired, and the repair is reported.
	got := (&DoctorHandlers{configDir: root}).checkProjectConfigSkew(true)
	if got.Status != "OK" {
		t.Fatalf("want OK after repair, got %s: %s\n%v", got.Status, got.Message, got.Items)
	}
	healed, _ := os.ReadFile(path)
	if !strings.Contains(string(healed), "displayName: Demo") {
		t.Fatalf("the key was not repaired:\n%s", healed)
	}
	if !strings.Contains(got.Message, "repaired") || len(got.Items) == 0 {
		t.Fatalf("the repair was not reported: %s %v", got.Message, got.Items)
	}
}

// A file with a misspelling AND a key unknown under every spelling is NOT
// fixed, and must not be reported as fixed — the misspelling is repaired, the
// unknown key stands, and the check stays ERROR.
func TestCheckProjectConfigSkew_FixDoesNotClaimAPartialRepairIsClean(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projects, "demo.yaml")
	src := "projectId: demo\ndisplay_name: Demo\nswarmId: dev-swarm\nbrandNewKeyFromTheFuture: 1\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	got := (&DoctorHandlers{configDir: root}).checkProjectConfigSkew(true)
	if got.Status != "ERROR" {
		t.Fatalf("a partially repaired file was reported as %s", got.Status)
	}
	healed, _ := os.ReadFile(path)
	if !strings.Contains(string(healed), "displayName: Demo") {
		t.Fatalf("the misspelling was not repaired:\n%s", healed)
	}
	if !strings.Contains(string(healed), "brandNewKeyFromTheFuture: 1") {
		t.Fatalf("an unknown key was removed — that is the repair §11 refuses:\n%s", healed)
	}
}
