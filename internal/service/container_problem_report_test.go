package service

import (
	"context"
	"errors"
	url2 "net/url"
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

// REGRESSION 2026-08-03 (operator report): a CE customer's bug report named neither
// the edition nor the build, so triage could not tell whether the behaviour was even
// reachable in the build they ran. The chat body must mark CE/EE and the build.
func TestProblemReportBuilder_MarksEditionAndBuild(t *testing.T) {
	b := &problemReportBuilder{
		version:   "2026.7.7",
		edition:   "enterprise",
		buildDate: "2026-08-03T09:14:00Z",
		hostname:  func() (string, error) { return "customer-prod-01", nil },
	}

	url, body, err := b.BuildProblemReport(context.Background(), "the bot stopped answering")
	if err != nil {
		t.Fatalf("BuildProblemReport: %v", err)
	}
	for _, want := range []string{"enterprise (EE)", "2026-08-03T09:14:00Z"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	// The title carries the tag so EE and CE reports are separable in the issue list.
	if !strings.Contains(url, url2.QueryEscape("[EE]")) {
		t.Errorf("url = %q, want an [EE]-tagged title", url)
	}
}

// SECURITY INVARIANT (operator decision 2026-08-03: "we should not allow non cli
// commands to run other shell commands — so the doctor should not be available via the
// chat channel … even a remote possibility of an RCE or DoS is unacceptable").
//
// A chat-triggered report must collect NOTHING: no doctor sweep (it invokes podman), no
// journal tail (it invokes journalctl). This test is the guard — it fails the day a
// collector is reintroduced on this path. Diagnostics reach a report only when an
// operator runs vornikctl report / support-report deliberately in their own shell.
func TestProblemReportBuilder_CollectsNothingOnTheChatPath(t *testing.T) {
	b := &problemReportBuilder{
		version:   "2026.7.7",
		edition:   "community",
		buildDate: "2026-08-03T09:14:00Z",
		hostname:  func() (string, error) { return "customer-prod-01", nil },
	}

	_, body, err := b.BuildProblemReport(context.Background(), "tasks complete with no output")
	if err != nil {
		t.Fatalf("BuildProblemReport: %v", err)
	}
	for _, forbidden := range []string{"Doctor findings", "journal", "podman"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("chat body contains %q — collection is NOT allowed on this path:\n%s", forbidden, body)
		}
	}
	// It still files a useful report: build identity + the reporter's own words.
	for _, want := range []string{"community (CE)", "2026.7.7", "tasks complete with no output"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// The container must hand the builder the build stamp, or the marking above is dead
// code in production.
func TestContainerProblemReportBuilder_CarriesBuildDate(t *testing.T) {
	c := &Container{}
	c.SetBuildDate("2026-08-03T09:14:00Z")

	b, ok := c.problemReportBuilder().(*problemReportBuilder)
	if !ok {
		t.Fatal("builder has unexpected type")
	}
	if b.buildDate != "2026-08-03T09:14:00Z" {
		t.Errorf("buildDate = %q, want the stamped value", b.buildDate)
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
