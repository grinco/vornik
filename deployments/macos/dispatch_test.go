package macos

import (
	"strings"
	"testing"
)

// TestQuickstartHandsOffToMacInstaller pins the unified get.vornik.io entry
// point: `curl -fsSL https://get.vornik.io | bash` serves quickstart.sh for
// every path, and quickstart.sh differentiates the OS IN-SCRIPT — on macOS it
// hands off to the Lima-VM installer rather than dying. This keeps ONE
// one-liner for both OSes with no per-URL routing and reuses the existing
// quickstart.sh.sha256 verified-install path.
func TestQuickstartHandsOffToMacInstaller(t *testing.T) {
	qs := repoFile(t, "deployments/podman/quickstart.sh")

	// The Darwin branch must appear and route to the mac installer.
	di := strings.Index(qs, `"$(uname -s)" = "Darwin"`)
	if di < 0 {
		t.Fatal("quickstart.sh must detect macOS (uname -s = Darwin) and hand off")
	}
	if !strings.Contains(qs[di:], "macos/install.sh") {
		t.Error("the Darwin branch must run deployments/macos/install.sh")
	}
	if !strings.Contains(qs[di:], "VORNIK_REF") {
		t.Error("the macOS handoff must forward VORNIK_REF so the VM checks out the matching source")
	}

	// The Darwin handoff must PRECEDE the Linux-only guard, so macOS is handled
	// before the "targets Linux or macOS" die (which now only rejects other OSes).
	li := strings.Index(qs, `"$(uname -s)" = "Linux"`)
	if li < 0 || di > li {
		t.Errorf("Darwin handoff (%d) must come before the Linux guard (%d)", di, li)
	}
}
