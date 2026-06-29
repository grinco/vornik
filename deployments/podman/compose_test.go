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

// TestClusterUIConfigsMountIsWritable — in the cluster topology only the
// UI node serves the web UI and writes config; workers/webhook stay
// read-only. The UI node's configs mount must be writable.
func TestClusterUIConfigsMountIsWritable(t *testing.T) {
	compose := readCompose(t, "cluster.compose.yaml")
	if !strings.Contains(compose, "../../configs:/etc/vornik/configs:rw,Z") {
		t.Error("cluster.compose.yaml: the UI node's configs mount must be writable (rw,Z) so UI project creation/edits persist")
	}
}
