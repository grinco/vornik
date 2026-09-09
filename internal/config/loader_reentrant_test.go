package config

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// config.Load() registered --config and --version on flag.CommandLine on EVERY
// call, so a second call in one process panicked with "flag redefined".
//
// Nothing in production hit it because no command loaded the config twice —
// until the CE support bundle's local driver, which loads it to open the
// database and then runs the offline doctor, which loaded it again. That was
// worked around at the call site (buildOfflineDoctorReportFrom takes the
// already-loaded config), and the integration harness carries a
// dbcovResetFlags() helper that swaps flag.CommandLine for a fresh set for the
// same reason. Two workarounds for one defect is the signal.
//
// Filed 2026-09-04, fixed 2026-09-07.
func TestLoad_IsCallableTwiceInOneProcess(t *testing.T) {
	// Isolate from the rest of the package: a clean directory with no config, an
	// argv with no --config, and a fresh flag set. Siblings in this file's
	// package set os.Args to carry --config and reset flag.CommandLine between
	// cases (resetFlags), and an inherited value would make this test read
	// another one's config file rather than exercise re-entrancy.
	//
	// The reset happens ONCE, before the first Load. The point is that the
	// SECOND Load needs no reset — that is the defect.
	// Isolate the USER config locations too. Load() searches
	// $XDG_CONFIG_HOME/vornik and $HOME/.config/vornik after the cwd, and this
	// test passed for two days only on hosts that had a production config
	// there — the defaults alone fail validation (api.auth_enabled is on with
	// no keys), so on every clean runner the FIRST Load returned
	// "api.api_keys is required" and the re-entrancy it exists to prove was
	// never reached. Found red on grinco/vornik CI, 2026-09-07..09, and on
	// the 2026.9.4 release-prep run. The minimal config below makes the
	// defaults valid without depending on anything outside the test.
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("VORNIK_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("api:\n  auth_enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	origArgs := os.Args
	os.Args = []string{"vornik"}
	origFlags := flag.CommandLine
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	flag.CommandLine.Usage = func() {}
	t.Cleanup(func() {
		os.Args = origArgs
		flag.CommandLine = origFlags
	})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a second config.Load() panicked: %v", r)
		}
	}()

	for i := 0; i < 2; i++ {
		_, _, err := Load()
		// Anything but success is a real failure; ErrVersionRequested cannot
		// happen under `go test`.
		if err != nil && !errors.Is(err, ErrVersionRequested) {
			t.Fatalf("load %d: %v", i+1, err)
		}
	}
}
