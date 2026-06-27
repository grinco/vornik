package cli

// Install ledger — the single source of truth for what
// `vornikctl skill install` has materialised on the operator's
// machine. Lives at `~/.config/vornik/installed-skills.yaml`.
//
// Every install command writes a row; remove / update consult
// it to know which materialised files to drop or re-emit. The
// ledger is YAML so an operator can read or hand-edit it (e.g.
// when a botched install leaves a partial state) without
// learning a new format.
//
// We deliberately store the path of every materialised file
// instead of recomputing them from the (handle, skill) tuple:
// future schema changes to the namespace prefix would otherwise
// orphan files that the prior install wrote. The recorded list
// is authoritative.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SkillLedgerSchemaVersion is the only legal value for the
// ledger's `version:` field. Bumped when the on-disk shape
// changes incompatibly; a migrator ships with the bump.
const SkillLedgerSchemaVersion = 1

// SkillLedger is the in-memory representation of the install
// ledger. The fields carry both YAML tags (for the on-disk
// format) and JSON tags (for `vornikctl skill list --json` /
// `info --json`).
type SkillLedger struct {
	// Version is the ledger schema; checked on load.
	Version int `yaml:"version" json:"version"`
	// Skills is the list of installed entries, sorted by
	// (handle, skill) on save so the file diff stays clean.
	Skills []SkillLedgerEntry `yaml:"skills" json:"skills"`
}

// SkillLedgerEntry records one installed skill.
type SkillLedgerEntry struct {
	Handle            string    `yaml:"handle" json:"handle"`
	Skill             string    `yaml:"skill" json:"skill"`
	SkillVersion      string    `yaml:"skill_version" json:"skill_version"`
	VornikSkillSchema int       `yaml:"vornik_skill_schema" json:"vornik_skill_schema"`
	InstalledAt       time.Time `yaml:"installed_at" json:"installed_at"`
	Source            string    `yaml:"source" json:"source"`
	SourceRevision    string    `yaml:"source_revision,omitempty" json:"source_revision,omitempty"`
	// SourceCommitSHA is the git commit the install landed on (rev-parse
	// HEAD), recorded even for branch/tag installs so a re-install can
	// detect a mutable ref drifting to different code (supply-chain
	// Option A). Empty for non-git sources / pre-2026-06-16 ledger rows.
	SourceCommitSHA    string                  `yaml:"source_commit_sha,omitempty" json:"source_commit_sha,omitempty"`
	SourceFileChecksum string                  `yaml:"source_file_checksum,omitempty" json:"source_file_checksum,omitempty"`
	Materialised       SkillLedgerMaterialised `yaml:"materialised" json:"materialised"`
}

// SkillLedgerMaterialised lists the configs-tree files an
// install wrote. Each entry is the basename inside its kind dir
// (workflows/ or swarms/) so the ledger stays valid if the
// operator points `--configs-dir` at a different tree later.
type SkillLedgerMaterialised struct {
	Workflows []string `yaml:"workflows,omitempty" json:"workflows,omitempty"`
	Swarms    []string `yaml:"swarms,omitempty" json:"swarms,omitempty"`
}

// DefaultSkillLedgerPath returns the canonical ledger location,
// honouring VORNIK_CONFIG_HOME when set (matches the rest of the
// CLI's path discovery convention).
func DefaultSkillLedgerPath() (string, error) {
	if env := strings.TrimSpace(os.Getenv("VORNIK_SKILL_LEDGER_PATH")); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	return filepath.Join(home, ".config", "vornik", "installed-skills.yaml"), nil
}

// LoadSkillLedger reads the ledger from disk. A missing file is
// not an error — it's an empty ledger that subsequent writes
// will create. Any other I/O or parse problem surfaces, so a
// corrupted ledger doesn't silently get overwritten.
func LoadSkillLedger(path string) (*SkillLedger, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &SkillLedger{Version: SkillLedgerSchemaVersion}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return &SkillLedger{Version: SkillLedgerSchemaVersion}, nil
	}
	var l SkillLedger
	if err := yaml.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if l.Version == 0 {
		// Empty file with no version — assume v1.
		l.Version = SkillLedgerSchemaVersion
	}
	if l.Version != SkillLedgerSchemaVersion {
		return nil, fmt.Errorf("ledger %s: unsupported schema version %d (this build supports %d)", path, l.Version, SkillLedgerSchemaVersion)
	}
	return &l, nil
}

// SaveSkillLedger writes the ledger atomically — temp file in
// the same directory + rename — so a crashed write doesn't
// leave the operator with a half-written ledger.
func SaveSkillLedger(path string, ledger *SkillLedger) error {
	if ledger == nil {
		return fmt.Errorf("SaveSkillLedger: ledger is nil")
	}
	if ledger.Version == 0 {
		ledger.Version = SkillLedgerSchemaVersion
	}
	sort.Slice(ledger.Skills, func(i, j int) bool {
		if ledger.Skills[i].Handle != ledger.Skills[j].Handle {
			return ledger.Skills[i].Handle < ledger.Skills[j].Handle
		}
		return ledger.Skills[i].Skill < ledger.Skills[j].Skill
	})
	data, err := yaml.Marshal(ledger)
	if err != nil {
		return fmt.Errorf("marshal ledger: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".installed-skills.*.yaml")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, werr := tmp.Write(data); werr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp: %w", werr)
	}
	if cerr := tmp.Close(); cerr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", cerr)
	}
	// 0o600 so the ledger isn't world-readable — it carries
	// the source URLs of every installed skill, which might
	// be private git URLs.
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// FindSkillEntry looks up an installed entry by (handle, skill).
// Returns the index in ledger.Skills + true, or -1 + false.
func FindSkillEntry(ledger *SkillLedger, handle, skill string) (int, bool) {
	for i, e := range ledger.Skills {
		if e.Handle == handle && e.Skill == skill {
			return i, true
		}
	}
	return -1, false
}

// RemoveSkillEntry deletes the row at index i, preserving the
// stable-sorted invariant.
func RemoveSkillEntry(ledger *SkillLedger, i int) {
	if i < 0 || i >= len(ledger.Skills) {
		return
	}
	ledger.Skills = append(ledger.Skills[:i], ledger.Skills[i+1:]...)
}

// SkillNamespacedID is the prefix every materialised
// workflow / swarm ID carries. The double-underscore separator
// makes the registry-installed origin unambiguous at a glance
// — operator-authored IDs use single hyphens / underscores by
// convention.
func SkillNamespacedID(handle, skill, original string) string {
	return fmt.Sprintf("skill__%s__%s__%s", handle, skill, original)
}

// SkillNamespacedSwarmID is the swarm-side equivalent — the
// swarm gets a `skill__<handle>__<skill>` ID (no `<original>`
// suffix; a skill installs one swarm).
func SkillNamespacedSwarmID(handle, skill string) string {
	return fmt.Sprintf("skill__%s__%s", handle, skill)
}
