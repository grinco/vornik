package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The P2 that produced this file asked whether the workflow split — a validator
// reporting clean on a schema it never parses — exists for swarms, projects and
// roles. It does, and on the swarm side it lands on a control that FAILS OPEN.
//
// Design: https://docs.vornik.io

// swarmWithRoleKey builds a SWARM.md whose single role carries the given
// permissions key. `allowedTools` is the schema; `allowed_tools` is the trap —
// it is the CORRECT spelling in project YAML for the same concept.
func swarmWithRoleKey(permKey string) string {
	return `---
name: probe
swarmId: probe-swarm
description: probe swarm
version: 1.0.0
leadRole: lead
roles:
  - name: lead
    model: test-model
    permissions:
      ` + permKey + `: ["file_read"]
---

## Role: lead

be brief
`
}

// THE HAZARD. One character of casing, silently dropped, leaves the role with
// an EMPTY allowlist — and handlers.go reads empty as "do not narrow", i.e.
// unrestricted MCP access. Stated as the consequence rather than the mechanism,
// so it survives a refactor that moves where strictness lives.
func TestSwarmRoleTypoCannotEmptyTheAllowlist(t *testing.T) {
	sw, err := ParseSwarmMarkdown([]byte(swarmWithRoleKey("allowed_tools")), "probe.md")
	if err != nil {
		return // rejected at load: the outcome this test wants
	}
	for _, r := range sw.Roles {
		if len(r.Permissions.AllowedTools) == 0 {
			t.Fatalf("role %q loaded with an EMPTY allowlist from a misspelled key — "+
				"the MCP gate reads that as unrestricted (handlers.go: mcpGapRoleDeclaresNone)", r.Name)
		}
	}
}

func TestSwarmRoleRejectsUnknownKey(t *testing.T) {
	_, err := ParseSwarmMarkdown([]byte(swarmWithRoleKey("allowed_tools")), "probe.md")
	if err == nil {
		t.Fatal("a role key matching no field must be a load error, not a silent drop")
	}
	if !strings.Contains(err.Error(), "allowed_tools") {
		t.Errorf("the error must name the offending key, got: %v", err)
	}
}

// The correct spelling must still work — a fix that rejected everything would
// pass the test above.
func TestSwarmRoleAcceptsTheRealKey(t *testing.T) {
	sw, err := ParseSwarmMarkdown([]byte(swarmWithRoleKey("allowedTools")), "probe.md")
	if err != nil {
		t.Fatalf("the schema spelling must load: %v", err)
	}
	if len(sw.Roles) != 1 || len(sw.Roles[0].Permissions.AllowedTools) != 1 {
		t.Fatalf("allowlist did not survive the decode: %+v", sw.Roles)
	}
}

// A YAML merge key shares defaults between roles. It names no field, and
// rejecting it would break valid YAML to catch a typo — found by probing the
// decoder before it shipped, 2026-08-27.
func TestSwarmRoleAcceptsYAMLMergeKey(t *testing.T) {
	md := `---
name: probe
swarmId: probe-swarm
leadRole: lead
roleDefaults: &d
  model: shared-model
roles:
  - <<: *d
    name: lead
---

## Role: lead

be brief
`
	sw, err := ParseSwarmMarkdown([]byte(md), "probe.md")
	if err != nil {
		t.Fatalf("a YAML merge key must not read as an unknown field: %v", err)
	}
	if len(sw.Roles) != 1 || sw.Roles[0].Model != "shared-model" {
		t.Errorf("the merged defaults did not reach the role: %+v", sw.Roles)
	}
}

// …and a merge must not become a way to smuggle an unknown key past the check.
// Measured 2026-08-27: it does not, because the anchor's own `permissions:`
// node decodes through SwarmRolePermissions.UnmarshalYAML when the anchor is
// parsed. Pinned because that is a property of yaml.v3's decode order, not of
// this package (code review 2026-08-27 suggestion 1).
func TestSwarmRoleMergeCannotSmuggleAnUnknownKey(t *testing.T) {
	md := `---
name: probe
swarmId: probe-swarm
leadRole: lead
roleDefaults: &d
  permissions:
    allowed_tools: ["file_read"]
roles:
  - <<: *d
    name: lead
    model: m
---

## Role: lead

be brief
`
	_, err := ParseSwarmMarkdown([]byte(md), "probe.md")
	if err == nil {
		t.Fatal("an unknown key arriving through a YAML merge must still be rejected")
	}
	if !strings.Contains(err.Error(), "allowed_tools") {
		t.Errorf("the error must name the merged key, got: %v", err)
	}
}

