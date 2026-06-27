package ui

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// Phase 33 — shortID lock-in. The display format is contractual
// because operators learn to recognise short IDs visually
// (T-af2e on this card matches T-af2e in the URL bar, etc.).
// A regression that changes the prefix or character count is
// a UX break.

// TestTaskSummary_ExtractsPromptFromPayload covers the priority
// ordering of the payload-field lookup. The roadmap entry
// scoped four field paths in priority order; this test pins
// the production behaviour against each.
func TestTaskSummary_ExtractsPromptFromPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "args.prompt wins over context.prompt",
			payload: `{"args":{"prompt":"refresh the watchlist for Q3"},"context":{"prompt":"older field"}}`,
			want:    "refresh the watchlist for Q3",
		},
		{
			name:    "context.prompt fallback",
			payload: `{"context":{"prompt":"build the landing page"}}`,
			want:    "build the landing page",
		},
		{
			name:    "context.brief fallback",
			payload: `{"context":{"brief":"summarise yesterday's signals"}}`,
			want:    "summarise yesterday's signals",
		},
		{
			name:    "args.body fallback (email-ingested)",
			payload: `{"args":{"body":"please review the attached spec\nand reply"}}`,
			want:    "please review the attached spec",
		},
		{
			name:    "slug prefix stripped",
			payload: `{"args":{"prompt":"linkedin-jobs-cz: search for Go roles"}}`,
			want:    "search for Go roles",
		},
		{
			name:    "long input truncated with ellipsis",
			payload: `{"args":{"prompt":"` + strings.Repeat("x", 200) + `"}}`,
			want:    strings.Repeat("x", 139) + "…",
		},
		{
			name:    "empty payload",
			payload: ``,
			want:    "",
		},
		{
			name:    "malformed json",
			payload: `{not json}`,
			want:    "",
		},
		{
			name:    "no recognised field",
			payload: `{"foo":"bar"}`,
			want:    "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := taskSummary([]byte(c.payload))
			if got != c.want {
				t.Errorf("taskSummary(%q) = %q, want %q", c.payload, got, c.want)
			}
		})
	}
}

func TestShortID_TaskPrefix(t *testing.T) {
	got := shortID("task_20260509213432_0655946c0465af2e")
	if got != "T-af2e" {
		t.Errorf("got %q want T-af2e", got)
	}
}

func TestShortID_AllPrefixes(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"task_20260509213432_0655946c0465af2e", "T-af2e"},
		{"exec_20260509213433_b7edd48f2fb4c68a", "X-c68a"},
		{"execution_20260509_b7edd48f2fb4c68a", "X-c68a"},
		{"tmsg_20260509213439_1c6b7e05e63e52bf", "M-52bf"},
		{"msg_20260509_1c6b7e05e63e52bf", "M-52bf"},
		{"cep_20260507143012_2b8a9c4d", "E-9c4d"},
		{"epoch_20260507_2b8a9c4d", "E-9c4d"},
		{"art_20260507_1f0a", "A-1f0a"},
		{"artifact_20260507_a1f0a", "A-1f0a"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := shortID(tc.in); got != tc.want {
				t.Errorf("shortID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestShortID_UnknownPrefixUnchanged(t *testing.T) {
	// External / legacy IDs without a known prefix should pass
	// through unchanged so callers can pipe arbitrary values.
	cases := []string{
		"abc",
		"unknown_2026_xyzw",
		"plain-no-underscore",
		"ta_1778354637_call_TN77D0NwqnP0t7gsVSekR4X9",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if got := shortID(in); got != in {
				t.Errorf("expected unchanged for %q, got %q", in, got)
			}
		})
	}
}

func TestShortID_TooShort(t *testing.T) {
	if got := shortID("tsk"); got != "tsk" {
		t.Errorf("very short input must pass through, got %q", got)
	}
	if got := shortID(""); got != "" {
		t.Errorf("empty input must pass through, got %q", got)
	}
}

