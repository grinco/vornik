package config

import (
	"errors"
	"flag"
	"os"
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
	t.Chdir(t.TempDir())
	t.Setenv("VORNIK_CONFIG", "")
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
		// A missing config file is fine — it returns defaults. Anything else is
		// a real failure, and ErrVersionRequested cannot happen under `go test`.
		if err != nil && !errors.Is(err, ErrVersionRequested) {
			t.Fatalf("load %d: %v", i+1, err)
		}
	}
}
