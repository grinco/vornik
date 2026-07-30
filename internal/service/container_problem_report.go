package service

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/report"
	"vornik.io/vornik/internal/version"
)

// problemReportBuilder backs the report_problem dispatcher tool, so a customer can file
// an anonymised bug report from Slack, Telegram or email instead of needing shell access
// to the host.
//
// OPERATOR REQUEST 2026-07-30: "customers expect to be able to submit the bug report via
// the chat channels". `vornikctl report` has done this from a terminal since it shipped;
// a customer talking to the bot had no way in.
//
// It reuses internal/report verbatim — the same two-tier anonymisation (secret redaction
// plus hostname / path / IP / email scrubbing) and the same prefilled-issue-URL output
// that the CLI produces. Reusing rather than reimplementing is the point: the scrubber is
// the security control, and a second copy would drift from it.
type problemReportBuilder struct {
	version string
	edition string
	// daemonUp is always true here, unlike the CLI's offline path: this code only runs
	// inside a live daemon answering a chat turn.
	hostname func() (string, error)
}

// BuildProblemReport implements dispatcher.ProblemReportBuilder.
//
// Returns the review URL and the anonymised body. It does NOT submit: the body goes to a
// PUBLIC repository and anonymisation cannot prove the user's own words are free of a
// customer name, so the human presses submit. Same gate as the CLI and the
// report-problem skill.
func (b *problemReportBuilder) BuildProblemReport(ctx context.Context, symptom string) (string, string, error) {
	if b == nil {
		return "", "", fmt.Errorf("problem reporting not configured")
	}
	if strings.TrimSpace(symptom) == "" {
		return "", "", fmt.Errorf("empty symptom")
	}
	// The hostname is passed in so AnonymizeBody can redact it LITERALLY wherever it
	// appears, not just where it matches a pattern. A lookup failure is fatal to the
	// report rather than something to paper over: without it the scrubber cannot promise
	// the host is absent, and this body is destined for a public repo.
	host, err := b.hostname()
	if err != nil {
		return "", "", fmt.Errorf("resolve hostname for redaction: %w", err)
	}

	body, err := report.AnonymizeBody(report.BodyInput{
		Version:  b.version,
		Edition:  b.edition,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Hostname: host,
		DaemonUp: true,
		Symptom:  symptom,
	})
	if err != nil {
		// AnonymizeBody fails CLOSED — it returns an error rather than a
		// partially-scrubbed body. Propagate that; the tool turns it into "I have not
		// created one, nothing was sent anywhere".
		return "", "", err
	}
	_ = ctx // no I/O today; kept for a future doctor-check enrichment
	return report.IssueURL("problem report from chat", body), body, nil
}

// problemReportBuilder returns the bug-report seam for the dispatcher.
//
// The version falls back to version.Default rather than disabling the tool.
//
// CORRECTION 2026-07-30: an earlier revision of this comment claimed nothing in the
// daemon calls Container.SetVersion. That was wrong — service.Run calls it with the
// ldflag-injected main.Version. What was actually empty was the injection: the binary had
// been built with only -X main.Edition, so main.Version kept version.Default and the UI
// reported 2026.4.5. Fixed by building through the Makefile, which stamps
// -X main.Version from `git describe`.
//
// The fallback stays regardless, because it is the honest answer for a build that really
// has no git metadata (the archive case `vornikctl report` already handles the same way).
// A report naming an imprecise version is worth far more than no report — but it must not
// be the normal path, and it no longer is.
func (c *Container) problemReportBuilder() dispatcher.ProblemReportBuilder {
	if c == nil {
		return nil
	}
	ver := strings.TrimSpace(c.version)
	if ver == "" {
		ver = version.Default
	}
	return &problemReportBuilder{
		version:  ver,
		edition:  c.Edition(),
		hostname: osHostname,
	}
}

// osHostname is a seam so tests can drive the redaction input without depending on the
// machine they run on.
var osHostname = os.Hostname
