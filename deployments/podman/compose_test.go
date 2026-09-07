// Package podman holds the podman-compose deployment manifests. These
// lightweight lints guard the manifests against regressing the topology
// the quickstart relies on:
//   - single-node: the daemon runs ON THE HOST (systemd --user), and only
//     PostgreSQL runs in a container (deps.compose.yaml). The old
//     daemon-in-a-container model — and its writable config-mount lints —
//     was retired (see quickstart-host-daemon-install-design.md).
//   - cluster: the UI node serves the web UI and must keep a writable
//     registry mount.
package podman

import (
	"os"
	"strings"
	"testing"
)

func readCompose(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("failed to read %s: %v", name, err)
	}
	return string(data)
}

// TestDepsComposeIsPostgresOnly pins the host-daemon split: deps.compose.yaml
// brings up PostgreSQL only — the daemon runs on the host, NOT as a compose
// service, and there is no host-podman-socket mount (the daemon shells out to
// the podman CLI directly). Regressing any of these reintroduces the
// daemon-in-a-container DooD bind-mount failures the redesign removed.
func TestDepsComposeIsPostgresOnly(t *testing.T) {
	compose := readCompose(t, "deps.compose.yaml")
	if !strings.Contains(compose, "pgvector/pgvector") {
		t.Error("deps.compose.yaml must define the PostgreSQL+pgvector service")
	}
	// No in-container daemon service.
	if strings.Contains(compose, "localhost/vornik:latest") ||
		strings.Contains(compose, "deployments/docker/Dockerfile") {
		t.Error("deps.compose.yaml must NOT run the vornik daemon as a container — it runs on the host")
	}
	// No host podman socket mount (the daemon-in-a-container DooD hook).
	if strings.Contains(compose, "podman.sock") {
		t.Error("deps.compose.yaml must NOT mount the host podman socket — the host daemon uses the podman CLI directly")
	}
	// Postgres published on loopback by default (not the LAN).
	if !strings.Contains(compose, "127.0.0.1") {
		t.Error("deps.compose.yaml: Postgres should bind loopback by default")
	}
}

// TestPagedropViewBindIsNotAHardcodedHostIP — the view port used to publish on
// the literal LAN address `192.0.2.10:8888`. A published port is bound at
// container START and the address must already be on the host, so when the box
// came back from the 2026-09-06 reboot on a different subnet (192.168.0.142)
// that bind became impossible and aborted the whole restore:
//
//	Error: unable to start container ... rootlessport listen tcp
//	192.0.2.10:8888: bind: cannot assign requested address
//
// Because podman-restart then exited non-zero, every container it had not yet
// reached stayed down (incident 2026-09-06). A hardcoded host IP is also the
// one bind that cannot be reused on another host. The view bind is now
// parameterised and defaults to 0.0.0.0, which is always assignable.
func TestPagedropViewBindIsNotAHardcodedHostIP(t *testing.T) {
	compose := readCompose(t, "pagedrop.compose.yaml")

	if strings.Contains(compose, "192.0.2.10:") {
		t.Error("pagedrop.compose.yaml must not publish on a hardcoded host IP — " +
			"it is unbindable until that address is configured, which aborts podman-restart at boot")
	}
	if !strings.Contains(compose, "PAGEDROP_VIEW_BIND:-0.0.0.0") {
		t.Error("pagedrop.compose.yaml: the view port must bind ${PAGEDROP_VIEW_BIND:-0.0.0.0} so it is overridable and always assignable")
	}
	// The widening applies to VIEWING only. The write API mints and accepts the
	// Bearer token; it stays loopback-only.
	if !strings.Contains(compose, `"127.0.0.1:8081:8081"`) {
		t.Error("pagedrop.compose.yaml: the write API must stay bound to loopback — only the view port is exposed")
	}
}

// TestAuditJournalMountHasADefault — the broker's audit journal is mounted
// `${VORNIK_BROKER_AUDIT_JOURNAL_HOST_DIR}:/var/lib/vornik-broker:Z`. Only the
// Makefile defines that variable (`?= $(HOME)/.local/share/vornik-broker`), so
// bringing the stack up with podman-compose directly — which README.md
// documents — left it EMPTY, and the mount silently degraded from a host bind
// into an anonymous volume.
//
// That failure is invisible until it costs you something. Found 2026-09-06:
//
//   - the journal was not in operator space, so `make` targets that create and
//     chmod that directory were maintaining a path nothing used;
//   - `:Z` cannot relabel a bind that is not one, so the volume kept the
//     ORIGINATING container's SELinux MCS categories. After a recreate the new
//     container (c699,c869) could not read a journal labelled c159,c973 and
//     logged `permission denied` — meaning every audit event it failed to POST
//     during a daemon outage was dropped rather than journalled.
//
// A default in the compose file makes the manifest correct however it is
// invoked, instead of only under `make`.
func TestAuditJournalMountHasADefault(t *testing.T) {
	compose := readCompose(t, "trading.compose.yaml")
	if strings.Contains(compose, "${VORNIK_BROKER_AUDIT_JOURNAL_HOST_DIR}:") {
		t.Error("trading.compose.yaml: the audit journal mount must supply a default " +
			"(${VORNIK_BROKER_AUDIT_JOURNAL_HOST_DIR:-...}) — unset, it degrades from a :Z host bind " +
			"to an anonymous volume the broker cannot read back after a recreate")
	}
	if !strings.Contains(compose, "VORNIK_BROKER_AUDIT_JOURNAL_HOST_DIR:-") {
		t.Error("trading.compose.yaml: expected a defaulted audit journal mount")
	}
}

// TestClusterUIConfigsMountIsWritable — in the cluster topology only the
// UI node serves the web UI and writes config; workers/webhook stay
// read-only. The UI node's configs mount must be writable.
func TestClusterUIConfigsMountIsWritable(t *testing.T) {
	compose := readCompose(t, "cluster.compose.yaml")
	if !strings.Contains(compose, "../../configs:/etc/vornik/configs:rw,Z") {
		t.Error("cluster.compose.yaml: the UI node's configs mount must be writable (rw,Z) so UI project creation/edits persist")
	}
}