// G2: the pre-deploy gate must see what the loader rejects. Before this change
// `vornikctl skill validate` reported only SKILL.md envelope findings.
func TestSkillValidateReportsLoaderRejection(t *testing.T) {
	rep := ValidateSwarmSkillMarkdown([]byte(swarmWithRoleKey("allowed_tools")), "probe.md")
	for _, f := range rep.Findings {
		if f.Severity == SeverityError && strings.Contains(f.Message, "allowed_tools") {
			return
		}
	}
	t.Fatalf("no ERROR finding naming the rejected key; findings=%+v", rep.Findings)
}

// G4, and the measurement that made this change cheap: every config this
// deployment can reach must still load.
func TestEveryShippedConfigStillLoads(t *testing.T) {
	swarmRoots := []string{
		os.ExpandEnv("$HOME/.config/vornik/configs/swarms"),
		"../../configs/swarms",
	}
	for _, root := range swarmRoots {
		for _, f := range globMD(t, root) {
			content, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			if _, perr := ParseSwarmMarkdown(content, f); perr != nil {
				t.Errorf("shipped swarm %s no longer loads: %v", filepath.Base(f), perr)
			}
		}
	}

	projectRoots := []string{
		os.ExpandEnv("$HOME/.config/vornik/configs/projects"),
		"../../configs/projects",
	}
	for _, root := range projectRoots {
		if _, err := os.Stat(root); err != nil {
			continue
		}
		if _, err := LoadProjects(root); err != nil {
			t.Errorf("shipped projects under %s no longer load: %v", root, err)
		}
	}
}

func globMD(t *testing.T, root string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "*.md"))
	if err != nil {
		return nil
	}
	return files
}

// G3 — THE STANDING CONTRACT. A validator that re-implements the loader's rules
// drifts from it, and a gate that drifts from the thing it gates is worse than
// no gate. One row per surface; a surface added later has an obvious place to
// go, and omitting it is visible.
func TestLoaderValidatorAgreement(t *testing.T) {
	type surface struct {
		name string
		// loads reports whether the loader accepts the bytes.
		loads func([]byte) error
		// errorFindings counts ERROR-severity findings from the validator.
		// nil when the surface has no markdown validator (project YAML).
		errorFindings func([]byte) int
		rejected      string
		accepted      string
	}

	surfaces := []surface{
		{
			name:  "swarm",
			loads: func(b []byte) error { _, err := ParseSwarmMarkdown(b, "probe.md"); return err },
			errorFindings: func(b []byte) int {
				n := 0
				for _, f := range ValidateSwarmSkillMarkdown(b, "probe.md").Findings {
					if f.Severity == SeverityError {
						n++
					}
				}
				return n
			},
			rejected: swarmWithRoleKey("allowed_tools"),
			accepted: swarmWithRoleKey("allowedTools"),
		},
	}

	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			// Direction 1: what the loader rejects, the validator must ERROR on.
			if err := s.loads([]byte(s.rejected)); err == nil {
				t.Fatal("fixture is not actually rejected by the loader")
			}
			if s.errorFindings != nil && s.errorFindings([]byte(s.rejected)) == 0 {
				t.Error("the loader rejects this config but the validator reports no ERROR — " +
					"the pre-deploy gate is blind to what the daemon refuses")
			}

			// Direction 2: what the loader accepts must not be reported as a
			// schema ERROR. Envelope warnings are expected and fine.
			if err := s.loads([]byte(s.accepted)); err != nil {
				t.Fatalf("fixture is not actually accepted by the loader: %v", err)
			}
			if s.errorFindings != nil {
				for _, f := range ValidateSwarmSkillMarkdown([]byte(s.accepted), "probe.md").Findings {
					if f.Severity == SeverityError && strings.Contains(f.Code, "schema") {
						t.Errorf("validator reports a schema ERROR on a config the loader accepts: %+v", f)
					}
				}
			}
		})
	}
}
