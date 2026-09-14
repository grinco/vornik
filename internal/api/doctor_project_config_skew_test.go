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

	got := h.checkProjectConfigSkew()
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

	got := h.checkProjectConfigSkew()
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
	got := (&DoctorHandlers{}).checkProjectConfigSkew()
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

	got := (&DoctorHandlers{configDir: root}).checkProjectConfigSkew()
	if got.Status == "OK" {
		t.Fatalf("an unreadable project file must not produce a clean result: %+v", got)
	}
	if !strings.Contains(strings.Join(got.Items, "\n"), "NOT examined") {
		t.Errorf("the unreadable file must be named as not examined: %v", got.Items)
	}
}
