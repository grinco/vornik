package cli

// Coverage sweep for the scraper-login CLI. We deliberately exercise
// only the side-effect-free paths: the non-local-bind validation gate
// (returns before any podman/systemctl call), the read-only status
// command, and the lanIPHint / loginContainerRunning helpers. We never
// drive runScraperLoginStart past its validation gate or
// runScraperLoginStop, because both shell out to `systemctl --user`
// against the real scraper unit.

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func scraperCov_reset() {
	scraperLoginProject, scraperLoginProfile = "", "default"
	scraperLoginURL = "about:blank"
	scraperLoginPassword = ""
	scraperLoginPort = loginVNCPort
	scraperLoginBind = "127.0.0.1"
}

func TestRunScraperLoginStart_RefusesNonLocalBindWithoutPassword(t *testing.T) {
	scraperCov_reset()
	scraperLoginProject = "janka"
	scraperLoginBind = "0.0.0.0"
	scraperLoginPassword = "" // the unsafe combination

	c := &cobra.Command{}
	err := runScraperLoginStart(c, nil)
	if err == nil || !strings.Contains(err.Error(), "non-local bind without --vnc-password") {
		t.Fatalf("expected non-local-bind refusal, got %v", err)
	}
}

func TestRunScraperLoginStatus_NoSidecar(t *testing.T) {
	// On a machine without the login container running (the test
	// environment), status reports "no login sidecar running" and
	// returns nil. If podman is absent loginContainerRunning swallows
	// the error and returns false, so this is stable either way.
	if running, _ := loginContainerRunning(); running {
		t.Skip("a login sidecar is actually running on this host; skip")
	}
	scraperCov_reset()
	c := &cobra.Command{}
	buf := &strings.Builder{}
	c.SetOut(buf)
	if err := runScraperLoginStatus(c, nil); err != nil {
		t.Fatalf("runScraperLoginStatus: %v", err)
	}
	if !strings.Contains(buf.String(), "no login sidecar running") {
		t.Errorf("status output: %s", buf.String())
	}
}

func TestLanIPHint_ReturnsUsableValue(t *testing.T) {
	// Either a non-loopback IPv4 (most CI hosts) or the "<host>"
	// placeholder when detection fails — both are valid, neither
	// should be "0.0.0.0" or empty.
	got := lanIPHint()
	if got == "" || got == "0.0.0.0" {
		t.Errorf("lanIPHint returned unusable value %q", got)
	}
}

func TestLoginContainerRunning_NoPanic(t *testing.T) {
	// Read-only probe (`podman ps`); on a host without the container
	// (or without podman) it must return false without panicking.
	running, port := loginContainerRunning()
	if running && port <= 0 {
		t.Errorf("running container reported non-positive port %d", port)
	}
}
