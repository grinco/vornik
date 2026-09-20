package projectdeps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Container layout of a materialised tree. Fixed rather than configurable:
// the env injection below has to name these paths, and a second source of
// truth for "where does lib live" is how the injection and the materialiser
// drift apart.
const (
	// ContainerDepsRoot is where a project's materialised ecosystems mount.
	ContainerDepsRoot = "/app/deps"
	// SiteDir holds the installed packages (pip --target).
	SiteDir = "lib"
	// ScriptDir holds console scripts.
	ScriptDir = "bin"
	// SiteCustomiseFile is imported by `site` at interpreter start, for
	// every interpreter, without the project cooperating.
	SiteCustomiseFile = "sitecustomize.py"
)

// Mount is one materialised ecosystem, ready to bind-mount read-only.
//
// Read-only is load-bearing (design §5.3): a writable cache lets one task's
// failed install corrupt another task's dependencies, and the corruption would
// present as a test failure in an unrelated project.
type Mount struct {
	Ecosystem Ecosystem
	// HostPath is the materialised directory under the deps root.
	HostPath string
}

// ContainerPath is where this ecosystem appears inside the container.
func (m Mount) ContainerPath() string {
	return filepath.Join(ContainerDepsRoot, string(m.Ecosystem))
}

// VolumeSpec is the podman --volume argument for this mount.
//
// Shared SELinux label (:z), not private (:Z): one materialisation is mounted
// by every container that needs it, and :Z would relabel it with a private MCS
// category that blocks the next container — the same reason the project dir
// uses :z.
func (m Mount) VolumeSpec() string {
	return fmt.Sprintf("%s:%s:ro,z", m.HostPath, m.ContainerPath())
}

// InjectEnv returns the environment additions for a set of mounts, given the
// container's existing PATH.
//
// A project with NO manifest produces NO mounts and therefore NO injection —
// not an empty variable, not a path that does not exist (design §5.4). An env
// var pointing at a missing directory is the kind of thing that works until an
// ecosystem decides it is an error.
func InjectEnv(mounts []Mount, existingPath string) map[string]string {
	if len(mounts) == 0 {
		return nil
	}
	env := make(map[string]string, 2)
	var pyPaths, binPaths []string

	for _, m := range mounts {
		if m.Ecosystem != EcosystemPip {
			continue
		}
		root := m.ContainerPath()
		// The tree ROOT carries sitecustomize.py and must be importable
		// for `site` to find it; the lib dir carries the packages. Both
		// go on PYTHONPATH, root first.
		pyPaths = append(pyPaths, root, filepath.Join(root, SiteDir))
		binPaths = append(binPaths, filepath.Join(root, ScriptDir))
	}

	if len(pyPaths) > 0 {
		env["PYTHONPATH"] = strings.Join(pyPaths, string(os.PathListSeparator))
	}
	if len(binPaths) > 0 {
		// PREPENDED, because a project's own pinned tool must win over a
		// same-named one baked into the image. Console scripts not being
		// on PATH is precisely the "the tests did not run" outcome this
		// design exists to remove (design §5.3).
		joined := strings.Join(binPaths, string(os.PathListSeparator))
		if existingPath != "" {
			joined += string(os.PathListSeparator) + existingPath
		}
		env["PATH"] = joined
	}
	return env
}

// siteCustomiseSource is written into the materialised tree so that `.pth`
// files in the site directory are PROCESSED.
//
// This is not a detail. `.pth` files are handled for SITE directories, not for
// PYTHONPATH entries: a dependency relying on `.pth` wiring at interpreter
// start silently does not initialise, and PYTHONPATH alone does NOT trigger it.
// site.addsitedir is the active mechanism and something has to call it, so this
// file does — PYTHONSTARTUP is not an option, because it does not run for
// non-interactive interpreters, which is every interpreter an agent starts.
const siteCustomiseSource = `# Written by vornik's dependency provisioner. Do not edit: this tree is
# content-addressed and mounted read-only.
#
# PYTHONPATH makes the packages importable but does NOT process .pth files,
# which are handled only for site directories. addsitedir does both.
import os
import site

_here = os.path.dirname(os.path.abspath(__file__))
site.addsitedir(os.path.join(_here, "` + SiteDir + `"))
`

// WriteSiteCustomise places sitecustomize.py at the root of a materialising
// tree.
//
// It runs at MATERIALISATION time, in the staging directory, and not at mount
// time as an earlier draft of the design said: the mount is read-only, so the
// runtime cannot write into it. The design is amended to match.
func WriteSiteCustomise(dir string) error {
	path := filepath.Join(dir, SiteCustomiseFile)
	if err := os.WriteFile(path, []byte(siteCustomiseSource), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", SiteCustomiseFile, err)
	}
	return nil
}

// WithSiteCustomise wraps a Fetcher so every pip materialisation carries its
// sitecustomize.py. Wrapping rather than leaving it to each call site: a
// materialisation that skipped it would import packages and silently fail to
// initialise the ones that need .pth wiring, which is the failure mode hardest
// to attribute from inside an agent.
func WithSiteCustomise(fetch Fetcher) Fetcher {
	return func(ctx context.Context, dir string) error {
		if err := fetch(ctx, dir); err != nil {
			return err
		}
		return WriteSiteCustomise(dir)
	}
}
