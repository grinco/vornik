package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stubReportBuilder struct {
	url, body string
	err       error
	calls     int
	gotSympt  string
	// edition drives the bundle guidance the tool appends. Empty normalizes
	// to Community, so tests asserting the Enterprise text must say so.
	edition string
}

func (s *stubReportBuilder) BuildProblemReport(_ context.Context, symptom string) (string, string, error) {
	s.calls++
	s.gotSympt = symptom
	return s.url, s.body, s.err
}

func (s *stubReportBuilder) Edition() string { return s.edition }

// OPERATOR REQUEST 2026-07-30: "customers expect to be able to submit the bug report via
// the chat channels (slack/telegram/email)". vornikctl report needed shell access to the
// host, so a customer talking to the bot had no way in.
func TestReportProblem_ReturnsAReviewLinkForTheUser(t *testing.T) {
	b := &stubReportBuilder{
		url:  "https://github.com/grinco/vornik/issues/new?title=x",
		body: "### vornik problem report\n\n- **version:** 1.2.3\n",
	}
	te := &ToolExecutor{problemReports: b}

	res := te.reportProblem(context.Background(), `{"summary":"the bot stopped answering in threads"}`)

	if b.calls != 1 {
		t.Fatalf("builder calls = %d, want 1", b.calls)
	}
	if b.gotSympt != "the bot stopped answering in threads" {
		t.Errorf("symptom = %q, want the user's words", b.gotSympt)
	}
	if !strings.Contains(res.Content, b.url) {
		t.Error("result does not carry the review link, so the user cannot file it")
	}
	// The body is shown so the person can see what they are about to publish.
	if !strings.Contains(res.Content, "vornik problem report") {
		t.Error("result does not show the body it will submit")
	}
}

// THE LOAD-BEARING PROPERTY. The report goes to a PUBLIC repository, and anonymisation is
// a best effort over free text — it cannot prove the user's own words carry no customer
// name or credential. So the tool must never submit; it hands back a link and says so in
// terms a person will actually read.
func TestReportProblem_NeverSubmitsAndSaysSo(t *testing.T) {
	b := &stubReportBuilder{url: "https://github.com/grinco/vornik/issues/new", body: "body"}
	te := &ToolExecutor{problemReports: b}

	res := te.reportProblem(context.Background(), `{"summary":"crash on startup"}`)

	lower := strings.ToLower(res.Content)
	if !strings.Contains(lower, "not been submitted") && !strings.Contains(lower, "nothing has been submitted") {
		t.Errorf("the result must state that nothing was submitted yet; got: %s", res.Content)
	}
	if !strings.Contains(lower, "public") {
		t.Error("the result must warn that the destination is public before the user submits")
	}
}

// Anonymisation fails CLOSED by design. A failure must produce no link and must say
// plainly that nothing left the building — the one thing worse than no bug report is a
// half-scrubbed one on a public issue tracker.
func TestReportProblem_AnonymisationFailureProducesNoLink(t *testing.T) {
	b := &stubReportBuilder{err: errors.New("anonymization failed")}
	te := &ToolExecutor{problemReports: b}

	res := te.reportProblem(context.Background(), `{"summary":"something broke"}`)

	if strings.Contains(res.Content, "http") {
		t.Errorf("a failed report must not hand back any URL: %s", res.Content)
	}
	if !strings.Contains(strings.ToLower(res.Content), "nothing was sent") {
		t.Errorf("the user must be told nothing was sent; got: %s", res.Content)
	}
}

// A deployment without the seam wired points at the CLI rather than failing opaquely.
func TestReportProblem_UnwiredExplainsTheAlternative(t *testing.T) {
	te := &ToolExecutor{}
	res := te.reportProblem(context.Background(), `{"summary":"x"}`)
	if !strings.Contains(res.Content, "vornikctl report") {
		t.Errorf("an unwired deployment should name the CLI fallback; got: %s", res.Content)
	}
}

// Bad or empty input asks for what is missing instead of filing an empty report.
func TestReportProblem_RejectsEmptyAndMalformedInput(t *testing.T) {
	b := &stubReportBuilder{url: "u", body: "b"}
	te := &ToolExecutor{problemReports: b}

	for _, args := range []string{`{"summary":"   "}`, `{}`, ``, `not json`} {
		res := te.reportProblem(context.Background(), args)
		if strings.Contains(res.Content, "NOTHING HAS BEEN SUBMITTED") {
			t.Errorf("args %q produced a report link", args)
		}
	}
	if b.calls != 0 {
		t.Errorf("builder called %d times for invalid input, want 0", b.calls)
	}
}

