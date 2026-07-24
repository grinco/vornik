package taintlineage

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestClassifyTool_Table(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		input   string
		wantSev Severity
		wantRef string
	}{
		{"web_fetch with url", "web_fetch", `{"url":"https://evil.example/x"}`, SeverityHigh, "https://evil.example/x"},
		{"web_fetch no url", "web_fetch", `{}`, SeverityHigh, "web_fetch"},
		{"bare fetch", "fetch", "https://a.example/p", SeverityHigh, "https://a.example/p"},
		{"scraper mcp", "mcp__scraper__get", `{"url":"http://s.example"}`, SeverityHigh, "http://s.example"},
		{"mcp fetch-in-name", "mcp__docs__fetchPage", `{"url":"https://d.example/a"}`, SeverityHigh, "https://d.example/a"},
		{"mcp browse-in-name", "mcp__web__browseSite", "no-url-here", SeverityHigh, "mcp__web__browseSite"},
		{"query_api provider path", "query_api", `{"provider":"stripe","path":"/v1/charges"}`, SeverityHigh, "stripe·/v1/charges"},
		{"query_api malformed", "query_api", "not json", SeverityHigh, "query_api"},
		{"query_api provider only", "query_api", `{"provider":"stripe"}`, SeverityHigh, "stripe"},
		{"other mcp", "mcp__github__list_issues", `{}`, SeverityHigh, "mcp__github__list_issues"},
		{"memory_search with query", "memory_search", `{"query":"how to X"}`, SeverityLow, "how to X"},
		{"memory_search raw", "memory_search", "raw query text", SeverityLow, "raw query text"},
		{"memory_search empty", "memory_search", `{}`, SeverityLow, "memory_search"},
		{"first-party file_read", "file_read", `{"path":"/etc/hosts"}`, SeverityNone, ""},
		{"first-party run_shell", "run_shell", `{"cmd":"ls"}`, SeverityNone, ""},
		{"unknown tool", "some_new_tool", `{}`, SeverityUnknown, "some_new_tool"},
		// I1: web_search / web_scrape / http_get are external-content tools → High.
		{"web_search query field", "web_search", `{"query":"how to X"}`, SeverityHigh, "how to X"},
		{"web_search q field", "web_search", `{"q":"cats"}`, SeverityHigh, "cats"},
		{"web_search no query", "web_search", `{}`, SeverityHigh, "web_search"},
		{"web_scrape url", "web_scrape", `{"url":"https://s.example/p"}`, SeverityHigh, "https://s.example/p"},
		{"web_scrape no url", "web_scrape", `{}`, SeverityHigh, "web_scrape"},
		{"http_get url", "http_get", `{"url":"https://api.example/v1"}`, SeverityHigh, "https://api.example/v1"},
		{"http_get no url", "http_get", "not a url", SeverityHigh, "http_get"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sev, ref := classifyTool(tc.tool, tc.input)
			if sev != tc.wantSev {
				t.Fatalf("severity = %v, want %v", sev, tc.wantSev)
			}
			if ref != tc.wantRef {
				t.Fatalf("ref = %q, want %q", ref, tc.wantRef)
			}
		})
	}
}

func TestClassify_MaxSeverityAndFlags(t *testing.T) {
	// Mixed: file_read (None) + memory_search (Low) + web_fetch (High) → High.
	st := Classify([]ToolCall{
		{Tool: "file_read", Input: `{}`},
		{Tool: "memory_search", Input: `{"query":"q"}`},
		{Tool: "web_fetch", Input: `{"url":"https://a.example"}`},
	})
	if st.MaxSeverity != SeverityHigh {
		t.Fatalf("max severity = %v, want High", st.MaxSeverity)
	}
	if !st.Used {
		t.Fatalf("Used should be true")
	}
	if !st.RequiresReview {
		t.Fatalf("RequiresReview should be true when High present")
	}
	// file_read contributes no source; memory_search + web_fetch do.
	if len(st.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(st.Sources))
	}
}

// F3: an Unknown-only step is used=true but requires_review=false.
func TestClassify_UnknownOnly_UsedButNotReview(t *testing.T) {
	st := Classify([]ToolCall{{Tool: "mysterious_tool", Input: `{}`}})
	if !st.Used {
		t.Fatalf("Unknown-only step must be Used=true (F3)")
	}
	if st.RequiresReview {
		t.Fatalf("Unknown must NOT set RequiresReview (D8 — gate handles it)")
	}
	if st.MaxSeverity != SeverityUnknown {
		t.Fatalf("max severity = %v, want Unknown", st.MaxSeverity)
	}
}

