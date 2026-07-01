//go:build e2e_migrate

// Package e2e_test — CE→EE migrate-ce black-box e2e.
//
// Exercises the REAL compiled vornik-enterprise binary's `migrate-ce`
// subcommand against a synthetic Community install: a throwaway OS user
// whose home is a temp dir we own, a fake ~/.config/vornik pointing at
// the CI Postgres service container, and a `systemctl` shim on PATH so
// quiesceCE confirms "inactive" without a real user manager. This is the
// "upgrade to EE" half of the installer e2e — it proves the binary's CLI
// wiring (main interceptor + flag parsing), the real user.Lookup path,
// the real preflight DB connect, and the atomic config/secrets/configs
// writes all work end-to-end.
//
// The unit suite (cmd/vornik-enterprise/migrate_ce_test.go) covers the
// same orchestration with stub seams; this test closes the gap between
// those stubs and the actual binary a customer runs.
//
// Requires (skips otherwise — run in the `test-e2e-migrate` CI job):
//   - POSTGRES_USER/POSTGRES_PASSWORD/POSTGRES_DB env (pgvector service
//     container; POSTGRES_HOST defaults to 127.0.0.1, POSTGRES_PORT to 5432).
//   - passwordless `sudo -n useradd`/`userdel` (GitHub ubuntu runners have
//     it; a dev host without it skips — which also protects any live
//     ~/.config/vornik on the dev host, since the fake install lives in a
//     throwaway user's temp home, never the runner's real home).
//
// Run: go test -tags=e2e_migrate -count=1 -v ./test/e2e/...
package e2e_test

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// migrateBin builds ./cmd/vornik-enterprise once for the process.
var (
	migrateBinOnce sync.Once
	migrateBinPath string
	migrateBinDir  string // temp dir holding the built binary; cleaned in TestMain
	migrateBinErr  error
)

// TestMain runs the e2e_migrate suite, then removes the shared build temp
// dir (the binary is built once across tests via sync.Once, so per-test
// t.Cleanup can't reach it). Defers nothing on a fatal build — the dir is
// still removed.
func TestMain(m *testing.M) {
	code := m.Run()
	if migrateBinDir != "" {
		_ = os.RemoveAll(migrateBinDir)
	}
	os.Exit(code)
}

func migrateBin(t *testing.T) string {
	t.Helper()
	migrateBinOnce.Do(func() {
		root := repoRootE2E()
		work, err := os.MkdirTemp("", "vornik-e2e-migrate-*")
		if err != nil {
			migrateBinErr = err
			return
		}
		migrateBinDir = work
		bin := filepath.Join(work, "vornik-enterprise")
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/vornik-enterprise")
		cmd.Dir = root
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			migrateBinErr = fmt.Errorf("build vornik-enterprise: %w", err)
			return
		}
		migrateBinPath = bin
	})
	if migrateBinErr != nil {
		t.Skipf("could not build EE binary: %v", migrateBinErr)
	}
	return migrateBinPath
}