// The status line the channel shows must not leak arguments — a bug report's summary is
// the user's own prose, visible to everyone in a shared channel.
func TestToolStatusLine_NamesTheToolNotItsArguments(t *testing.T) {
	if got := toolStatusLine("report_problem"); !strings.Contains(got, "bug report") {
		t.Errorf("report_problem status = %q, want a human phrase", got)
	}
	if got := toolStatusLine("mcp__google-workspace__gmail_search"); !strings.Contains(got, "gmail_search") {
		t.Errorf("unknown tool status = %q, want the bare name rather than something invented", got)
	}
	if got := toolStatusLine(""); got != "working on it…" {
		t.Errorf("empty tool status = %q", got)
	}
}

// REGRESSION 2026-07-30, found in production rather than in these tests: NewAgent builds
// a.toolExecutor ONCE, copying the agent's fields at that moment. A late-bind setter that
// assigned only a.problemReports left the executor holding nil, so the daemon logged "chat
// bug-report path wired" while the bot told the user "filing a bug report isn't available
// on this deployment". The setter has to write through.
func TestSetProblemReportBuilder_WritesThroughToTheToolExecutor(t *testing.T) {
	a := &Agent{toolExecutor: &ToolExecutor{}}
	b := &stubReportBuilder{url: "u", body: "b"}

	a.SetProblemReportBuilder(b)

	if a.toolExecutor.problemReports == nil {
		t.Fatal("tool executor still holds nil — the tool would report itself unavailable")
	}
	res := a.toolExecutor.reportProblem(context.Background(), `{"summary":"x"}`)
	if strings.Contains(res.Content, "not available") {
		t.Errorf("tool still reports unavailable after wiring: %s", res.Content)
	}
}

// A nil agent and a nil executor must both be safe: the container wires optional seams
// unconditionally.
func TestSetProblemReportBuilder_NilSafe(_ *testing.T) {
	var a *Agent
	a.SetProblemReportBuilder(&stubReportBuilder{})
	(&Agent{}).SetProblemReportBuilder(&stubReportBuilder{})
}

// OPERATOR INSTRUCTION 2026-08-03: "make sure the appropriate logs are included —
// and customer has an option to review them before sending"; "the user should be
// instructed where logs/blackbox export archive is and how to upload it".
//
// A chat reporter has no terminal in front of them, so the tool response is the
// ONLY place they learn that a fuller evidence bundle exists, where it lands, and
// that they attach it themselves after reading it.
func TestReportProblem_TellsThemAboutTheEvidenceBundle(t *testing.T) {
	b := &stubReportBuilder{
		url: "https://github.com/grinco/vornik/issues/new?title=x",
		// Enterprise: support-report only exists there, so that is the edition
		// whose response names it. The Community response is asserted by
		// TestReportProblem_CommunityNamesTheLocalBundleCommand below.
		body:    "### vornik problem report\n\n- **edition:** enterprise (EE)\n",
		edition: "enterprise",
	}
	te := &ToolExecutor{problemReports: b}

	got := te.reportProblem(context.Background(), `{"summary":"tasks complete with no output"}`).Content

	for _, want := range []string{
		"support-report",  // the command that produces the logs + Black Box trace
		"vornik-support-", // where the archive lands
		"drag",            // how to upload it
		"25 MB",           // the limit they will hit
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tool response missing %q:\n%s", want, got)
		}
	}
	// Review-before-send stays explicit: nothing is submitted by the tool, and the
	// body is shown in-channel so the customer reads it first.
	for _, want := range []string{"nothing has been submitted", "This is what it will say"} {
		if !strings.Contains(got, want) {
			t.Errorf("tool response lost the review gate (%q):\n%s", want, got)
		}
	}
}

// TestReportProblem_CommunityNamesTheLocalBundleCommand — the second half of
// the 2026-08-05 CE dead end.
//
// The first fix was to stop naming an Enterprise-only command to a Community
// chat reporter, who cannot even see the 501 that would explain it. The real
// fix, 2026-09-04, was to give Community a path that works: `support-report
// --local` collects the bundle on the host. So the CE response names it again
// — the local form only, since the plain form still 501s.
func TestReportProblem_CommunityNamesTheLocalBundleCommand(t *testing.T) {
	b := &stubReportBuilder{
		url:     "https://github.com/grinco/vornik/issues/new?title=x",
		body:    "### vornik problem report\n\n- **edition:** community (CE)\n",
		edition: "community",
	}
	te := &ToolExecutor{problemReports: b}

	got := te.reportProblem(context.Background(), `{"summary":"tasks complete with no output"}`).Content

	if !strings.Contains(got, "vornikctl support-report --local") {
		t.Errorf("Community response must name the local bundle path, which works there:\n%s", got)
	}
	if strings.Contains(got, "vornikctl support-report --task <task id>\n\nIt writes") {
		t.Errorf("Community response named the DAEMON form, which still 501s there:\n%s", got)
	}
	// It still has to state the limits rather than implying a full bundle.
	for _, want := range []string{"Enterprise", "doctor", "health and metrics"} {
		if !strings.Contains(got, want) {
			t.Errorf("Community response missing %q — it must say which sections it cannot collect:\n%s", want, got)
		}
	}
}