// statusPill is rendered into HTML; lock the contract that the
// label text is the full status string (no abbreviation, no
// colour-only). That's a Phase 41 a11y guarantee.
func TestStatusPill_LabelIsFullStatus(t *testing.T) {
	html := string(statusPill("AWAITING_INPUT"))
	if !strings.Contains(html, "AWAITING_INPUT") {
		t.Errorf("pill must contain the full status text, got %q", html)
	}
	// classes for AWAITING_INPUT are amber-tinted.
	if !strings.Contains(html, "amber") {
		t.Errorf("AWAITING_INPUT pill should have amber colour class")
	}
}

func TestStatusPill_UnknownStatusFallsBackToNeutral(t *testing.T) {
	html := string(statusPill("WEIRD_NEW_STATE"))
	if !strings.Contains(html, "WEIRD_NEW_STATE") {
		t.Errorf("unknown status must still render its label")
	}
	// Falls back to gray (no amber/brand/etc.).
	if strings.Contains(html, "amber") || strings.Contains(html, "brand") {
		t.Errorf("unknown status must fall back to neutral, got %q", html)
	}
}

// renderInlineMarkdown is rendered into messages in the conversation
// thread; it must be HTML-safe (escape-first, mark-second) so an
// operator pasting `<script>` into a task message can't XSS the
// next person to view the thread. Lock the contract.

func TestRenderMarkdown_EscapesHTMLBeforeMarkup(t *testing.T) {
	got := string(renderInlineMarkdown(`<script>alert(1)</script>`))
	if strings.Contains(got, "<script>") {
		t.Errorf("must escape raw HTML, got %q", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Errorf("expected escaped script tag, got %q", got)
	}
}

func TestRenderMarkdown_BoldItalicCode(t *testing.T) {
	got := string(renderInlineMarkdown("**bold** *italic* `code`"))
	if !strings.Contains(got, "<strong>bold</strong>") {
		t.Errorf("bold not rendered, got %q", got)
	}
	if !strings.Contains(got, "<em>italic</em>") {
		t.Errorf("italic not rendered, got %q", got)
	}
	if !strings.Contains(got, "<code") || !strings.Contains(got, ">code</code>") {
		t.Errorf("code not rendered, got %q", got)
	}
}

func TestRenderMarkdown_Links_OnlyHTTPS(t *testing.T) {
	// http(s) links render; javascript: / file: don't.
	ok := string(renderInlineMarkdown("[click](https://example.com/path)"))
	if !strings.Contains(ok, `href="https://example.com/path"`) {
		t.Errorf("https link missed, got %q", ok)
	}
	bad := string(renderInlineMarkdown("[xss](javascript:alert(1))"))
	if strings.Contains(bad, "javascript:") && strings.Contains(bad, "<a ") {
		t.Errorf("javascript: link must NOT render as <a>, got %q", bad)
	}
}

// TestRenderMarkdown_Links_RejectsControlChars asserts the
// renderer rejects URLs containing whitespace or control bytes.
// Well-formed http(s) URLs never carry these unencoded — when
// they appear it's a sign of attacker shaping (trying to inject
// extra attributes) or a malformed source. Stricter rejection
// keeps the safety invariant explicit even when the surrounding
// HTML-escape pass would normally make the markup safe anyway.
func TestRenderMarkdown_Links_RejectsControlChars(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"space-in-url", "[x](https://e.com/ foo)"},
		{"tab-in-url", "[x](https://e.com/\tfoo)"},
		{"newline-in-url", "[x](https://e.com/\nfoo)"},
		{"null-byte", "[x](https://e.com/\x00)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(renderInlineMarkdown(c.in))
			// On rejection the link is left as-is (markdown
			// fallback); no <a> tag is emitted.
			if strings.Contains(got, "<a ") {
				t.Errorf("malformed URL %q rendered as <a>, want literal pass-through; got %q", c.in, got)
			}
		})
	}
}

