package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A companion user whose daemon runs on ANOTHER host cannot file a problem report: the
// report-problem skill drives `vornikctl report` locally, and there was no companion MCP
// verb for it (BACKLOG 2026-08-03, deferred from the report-marking work).
//
// The constraint is the operator's 2026-08-03 ruling: no chat/REST/MCP-reachable path may
// execute a program, so this verb carries the daemon's build identity and the reporter's
// words ONLY. It reuses the same builder the chat tool uses, which is collector-free by
// construction — reusing rather than reimplementing is the point, because the scrubber is
// the security control and a second copy would drift from it.

type stubReportBuilder struct {
	url  string
	body string
	err  error
	// symptom records what reached the builder, so the test can prove the tool passes
	// the reporter's words through rather than composing its own.
	symptom string
}

func (s *stubReportBuilder) BuildProblemReport(_ context.Context, symptom string) (string, string, error) {
	s.symptom = symptom
	if s.err != nil {
		return "", "", s.err
	}
	return s.url, s.body, nil
}

func TestCompanionMCP_ReportProblem_ReturnsTheURLAndBodyWithoutSubmitting(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	stub := &stubReportBuilder{
		url:  "https://github.com/grinco/vornik/issues/new?title=x&body=y",
		body: "## Environment\nversion: 2026.7.7\n\n## Symptom\nthe daemon wedged",
	}
	srv.problemReports = stub

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "report_problem",
		"arguments": map[string]any{"symptom": "the daemon wedged"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, "got: %s", text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))

	assert.Equal(t, stub.url, out["review_url"])
	assert.Equal(t, stub.body, out["body"])
	assert.Equal(t, "the daemon wedged", stub.symptom,
		"the reporter's own words must reach the builder verbatim")
	// Nothing is submitted: the body goes to a PUBLIC repo and anonymisation cannot
	// prove the user's own words are free of a customer name, so a human presses submit.
	// The response must say so rather than leaving the client to assume either way.
	require.Contains(t, out, "submitted")
	assert.Equal(t, false, out["submitted"])
}

func TestCompanionMCP_ReportProblem_RequiresASymptom(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	srv.problemReports = &stubReportBuilder{url: "u", body: "b"}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "report_problem",
		"arguments": map[string]any{"symptom": "   "},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	assert.True(t, isErr, "a blank symptom must be refused, not filed: %s", text)
}

// TestCompanionMCP_ReportProblem_UnwiredIsAClearRefusal — a lean deployment with no
// builder must say so, not panic and not silently return an empty URL the client would
// hand a user.
func TestCompanionMCP_ReportProblem_UnwiredIsAClearRefusal(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	srv.problemReports = nil

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "report_problem",
		"arguments": map[string]any{"symptom": "anything"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	assert.True(t, isErr)
	assert.Contains(t, text, "not configured")
}

// TestCompanionMCP_ReportProblem_AnonymisationFailureFilesNothing — AnonymizeBody fails
// CLOSED, and so must this verb: a partially-scrubbed body destined for a public repo is
// the one outcome worse than no report.
func TestCompanionMCP_ReportProblem_AnonymisationFailureFilesNothing(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	srv.problemReports = &stubReportBuilder{err: errors.New("scrubber could not resolve the hostname")}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "report_problem",
		"arguments": map[string]any{"symptom": "the daemon wedged"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	assert.True(t, isErr)
	assert.NotContains(t, text, "issues/new", "no URL may be returned when scrubbing failed")
}

// TestCompanionToolDefs_ReportProblemIsAdvertised — a tool the dispatch switch handles but
// tools/list never mentions is unreachable for every client that discovers capabilities.
func TestCompanionToolDefs_ReportProblemIsAdvertised(t *testing.T) {
	var found bool
	for _, def := range companionToolDefs() {
		if def.Name == "report_problem" {
			found = true
			assert.Contains(t, def.Description, "COLLECTS NOTHING",
				"the description must state that it collects nothing host-side — that is the ruling it ships under, "+
					"and the host LLM only ever sees this text")
			assert.Contains(t, def.Description, "NOTHING IS SUBMITTED",
				"a client that assumes this files the issue would post an unreviewed body to a PUBLIC repo")
		}
	}
	assert.True(t, found, "report_problem is not advertised in tools/list")
}

// The three input-boundary cases review-20260804-ee60 asked for. The reporter's text is
// attacker-controllable on an authenticated path, is echoed to a terminal client, and is
// published to a PUBLIC repository — so the boundary is worth pinning rather than
// assuming the builder handles it.

func TestCompanionMCP_ReportProblem_RefusesAnOversizedSymptom(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	stub := &stubReportBuilder{url: "https://github.com/grinco/vornik/issues/new?x=1", body: "b"}
	srv.problemReports = stub

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "report_problem",
		"arguments": map[string]any{"symptom": strings.Repeat("x", maxReportSymptomBytes+1)},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.True(t, isErr, "an unbounded symptom must be refused before it reaches the anonymiser")
	assert.Contains(t, text, "support-report",
		"the refusal should point at where logs actually belong")
	assert.Empty(t, stub.symptom, "the builder must not be called at all")
}

func TestCompanionMCP_ReportProblem_StripsControlCharacters(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	stub := &stubReportBuilder{url: "https://github.com/grinco/vornik/issues/new?x=1", body: "b"}
	srv.problemReports = stub

	// An ANSI escape plus a NUL, wrapped in legitimate prose that must survive intact
	// along with its newline and tab.
	dirty := "the daemon\twedged\n\x1b[31mRED\x1b[0m and\x00 then stopped"
	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "report_problem",
		"arguments": map[string]any{"symptom": dirty},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	_, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr)
	assert.NotContains(t, stub.symptom, "\x1b", "an ANSI escape reached the report body")
	assert.NotContains(t, stub.symptom, "\x00", "a NUL byte reached the report body")
	assert.Contains(t, stub.symptom, "the daemon\twedged\n", "legitimate whitespace must survive")
	assert.Contains(t, stub.symptom, "RED", "the words themselves must survive — only the escapes go")
}

// TestCompanionMCP_ReportProblem_ReturnsAUsableURL — the client hands this link to a
// human, so an empty or unparseable URL is a broken hand-off rather than a cosmetic bug.
func TestCompanionMCP_ReportProblem_ReturnsAUsableURL(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	srv.problemReports = &stubReportBuilder{
		url:  "https://github.com/grinco/vornik/issues/new?title=t&body=b&labels=bug",
		body: "## Symptom\nthe daemon wedged",
	}

	req := mcpRequest(t, "tools/call", map[string]any{
		"name":      "report_problem",
		"arguments": map[string]any{"symptom": "the daemon wedged"},
	})
	req = withCompanionBearer(req, raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)

	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))

	rawURL, _ := out["review_url"].(string)
	require.NotEmpty(t, rawURL, "an empty URL would be handed to a user as a link")
	u, err := url.Parse(rawURL)
	require.NoError(t, err, "the URL must be parseable")
	assert.Equal(t, "https", u.Scheme)
	assert.Equal(t, "github.com", u.Host)
	assert.Contains(t, u.Path, "/issues/new", "the link must open a new issue, not a repo page")
	// The instruction is part of the contract: a client that assumes this verb filed the
	// issue would leave an unreviewed body on a public tracker.
	assert.NotEmpty(t, out["next"])
}
