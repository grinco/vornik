package templates

import (
	"fmt"
	"os"
	"path/filepath"
)

// SuggestFreeProjectID returns projectID when no
// projects/<id>.yaml exists under configsRoot, otherwise the first
// free "<id>-N" (N = 2..99). Empty string when nothing is free —
// callers omit the suggestion in that case. The project YAML is
// the canonical conflict signal: every other rendered file
// (swarms/<id>-swarm.md, workflows/<id>-*.md) derives its name
// from the same projectId, so a free project file implies a free
// file set for the shipped templates.
func SuggestFreeProjectID(configsRoot, projectID string) string {
	free := func(id string) bool {
		_, err := os.Stat(filepath.Join(configsRoot, "projects", id+".yaml"))
		return os.IsNotExist(err)
	}
	if free(projectID) {
		return projectID
	}
	for n := 2; n < 100; n++ {
		candidate := fmt.Sprintf("%s-%d", projectID, n)
		if free(candidate) {
			return candidate
		}
	}
	return ""
}