func TestRenderMarkdown_FencedCode(t *testing.T) {
	got := string(renderInlineMarkdown("```go\nfmt.Println(\"hi\")\n```"))
	if !strings.Contains(got, "<pre") || !strings.Contains(got, "fmt.Println") {
		t.Errorf("fenced code not rendered, got %q", got)
	}
	// HTML inside the fence must still be escaped.
	got2 := string(renderInlineMarkdown("```\n<script>x</script>\n```"))
	if strings.Contains(got2, "<script>") {
		t.Errorf("fenced HTML must escape, got %q", got2)
	}
}

func TestRenderMarkdown_Newlines(t *testing.T) {
	got := string(renderInlineMarkdown("line one\nline two"))
	if !strings.Contains(got, "line one<br>line two") {
		t.Errorf("newline → <br> failed, got %q", got)
	}
}

func TestRenderMarkdown_Empty(t *testing.T) {
	if got := renderInlineMarkdown(""); got != "" {
		t.Errorf("empty input must yield empty string, got %q", got)
	}
}

func TestStatusPillClasses_AllConversationalStatusesCovered(t *testing.T) {
	// Every status from the conversational lifecycle taxonomy
	// (Phase 23) must map to a non-default colour. Catches a
	// regression that adds a new status without updating
	// statusPillClasses.
	statuses := []string{
		"PENDING", "QUEUED", "LEASED", "RUNNING",
		"WAITING_FOR_CHILDREN", "COMPLETED", "FAILED", "CANCELLED",
		"AWAITING_INPUT", "AWAITING_EXTERNAL", "PAUSED", "CLOSED",
	}
	defaultCls := statusPillClasses("UNMAPPED_FALLBACK")
	for _, s := range statuses {
		if statusPillClasses(s) == defaultCls {
			t.Errorf("status %q falls back to default classes; add an explicit branch", s)
		}
	}
}

// TestTaskStatusBadgeTemplate_AllConversationalStatusesCovered
// pins the taskStatusBadge template (used by tasks.html, the
// project task table, and the spend report) against the same Phase
// 23 taxonomy. Pre-fix AWAITING_INPUT / AWAITING_EXTERNAL / PAUSED /
// CLOSED fell through to the unlabelled gray fallback while
// statusPill (Go helper) rendered them correctly — operators saw
// "right pill on the detail page, blank-looking pill on the list".
// 2026-05-21 reported gap.
func TestTaskStatusBadgeTemplate_AllConversationalStatusesCovered(t *testing.T) {
	srv := NewServer()
	if srv.templates == nil {
		t.Fatal("templates not loaded on NewServer()")
	}
	statuses := map[string]string{
		"PENDING":              "Pending",
		"QUEUED":               "Queued",
		"LEASED":               "Leased",
		"RUNNING":              "Running",
		"WAITING_FOR_CHILDREN": "Waiting",
		"AWAITING_INPUT":       "Awaiting input",
		"AWAITING_EXTERNAL":    "Awaiting external",
		"PAUSED":               "Paused",
		"COMPLETED":            "Completed",
		"CLOSED":               "Closed",
		"FAILED":               "Failed",
		"CANCELLED":            "Cancelled",
	}
	for status, wantLabel := range statuses {
		var buf bytes.Buffer
		if err := srv.templates.ExecuteTemplate(&buf, "taskStatusBadge", status); err != nil {
			t.Fatalf("execute taskStatusBadge for %q: %v", status, err)
		}
		got := buf.String()
		if !strings.Contains(got, wantLabel) {
			t.Errorf("status %q: rendered badge missing label %q; got: %s", status, wantLabel, got)
		}
		// Fallback branch renders the status literal in a generic
		// gray pill — assert no covered status falls into it.
		if strings.Contains(got, ">"+status+"</span>") && wantLabel != status {
			t.Errorf("status %q rendered as raw uppercase (fallback branch fired) — add a {{else if}} clause", status)
		}
	}
}

