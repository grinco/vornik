package service

import (
	"os"
	"path/filepath"
)

// resolveProjectWorkspacePath returns the effective base directory for
// per-project persistent workspaces: the configured
// runtime.project_workspace_path when set, otherwise
// $VORNIK_DATA_DIR/workspaces, otherwise "". Both the executor's
// staging guard and the dispatcher's create_task input-confinement
// gate derive their per-project uploads/ allow-list entry from this
// value — they MUST resolve it identically, or a channel upload the
// executor would happily stage gets rejected at create_task
// (incident-telegram-upload-input-roots-20260712).
func resolveProjectWorkspacePath(configured string) string {
	if configured != "" {
		return configured
	}
	if dataDir := os.Getenv("VORNIK_DATA_DIR"); dataDir != "" {
		return filepath.Join(dataDir, "workspaces")
	}
	return ""
}
