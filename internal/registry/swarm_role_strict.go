package registry

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// A swarm ROLE is decoded STRICTLY: a key matching no field is an error, not a
// silent drop.
//
// WHY THE ROLE AND NOT THE DOCUMENT. Same reasoning decodeWorkflowFrontmatter
// records for steps: SWARM.md frontmatter is a SKILL.md-shaped envelope that
// other subsystems write into, so document-level strictness would reject files
// that are fine. Roles are where the demonstrated harm is.
//
// WHAT THE HARM IS, measured 2026-08-27. `permissions.allowedTools` misspelled
// as `allowed_tools` parsed cleanly and left the role with an EMPTY allowlist.
// Empty is not neutral: internal/api/handlers.go returns nil for an empty
// allowlist (mcpGapRoleDeclaresNone), which the MCP gate reads as "do not
// narrow" — unrestricted. So one character of casing silently removed a
// security control, and `vornikctl skill validate` reported the file clean.
//
// The casing is a genuine trap rather than carelessness: `allowed_tools` IS the
// correct spelling in project YAML for the same concept — the two collide
// inside internal/registry/project.go itself (allowed_tools narrows one MCP
// server; allowedTools is the agent permission).
//
// A typo at DOCUMENT level (`rols:`) stays lenient deliberately. It yields a
// swarm with zero roles, which Swarm.Validate then rejects loudly — "roles - at
// least one role is required". Strictness belongs where failure is silent and
// OPEN, not where it is loud and closed.
//
// Design: https://docs.vornik.io

// swarmRoleFieldNames is the set of yaml keys SwarmRole models, derived from the
// struct tags so it cannot drift from the type it describes.
var swarmRoleFieldNames = sync.OnceValue(func() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(SwarmRole{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		if name := strings.Split(tag, ",")[0]; name != "" {
			out[name] = true
		}
	}
	return out
})

// swarmRolePermissionsFieldNames does the same for the nested permissions block,
// which is where the allowlist lives and therefore where the typo bites.
var swarmRolePermissionsFieldNames = sync.OnceValue(func() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(SwarmRolePermissions{})
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		if name := strings.Split(tag, ",")[0]; name != "" {
			out[name] = true
		}
	}
	return out
})

// UnmarshalYAML decodes a role and rejects keys it does not model.
//
// The alias type is what stops this recursing: decoding into a type without
// this method uses the default struct decoding.
func (r *SwarmRole) UnmarshalYAML(value *yaml.Node) error {
	if err := rejectUnknownKeys(value, swarmRoleFieldNames(), "role"); err != nil {
		return err
	}
	type roleAlias SwarmRole
	var alias roleAlias
	if err := value.Decode(&alias); err != nil {
		return err
	}
	*r = SwarmRole(alias)
	return nil
}

// UnmarshalYAML decodes a role's permissions block strictly. This is the site
// of the measured hazard — see the package comment above.
func (p *SwarmRolePermissions) UnmarshalYAML(value *yaml.Node) error {
	if err := rejectUnknownKeys(value, swarmRolePermissionsFieldNames(), "role permissions"); err != nil {
		return err
	}
	type permAlias SwarmRolePermissions
	var alias permAlias
	if err := value.Decode(&alias); err != nil {
		return err
	}
	*p = SwarmRolePermissions(alias)
	return nil
}

// yamlMergeKey is YAML's merge key, which injects an anchored mapping's keys
// into this one. It names no field and must not be mistaken for a typo.
const yamlMergeKey = "<<"

// rejectUnknownKeys reports every key in a mapping node that the known set does
// not contain, naming all of them rather than only the first — an operator
// fixing a config wants the whole list.
func rejectUnknownKeys(value *yaml.Node, known map[string]bool, what string) error {
	if value.Kind != yaml.MappingNode {
		return nil
	}
	var unknown []string
	// A mapping node's Content alternates key, value, key, value…
	for i := 0; i+1 < len(value.Content); i += 2 {
		key := value.Content[i].Value
		if key == yamlMergeKey {
			// `<<: *anchor` is valid YAML for sharing defaults between roles,
			// and yaml.v3 resolves it during Decode below. Rejecting it as an
			// unknown field would break a legitimate config idiom to catch a
			// typo — found by probing this decoder before shipping it.
			//
			// Allowing it does NOT open a smuggling path: an unknown key
			// inside the anchored mapping is still rejected, because the
			// anchor's own nested nodes decode through these same strict
			// UnmarshalYAML methods when the anchor is parsed. Measured, and
			// pinned by TestSwarmRoleMergeCannotSmuggleAnUnknownKey — it is a
			// property of yaml.v3's decode order rather than of this code.
			continue
		}
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("line %d: unknown %s field(s) %s", value.Line, what, strings.Join(quoteAll(unknown), ", "))
}