// TestIndentStyle covers depth → CSS mapping including the zero-
// depth no-op and the 10-level cap that guards against absurd
// indents from malformed depth values.
func TestIndentStyle(t *testing.T) {
	if got := string(indentStyle(0)); got != "" {
		t.Errorf("depth 0 should be empty, got %q", got)
	}
	if got := string(indentStyle(1)); got != "padding-left: 1rem;" {
		t.Errorf("depth 1: %q", got)
	}
	if got := string(indentStyle(7)); got != "padding-left: 7rem;" {
		t.Errorf("depth 7: %q", got)
	}
	// Cap kicks in.
	if got := string(indentStyle(50)); got != "padding-left: 10rem;" {
		t.Errorf("depth 50 should be capped at 10rem, got %q", got)
	}
	// Negative values treated as zero.
	if got := string(indentStyle(-3)); got != "" {
		t.Errorf("negative depth should be empty, got %q", got)
	}
}

// TestHierarchyMeta_NilSafe covers the template-side accessor.
func TestHierarchyMeta_NilSafe(t *testing.T) {
	zero := TaskHierarchyMeta{}
	if got := hierarchyMeta(nil, "anything"); got != zero {
		t.Errorf("nil map should return zero meta, got %+v", got)
	}
	m := map[string]TaskHierarchyMeta{"a": {Depth: 2, ChildCount: 5}}
	if got := hierarchyMeta(m, "a"); got.Depth != 2 || got.ChildCount != 5 {
		t.Errorf("expected scripted meta, got %+v", got)
	}
	if got := hierarchyMeta(m, "missing"); got != zero {
		t.Errorf("missing key should return zero meta, got %+v", got)
	}
}

func TestLogHTMLAndSSEData(t *testing.T) {
	html := logHTML("[INFO] <hello>\n[ERROR] boom\n")
	if strings.Contains(html, "<hello>") {
		t.Fatalf("logHTML must escape raw HTML, got %q", html)
	}
	for _, want := range []string{"&lt;hello&gt;", "text-blue-400", "text-red-500", "<pre"} {
		if !strings.Contains(html, want) {
			t.Fatalf("logHTML missing %q in %q", want, html)
		}
	}

	empty := logHTML("")
	if !strings.Contains(empty, "No logs available yet.") {
		t.Fatalf("empty logHTML should render fallback, got %q", empty)
	}

	got := sseData("line1\nline2")
	want := "data: line1\ndata: line2\n\n"
	if got != want {
		t.Fatalf("sseData() = %q, want %q", got, want)
	}
}

func TestTrimLogLines(t *testing.T) {
	in := "a\nb\nc\nd\n"
	if got := trimLogLines(in, 0); got != in {
		t.Fatalf("trimLogLines max<=0 = %q", got)
	}
	if got := trimLogLines(in, 2); got != "c\nd" {
		t.Fatalf("trimLogLines tail = %q", got)
	}
	if got := trimLogLines("a\nb", 5); got != "a\nb" {
		t.Fatalf("trimLogLines short = %q", got)
	}
}

func TestMemoryTemplateMathHelpers(t *testing.T) {
	f := pipelineFunnel{Enqueued: 10, Admitted: 25, Quarantined: 8}
	if got := f.BarMax(); got != 25 {
		t.Fatalf("BarMax() = %d, want 25", got)
	}
	if got := (pipelineFunnel{}).BarMax(); got != 1 {
		t.Fatalf("zero BarMax() = %d, want 1", got)
	}

	cases := []struct {
		n, max, want int
	}{
		{25, 100, 25},
		{200, 100, 100},
		{-1, 100, 0},
		{10, 0, 0},
	}
	for _, tc := range cases {
		if got := pctOf(tc.n, tc.max); got != tc.want {
			t.Fatalf("pctOf(%d,%d) = %d, want %d", tc.n, tc.max, got, tc.want)
		}
	}

	if got := mathLog(math.E); math.Abs(got-1) > 0.0001 {
		t.Fatalf("mathLog(e) = %f, want 1", got)
	}
}
