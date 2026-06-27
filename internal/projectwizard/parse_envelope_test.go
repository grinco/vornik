package projectwizard

import "testing"

func TestParseEnvelope_PlainJSON(t *testing.T) {
	env, err := parseEnvelope(`{"message":"hi","ready_to_commit":false}`)
	if err != nil {
		t.Fatalf("plain JSON: %v", err)
	}
	if env.Message != "hi" {
		t.Errorf("message = %q", env.Message)
	}
}

func TestParseEnvelope_FencedJSON(t *testing.T) {
	env, err := parseEnvelope("```json\n{\"message\":\"fenced\",\"ready_to_commit\":false}\n```")
	if err != nil {
		t.Fatalf("fenced: %v", err)
	}
	if env.Message != "fenced" {
		t.Errorf("message = %q", env.Message)
	}
}

// The regression: Gemini-via-Vertex returns prose around the JSON and
// ignores response_format. parseEnvelope must recover the object rather
// than failing on the leading 'A'.
func TestParseEnvelope_ProseWrapped(t *testing.T) {
	raw := `Absolutely! Here's the proposal for your project:
{"message":"I'll set that up.","ready_to_commit":false,"suggested_template":"news-feed"}
Let me know if you'd like changes.`
	env, err := parseEnvelope(raw)
	if err != nil {
		t.Fatalf("prose-wrapped: %v", err)
	}
	if env.Message != "I'll set that up." {
		t.Errorf("message = %q", env.Message)
	}
	if env.SuggestedTemplate != "news-feed" {
		t.Errorf("suggested_template = %q", env.SuggestedTemplate)
	}
}

// Braces inside string values must not throw off the balanced scan.
func TestParseEnvelope_ProseWrappedWithBracesInStrings(t *testing.T) {
	raw := `Sure: {"message":"use {{.projectId}} and a } brace","ready_to_commit":true}  — done`
	env, err := parseEnvelope(raw)
	if err != nil {
		t.Fatalf("braces-in-strings: %v", err)
	}
	if env.Message != "use {{.projectId}} and a } brace" {
		t.Errorf("message = %q", env.Message)
	}
	if !env.ReadyToCommit {
		t.Error("ready_to_commit should be true")
	}
}

func TestParseEnvelope_NestedProposalObject(t *testing.T) {
	raw := `Here you go:\n{"message":"draft","ready_to_commit":false,"proposal":{"raw":{"projectId":"acme","autonomy":{"enabled":true}}}}`
	env, err := parseEnvelope(raw)
	if err != nil {
		t.Fatalf("nested: %v", err)
	}
	if env.Proposal == nil || env.Proposal.Raw["projectId"] != "acme" {
		t.Errorf("nested proposal not recovered: %+v", env.Proposal)
	}
}

func TestParseEnvelope_EmptyAndNoJSON(t *testing.T) {
	// Truly empty → still an error (nothing to render).
	if _, err := parseEnvelope("   "); err == nil {
		t.Error("empty body must error")
	}
	// Valid JSON object but missing the required message field → error
	// (it's structured-but-invalid, not a chatty model).
	if _, err := parseEnvelope(`{"ready_to_commit":false}`); err == nil {
		t.Error("missing message in a JSON envelope must error")
	}
}

// TestParseEnvelope_PureProseBecomesMessage: a model that ignores
// response_format and answers in plain prose (no JSON at all) must not
// 502 the wizard turn — the prose becomes the assistant's chat message
// so the conversation continues. Regression for the minimax/kimi/gemini
// "invalid character 'W'/'G'" failures.
func TestParseEnvelope_PureProseBecomesMessage(t *testing.T) {
	env, err := parseEnvelope("What's the main goal for these workflows? For example: data processing, or API integration?")
	if err != nil {
		t.Fatalf("pure prose must not error, got %v", err)
	}
	if env.Message == "" {
		t.Error("prose should be carried as the message")
	}
	if env.ReadyToCommit {
		t.Error("a prose turn is never ready_to_commit")
	}
	if env.Proposal != nil {
		t.Error("a prose turn carries no proposal")
	}
}

func TestFirstJSONObject(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{`pre {"a":1} post`, `{"a":1}`, true},
		{`{"a":{"b":2}}`, `{"a":{"b":2}}`, true},
		{`{"s":"}{"}`, `{"s":"}{"}`, true}, // braces in string ignored
		{`no object`, "", false},
		{`{"unterminated":`, "", false}, // no matching close
	}
	for _, c := range cases {
		got, ok := firstJSONObject(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("firstJSONObject(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
