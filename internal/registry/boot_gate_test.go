package registry

import (
	"errors"
	"strings"
	"testing"
)

// Loader-validator agreement design §13.1. At boot there is no last-known-good,
// so a project this binary refuses is either dropped or fatal. The default is to
// START with that project dark — the loader prints the diagnosis and
// project_config_skew reports it at ERROR, so a degraded boot is loud rather
// than silent, and refusing by default would turn one dark project into a dark
// deployment.
//
// It is a KEY because "the larger outage" is not the same judgement everywhere:
// a regulated deployment may legitimately prefer a daemon that does not start
// over one serving a configuration it cannot fully load.

func rejected(n int) []RejectedFile {
	out := make([]RejectedFile, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, RejectedFile{
			Kind: "project", Path: "projects/p.yaml", Layer: "/deployed",
			Error: "unknown key \"autonomyy\" — did you mean \"autonomy\"?",
		})
	}
	return out
}

func TestBootRejectionError_IsNilWhenNothingWasRejected(t *testing.T) {
	if err := BootRejectionError(nil, true); err != nil {
		t.Fatalf("a clean load refused to boot: %v", err)
	}
	if err := BootRejectionError([]RejectedFile{}, true); err != nil {
		t.Fatalf("an empty rejection list refused to boot: %v", err)
	}
}

// The default: rejected files do NOT stop the daemon.
func TestBootRejectionError_DegradedByDefault(t *testing.T) {
	if err := BootRejectionError(rejected(1), false); err != nil {
		t.Fatalf("the default boot policy refused to start: %v", err)
	}
}

// Opted in: it refuses, and the refusal carries the per-file diagnosis rather
// than a count — an operator told only "refused" has to go and find out which
// file and why, which is the gap §11 closed.
func TestBootRejectionError_RefusesWhenConfigured(t *testing.T) {
	err := BootRejectionError(rejected(2), true)
	if err == nil {
		t.Fatal("refuse_start_on_rejected_project did not refuse")
	}
	msg := err.Error()
	for _, want := range []string{"2 file(s)", "projects/p.yaml", "autonomyy", "did you mean"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the refusal is missing %q:\n%s", want, msg)
		}
	}
	// It must name the key that makes it configurable, or an operator meeting
	// it on a Sunday cannot get the daemon up.
	if !strings.Contains(msg, "refuse_start_on_rejected_project") {
		t.Fatalf("the refusal does not name the key that produced it:\n%s", msg)
	}
}

// Typed, so a caller can tell this refusal from any other boot failure.
func TestBootRejectionError_IsTyped(t *testing.T) {
	err := BootRejectionError(rejected(1), true)
	var typed *BootRejectedError
	if !errors.As(err, &typed) {
		t.Fatalf("want *BootRejectedError, got %T", err)
	}
	if len(typed.Rejected) != 1 {
		t.Fatalf("the typed error dropped its rejections: %+v", typed)
	}
}
