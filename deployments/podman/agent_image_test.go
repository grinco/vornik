package podman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestQuickstartBuildsAgentImage — a fresh quickstart install must build
// the task-agent image, qualified as localhost/vornik-agent:latest. The
// daemon spawns each job's agent as a sibling container on the host
// podman; without this image (and a fully-qualified name) every job fails
// at container start with podman's enforced short-name resolution error.
func TestQuickstartBuildsAgentImage(t *testing.T) {
	qs := readCompose(t, "quickstart.sh")
	if !strings.Contains(qs, "images/vornik-agent/Containerfile") {
		t.Error("quickstart.sh must build the agent image from images/vornik-agent/Containerfile")
	}
	if !strings.Contains(qs, "-t localhost/vornik-agent:latest") {
		t.Error("quickstart.sh must tag the agent image fully-qualified (localhost/vornik-agent:latest)")
	}
}

// TestShippedSwarmsQualifyAgentImage — every agent image reference in the
// shipped swarm configs and template swarms must be fully qualified
// (localhost/...). A bare "vornik-agent:latest" is a short-name podman
// refuses to resolve without a TTY, so it breaks job container start.
func TestShippedSwarmsQualifyAgentImage(t *testing.T) {
	const bare = `"vornik-agent:latest"`
	var offenders []string
	for _, root := range []string{"../../configs/swarms", "../../configs/project-templates"} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || (!strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".tmpl")) {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if strings.Contains(string(b), bare) {
				offenders = append(offenders, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("unqualified agent image %s found in %v — use localhost/vornik-agent:latest "+
			"(podman cannot resolve a bare short-name headless)", bare, offenders)
	}
}
