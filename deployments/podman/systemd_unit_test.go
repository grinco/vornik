package podman

import (
	"os"
	"strings"
	"testing"
)

// readUnit returns the shipped user unit that quickstart.sh installs verbatim
// into ~/.config/systemd/user/vornik.service (quickstart.sh: `install -m 0644
// deployments/podman/systemd/vornik.service`). Fixing the template therefore
// fixes every install that came through the one-liner.
func readUnit(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("systemd/vornik.service")
	if err != nil {
		t.Fatalf("failed to read systemd/vornik.service: %v", err)
	}
	return string(data)
}

// TestUnitWaitsForDatabaseBeforeStarting pins the boot-ordering fix.
//
// Incident 2026-09-06: after a host reboot the daemon exited 324 times in a
// row with
//
//	failed to open database: storage: connect postgres: failed to connect to
//	database: dial tcp [::1]:5432: connect: connection refused
//
// The unit only ordered itself After=network-online.target, so systemd started
// it the instant the network was up — long before the containerized Postgres
// was listening. The daemon treats a database connect failure at boot as
// fatal, so every start died in under a second and Restart=on-failure spun it
// again 5s later, forever. Nothing in the unit expressed "my database lives in
// a container that has to come back first".
//
// The fix is a bounded readiness gate: ExecStartPre blocks until the DB port
// accepts a connection, so a slow dependency becomes a WAIT instead of a crash
// loop, and a genuinely absent one fails once with a message that names the
// cause.
func TestUnitWaitsForDatabaseBeforeStarting(t *testing.T) {
	unit := readUnit(t)

	if !strings.Contains(unit, "ExecStartPre=") {
		t.Fatal("vornik.service must gate startup on the database being reachable (ExecStartPre); " +
			"without it a container-hosted Postgres that is not up yet turns boot into a restart loop")
	}

	// The gate must be driven by overridable settings, not a hardcoded address,
	// so an operator who moves Postgres does not silently keep probing 127.0.0.1.
	for _, v := range []string{"VORNIK_DB_WAIT_HOST", "VORNIK_DB_WAIT_PORT", "VORNIK_DB_WAIT_SECONDS"} {
		if !strings.Contains(unit, v) {
			t.Errorf("vornik.service must expose %s so the readiness gate is configurable", v)
		}
	}

	// The gate is useless if systemd kills the start before the wait budget
	// elapses: TimeoutStartSec must exceed the default wait.
	if !strings.Contains(unit, "TimeoutStartSec=300") {
		t.Error("vornik.service: TimeoutStartSec must leave room for the DB wait budget plus first-boot migrations")
	}
}

// TestUnitOrdersAfterContainerRestore — the daemon's Postgres is a `restart:
// always` compose container, and on a rootless host it is podman-restart.service
// that brings those back after a reboot. Without an ordering edge systemd is
// free to start the daemon first, which is exactly what happened on 2026-09-06.
// After= is a no-op when the unit is absent, so this stays safe on hosts that
// do not ship podman-restart.
func TestUnitOrdersAfterContainerRestore(t *testing.T) {
	unit := readUnit(t)
	if !strings.Contains(unit, "After=podman-restart.service") {
		t.Error("vornik.service must be ordered After=podman-restart.service so the Postgres container is restored first")
	}
	if !strings.Contains(unit, "Wants=podman-restart.service") {
		t.Error("vornik.service should Want=podman-restart.service so the container restore is pulled into the boot transaction")
	}
}

// TestUnitSurvivesALongDependencyOutage — systemd's default start rate limit
// (5 starts in 10s) puts a unit permanently into `failed` once tripped. With a
// readiness gate the daemon should keep waiting through an outage of any
// length rather than needing a manual `systemctl reset-failed`.
func TestUnitSurvivesALongDependencyOutage(t *testing.T) {
	unit := readUnit(t)
	if !strings.Contains(unit, "StartLimitIntervalSec=0") {
		t.Error("vornik.service must disable the start rate limit so a long database outage does not wedge the unit in `failed`")
	}
}
