package projectdeps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMountVolumeSpecIsReadOnlyAndSharedLabel(t *testing.T) {
	m := Mount{Ecosystem: EcosystemPip, HostPath: "/var/lib/vornik/deps/pip-linux-amd64-abc"}
	spec := m.VolumeSpec()

	if !strings.HasSuffix(spec, ":ro,z") {
		// Read-only is load-bearing: a writable cache lets one task's
		// failed install corrupt another task's dependencies, and the
		// corruption presents as a test failure in an unrelated project.
		t.Fatalf("VolumeSpec() = %q, want it read-only with a shared SELinux label", spec)
	}
	if !strings.Contains(spec, m.HostPath+":"+ContainerDepsRoot+"/pip:") {
		t.Fatalf("VolumeSpec() = %q, want %q mounted at %s/pip", spec, m.HostPath, ContainerDepsRoot)
	}
}

func TestInjectEnvGivesImportsScriptsAndSiteWiring(t *testing.T) {
	m := Mount{Ecosystem: EcosystemPip, HostPath: "/host/pip-abc"}
	env := InjectEnv([]Mount{m}, "/usr/local/bin:/usr/bin")

	sep := string(os.PathListSeparator)
	root := ContainerDepsRoot + "/pip"

	py := strings.Split(env["PYTHONPATH"], sep)
	if len(py) != 2 || py[0] != root || py[1] != root+"/"+SiteDir {
		// The tree ROOT must be importable too, or `site` never finds
		// sitecustomize.py and .pth wiring silently does not run.
		t.Fatalf("PYTHONPATH = %q, want the tree root then its lib dir", env["PYTHONPATH"])
	}

	path := env["PATH"]
	if !strings.HasPrefix(path, root+"/"+ScriptDir+sep) {
		// Console scripts not being on PATH is precisely the "the tests
		// did not run" outcome this design exists to remove.
		t.Fatalf("PATH = %q, want the mount's bin dir PREPENDED", path)
	}
	if !strings.HasSuffix(path, "/usr/local/bin:/usr/bin") {
		t.Fatalf("PATH = %q, want the image's own PATH preserved after it", path)
	}
}

func TestInjectEnvWithNoMountsInjectsNothing(t *testing.T) {
	// design §5.4: a project with no manifest gets NO injection at all —
	// not an empty variable, not a path that does not exist. An env var
	// pointing at a missing directory works until an ecosystem decides it
	// is an error.
	if env := InjectEnv(nil, "/usr/bin"); env != nil {
		t.Fatalf("InjectEnv(nil) = %v, want nil", env)
	}
	if env := InjectEnv([]Mount{}, "/usr/bin"); env != nil {
		t.Fatalf("InjectEnv([]) = %v, want nil", env)
	}
}

func TestInjectEnvWithAnEmptyExistingPath(t *testing.T) {
	env := InjectEnv([]Mount{{Ecosystem: EcosystemPip, HostPath: "/host/pip-abc"}}, "")
	if got, want := env["PATH"], ContainerDepsRoot+"/pip/"+ScriptDir; got != want {
		t.Fatalf("PATH = %q, want %q with no trailing separator", got, want)
	}
}

func TestWithSiteCustomiseWritesTheAddsitedirShim(t *testing.T) {
	dir := t.TempDir()
	fetched := false
	fetch := WithSiteCustomise(func(_ context.Context, d string) error {
		fetched = true
		return os.MkdirAll(filepath.Join(d, SiteDir), 0o700)
	})
	if err := fetch(context.Background(), dir); err != nil {
		t.Fatalf("fetch = %v", err)
	}
	if !fetched {
		t.Fatal("the wrapped fetcher did not run")
	}

	body, err := os.ReadFile(filepath.Join(dir, SiteCustomiseFile))
	if err != nil {
		t.Fatalf("sitecustomize.py missing: %v", err)
	}
	// site.addsitedir is the ACTIVE mechanism: PYTHONPATH alone does not
	// process .pth files, so a dependency relying on .pth wiring at
	// interpreter start silently does not initialise.
	if !strings.Contains(string(body), "site.addsitedir") {
		t.Fatalf("sitecustomize.py = %q, want it to call site.addsitedir", body)
	}
	if !strings.Contains(string(body), SiteDir) {
		t.Fatalf("sitecustomize.py = %q, want it to add the lib dir", body)
	}
}

func TestWithSiteCustomiseDoesNotWriteWhenTheFetchFails(t *testing.T) {
	dir := t.TempDir()
	boom := errors.New("index unreachable")
	err := WithSiteCustomise(func(context.Context, string) error { return boom })(context.Background(), dir)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the fetch error", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, SiteCustomiseFile)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a failed fetch must not leave a shim behind: %v", statErr)
	}
}

func TestInjectEnvSkipsEcosystemsWithNoInjection(t *testing.T) {
	// npm and go materialise into the same cache but need their own
	// variables; until they do, a mount of one must not silently produce
	// pip's PYTHONPATH.
	if env := InjectEnv([]Mount{{Ecosystem: EcosystemNPM, HostPath: "/host/npm-abc"}}, "/usr/bin"); len(env) != 0 {
		t.Fatalf("InjectEnv(npm) = %v, want no pip variables", env)
	}
}

func TestWriteSiteCustomiseSurfacesAWriteError(t *testing.T) {
	err := WriteSiteCustomise(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil || !strings.Contains(err.Error(), SiteCustomiseFile) {
		t.Fatalf("WriteSiteCustomise() = %v, want an error naming the file", err)
	}
}
