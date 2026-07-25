package cli

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/report"
)

func TestReportTitle_SafeFromCheckNames(t *testing.T) {
	// A failing check uses a fixed title even if a future/custom daemon emits
	// an identifier-bearing check name.
	got := reportTitle([]report.Check{{Name: "config", Status: "ok"}, {Name: "secret-project@example.com", Status: "fail"}}, false)
	if got != "vornik: doctor check failing" {
		t.Errorf("title = %q", got)
	}
	if strings.Contains(got, "secret-project") {
		t.Fatalf("title leaked unrestricted check name: %q", got)
	}
	// No failing check, daemon down → offline title.
	if got := reportTitle(nil, false); got != "vornik problem (offline / install)" {
		t.Errorf("offline title = %q", got)
	}
	// No failing check, daemon up → generic title.
	if got := reportTitle([]report.Check{{Name: "config", Status: "ok"}}, true); got != "vornik problem report" {
		t.Errorf("up title = %q", got)
	}
}

func TestToReportChecks_MapsFields(t *testing.T) {
	out := toReportChecks([]doctorCheck{{Name: "n", Status: "fail", Message: "m"}})
	if len(out) != 1 || out[0].Name != "n" || out[0].Status != "fail" || out[0].Message != "m" {
		t.Errorf("mapping wrong: %+v", out)
	}
	// The title built from a message-bearing check must never contain the message.
	title := reportTitle(out, true)
	if strings.Contains(title, "m") && !strings.Contains(title, "vornik") {
		t.Errorf("title leaked message content: %q", title)
	}
}
