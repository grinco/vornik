// Package agentpackage is the manifest, provenance and lifecycle logic for
// Vornik extension packages.
//
// The design is https://docs.vornik.io
// design.md. Its framing is worth keeping in view: what was missing is not a
// loader — MCP servers, workflows, skills, swarms and roles are all live
// extension points with validators behind them. What was missing is ONE
// artifact an operator can hand someone, and one lifecycle to install it with.
//
// Slice 1 (§6) is workflows and roles, install/list/uninstall, provenance rows
// WITH content hashes, conflict refusal.
package agentpackage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Kind is a contribution kind.
type Kind string

const (
	// KindWorkflow is a contributed workflow, deployed to workflows/.
	KindWorkflow Kind = "workflow"
	// KindRole is a contributed role archetype, deployed to role-library/.
	KindRole Kind = "role"
)

// Contributions is the manifest's `contributes:` block. Every key is declared,
// including the ones slice 1 refuses: a key that is absent from the schema
// reads as a typo, and a key that is present and refused reads as a boundary
// with a reason behind it.
type Contributions struct {
	Workflows      []string `yaml:"workflows"`
	Roles          []string `yaml:"roles"`
	SwarmFragments []string `yaml:"swarm_fragments"`
	MCPServers     []string `yaml:"mcp_servers"`
	Skills         []string `yaml:"skills"`
	Schedules      []string `yaml:"schedules"`
	Guidance       []string `yaml:"guidance"`
}

// Manifest is a package's declaration.
type Manifest struct {
	Package     string        `yaml:"package"`
	Version     string        `yaml:"version"`
	Contributes Contributions `yaml:"contributes"`
}

var (
	packageNameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	versionRE     = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)
)

// ValidationError is a manifest defect.
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Message) }

// Validate checks the manifest's shape and refuses every contribution kind
// slice 1 does not implement — each with the reason it is deferred, because
// "not yet" and "never" are different boundaries and an operator planning a
// package needs to know which one they met.
func (m Manifest) Validate() error {
	if m.Package == "" {
		return ValidationError{Field: "package", Message: "is required"}
	}
	if !packageNameRE.MatchString(m.Package) {
		return ValidationError{Field: "package", Message: fmt.Sprintf("%q must be lower-case kebab-case; it names a directory and a provenance row", m.Package)}
	}
	if m.Version == "" {
		return ValidationError{Field: "version", Message: "is required"}
	}
	if !versionRE.MatchString(m.Version) {
		return ValidationError{Field: "version", Message: fmt.Sprintf("%q must be MAJOR.MINOR.PATCH", m.Version)}
	}

	deferred := []struct {
		field  string
		values []string
		reason string
	}{
		{"contributes.guidance", m.Contributes.Guidance,
			"a guidance block is PROMPT TEXT, and its whole mechanism is the install-time print-and-confirm that makes that visible. That surface does not exist yet, and shipping the key before the confirmation would give prompt contributions a home and no gate"},
		{"contributes.mcp_servers", m.Contributes.MCPServers,
			"deferred for a SECURITY reason, not an effort one: every other contribution kind is data the daemon validates, while an MCP server contribution points a tool channel at an ENDPOINT, so a tampered package can wire the model to an attacker's server with the operator's approval as the only gate"},
		{"contributes.skills", m.Contributes.Skills,
			"skills already have their own lifecycle and ledger (`vornikctl skill import`), and two stores writing one deployed file with no cross-check is how an uninstall removes something another lifecycle believes it owns"},
		{"contributes.swarm_fragments", m.Contributes.SwarmFragments, "not in slice 1"},
		{"contributes.schedules", m.Contributes.Schedules, "not in slice 1"},
	}
	for _, d := range deferred {
		if len(d.values) > 0 {
			return ValidationError{Field: d.field, Message: d.reason}
		}
	}

	if len(m.Contributes.Workflows) == 0 && len(m.Contributes.Roles) == 0 {
		return ValidationError{Field: "contributes", Message: "declares nothing; slice 1 installs workflows and roles"}
	}

	seen := map[string]string{}
	for field, paths := range map[string][]string{
		"contributes.workflows": m.Contributes.Workflows,
		"contributes.roles":     m.Contributes.Roles,
	} {
		for _, p := range paths {
			if err := validatePayloadPath(field, p); err != nil {
				return err
			}
			key := filepath.Clean(p)
			if prev, dup := seen[key]; dup {
				return ValidationError{Field: field, Message: fmt.Sprintf("%q is already contributed as %s", p, prev)}
			}
			seen[key] = field
		}
	}
	return nil
}

// validatePayloadPath keeps a contribution inside the package's payload tree.
// A package is a tarball an operator was handed; a path that escapes it writes
// wherever the daemon can write.
func validatePayloadPath(field, p string) error {
	if strings.TrimSpace(p) == "" {
		return ValidationError{Field: field, Message: "carries an empty path"}
	}
	if filepath.IsAbs(p) {
		return ValidationError{Field: field, Message: fmt.Sprintf("%q must be relative to the package root", p)}
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return ValidationError{Field: field, Message: fmt.Sprintf("%q escapes the package tree", p)}
	}
	return nil
}

// ContentHash is the hash recorded at install and compared at uninstall.
//
// The prior art has exactly this blind spot — the skill registry's ledger
// records materialised FILENAMES and no hashes — so inheriting its shape would
// inherit a guarantee that cannot be checked. A row id alone says "this
// package put something here"; it cannot say whether what is there NOW is
// still what the package put.
func ContentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