// requireE2EPostgres skips unless the CI pgvector service container env is
// present (the preflight genuinely connects, so we need a reachable DB).
func requireE2EPostgres(t *testing.T) (host, port, db, u, pw string) {
	t.Helper()
	u = os.Getenv("POSTGRES_USER")
	pw = os.Getenv("POSTGRES_PASSWORD")
	db = os.Getenv("POSTGRES_DB")
	if u == "" || pw == "" || db == "" {
		t.Skip("needs POSTGRES_USER/POSTGRES_PASSWORD/POSTGRES_DB (CI pgvector service container)")
	}
	host = os.Getenv("POSTGRES_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port = os.Getenv("POSTGRES_PORT")
	if port == "" {
		port = "5432"
	}
	return host, port, db, u, pw
}

// requireThrowawayUser creates a real OS user whose home is a temp dir we
// own (so migrate-ce's user.Lookup path resolves, but the fake CE install
// never touches the runner's real ~/.config/vornik). Skips without
// passwordless sudo (dev hosts). Returns the username + its temp home.
func requireThrowawayUser(t *testing.T) (name, home string) {
	t.Helper()
	home = t.TempDir()
	ceHome := filepath.Join(home, "home")
	if err := os.MkdirAll(ceHome, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pick a unique username. GH usernames are capped at 32 chars and must
	// start with a letter/underscore; pad the pid-ish suffix to stay unique
	// across concurrent runs.
	name = "vornik-e2e-" + strconv.Itoa(os.Getpid())
	// -M: don't create the home dir (we own it); -d: set the home field to
	// our temp dir; -N: no usergroup (avoids needing a free gid name).
	if err := exec.Command("sudo", "-n", "useradd", "-M", "-N",
		"-d", ceHome, "-s", "/usr/sbin/nologin", name).Run(); err != nil {
		t.Skipf("no passwordless sudo for useradd (skipping on dev hosts): %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("sudo", "-n", "userdel", name).Run()
	})
	// Sanity: user.Lookup resolves to our temp home.
	u, err := user.Lookup(name)
	if err != nil {
		t.Fatalf("user.Lookup(%q): %v", name, err)
	}
	if u.HomeDir != ceHome {
		t.Fatalf("user.Lookup home = %q, want %q", u.HomeDir, ceHome)
	}
	return name, ceHome
}

func requireInstalledEEExample(t *testing.T) {
	t.Helper()
	const installedExample = "/etc/vornik/vornik.yaml.example"
	if _, err := os.Stat(installedExample); err == nil {
		return
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", installedExample, err)
	}
	src := filepath.Join(repoRootE2E(), "configs", "vornik.yaml.example")
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("repo EE example missing at %s: %v", src, err)
	}
	cmd := exec.Command("sudo", "-n", "install", "-D", "-m", "0644", src, installedExample)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot stage %s (skipping on dev hosts without passwordless sudo): %v\n%s", installedExample, err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("sudo", "-n", "rm", "-f", installedExample).Run()
		_ = exec.Command("sudo", "-n", "rmdir", "/etc/vornik").Run()
	})
}

// systemctlShim writes a fake `systemctl` that reports the CE unit inactive
// (so quiesceCE confirms stopped → EE-ready) and no-ops stop/disable/mask.
func systemctlShim(t *testing.T) string {
	t.Helper()
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "systemctl")
	script := `#!/bin/sh
# e2e shim: quiesceCE calls ` + "`systemctl --user -M <uid>@.host <verb> vornik`" + `.
# is-active -> "inactive" (confirms stopped); stop/disable/mask -> no-op.
for a in "$@"; do
  case "$a" in
    is-active) echo inactive; exit 0 ;;
    stop|disable|mask) exit 0 ;;
  esac
done
exit 0
`
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return shimDir
}

