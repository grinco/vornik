package ui

// Brief editor — the UI side of the PROJECT.md authoring
// primitive introduced in Phase 1A. Operators create or edit
// their project's brief (Goal / Audience / Success criteria /
// optional Out of scope / Risk & cadence) without touching the
// filesystem. See https://docs.vornik.io
//
// Save flow: form values → *registry.ProjectBrief →
// SerializeProjectBrief → validate via re-parse →
// writeProjectConfigAtomic on the projects/<id>.md path →
// configReloader / projectReg.Load.
//
// Extra sections (level-2 headings outside the five-known set)
// are preserved across the round trip: the GET handler loads
// the existing brief if any, the form only edits the named
// fields, and the POST handler re-attaches the parsed Extra
// list before serialisation.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// ProjectBriefData backs the brief editor template.
type ProjectBriefData struct {
	Title       string
	CurrentPage string
	ProjectID   string
	BriefPath   string

	Error      string
	Success    string
	BackupPath string

	// HasExisting reports whether a PROJECT.md companion exists
	// on disk for this project. Drives the "Create" vs "Edit"
	// page heading + the cancel-button target.
	HasExisting bool

	// Form fields — string for every section so the textarea
	// re-renders the operator's most recent input on validation
	// failure, regardless of how the parser would categorise it.
	DisplayName     string
	Description     string
	Goal            string
	Audience        string
	SuccessCriteria string
	OutOfScope      string
	RiskCadence     string

	// extraSections is not exposed to the template; it's carried
	// across the POST flow so unknown level-2 sections from the
	// existing PROJECT.md survive a save without the form needing
	// to render them as editable fields.
	extraSections []registry.ProjectBriefSection
}

// ProjectBriefEdit renders the brief editor.
func (s *Server) ProjectBriefEdit(w http.ResponseWriter, r *http.Request, projectID string) {
	data := s.projectBriefData(projectID)
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
	}
	s.render(w, "project_brief.html", data)
}

// ProjectBriefSave applies the form to a PROJECT.md, validates
// by re-parsing the serialised output, writes atomically, and
// reloads the registry.
func (s *Server) ProjectBriefSave(w http.ResponseWriter, r *http.Request, projectID string) {
	data := s.projectBriefData(projectID)
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "project_brief.html", data)
		return
	}

	if err := r.ParseForm(); err != nil {
		data.Error = "Failed to parse form: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_brief.html", data)
		return
	}
	overlayBriefFormValues(&data, r)

	brief := &registry.ProjectBrief{
		ProjectID:       projectID,
		DisplayName:     data.DisplayName,
		Description:     data.Description,
		Goal:            data.Goal,
		Audience:        data.Audience,
		SuccessCriteria: data.SuccessCriteria,
		OutOfScope:      data.OutOfScope,
		RiskCadence:     data.RiskCadence,
		Extra:           data.extraSections,
	}

	out, err := registry.SerializeProjectBrief(brief)
	if err != nil {
		data.Error = err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_brief.html", data)
		return
	}
	// Re-parse our own output before touching disk. Surfaces
	// validator complaints (required-section missing, etc.)
	// without leaving a half-written file behind.
	if _, err := registry.ParseProjectMarkdown(out, filepath.Base(data.BriefPath)); err != nil {
		data.Error = trimParserPrefix(err.Error())
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_brief.html", data)
		return
	}

	backupPath, err := writeProjectConfigAtomic(data.BriefPath, out)
	if err != nil {
		data.Error = "Failed to write brief: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "project_brief.html", data)
		return
	}
	data.BackupPath = backupPath

	if s.configReloader != nil {
		if err := s.configReloader.Reload(); err != nil {
			data.Error = "Saved, but reload failed: " + err.Error()
			if backupPath != "" {
				data.Error += "\nBackup: " + backupPath
			}
			w.WriteHeader(http.StatusConflict)
			s.render(w, "project_brief.html", data)
			return
		}
	} else if s.projectReg != nil {
		if err := s.projectReg.Load(s.configDir()); err != nil {
			data.Error = "Saved, but registry reload failed: " + err.Error()
			if backupPath != "" {
				data.Error += "\nBackup: " + backupPath
			}
			w.WriteHeader(http.StatusConflict)
			s.render(w, "project_brief.html", data)
			return
		}
	}

	data.HasExisting = true
	data.Success = "Brief saved and reloaded."
	if backupPath != "" {
		data.Success += " Backup: " + backupPath
	}
	s.render(w, "project_brief.html", data)
}

// projectBriefData builds the initial render-state. Mirrors the
// form-editor pattern: invalid project ID → 404; missing
// project.yaml → 404 (we don't scaffold briefs for nonexistent
// projects); existing PROJECT.md is parsed and surfaced as
// initial form values.
func (s *Server) projectBriefData(projectID string) ProjectBriefData {
	data := ProjectBriefData{
		Title:       "Project Brief: " + projectID,
		CurrentPage: "projects",
		ProjectID:   projectID,
	}
	if projectID == "" || strings.Contains(projectID, "/") || strings.Contains(projectID, string(os.PathSeparator)) {
		data.Error = "Invalid project id"
		return data
	}
	configDir := s.configDir()
	if configDir == "" {
		data.Error = "Registry config directory is not configured"
		return data
	}
	if s.projectReg == nil || s.projectReg.GetProject(projectID) == nil {
		data.Error = "Project not found"
		return data
	}
	data.BriefPath = filepath.Join(configDir, "projects", projectID+".md")
	if existing, err := os.ReadFile(data.BriefPath); err == nil {
		if parsed, err := registry.ParseProjectMarkdown(existing, filepath.Base(data.BriefPath)); err == nil {
			data.HasExisting = true
			data.DisplayName = parsed.DisplayName
			data.Description = parsed.Description
			data.Goal = parsed.Goal
			data.Audience = parsed.Audience
			data.SuccessCriteria = parsed.SuccessCriteria
			data.OutOfScope = parsed.OutOfScope
			data.RiskCadence = parsed.RiskCadence
			data.extraSections = parsed.Extra
		}
	}
	return data
}

// overlayBriefFormValues copies posted form values onto the
// data struct so a render-after-failure shows the operator's
// most recent input.
func overlayBriefFormValues(data *ProjectBriefData, r *http.Request) {
	data.DisplayName = strings.TrimSpace(r.FormValue("displayName"))
	data.Description = r.FormValue("description")
	data.Goal = r.FormValue("goal")
	data.Audience = r.FormValue("audience")
	data.SuccessCriteria = r.FormValue("successCriteria")
	data.OutOfScope = r.FormValue("outOfScope")
	data.RiskCadence = r.FormValue("riskCadence")
}

// trimParserPrefix strips the noisy "PROJECT.md <file>:" prefix
// the parser embeds in its error messages so the inline form
// banner shows just the actionable part to the operator.
func trimParserPrefix(msg string) string {
	// Format is "PROJECT.md <filename>: <reason>" — drop
	// everything up to the first ": ".
	if i := strings.Index(msg, ": "); i >= 0 && strings.HasPrefix(msg, "PROJECT.md") {
		return strings.TrimSpace(msg[i+2:])
	}
	return msg
}
