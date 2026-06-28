// Package podman holds the podman-compose deployment manifests. The
// test here is a lightweight lint that guards the registry config mount
// against regressing to read-only: the daemon/UI writes project, swarm
// and workflow files into /etc/vornik/configs when an operator creates
// or edits them through the web UI, so that bind mount MUST be writable.
// A read-only mount makes "add project" / "edit config" fail with a
// permission error at write time.
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

// TestSingleNodeConfigsMountIsWritable — the single-node daemon serves
// the web UI and must be able to write back into the registry tree.
func TestSingleNodeConfigsMountIsWritable(t *testing.T) {
	compose := readCompose(t, "podman-compose.yaml")
	if !strings.Contains(compose, "../../configs:/etc/vornik/configs:rw,Z") {
		t.Error("podman-compose.yaml: configs mount must be writable (rw,Z) so UI project creation/edits persist")
	}
	if strings.Contains(compose, "../../configs:/etc/vornik/configs:ro") {
		t.Error("podman-compose.yaml: configs mount is read-only — UI 'add project'/'edit config' will fail to write")
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
