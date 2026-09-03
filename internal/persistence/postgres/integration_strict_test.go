//go:build integration

package postgres

import (
	"strings"
	"testing"
)

// The integration lane FAILED OPEN. With an unreachable database every test
// called t.Skip, Go reported PASS, and the run exited 0 — so the lane RELEASE.md
// names as the gate for persistence changes reported success having verified
// nothing. Only the 0.013s runtime gave it away, and it nearly hid a real
// migration bug on 2026-09-01.
//
// Asking for -tags=integration is asking to run integration tests. An
// unavailable database means they did not run, and that is a failure unless the
// operator says otherwise out loud.

func TestIntegrationUnavailableIsFatalByDefault(t *testing.T) {
	fatal, msg := integrationUnavailableIsFatal("")
	if !fatal {
		t.Fatal("an unreachable database defaults to non-fatal — the lane still fails open")
	}
	if msg == "" {
		t.Error("no guidance given for a failure the operator has to act on")
	}
}

func TestIntegrationUnavailableOptOutIsExplicit(t *testing.T) {
	// Only an exact opt-out counts. A stray or truthy-looking value must not
	// silently disable the gate.
	for _, v := range []string{"1", "true", "TRUE"} {
		if fatal, _ := integrationUnavailableIsFatal(v); fatal {
			t.Errorf("opt-out %q was ignored", v)
		}
	}
	for _, v := range []string{"", "0", "false", "yes", "please"} {
		if fatal, _ := integrationUnavailableIsFatal(v); !fatal {
			t.Errorf("value %q disabled the gate; only an explicit opt-out may", v)
		}
	}
}

// The message must name the override AND the likely cause, because the default
// credentials are not what every host uses — that is exactly how this went
// unnoticed.
func TestIntegrationUnavailableMessageIsActionable(t *testing.T) {
	_, msg := integrationUnavailableIsFatal("")
	for _, want := range []string{"POSTGRES_USER", "VORNIK_INTEGRATION_OPTIONAL"} {
		if !strings.Contains(msg, want) {
			t.Errorf("guidance does not mention %s:\n%s", want, msg)
		}
	}
}
