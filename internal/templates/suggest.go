package templates

import (
	"fmt"
	"os"

	"vornik.io/vornik/internal/safepath"
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
	// CodeQL go/path-injection (2026-07-18): validate the id as a single safe
	// path component before joining it into the config tree; candidates below
	// are derived from it ("<id>-N") so they inherit the guarantee.
	if _, err := safepath.CleanPathComponent(projectID); err != nil {
		return ""
	}
	free := func(id string) bool {
		p, err := safepath.JoinUnder(configsRoot, "projects", id+".yaml")
		if err != nil {
			return false
		}
		_, err = os.Stat(p)
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
