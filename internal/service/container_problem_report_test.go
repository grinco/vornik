package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The scrubber is the security control, and this path feeds a PUBLIC repository. These
// pin that the chat path gets the same treatment the CLI does rather than a second,
// drifting copy.
func TestProblemReportBuilder_RedactsTheHostAndReturnsAReviewURL(t *testing.T) {
	b := &problemReportBuilder{
		version:  "2026.7.7",
		edition:  "enterprise",
		hostname: func() (string, error) { return "customer-prod-01", nil },
	}

	url, body, err := b.BuildProblemReport(context.Background(),
		"on customer-prod-01 the bot stopped answering; log said /var/home/alice/vornik/x.log and 10.1.2.3")
	if err != nil {
		t.Fatalf("BuildProblemReport: %v", err)
	}
	if !strings.Contains(url, "github.com/") || !strings.Contains(url, "issues/new") {
		t.Errorf("url = %q, want a prefilled issue URL", url)
	}
	for _, leak := range []string{"customer-prod-01", "/var/home/alice", "10.1.2.3"} {
		if strings.Contains(body, leak) {
			t.Errorf("body leaks %q into a public report:\n%s", leak, body)
		}
	}
	if !strings.Contains(body, "2026.7.7") {
		t.Error("body lost the version, which is what makes a report actionable")
	}
}

// A hostname lookup failure is fatal to the report, not something to paper over: without
// it the scrubber cannot promise the host is absent from a body bound for a public repo.
func TestProblemReportBuilder_HostnameFailureRefusesToBuild(t *testing.T) {
	b := &problemReportBuilder{
		version:  "1.0.0",
		hostname: func() (string, error) { return "", errors.New("no hostname") },
	}
	if _, _, err := b.BuildProblemReport(context.Background(), "something broke"); err == nil {
		t.Fatal("expected an error when the hostname cannot be resolved")
	}
}

// An empty symptom produces no report.
func TestProblemReportBuilder_RejectsEmptySymptom(t *testing.T) {
	b := &problemReportBuilder{version: "1.0.0", hostname: func() (string, error) { return "h", nil }}
	if _, _, err := b.BuildProblemReport(context.Background(), "   "); err == nil {
		t.Fatal("expected an error for an empty symptom")
	}
}

// REGRESSION 2026-07-30, caught on first deploy by the ABSENCE of the "chat bug-report
// path wired" log line: an earlier revision returned nil when c.version was empty, and
// c.version is ALWAYS empty because Container.SetVersion exists but nothing in the daemon
// calls it. The tool was permanently dark. It must be available on a stock container.
func TestContainerProblemReportBuilder_AvailableWithoutAnExplicitVersion(t *testing.T) {
	c := &Container{}
	got := c.problemReportBuilder()
	if got == nil {
		t.Fatal("builder = nil on a stock container — the tool would never be offered")
	}
	b, ok := got.(*problemReportBuilder)
	if !ok {
		t.Fatalf("builder has unexpected type %T", got)
	}
	if strings.TrimSpace(b.version) == "" {
		t.Error("builder carries no version, so the report cannot be acted on")
	}
}
