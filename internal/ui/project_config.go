package ui

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/registry"
)

// ProjectConfigData backs the project YAML editor.
type ProjectConfigData struct {
	Title       string
	CurrentPage string
	ProjectID   string
	ConfigPath  string
	Content     string
	Error       string
	Success     string
	BackupPath  string
}

// ProjectConfigEdit renders the project YAML editor.
func (s *Server) ProjectConfigEdit(w http.ResponseWriter, r *http.Request, projectID string) {
	data := s.projectConfigData(projectID)
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
	}
	s.render(w, "project_config.html", data)
}

// ProjectConfigSave validates, writes, and reloads a project YAML edit.
func (s *Server) ProjectConfigSave(w http.ResponseWriter, r *http.Request, projectID string) {
	// D2 (audit 2026-06-10): the project YAML carries autonomy gates,
	// tool allowlists, and rate limits — rewriting it is an authoring
	// action, not an operate action. Restrict to admin scope (the
	// read-only ProjectConfigEdit view stays available to members).
	if !s.uiRequireAdminMutation(w, r) {
		return
	}
	data := s.projectConfigData(projectID)
	if data.Error != "" {
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "project_config.html", data)
		return
	}

	if err := r.ParseForm(); err != nil {
		data.Error = "Failed to parse form: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_config.html", data)
		return
	}
	content := r.FormValue("content")
	data.Content = content

	if err := validateProjectConfigEdit(s.configDir(), projectID, []byte(content)); err != nil {
		data.Error = "Validation failed: " + err.Error()
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "project_config.html", data)
		return
	}

	backupPath, err := writeProjectConfigAtomic(data.ConfigPath, []byte(content))
	if err != nil {
		data.Error = "Failed to write config: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		s.render(w, "project_config.html", data)
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
			s.render(w, "project_config.html", data)
			return
		}
	} else if s.projectReg != nil {
		if err := s.projectReg.Load(s.configDir()); err != nil {
			data.Error = "Saved, but registry reload failed: " + err.Error()
			if backupPath != "" {
				data.Error += "\nBackup: " + backupPath
			}
			w.WriteHeader(http.StatusConflict)
			s.render(w, "project_config.html", data)
			return
		}
	}

	data.Success = "Project config saved and reloaded."
	if backupPath != "" {
		data.Success += " Backup: " + backupPath
	}
	s.render(w, "project_config.html", data)
}

func (s *Server) projectConfigData(projectID string) ProjectConfigData {
	data := ProjectConfigData{
		Title:       "Project Config: " + projectID,
		CurrentPage: "projects",
		ProjectID:   projectID,
	}
	if projectID == "" || strings.Contains(projectID, "/") || strings.Contains(projectID, string(filepath.Separator)) {
		data.Error = "Invalid project id"
		return data
	}
	configDir := s.configDir()
	if configDir == "" {
		data.Error = "Registry config directory is not configured"
		return data
	}
	path := filepath.Join(configDir, "projects", projectID+".yaml")
	data.ConfigPath = path
	content, err := os.ReadFile(path)
	if err != nil {
		data.Error = "Project config not found: " + err.Error()
		return data
	}
	data.Content = string(content)
	return data
}

func (s *Server) configDir() string {
	if s.projectReg == nil {
		return ""
	}
	return s.projectReg.GetConfigDir()
}

func validateProjectConfigEdit(configDir, projectID string, content []byte) error {
	if configDir == "" {
		return fmt.Errorf("registry config directory is not configured")
	}
	tmp, err := os.MkdirTemp("", "vornik-project-config-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := copyUIDir(filepath.Join(configDir, sub), filepath.Join(tmp, sub)); err != nil {
			return err
		}
	}
	path := filepath.Join(tmp, "projects", projectID+".yaml")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return err
	}
	reg := registry.New()
	if err := reg.Load(tmp); err != nil {
		return err
	}
	if reg.GetProject(projectID) == nil {
		return fmt.Errorf("edited config does not define project %q", projectID)
	}
	return nil
}

func copyUIDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := copyUIDir(from, to); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		// 0o600 — copies preserve sensitivity of the originals;
		// configs/swarms/workflows tree may contain credentials.
		if err := os.WriteFile(to, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func writeProjectConfigAtomic(path string, content []byte) (string, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	var backupPath string
	mode := os.FileMode(0o600)

	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if existing, err := os.ReadFile(path); err == nil {
		backupPath = filepath.Join(dir, fmt.Sprintf("%s.bak-%s", base, time.Now().Format("20060102150405")))
		if err := os.WriteFile(backupPath, existing, mode); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}

	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", err
	}
	cleanup = false
	return backupPath, nil
}
