package imagemanifest

import (
	"os/exec"
	"path"
	"strings"
)

// composeConfigFilesLabel is the compose label naming the file(s) a stack was
// created from.
//
// It is the discriminator, and the obvious alternative is wrong: podman's
// `io.podman.compose.project` is derived from the compose file's DIRECTORY, so
// on a real host every stack under deployments/podman/ reports
// `project=podman`. Filtering on the project name matched nothing and silently
// skipped every broker image — see TestStackMatchesComposeLabel.
const composeConfigFilesLabel = "com.docker.compose.project.config_files"

// HostProber resolves manifest conditions against real host state.
//
// One implementation, used by both the daemon's doctor check and the
// vornik-images emitter. They had a copy each for about ten minutes, which is
// the same two-lists-of-one-vocabulary hazard this package was created to
// remove.
//
// Both probes read INTENT, not running state: `systemctl is-enabled` rather
// than `is-active`, and `podman ps -a` so stopped containers still count. A
// stack stopped for maintenance is still intended, and skipping its images
// would hand it code older than the daemon at its next start.
type HostProber struct{}

// UnitEnabled reports whether a systemd user unit is enabled.
func (HostProber) UnitEnabled(name string) bool {
	return exec.Command("systemctl", "--user", "is-enabled", "--quiet", name).Run() == nil
}

// StackHasContainers reports whether a compose stack has any containers on
// this host, running or stopped.
func (HostProber) StackHasContainers(stack string) bool {
	out, err := exec.Command("podman", "ps", "-a", "--format",
		"{{index .Labels \""+composeConfigFilesLabel+"\"}}").Output()
	if err != nil {
		// No podman, or it failed. Reporting "no containers" is the safe
		// direction: the images are skipped rather than built against a
		// host we cannot read.
		return false
	}
	return stackMatchesConfigFiles(strings.Split(string(out), "\n"), stack)
}

// stackMatchesConfigFiles reports whether any compose config-files label value
// names <stack>.compose.yaml.
//
// Split out from StackHasContainers so the matching rule is unit-testable
// without podman. Matching is on the path BASENAME and is exact — a substring
// test would make `cluster` match supercluster.compose.yaml and `trade` match
// trading.compose.yaml.
func stackMatchesConfigFiles(values []string, stack string) bool {
	want := stack + ".compose.yaml"
	for _, value := range values {
		// One container's label can name several files, comma-separated.
		for _, file := range strings.Split(value, ",") {
			file = strings.TrimSpace(file)
			if file == "" {
				continue
			}
			// podman reports this label as a full path in some versions
			// and a bare filename in others; both were observed on one
			// host, so compare basenames.
			if path.Base(file) == want {
				return true
			}
		}
	}
	return false
}
