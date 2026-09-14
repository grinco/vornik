package configassist

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// PermissionDiff is the EFFECTIVE-permission diff a class-E proposal renders
// (design §6.2 item 1, test 20): the resolved allowlist before and after,
// computed the way the daemon resolves it — a role with NO allowedTools is
// UNRESTRICTED (internal/registry/workflow.go), so `allowed_tools:`
// replacing `allowedTools:` renders as "gains: every tool", never as a
// one-word text diff.
type PermissionDiff struct {
	Subject string   `json:"subject"` // "swarm dev role reviewer" | "project x permissions"
	Gains   []string `json:"gains"`
	Loses   []string `json:"loses"`
	Note    string   `json:"note,omitempty"`
}

// EffectivePermissionDiff computes the permission diffs for every swarm or
// project op, given the original (snapshot) bytes per path.
func EffectivePermissionDiff(ops []Op, original map[string][]byte) []PermissionDiff {
	var out []PermissionDiff
	for _, op := range ops {
		lower := strings.ToLower(op.Path)
		switch {
		case strings.HasPrefix(lower, "swarms/") && strings.HasSuffix(lower, ".md"):
			out = append(out, swarmPermissionDiff(op, original[op.Path])...)
		case strings.HasPrefix(lower, "projects/") && (strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")):
			if d, ok := projectPermissionDiff(op, original[op.Path]); ok {
				out = append(out, d)
			}
		}
	}
	return out
}

func swarmPermissionDiff(op Op, before []byte) []PermissionDiff {
	name := path.Base(op.Path)
	var beforeRoles, afterRoles map[string][]string
	unrestrictedBefore := map[string]bool{}
	unrestrictedAfter := map[string]bool{}
	if len(before) > 0 {
		if sw, err := registry.ParseSwarmMarkdown(before, name); err == nil {
			beforeRoles = roleTools(sw, unrestrictedBefore)
		}
	}
	sw, err := registry.ParseSwarmMarkdown([]byte(op.Content), name)
	if err != nil {
		return []PermissionDiff{{Subject: "swarm " + strings.TrimSuffix(name, ".md"), Note: "swarm does not parse after the edit: " + err.Error()}}
	}
	afterRoles = roleTools(sw, unrestrictedAfter)
	var out []PermissionDiff
	roles := map[string]bool{}
	for r := range beforeRoles {
		roles[r] = true
	}
	for r := range afterRoles {
		roles[r] = true
	}
	names := make([]string, 0, len(roles))
	for r := range roles {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, r := range names {
		d := PermissionDiff{Subject: fmt.Sprintf("swarm %s role %s", sw.ID, r)}
		switch {
		case unrestrictedAfter[r] && !unrestrictedBefore[r]:
			d.Gains = []string{"every tool (role now declares no allowedTools = UNRESTRICTED)"}
			d.Loses = nil
		case unrestrictedBefore[r] && !unrestrictedAfter[r]:
			d.Loses = []string{"unrestricted access (role is now limited to: " + strings.Join(afterRoles[r], ",") + ")"}
		default:
			d.Gains, d.Loses = setDiff(beforeRoles[r], afterRoles[r])
		}
		if _, was := beforeRoles[r]; !was {
			d.Note = "new role"
		}
		if _, is := afterRoles[r]; !is {
			d.Note = "role removed"
		}
		if len(d.Gains) > 0 || len(d.Loses) > 0 || d.Note != "" {
			out = append(out, d)
		}
	}
	return out
}

func roleTools(sw *registry.Swarm, unrestricted map[string]bool) map[string][]string {
	out := map[string][]string{}
	for _, r := range sw.Roles {
		tools := append([]string(nil), r.Permissions.AllowedTools...)
		sort.Strings(tools)
		out[r.Name] = tools
		if len(tools) == 0 {
			unrestricted[r.Name] = true
		}
	}
	return out
}

func projectPermissionDiff(op Op, before []byte) (PermissionDiff, bool) {
	b := Flatten(before)
	a := Flatten([]byte(op.Content))
	var bt, at []string
	for k, v := range b {
		if strings.HasPrefix(k, "permissions.allowedTools.") {
			bt = append(bt, v)
		}
	}
	for k, v := range a {
		if strings.HasPrefix(k, "permissions.allowedTools.") {
			at = append(at, v)
		}
	}
	gains, loses := setDiff(bt, at)
	if len(gains) == 0 && len(loses) == 0 {
		return PermissionDiff{}, false
	}
	pid := a["projectId"]
	if pid == "" {
		pid = strings.TrimSuffix(path.Base(op.Path), path.Ext(op.Path))
	}
	return PermissionDiff{Subject: "project " + pid + " permissions.allowedTools", Gains: gains, Loses: loses}, true
}

func setDiff(before, after []string) (gains, loses []string) {
	bs := map[string]bool{}
	as := map[string]bool{}
	for _, x := range before {
		bs[x] = true
	}
	for _, x := range after {
		as[x] = true
	}
	for x := range as {
		if !bs[x] {
			gains = append(gains, x)
		}
	}
	for x := range bs {
		if !as[x] {
			loses = append(loses, x)
		}
	}
	sort.Strings(gains)
	sort.Strings(loses)
	return gains, loses
}

// RenderPermissionDiff renders the diffs for a human.
func RenderPermissionDiff(diffs []PermissionDiff) string {
	var sb strings.Builder
	for _, d := range diffs {
		g, l := "nothing", "nothing"
		if len(d.Gains) > 0 {
			g = strings.Join(d.Gains, ", ")
		}
		if len(d.Loses) > 0 {
			l = strings.Join(d.Loses, ", ")
		}
		fmt.Fprintf(&sb, "%s gains: %s; loses: %s", d.Subject, g, l)
		if d.Note != "" {
			sb.WriteString(" (" + d.Note + ")")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
