// Package swarmenv performs a surgical, comment-preserving edit of a single
// role's runtime.envVars key in a SWARM.md file. It is deliberately line-based
// rather than a YAML round-trip: SWARM.md is markdown-wrapped multi-doc YAML
// carrying extensive operator comments that a yaml.v3 re-marshal would strip.
// Used by the Phase-2 cost/quality actionizer to compute the new full-file
// content for a control-plane `config` replace proposal.
package swarmenv

import (
	"fmt"
	"regexp"
	"strings"
)

// nameRe matches a role list item: `<indent>- name: "role"` (quotes optional).
var nameRe = regexp.MustCompile(`^(\s*)- name:\s*"?([^"\s]+)"?\s*$`)

func leadingSpaces(s string) int {
	return len(s) - len(strings.TrimLeft(s, " "))
}

// SetRoleEnv sets role→runtime.envVars[key]=value in a SWARM.md file's content,
// updating the key in place if present or inserting it into that role's envVars
// block if absent. Value is emitted quoted. Everything else (comments, other
// roles, ordering) is preserved. Errors if the role or its envVars block is not
// found.
func SetRoleEnv(content, role, key, value string) (string, error) {
	lines := strings.Split(content, "\n")

	// 1. Locate the role block: [roleStart, roleEnd).
	roleStart, roleIndent := -1, -1
	for i, ln := range lines {
		if m := nameRe.FindStringSubmatch(ln); m != nil && m[2] == role {
			roleStart, roleIndent = i, len(m[1])
			break
		}
	}
	if roleStart < 0 {
		return "", fmt.Errorf("swarmenv: role %q not found", role)
	}
	roleEnd := len(lines)
	for i := roleStart + 1; i < len(lines); i++ {
		if m := nameRe.FindStringSubmatch(lines[i]); m != nil && len(m[1]) == roleIndent {
			roleEnd = i
			break
		}
	}

	// 2. Locate envVars: within the role block.
	envIdx, envIndent := -1, -1
	for i := roleStart + 1; i < roleEnd; i++ {
		if strings.TrimSpace(lines[i]) == "envVars:" {
			envIdx, envIndent = i, leadingSpaces(lines[i])
			break
		}
	}
	if envIdx < 0 {
		return "", fmt.Errorf("swarmenv: role %q has no envVars block", role)
	}

	// 3. Scan the envVars block: update the key in place if present; else record
	//    the existing key indent for an insert.
	keyRe := regexp.MustCompile(`^(\s+)` + regexp.QuoteMeta(key) + `:\s*.*$`)
	keyIndent := -1
	for i := envIdx + 1; i < roleEnd; i++ {
		ln := lines[i]
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if leadingSpaces(ln) <= envIndent {
			break // dedent → end of envVars block
		}
		if keyIndent < 0 && !strings.HasPrefix(strings.TrimSpace(ln), "#") {
			keyIndent = leadingSpaces(ln)
		}
		if keyRe.MatchString(ln) {
			lines[i] = strings.Repeat(" ", leadingSpaces(ln)) + key + `: "` + value + `"`
			return strings.Join(lines, "\n"), nil
		}
	}

	// 4. Insert a new key right after `envVars:`.
	if keyIndent < 0 {
		keyIndent = envIndent + 4
	}
	newLine := strings.Repeat(" ", keyIndent) + key + `: "` + value + `"`
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:envIdx+1]...)
	out = append(out, newLine)
	out = append(out, lines[envIdx+1:]...)
	return strings.Join(out, "\n"), nil
}
