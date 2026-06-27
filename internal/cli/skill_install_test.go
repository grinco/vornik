package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildInstallTestEnv lays out a minimal configs tree, points
// the ledger path env at a tmp file, and returns both paths.
func buildInstallTestEnv(t *testing.T) (configsDir, ledgerPath string) {
	t.Helper()
	configsDir = t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(configsDir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	ledgerPath = filepath.Join(t.TempDir(), "installed-skills.yaml")
	t.Setenv("VORNIK_CONFIGS_DIR", configsDir)
	t.Setenv("VORNIK_SKILL_LEDGER_PATH", ledgerPath)
	return configsDir, ledgerPath
}

const installFixture = `---
name: research-skill
description: Research and write.
version: 1.0.0
author: vadim
license: MIT
metadata:
  vornik:
    schema_version: 1
    workflow:
      workflowId: research
      entrypoint: research
      steps:
        research:
          type: agent
          role: researcher
    roles:
      - name: researcher
---

# Research

## Prompts

### research

Find facts.

## Role prompts

### researcher

You are a researcher.
`

// runSkillInstallForTest invokes runSkillInstall directly with
// a caller-controlled flag set; the package-level flags are
// snapshot/restored so consecutive tests don't bleed.
func runSkillInstallForTest(t *testing.T, source string, configure func()) error {
	t.Helper()
	prev := struct {
		registry, handle, skill string
		force, dryRun, allowSrc bool
	}{skillInstallRegistry, skillInstallHandle, skillInstallSkillName, skillInstallForce, skillInstallDryRun, skillInstallAllowSourceChange}
	defer func() {
		skillInstallRegistry = prev.registry
		skillInstallHandle = prev.handle
		skillInstallSkillName = prev.skill
		skillInstallForce = prev.force
		skillInstallDryRun = prev.dryRun
		skillInstallAllowSourceChange = prev.allowSrc
	}()
	skillInstallRegistry = ""
	skillInstallHandle = ""
	skillInstallSkillName = ""
	skillInstallForce = false
	skillInstallDryRun = false
	skillInstallAllowSourceChange = false
	configure()

	skillInstallCmd.SetOut(os.Stderr)
	skillInstallCmd.SetErr(os.Stderr)
	return runSkillInstall(skillInstallCmd, []string{source})
}

func TestSkillInstall_LocalFile(t *testing.T) {
	configsDir, ledgerPath := buildInstallTestEnv(t)

	fixDir := t.TempDir()
	src := filepath.Join(fixDir, "skill.md")
	if err := os.WriteFile(src, []byte(installFixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if err := runSkillInstallForTest(t, src, func() {}); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Verify workflow + swarm landed under namespaced IDs.
	wfPath := filepath.Join(configsDir, "workflows", "skill__local__research-skill__research.md")
	if _, err := os.Stat(wfPath); err != nil {
		t.Errorf("namespaced workflow not at %s: %v", wfPath, err)
	}
	swPath := filepath.Join(configsDir, "swarms", "skill__local__research-skill.md")
	if _, err := os.Stat(swPath); err != nil {
		t.Errorf("namespaced swarm not at %s: %v", swPath, err)
	}

	// Verify ledger row.
	ledger, err := LoadSkillLedger(ledgerPath)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	if len(ledger.Skills) != 1 {
		t.Fatalf("ledger rows: got %d want 1", len(ledger.Skills))
	}
	row := ledger.Skills[0]
	if row.Handle != "local" || row.Skill != "research-skill" {
		t.Errorf("ledger identity: got (%q,%q)", row.Handle, row.Skill)
	}
	if row.SkillVersion != "1.0.0" {
		t.Errorf("ledger version: got %q", row.SkillVersion)
	}
}

func TestSkillInstall_DryRunDoesNotWrite(t *testing.T) {
	configsDir, ledgerPath := buildInstallTestEnv(t)

	fixDir := t.TempDir()
	src := filepath.Join(fixDir, "skill.md")
	_ = os.WriteFile(src, []byte(installFixture), 0o644)

	if err := runSkillInstallForTest(t, src, func() {
		skillInstallDryRun = true
	}); err != nil {
		t.Fatalf("dry-run install: %v", err)
	}

	if _, err := os.Stat(filepath.Join(configsDir, "workflows", "skill__local__research-skill__research.md")); err == nil {
		t.Errorf("dry-run wrote workflow file")
	}
	if _, err := os.Stat(ledgerPath); err == nil {
		t.Errorf("dry-run wrote ledger")
	}
}

func TestSkillInstall_DuplicateRejected(t *testing.T) {
	_, _ = buildInstallTestEnv(t)
	fixDir := t.TempDir()
	src := filepath.Join(fixDir, "skill.md")
	_ = os.WriteFile(src, []byte(installFixture), 0o644)

	if err := runSkillInstallForTest(t, src, func() {}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	err := runSkillInstallForTest(t, src, func() {})
	if err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Errorf("want already-installed error, got %v", err)
	}
}

func TestSkillInstall_ForceOverwrites(t *testing.T) {
	_, ledgerPath := buildInstallTestEnv(t)
	fixDir := t.TempDir()
	src := filepath.Join(fixDir, "skill.md")
	_ = os.WriteFile(src, []byte(installFixture), 0o644)

	if err := runSkillInstallForTest(t, src, func() {}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := runSkillInstallForTest(t, src, func() {
		skillInstallForce = true
	}); err != nil {
		t.Fatalf("forced re-install: %v", err)
	}
	ledger, _ := LoadSkillLedger(ledgerPath)
	if len(ledger.Skills) != 1 {
		t.Errorf("re-install should not create a second row: %d", len(ledger.Skills))
	}
}

func TestSkillSourceDrift(t *testing.T) {
	cases := []struct {
		name       string
		prior      SkillLedgerEntry
		result     *SkillSourceResult
		newChecks  string
		wantDrift  bool
		wantDetail string
	}{
		{
			name:      "first install, nothing pinned",
			prior:     SkillLedgerEntry{},
			result:    &SkillSourceResult{ResolvedSHA: "abc"},
			newChecks: "sum1",
			wantDrift: false,
		},
		{
			name:      "same git commit",
			prior:     SkillLedgerEntry{SourceCommitSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", SourceFileChecksum: "sum1"},
			result:    &SkillSourceResult{ResolvedSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
			newChecks: "sum1",
			wantDrift: false,
		},
		{
			name:       "git commit moved",
			prior:      SkillLedgerEntry{SourceCommitSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
			result:     &SkillSourceResult{ResolvedSHA: "feedfacefeedfacefeedfacefeedfacefeedface"},
			newChecks:  "sum2",
			wantDrift:  true,
			wantDetail: "commit",
		},
		{
			name:       "non-git content changed",
			prior:      SkillLedgerEntry{SourceFileChecksum: "sum1"},
			result:     &SkillSourceResult{},
			newChecks:  "sum2",
			wantDrift:  true,
			wantDetail: "content",
		},
		{
			name:      "non-git content unchanged",
			prior:     SkillLedgerEntry{SourceFileChecksum: "sum1"},
			result:    &SkillSourceResult{},
			newChecks: "sum1",
			wantDrift: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drift, detail := skillSourceDrift(tc.prior, tc.result, tc.newChecks)
			if drift != tc.wantDrift {
				t.Fatalf("drift = %v want %v (detail %q)", drift, tc.wantDrift, detail)
			}
			if tc.wantDetail != "" && !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("detail %q does not contain %q", detail, tc.wantDetail)
			}
		})
	}
}

// A forced re-install over a pin whose source content changed must be
// refused unless the operator passes --allow-source-change (resolve-and-pin
// supply-chain guard, Option A). Uses a local source so drift falls to the
// content-checksum path (no git SHA available).
func TestSkillInstall_SourceChangeRefusedWithoutFlag(t *testing.T) {
	_, ledgerPath := buildInstallTestEnv(t)
	fixDir := t.TempDir()
	src := filepath.Join(fixDir, "skill.md")
	if err := os.WriteFile(src, []byte(installFixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := runSkillInstallForTest(t, src, func() {}); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// Mutate the source body (same valid frontmatter → new checksum).
	mutated := installFixture + "\nExtra prose that changes the checksum.\n"
	if err := os.WriteFile(src, []byte(mutated), 0o644); err != nil {
		t.Fatalf("rewrite fixture: %v", err)
	}

	// --force alone is not enough: drift must be acknowledged.
	err := runSkillInstallForTest(t, src, func() { skillInstallForce = true })
	if err == nil || !strings.Contains(err.Error(), "changed source") {
		t.Fatalf("want changed-source refusal, got %v", err)
	}

	// With the explicit flag it proceeds and re-pins the new checksum.
	if err := runSkillInstallForTest(t, src, func() {
		skillInstallForce = true
		skillInstallAllowSourceChange = true
	}); err != nil {
		t.Fatalf("install with --allow-source-change: %v", err)
	}
	ledger, _ := LoadSkillLedger(ledgerPath)
	if len(ledger.Skills) != 1 {
		t.Fatalf("ledger rows: got %d want 1", len(ledger.Skills))
	}
	if got := ledger.Skills[0].SourceFileChecksum; got != skillSourceChecksum([]byte(mutated)) {
		t.Errorf("checksum not re-pinned to mutated source: got %q", got)
	}
}

func TestSkillInstall_HTTPSSource(t *testing.T) {
	configsDir, _ := buildInstallTestEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(installFixture))
	}))
	defer srv.Close()

	if err := runSkillInstallForTest(t, srv.URL+"/skill.md", func() {
		skillInstallHandle = "alice"
		skillInstallSkillName = "research-https"
	}); err != nil {
		t.Fatalf("https install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(configsDir, "workflows", "skill__alice__research-https__research.md")); err != nil {
		t.Errorf("https install workflow file: %v", err)
	}
}

func TestSkillInstall_InvalidFixtureRejected(t *testing.T) {
	_, _ = buildInstallTestEnv(t)
	fixDir := t.TempDir()
	src := filepath.Join(fixDir, "bad.md")
	_ = os.WriteFile(src, []byte("---\nname: BadName\n---\n"), 0o644)

	err := runSkillInstallForTest(t, src, func() {})
	if err == nil {
		t.Errorf("invalid fixture must be rejected")
	}
}

func TestSkillInstall_HandleSourceWithoutRegistry(t *testing.T) {
	_, _ = buildInstallTestEnv(t)
	err := runSkillInstallForTest(t, "vadim/missing", func() {
		// Point at a registry that doesn't exist so the index
		// resolver errors cleanly.
		skillInstallRegistry = "http://127.0.0.1:1"
	})
	if err == nil {
		t.Errorf("unresolvable handle should error")
	}
}

func TestSkillTelemetry_DisabledByDefault(t *testing.T) {
	if isSkillTelemetryEnabled() {
		t.Errorf("telemetry should default to off")
	}
}

func TestSkillTelemetry_EnvOptIn(t *testing.T) {
	t.Setenv("VORNIK_SKILL_TELEMETRY", "true")
	if !isSkillTelemetryEnabled() {
		t.Errorf("VORNIK_SKILL_TELEMETRY=true should opt in")
	}
}

func TestSkillTelemetry_InstanceIDStableWithinMonth(t *testing.T) {
	a := skillTelemetryInstanceID()
	b := skillTelemetryInstanceID()
	if a != b {
		t.Errorf("instance_id must be stable inside a month: %q vs %q", a, b)
	}
	if len(a) != 8 {
		t.Errorf("instance_id should be 8 hex chars: %q", a)
	}
}
