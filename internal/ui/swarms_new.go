package ui

// /ui/swarms/new — create a new SWARM.md from a blank starter.
// Companion to /ui/projects/new + the existing /ui/swarms/{id}/edit
// page: this closes the "operator wants a dedicated swarm not tied
// to a project template" gap surfaced 2026-05-19. The starter is
// intentionally minimal (one role, placeholder prompt); operators
// flesh it out in the edit page once the registry has loaded it.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// SwarmsNewData is the model for the swarms_new.html template.
// Mirrors projects_new (slug picker + form fields + error/created
// banners) but without the template-catalog indirection — there's
// no swarm-template surface yet, only a single starter.
type SwarmsNewData struct {
	Title       string
	CurrentPage string

	// Form state — survives a validation-failure re-render.
	SwarmID     string
	DisplayName string
	RoleName    string

	Error   string
	Success string

	// CreatedSwarmID + CreatedPath populate the success banner.
	CreatedSwarmID string
	CreatedPath    string
}

// swarmIDPattern mirrors the project-id pattern: lowercase,
// digits, hyphens; 3–32 chars. The registry itself doesn't
// enforce this, but matching the project convention keeps the
// filename + URL slug ASCII-clean.
var swarmIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)
var roleNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// SwarmsNew renders the blank-starter form. GET only.
func (s *Server) SwarmsNew(w http.ResponseWriter, r *http.Request) {
	data := SwarmsNewData{
		Title:       "New swarm",
		CurrentPage: "swarms",
		RoleName:    "assistant",
	}
	s.render(w, "swarms_new.html", data)
}

// SwarmsCreate handles POST /ui/swarms/new. Form fields:
//
//	swarmId      — filename slug + identity field
//	displayName  — human-readable name
//	roleName     — single starter role name (operator can add more
//	               in the edit page after this lands)
//
// On success: writes <configsDir>/swarms/<swarmId>.md, reloads
// the registry, and redirects to /ui/swarms/<swarmId>/edit so the
// operator can immediately customise the prompt and tools.
func (s *Server) SwarmsCreate(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.configsDir) == "" {
		http.Error(w, "Daemon doesn't know where to write swarm files (configsDir unset)", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	data := SwarmsNewData{
		Title:       "New swarm",
		CurrentPage: "swarms",
		SwarmID:     strings.TrimSpace(r.FormValue("swarmId")),
		DisplayName: strings.TrimSpace(r.FormValue("displayName")),
		RoleName:    strings.TrimSpace(r.FormValue("roleName")),
	}
	if data.RoleName == "" {
		data.RoleName = "assistant"
	}

	if data.SwarmID == "" || !swarmIDPattern.MatchString(data.SwarmID) {
		data.Error = "Swarm ID must be 3–32 chars: lowercase letters, digits, hyphens; cannot start/end with a hyphen."
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "swarms_new.html", data)
		return
	}
	if !roleNamePattern.MatchString(data.RoleName) {
		data.Error = "Role name must start with a lowercase letter and contain only lowercase letters, digits, hyphens (≤31 chars)."
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "swarms_new.html", data)
		return
	}
	if data.DisplayName == "" {
		data.DisplayName = data.SwarmID
	}

	body := renderSwarmStarter(data.SwarmID, data.DisplayName, data.RoleName)

	// Parse-through-the-real-parser before writing: catches any
	// formatting drift in renderSwarmStarter against future
	// SWARM.md changes, and means a successful POST guarantees a
	// registry-loadable file.
	parsed, perr := registry.ParseSwarmMarkdown([]byte(body), data.SwarmID+".md")
	if perr != nil {
		data.Error = "Internal: starter SWARM.md failed self-parse: " + perr.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "swarms_new.html", data)
		return
	}
	if vErr := parsed.Validate(data.SwarmID + ".md"); vErr != nil {
		data.Error = "Internal: starter SWARM.md failed validation: " + vErr.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "swarms_new.html", data)
		return
	}

	target := filepath.Join(s.configsDir, "swarms", data.SwarmID+".md")
	if _, err := os.Stat(target); err == nil {
		data.Error = fmt.Sprintf("A swarm at swarms/%s.md already exists. Pick a different ID or delete the existing one first.", data.SwarmID)
		w.WriteHeader(http.StatusConflict)
		s.render(w, "swarms_new.html", data)
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		data.Error = "Filesystem check failed: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "swarms_new.html", data)
		return
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		data.Error = "Mkdir failed: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "swarms_new.html", data)
		return
	}
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		data.Error = "Write failed: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "swarms_new.html", data)
		return
	}

	if s.configReloader != nil {
		if err := s.configReloader.Reload(); err != nil {
			data.Error = "Saved swarms/" + data.SwarmID + ".md but daemon reload failed: " + err.Error() +
				"\nThe file is on disk; restart the daemon or fix the cause and retry the reload."
			w.WriteHeader(http.StatusConflict)
			s.render(w, "swarms_new.html", data)
			return
		}
	}

	// Redirect to the existing editor — the operator wanted "new
	// swarm with one customisable role"; the edit page is where
	// they fill in the prompt + tools.
	http.Redirect(w, r, "/ui/swarms/"+data.SwarmID+"/edit", http.StatusSeeOther)
}

// renderSwarmStarter produces a minimal valid SWARM.md. The
// inline render (no html/template plumbing) keeps the format
// auditable in one place and avoids template-syntax overhead
// for a 25-line file.
func renderSwarmStarter(swarmID, displayName, roleName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "swarmId: %q\n", swarmID)
	fmt.Fprintf(&b, "displayName: %q\n", displayName)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "roles:\n")
	fmt.Fprintf(&b, "  - name: %q\n", roleName)
	fmt.Fprintf(&b, "    description: \"Starter role — replace this description in the swarm editor.\"\n")
	fmt.Fprintf(&b, "    runtime:\n")
	fmt.Fprintf(&b, "      image: \"vornik-agent:latest\"\n")
	fmt.Fprintf(&b, "    permissions:\n")
	fmt.Fprintf(&b, "      allowedTools:\n")
	fmt.Fprintf(&b, "        - \"current_time\"\n")
	fmt.Fprintf(&b, "        - \"file_read\"\n")
	fmt.Fprintf(&b, "        - \"file_write\"\n")
	fmt.Fprintf(&b, "---\n\n")
	fmt.Fprintf(&b, "# %s\n\n", displayName)
	fmt.Fprintf(&b, "Created via the swarm starter at `/ui/swarms/new`. Edit\n")
	fmt.Fprintf(&b, "this file in the swarm editor at `/ui/swarms/%s/edit`\n", swarmID)
	fmt.Fprintf(&b, "— add roles, tools, and rewrite the role prompt below.\n\n")
	fmt.Fprintf(&b, "## Role prompts\n\n")
	fmt.Fprintf(&b, "### %s\n\n", roleName)
	fmt.Fprintf(&b, "Replace this prose with the role's actual system prompt. The\n")
	fmt.Fprintf(&b, "starter has only `file_read` / `file_write` / `current_time` in\n")
	fmt.Fprintf(&b, "the allowlist; add more tools (memory_search, run_shell, etc.)\n")
	fmt.Fprintf(&b, "in the editor as the role's responsibilities require.\n")
	return b.String()
}