func TestMigrateCE_E2E_ImportsCEInstall(t *testing.T) {
	pgHost, pgPort, pgDB, pgUser, pgPw := requireE2EPostgres(t)
	bin := migrateBin(t)
	ceUser, ceHome := requireThrowawayUser(t)
	requireInstalledEEExample(t)

	// Lay out a synthetic CE install: ~/.config/vornik/{config.yaml,vornik.env,configs/}.
	cfgDir := filepath.Join(ceHome, ".config", "vornik")
	if err := os.MkdirAll(filepath.Join(cfgDir, "configs", "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	ceConfig := fmt.Sprintf(`server:
  address: "127.0.0.1:8080"
database:
  host: %s
  port: %s
  name: %s
  user: %s
  sslmode: disable
api:
  auth_enabled: false
chat:
  enabled: false
`, pgHost, pgPort, pgDB, pgUser)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(ceConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	ceEnv := fmt.Sprintf("VORNIK_DATABASE_PASSWORD=%s\n", pgPw)
	if err := os.WriteFile(filepath.Join(cfgDir, "vornik.env"), []byte(ceEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	// A registry file the import must copy into EE's configs dir.
	projYAML := "id: e2e-migrate-proj\ndisplay_name: E2E Migrate Project\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "configs", "projects", "e2e-migrate-proj.yaml"), []byte(projYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// EE target tree in a temp dir (migrate-ce's --out/--secrets-dir/--configs-dir).
	eeRoot := t.TempDir()
	outCfg := filepath.Join(eeRoot, "vornik.yaml")
	secretsDir := filepath.Join(eeRoot, "secrets")
	configsDir := filepath.Join(eeRoot, "configs")

	shimDir := systemctlShim(t)

	// Run the real binary. PATH prepends the systemctl shim so quiesceCE's
	// exec.Command("systemctl", ...) finds it.
	cmd := exec.Command(bin, "migrate-ce",
		"--from", ceUser,
		"--out", outCfg,
		"--secrets-dir", secretsDir,
		"--configs-dir", configsDir,
	)
	cmd.Env = append(os.Environ(), "PATH="+shimDir+":"+os.Getenv("PATH"))
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("migrate-ce exited %d\nstdout:\n%s\nstderr:\n%s", ee.ExitCode(), out.String(), errb.String())
		}
		t.Fatalf("run migrate-ce: %v\nstderr:\n%s", err, errb.String())
	}

	// 1. EE-ready guidance printed (quiesce confirmed via the shim).
	if !strings.Contains(out.String(), "enable --now vornik-enterprise") {
		t.Errorf("expected EE-ready guidance when CE confirmed stopped; got:\n%s", out.String())
	}

	// 2. Imported EE config: DB coords carried, auth flipped ON, fresh key.
	cfg, err := os.ReadFile(outCfg)
	if err != nil {
		t.Fatalf("EE config not written: %v", err)
	}
	for _, want := range []string{pgHost, pgPort, pgDB, pgUser, "auth_enabled: true"} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("EE config missing %q:\n%s", want, cfg)
		}
	}

	// 3. Secrets: database.env 0600 carrying the CE password.
	dbEnvPath := filepath.Join(secretsDir, "database.env")
	dbEnv, err := os.ReadFile(dbEnvPath)
	if err != nil {
		t.Fatalf("database.env not written: %v", err)
	}
	if !strings.Contains(string(dbEnv), "VORNIK_DATABASE_PASSWORD="+pgPw) {
		t.Errorf("database.env missing CE password:\n%s", dbEnv)
	}
	fi, _ := os.Stat(dbEnvPath)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("database.env mode = %o, want 600", fi.Mode().Perm())
	}

	// 4. Registry copied without clobber.
	copied, err := os.ReadFile(filepath.Join(configsDir, "projects", "e2e-migrate-proj.yaml"))
	if err != nil {
		t.Fatalf("CE registry not copied into EE configs: %v", err)
	}
	if !strings.Contains(string(copied), "e2e-migrate-proj") {
		t.Errorf("copied registry content mismatch:\n%s", copied)
	}
}

// TestMigrateCE_E2E_PreflightFailsBlocksEE proves a CE config pointing at
// an UNREACHABLE Postgres aborts BEFORE any config is written (the safety
// gate: EE must not inherit a dead DB). Uses a dead TCP port so the
// preflight connect fails fast.
func TestMigrateCE_E2E_PreflightFailsBlocksEE(t *testing.T) {
	bin := migrateBin(t)
	ceUser, ceHome := requireThrowawayUser(t)

	// A dead port: open+immediately-close a listener to grab a free port,
	// then close it so nothing is listening.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	cfgDir := filepath.Join(ceHome, ".config", "vornik")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ceConfig := fmt.Sprintf("database:\n  host: 127.0.0.1\n  port: %d\n  name: vornik\n  user: vornik\n  sslmode: disable\n", deadPort)
	_ = os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(ceConfig), 0o644)
	_ = os.WriteFile(filepath.Join(cfgDir, "vornik.env"), []byte("VORNIK_DATABASE_PASSWORD=pw\n"), 0o600)

	eeRoot := t.TempDir()
	outCfg := filepath.Join(eeRoot, "vornik.yaml")
	cmd := exec.Command(bin, "migrate-ce", "--from", ceUser, "--out", outCfg,
		"--secrets-dir", filepath.Join(eeRoot, "secrets"), "--configs-dir", filepath.Join(eeRoot, "configs"))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	if err == nil {
		t.Fatalf("migrate-ce must fail when the CE DB is unreachable; succeeded\nstdout:\n%s", out.String())
	}
	// Preflight aborts BEFORE writing the EE config — nothing must exist.
	if _, statErr := os.Stat(outCfg); statErr == nil {
		t.Errorf("EE config must NOT be written when preflight fails; --out exists")
	}
	if !strings.Contains(errb.String(), "cannot reach the Community database") {
		t.Errorf("stderr must explain the preflight failure; got:\n%s", errb.String())
	}
}

// repoRootE2E walks up from the test file to go.mod. (The e2e_http suite
// has its own copy under that tag; this is the e2e_migrate copy so the two
// build tags don't need to share a helpers file.)
func repoRootE2E() string {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	for i := 0; i < 10 && dir != "/"; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	panic("go.mod not found walking up from " + file)
}
