package verifier

import (
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Regression for the 2026-08-26 finding: a truncated tool_output makes a
// SUCCESSFUL fetch invisible to the verifiers.
//
// tool_output is cut to 4096 chars by a blind printf in the agent
// (entrypoint.sh:4035), which for a scraper result produces INVALID JSON.
// classifyAuditEntry parses the row to read the scraper's status/final_url/
// block_reason convention; the unmarshal fails, the entry falls through to a
// marker scan, finds no anchored marker, and returns (zero, false) — it does
// not contribute to the denominator at all.
//
// Measured on production `assistant` rows: 2,741 web_fetch entries, only 1,161
// (42%) parse as JSON; 704 sit in [4000,4200] ending mid-word. So
// min_successful_fetches is evaluated against a denominator that silently
// excludes ~three in five of its own rows.
//
// Design: https://docs.vornik.io §3b

// truncatedScraperRow builds the exact shape production stores: a valid
// scraper envelope whose page body pushes it past the cap, then cut blind.
func truncatedScraperRow(t *testing.T) *persistence.ToolAuditEntry {
	t.Helper()
	// Field ORDER matters and a Go map does not preserve it — json.Marshal
	// sorts keys, which would put `content` before `final_url`/`status` and
	// make the cut drop exactly the anchors production actually keeps. The real
	// scraper emits the envelope first, which is why those keys survive in 81%
	// of truncated rows. Build the fixture as ordered JSON so it reproduces
	// production rather than Go's map ordering.
	full := []byte(`{"status":200,"final_url":"https://example.com/article/123",` +
		`"content":"` + strings.Repeat("the quick brown fox jumps over the lazy dog. ", 300) +
		`","block_reason":""}`)
	if len(full) <= 4096 {
		t.Fatalf("fixture must exceed the 4096 cap, got %d", len(full))
	}
	return &persistence.ToolAuditEntry{
		ToolName:   "mcp__scraper__web_fetch",
		ToolOutput: string(full[:4096]) + "…", // the blind cut, verbatim
	}
}

func TestTruncatedScraperRowStillClassifies(t *testing.T) {
	e := truncatedScraperRow(t)

	// Sanity: the fixture really is unparseable, i.e. we are reproducing the
	// production condition and not testing a straw man.
	var probe map[string]any
	if json.Unmarshal([]byte(e.ToolOutput), &probe) == nil {
		t.Fatal("fixture parses as JSON — it does not reproduce the bug")
	}

	rep, ok := classifyAuditEntry(e)
	if !ok {
		t.Fatal("a truncated but SUCCESSFUL scraper fetch must still contribute to " +
			"the denominator — returning (zero,false) is what made 58% of production " +
			"rows invisible to min_successful_fetches")
	}
	if rep.blocked {
		t.Errorf("a status-200 fetch must not classify as blocked: %+v", rep)
	}
	if rep.url != "https://example.com/article/123" {
		t.Errorf("final_url must be salvaged, got %q", rep.url)
	}
}

// A truncated row for a fetch that WAS blocked must still classify as blocked —
// the salvage must not turn every unparseable row into a success.
func TestTruncatedBlockedRowStillClassifiesBlocked(t *testing.T) {
	full := []byte(`{"status":403,"final_url":"https://example.com/blocked",` +
		`"block_reason":"captcha","content":"` + strings.Repeat("x", 5000) + `"}`)
	e := &persistence.ToolAuditEntry{
		ToolName:   "mcp__scraper__web_fetch",
		ToolOutput: string(full[:4096]) + "…",
	}
	rep, ok := classifyAuditEntry(e)
	if !ok {
		t.Fatal("a truncated blocked fetch must classify")
	}
	if !rep.blocked {
		t.Errorf("block_reason=captcha must classify as blocked, got %+v", rep)
	}
}

// An UNTRUNCATED row must behave exactly as before. The salvage path is
// additive; it must not change the answer where parsing already worked.
func TestIntactRowIsUnaffected(t *testing.T) {
	full, _ := json.Marshal(map[string]any{
		"status": 200, "final_url": "https://example.com/ok", "block_reason": "",
	})
	rep, ok := classifyAuditEntry(&persistence.ToolAuditEntry{
		ToolName: "mcp__scraper__web_fetch", ToolOutput: string(full),
	})
	if !ok || rep.blocked || rep.url != "https://example.com/ok" {
		t.Fatalf("intact row regressed: ok=%v rep=%+v", ok, rep)
	}
}

// The salvage must NOT fire on arbitrary prose. This is the line between
// "recover a named JSON key from a known-shaped document" and the text-sniffing
// this codebase removed from the tool-audit path — an ordinary file_read result
// that happens to mention a status must not be classified as a fetch.
func TestSalvageDoesNotFireOnProse(t *testing.T) {
	for _, body := range []string{
		"The server returned status: 200 and the page loaded fine.",
		"final_url is a field name we use in the scraper convention.",
		`# Notes` + "\n" + `We saw "status": 200 mentioned in the docs.`,
	} {
		rep, ok := classifyAuditEntry(&persistence.ToolAuditEntry{
			ToolName: "file_read", ToolOutput: body,
		})
		if ok {
			t.Errorf("prose must not classify as a fetch: %q -> %+v", body[:40], rep)
		}
	}
}

// An empty or tiny body has nothing to salvage and must stay inert.
func TestEmptyBodyIsInert(t *testing.T) {
	if _, ok := classifyAuditEntry(&persistence.ToolAuditEntry{ToolName: "x"}); ok {
		t.Error("an empty tool_output must not classify")
	}
}
