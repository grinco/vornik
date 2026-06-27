package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSkillLedger_LoadMissingIsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.yaml")
	l, err := LoadSkillLedger(path)
	if err != nil {
		t.Fatalf("missing should not error: %v", err)
	}
	if l.Version != SkillLedgerSchemaVersion {
		t.Errorf("default version: got %d want %d", l.Version, SkillLedgerSchemaVersion)
	}
	if len(l.Skills) != 0 {
		t.Errorf("missing ledger should have 0 skills; got %d", len(l.Skills))
	}
}

func TestSkillLedger_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.yaml")

	in := &SkillLedger{
		Version: SkillLedgerSchemaVersion,
		Skills: []SkillLedgerEntry{
			{
				Handle:            "vadim",
				Skill:             "research",
				SkillVersion:      "1.0.0",
				VornikSkillSchema: 1,
				InstalledAt:       time.Date(2026, 5, 24, 19, 42, 0, 0, time.UTC),
				Source:            "https://example.com/skill.md",
				SourceRevision:    "v1.0.0",
				Materialised: SkillLedgerMaterialised{
					Workflows: []string{"skill__vadim__research__research.md"},
					Swarms:    []string{"skill__vadim__research.md"},
				},
			},
		},
	}
	if err := SaveSkillLedger(path, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadSkillLedger(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(out.Skills) != 1 {
		t.Fatalf("skill count: got %d want 1", len(out.Skills))
	}
	got := out.Skills[0]
	if got.Handle != "vadim" || got.Skill != "research" {
		t.Errorf("identity round-trip lost: %#v", got)
	}
	if got.Source != "https://example.com/skill.md" {
		t.Errorf("source lost: %q", got.Source)
	}
	if len(got.Materialised.Workflows) != 1 {
		t.Errorf("materialised workflows lost: %#v", got.Materialised)
	}
}

func TestSkillLedger_SaveSorts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.yaml")
	in := &SkillLedger{
		Skills: []SkillLedgerEntry{
			{Handle: "zeta", Skill: "x"},
			{Handle: "alpha", Skill: "y"},
			{Handle: "alpha", Skill: "x"},
		},
	}
	if err := SaveSkillLedger(path, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(data)
	if strings.Index(s, "alpha") > strings.Index(s, "zeta") {
		t.Errorf("ledger should be sorted alphabetically by handle:\n%s", s)
	}
}

func TestSkillLedger_UnsupportedSchemaRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.yaml")
	if err := os.WriteFile(path, []byte("version: 99\nskills: []\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadSkillLedger(path)
	if err == nil || !strings.Contains(err.Error(), "unsupported schema version") {
		t.Errorf("want schema-version error, got %v", err)
	}
}

func TestSkillLedger_FindRemove(t *testing.T) {
	l := &SkillLedger{
		Skills: []SkillLedgerEntry{
			{Handle: "a", Skill: "x"},
			{Handle: "b", Skill: "y"},
			{Handle: "c", Skill: "z"},
		},
	}
	i, ok := FindSkillEntry(l, "b", "y")
	if !ok || i != 1 {
		t.Errorf("find: got (%d,%v) want (1,true)", i, ok)
	}
	RemoveSkillEntry(l, i)
	if len(l.Skills) != 2 || l.Skills[0].Handle != "a" || l.Skills[1].Handle != "c" {
		t.Errorf("after remove: %#v", l.Skills)
	}
}

func TestSkillLedger_DefaultPathRespectsEnv(t *testing.T) {
	t.Setenv("VORNIK_SKILL_LEDGER_PATH", "/tmp/forced-path.yaml")
	p, err := DefaultSkillLedgerPath()
	if err != nil {
		t.Fatalf("default path: %v", err)
	}
	if p != "/tmp/forced-path.yaml" {
		t.Errorf("env override ignored: got %q", p)
	}
}

func TestSkillNamespacedID(t *testing.T) {
	if got := SkillNamespacedID("vadim", "research", "research"); got != "skill__vadim__research__research" {
		t.Errorf("namespaced id: got %q", got)
	}
	if got := SkillNamespacedSwarmID("vadim", "research"); got != "skill__vadim__research" {
		t.Errorf("namespaced swarm id: got %q", got)
	}
}
