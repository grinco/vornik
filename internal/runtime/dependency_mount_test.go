package runtime

import (
	"os"
	"strings"
	"testing"

	"vornik.io/vornik/internal/projectdeps"
)

func TestWithDependencyEnvLeavesAManifestlessProjectAlone(t *testing.T) {
	// The common case. A project with no manifest gets no mount and NO
	// injection at all — not an empty variable, not a path that does not
	// exist (design §5.4).
	cfg := &ContainerConfig{EnvVars: map[string]string{"PATH": "/usr/bin", "FOO": "bar"}}
	got := withDependencyEnv(cfg)

	if len(got) != 2 || got["PATH"] != "/usr/bin" || got["FOO"] != "bar" {
		t.Fatalf("withDependencyEnv() = %v, want the config's own env unchanged", got)
	}
	if _, ok := got["PYTHONPATH"]; ok {
		t.Fatalf("withDependencyEnv() injected PYTHONPATH with no mounts: %v", got)
	}
}

func TestWithDependencyEnvInjectsWithoutMutatingTheConfig(t *testing.T) {
	env := map[string]string{"PATH": "/usr/bin"}
	cfg := &ContainerConfig{
		EnvVars:          env,
		DependencyMounts: []projectdeps.Mount{{Ecosystem: projectdeps.EcosystemPip, HostPath: "/host/pip-abc"}},
	}

	got := withDependencyEnv(cfg)
	if got["PYTHONPATH"] == "" {
		t.Fatalf("withDependencyEnv() = %v, want PYTHONPATH", got)
	}
	if !strings.HasPrefix(got["PATH"], projectdeps.ContainerDepsRoot+"/pip/bin") {
		t.Fatalf("PATH = %q, want the mount's bin dir prepended", got["PATH"])
	}
	if !strings.HasSuffix(got["PATH"], "/usr/bin") {
		t.Fatalf("PATH = %q, want the image's PATH preserved behind it", got["PATH"])
	}
	// The config's map is reused across retries of the same step; writing
	// into it would compound the PATH prefix on every attempt.
	if env["PATH"] != "/usr/bin" {
		t.Fatalf("the caller's env was mutated: PATH = %q", env["PATH"])
	}
	if _, ok := env["PYTHONPATH"]; ok {
		t.Fatal("the caller's env gained PYTHONPATH")
	}
}

func TestDependencyMountsAreBoundReadOnly(t *testing.T) {
	m := &Manager{}
	cfg := &ContainerConfig{
		Image:        "vornik-agent:test",
		WorkspaceDir: t.TempDir(),
		DependencyMounts: []projectdeps.Mount{
			{Ecosystem: projectdeps.EcosystemPip, HostPath: "/host/deps/pip-linux-amd64-abc"},
		},
	}
	args := m.buildPreImageArgs(cfg)

	var spec string
	for i, a := range args {
		if a == "--volume" && i+1 < len(args) && strings.Contains(args[i+1], "/host/deps/") {
			spec = args[i+1]
		}
	}
	if spec == "" {
		t.Fatalf("no dependency volume in argv: %v", args)
	}
	if !strings.HasSuffix(spec, ":ro,z") {
		// A writable cache lets one task's failed install corrupt
		// another task's dependencies, and the corruption presents as a
		// test failure in an unrelated project.
		t.Fatalf("volume = %q, want read-only with a shared label", spec)
	}
	if !strings.Contains(spec, projectdeps.ContainerDepsRoot+"/pip") {
		t.Fatalf("volume = %q, want it mounted under %s", spec, projectdeps.ContainerDepsRoot)
	}
}

func TestDependencyEnvReachesTheContainerArgv(t *testing.T) {
	m := &Manager{}
	cfg := &ContainerConfig{
		Image:            "vornik-agent:test",
		WorkspaceDir:     t.TempDir(),
		EnvVars:          map[string]string{"PATH": "/usr/local/bin"},
		DependencyMounts: []projectdeps.Mount{{Ecosystem: projectdeps.EcosystemPip, HostPath: "/host/deps/pip-abc"}},
	}
	args := m.buildPreImageArgs(cfg)

	var sawPythonPath, sawPath bool
	for i, a := range args {
		if a != "--env" || i+1 >= len(args) {
			continue
		}
		switch {
		case strings.HasPrefix(args[i+1], "PYTHONPATH="):
			sawPythonPath = true
			// The tree ROOT must be importable, or `site` never finds
			// sitecustomize.py and .pth wiring silently does not run.
			if !strings.Contains(args[i+1], "PYTHONPATH="+projectdeps.ContainerDepsRoot+"/pip"+string(os.PathListSeparator)) {
				t.Fatalf("PYTHONPATH = %q, want the tree root first", args[i+1])
			}
		case strings.HasPrefix(args[i+1], "PATH="):
			sawPath = true
			if !strings.HasSuffix(args[i+1], "/usr/local/bin") {
				t.Fatalf("PATH = %q, want the image's PATH preserved", args[i+1])
			}
		}
	}
	if !sawPythonPath || !sawPath {
		t.Fatalf("argv missing dependency env (PYTHONPATH=%v PATH=%v): %v", sawPythonPath, sawPath, args)
	}
}