// Regression (explorer Q6): a memory_search-only step must NOT set requires_review.
func TestClassify_MemorySearchOnly_NoReview(t *testing.T) {
	st := Classify([]ToolCall{{Tool: "memory_search", Input: `{"query":"anything"}`}})
	if st.RequiresReview {
		t.Fatalf("memory_search-only must NOT set requires_review (explorer Q6 regression)")
	}
	if !st.Used {
		t.Fatalf("memory_search is Low → Used=true")
	}
	if st.MaxSeverity != SeverityLow {
		t.Fatalf("severity = %v, want Low", st.MaxSeverity)
	}
}

func TestClassify_Dedup(t *testing.T) {
	st := Classify([]ToolCall{
		{Tool: "web_fetch", Input: `{"url":"https://a.example"}`},
		{Tool: "web_fetch", Input: `{"url":"https://a.example"}`}, // dup (tool,ref)
		{Tool: "web_fetch", Input: `{"url":"https://b.example"}`}, // distinct ref
	})
	if len(st.Sources) != 2 {
		t.Fatalf("dedup by (tool,ref) failed: got %d sources, want 2", len(st.Sources))
	}
}

func TestClassify_CapWithCount(t *testing.T) {
	calls := make([]ToolCall, 0, MaxSources+5)
	for i := 0; i < MaxSources+5; i++ {
		calls = append(calls, ToolCall{Tool: "mcp__x__t", Input: fmt.Sprintf(`{"i":%d}`, i)})
	}
	// each has a distinct ref (tool name only — but ref is the qualified tool
	// name, identical!) — so use distinct inputs that surface distinct refs via
	// the other-mcp branch (ref = tool name → all identical → dedups to 1).
	// Instead drive distinct refs through web_fetch URLs.
	calls = calls[:0]
	for i := 0; i < MaxSources+5; i++ {
		calls = append(calls, ToolCall{Tool: "web_fetch", Input: fmt.Sprintf(`{"url":"https://a.example/%d"}`, i)})
	}
	st := Classify(calls)
	if len(st.Sources) != MaxSources {
		t.Fatalf("cap failed: got %d, want %d", len(st.Sources), MaxSources)
	}
	if st.DroppedSources != 5 {
		t.Fatalf("dropped count = %d, want 5", st.DroppedSources)
	}
}

func TestClassify_RefTruncation(t *testing.T) {
	longURL := "https://a.example/" + strings.Repeat("z", MaxRefLen+50)
	st := Classify([]ToolCall{{Tool: "web_fetch", Input: longURL}})
	if len(st.Sources) != 1 {
		t.Fatalf("want 1 source")
	}
	if len(st.Sources[0].Ref) != MaxRefLen {
		t.Fatalf("ref len = %d, want %d (truncated)", len(st.Sources[0].Ref), MaxRefLen)
	}
}

func TestHashSources_OrderIndependent(t *testing.T) {
	a := []Source{
		{Tool: "web_fetch", Ref: "https://a.example", Severity: SeverityHigh},
		{Tool: "web_fetch", Ref: "https://b.example", Severity: SeverityHigh},
		{Tool: "mcp__x__t", Ref: "mcp__x__t", Severity: SeverityHigh},
	}
	b := []Source{a[2], a[0], a[1]} // shuffled
	if HashSources(a) != HashSources(b) {
		t.Fatalf("hash must be order-independent")
	}
}

func TestHashSources_NoPrefixCollision(t *testing.T) {
	// ("ab","c") vs ("a","bc") must differ thanks to length prefixing.
	h1 := HashSources([]Source{{Tool: "ab", Ref: "c"}})
	h2 := HashSources([]Source{{Tool: "a", Ref: "bc"}})
	if h1 == h2 {
		t.Fatalf("length-prefixing must prevent field-boundary collisions")
	}
}

func TestHashSources_TruncationBeforeHash(t *testing.T) {
	base := "https://a.example/" + strings.Repeat("z", MaxRefLen)
	// Two refs identical up to MaxRefLen, differing only in the tail.
	s1 := []Source{{Tool: "web_fetch", Ref: base + "AAAA"}}
	s2 := []Source{{Tool: "web_fetch", Ref: base + "BBBB"}}
	if HashSources(s1) != HashSources(s2) {
		t.Fatalf("hash must apply truncation BEFORE hashing (beyond-MaxRefLen tail must not change the hash)")
	}
}

func TestRollup_UnionAndFlags(t *testing.T) {
	own := []StepTaint{
		{Used: true, MaxSeverity: SeverityLow, Sources: []Source{{Tool: "memory_search", Ref: "q", Severity: SeverityLow}}},
	}
	anc := []StepTaint{
		{Used: true, MaxSeverity: SeverityHigh, RequiresReview: true, Sources: []Source{{Tool: "web_fetch", Ref: "https://a.example", Severity: SeverityHigh}}},
		{Used: true, MaxSeverity: SeverityUnknown, Sources: []Source{{Tool: "weird", Ref: "weird", Severity: SeverityUnknown}}},
	}
	tt := Rollup(own, anc, true)
	if !tt.Tainted {
		t.Fatalf("tainted should be true")
	}
	if !tt.RequiresReview {
		t.Fatalf("RequiresReview should be true (ancestor High)")
	}
	if !tt.HasUnknown {
		t.Fatalf("HasUnknown should be true (ancestor Unknown)")
	}
	if tt.TotalSources != 3 {
		t.Fatalf("TotalSources = %d, want 3", tt.TotalSources)
	}
	if tt.SourceSetHash == "" {
		t.Fatalf("hash should be set")
	}
}

