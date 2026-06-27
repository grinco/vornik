package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installFixtureLocal lays a local-file install down on disk so
// lifecycle tests (list / remove / info / update) operate on a
// real ledger row.
func installFixtureLocal(t *testing.T) (configsDir, ledgerPath, srcPath string) {
	t.Helper()
	configsDir, ledgerPath = buildInstallTestEnv(t)
	fixDir := t.TempDir()
	srcPath = filepath.Join(fixDir, "skill.md")
	if err := os.WriteFile(srcPath, []byte(installFixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := runSkillInstallForTest(t, srcPath, func() {}); err != nil {
		t.Fatalf("install: %v", err)
	}
	return
}

func runLifecycleCmd(t *testing.T, run func(out, errBuf *bytes.Buffer) error) (string, string, error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	err := run(&out, &errBuf)
	return out.String(), errBuf.String(), err
}

func TestSkillList_Empty(t *testing.T) {
	_, _ = buildInstallTestEnv(t)
	out, _, err := runLifecycleCmd(t, func(out, errBuf *bytes.Buffer) error {
		skillListCmd.SetOut(out)
		skillListCmd.SetErr(errBuf)
		skillListJSON = false
		return runSkillList(skillListCmd, nil)
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "No skills installed") {
		t.Errorf("empty list output:\n%s", out)
	}
}

func TestSkillList_AfterInstall(t *testing.T) {
	installFixtureLocal(t)
	out, _, err := runLifecycleCmd(t, func(out, errBuf *bytes.Buffer) error {
		skillListCmd.SetOut(out)
		skillListCmd.SetErr(errBuf)
		skillListJSON = false
		return runSkillList(skillListCmd, nil)
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "research-skill") {
		t.Errorf("list missing installed entry:\n%s", out)
	}
	if !strings.Contains(out, "Total: 1") {
		t.Errorf("list missing total:\n%s", out)
	}
}

func TestSkillList_JSON(t *testing.T) {
	installFixtureLocal(t)
	out, _, err := runLifecycleCmd(t, func(out, errBuf *bytes.Buffer) error {
		skillListCmd.SetOut(out)
		skillListCmd.SetErr(errBuf)
		skillListJSON = true
		defer func() { skillListJSON = false }()
		return runSkillList(skillListCmd, nil)
	})
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	if !strings.Contains(out, "\"handle\"") {
		t.Errorf("json list missing fields:\n%s", out)
	}
}

func TestSkillInfo_HappyPath(t *testing.T) {
	installFixtureLocal(t)
	out, _, err := runLifecycleCmd(t, func(out, errBuf *bytes.Buffer) error {
		skillInfoCmd.SetOut(out)
		skillInfoCmd.SetErr(errBuf)
		skillInfoJSON = false
		return runSkillInfo(skillInfoCmd, []string{"local/research-skill"})
	})
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if !strings.Contains(out, "local/research-skill") {
		t.Errorf("info missing identity:\n%s", out)
	}
	if !strings.Contains(out, "version:") {
		t.Errorf("info missing version line:\n%s", out)
	}
}

func TestSkillInfo_NotInstalled(t *testing.T) {
	_, _ = buildInstallTestEnv(t)
	_, _, err := runLifecycleCmd(t, func(out, errBuf *bytes.Buffer) error {
		skillInfoCmd.SetOut(out)
		skillInfoCmd.SetErr(errBuf)
		return runSkillInfo(skillInfoCmd, []string{"ghost/nope"})
	})
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("expected not-installed error, got %v", err)
	}
}

func TestSkillRemove_DropsFilesAndLedger(t *testing.T) {
	configsDir, ledgerPath, _ := installFixtureLocal(t)
	wfPath := filepath.Join(configsDir, "workflows", "skill__local__research-skill__research.md")
	if _, err := os.Stat(wfPath); err != nil {
		t.Fatalf("install precondition: %v", err)
	}

	_, _, err := runLifecycleCmd(t, func(out, errBuf *bytes.Buffer) error {
		skillRemoveCmd.SetOut(out)
		skillRemoveCmd.SetErr(errBuf)
		return runSkillRemove(skillRemoveCmd, []string{"local/research-skill"})
	})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(wfPath); err == nil {
		t.Errorf("workflow file should be gone")
	}
	ledger, _ := LoadSkillLedger(ledgerPath)
	if len(ledger.Skills) != 0 {
		t.Errorf("ledger row not removed: %d", len(ledger.Skills))
	}
}

func TestSkillRemove_NotInstalled(t *testing.T) {
	_, _ = buildInstallTestEnv(t)
	_, _, err := runLifecycleCmd(t, func(out, errBuf *bytes.Buffer) error {
		skillRemoveCmd.SetOut(out)
		skillRemoveCmd.SetErr(errBuf)
		return runSkillRemove(skillRemoveCmd, []string{"ghost/nope"})
	})
	if err == nil {
		t.Errorf("expected error for missing skill")
	}
}

func TestSkillUpdate_All(t *testing.T) {
	configsDir, _, _ := installFixtureLocal(t)
	wfPath := filepath.Join(configsDir, "workflows", "skill__local__research-skill__research.md")
	originalBytes, err := os.ReadFile(wfPath)
	if err != nil {
		t.Fatalf("read post-install: %v", err)
	}

	// Re-run update with --all; should be idempotent (same bytes).
	_, _, err = runLifecycleCmd(t, func(out, errBuf *bytes.Buffer) error {
		skillUpdateCmd.SetOut(out)
		skillUpdateCmd.SetErr(errBuf)
		skillUpdateAll = true
		defer func() { skillUpdateAll = false }()
		return runSkillUpdate(skillUpdateCmd, nil)
	})
	if err != nil {
		t.Fatalf("update --all: %v", err)
	}
	updatedBytes, err := os.ReadFile(wfPath)
	if err != nil {
		t.Fatalf("read post-update: %v", err)
	}
	if !bytes.Equal(originalBytes, updatedBytes) {
		t.Errorf("update should be byte-stable when source unchanged")
	}
}

func TestSkillUpdate_RequiresTarget(t *testing.T) {
	installFixtureLocal(t)
	_, _, err := runLifecycleCmd(t, func(out, errBuf *bytes.Buffer) error {
		skillUpdateCmd.SetOut(out)
		skillUpdateCmd.SetErr(errBuf)
		skillUpdateAll = false
		return runSkillUpdate(skillUpdateCmd, nil)
	})
	if err == nil || !strings.Contains(err.Error(), "--all") {
		t.Errorf("update with no args + no --all should error, got %v", err)
	}
}

func TestSplitInstalledSkillRef(t *testing.T) {
	cases := map[string]bool{
		"vadim/research": true,
		"local/research": true,
		"missing-slash":  false,
		"vadim/":         false,
		"/research":      false,
	}
	for ref, ok := range cases {
		_, _, err := splitInstalledSkillRef(ref)
		if (err == nil) != ok {
			t.Errorf("splitInstalledSkillRef(%q) err=%v want ok=%v", ref, err, ok)
		}
	}
}