// M1 per-step-cap latch: a SINGLE step touching > MaxSources distinct High
// sources whose first-16 are stable but which gains a NEW overflow source on
// re-run → the persisted blob's full_hash (and thus the lineage SourceSetHash)
// CHANGES → the latch does NOT match → re-parks. This exercises the per-step
// grain of the F-cap hole: Classify caps Sources at 16 and drops the overflow
// BEFORE persistence, so without full_hash the stored set (and hash) would be
// identical across the re-run.
func TestClassify_PerStepCap_FullHashCoversOverflow(t *testing.T) {
	mk := func(n int) []ToolCall {
		calls := make([]ToolCall, 0, n)
		for i := 0; i < n; i++ {
			calls = append(calls, ToolCall{Tool: "web_fetch", Input: fmt.Sprintf(`{"url":"https://a.example/%03d"}`, i)})
		}
		return calls
	}
	before := Classify(mk(MaxSources + 3))
	after := Classify(append(mk(MaxSources+3), ToolCall{Tool: "web_fetch", Input: `{"url":"https://a.example/999"}`}))

	// Display list is capped identically; the overflow is invisible there.
	if len(before.Sources) != MaxSources || len(after.Sources) != MaxSources {
		t.Fatalf("display list must be capped at MaxSources")
	}
	// But the FULL-set hash differs → the latch key moves (M1).
	if before.FullHash == after.FullHash {
		t.Fatalf("per-step full_hash must change when a new overflow source is added (M1)")
	}

	// End-to-end through persistence blob → StepTaintFromBlob → Rollup: the
	// lineage SourceSetHash must differ across the re-run.
	bBlob, _ := json.Marshal(NewSourcesBlob(before))
	aBlob, _ := json.Marshal(NewSourcesBlob(after))
	bRoll := Rollup([]StepTaint{StepTaintFromBlob(bBlob, true)}, nil, true)
	aRoll := Rollup([]StepTaint{StepTaintFromBlob(aBlob, true)}, nil, true)
	if bRoll.SourceSetHash == aRoll.SourceSetHash {
		t.Fatalf("lineage latch key must change after a per-step overflow source (M1 end-to-end)")
	}
	// The reviewed (before) latch must NOT match the re-run (after) rollup.
	d := Decide(ModeEnforce, aRoll, []string{bRoll.SourceSetHash})
	if !d.Park {
		t.Fatalf("re-run with a new overflow source must re-park despite the prior latch (M1)")
	}
}

// F-cap cap-collision: a lineage of > MaxSources sources where a re-run adds a
// NEW high-severity source that sorts beyond the cap → the hash CHANGES → the
// latch does NOT falsely match (proves the hash is over the full set, not 16).
func TestRollup_CapCollision_HashOverFullSet(t *testing.T) {
	mk := func(n int) []StepTaint {
		srcs := make([]Source, 0, n)
		for i := 0; i < n; i++ {
			// zero-padded so lexical sort order == numeric; the added source
			// (below) sorts to the very end, beyond the MaxSources cap.
			srcs = append(srcs, Source{Tool: "web_fetch", Ref: fmt.Sprintf("https://a.example/%03d", i), Severity: SeverityHigh})
		}
		return []StepTaint{{Used: true, MaxSeverity: SeverityHigh, RequiresReview: true, Sources: srcs}}
	}
	before := Rollup(mk(MaxSources+5), nil, true)
	// Re-run: same set PLUS one new source that sorts LAST (beyond the cap).
	withNew := mk(MaxSources + 5)
	withNew[0].Sources = append(withNew[0].Sources, Source{Tool: "web_fetch", Ref: "https://a.example/999", Severity: SeverityHigh})
	after := Rollup(withNew, nil, true)

	if len(before.Sources) != MaxSources || len(after.Sources) != MaxSources {
		t.Fatalf("display list must be capped at MaxSources")
	}
	if before.SourceSetHash == after.SourceSetHash {
		t.Fatalf("hash must change when a NEW source is added beyond the cap (F-cap) — the latch must not falsely match")
	}
	if after.TotalSources != MaxSources+6 {
		t.Fatalf("TotalSources = %d, want %d", after.TotalSources, MaxSources+6)
	}
}
