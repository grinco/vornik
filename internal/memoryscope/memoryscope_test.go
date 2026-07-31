package memoryscope

import "testing"

// The behaviours pinned here are the ones whose absence produced the 2026-07-30
// census: five projects with every chunk NULL because nothing supplied a
// default, and a payload-only rule duplicated across two packages.

func TestFromPayload_AcceptedShapes(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"canonical nested context", `{"context":{"repo_scope":"github.com/acme/x"}}`, "github.com/acme/x"},
		{"legacy unnested", `{"repo_scope":"github.com/acme/y"}`, "github.com/acme/y"},
		// Nested wins: taskcreate.Creator wraps RawContext under "context", so a
		// payload carrying both is the canonical value plus a stale legacy one.
		{"nested beats legacy", `{"repo_scope":"legacy","context":{"repo_scope":"nested"}}`, "nested"},
		{"cross-cutting star is a real value", `{"context":{"repo_scope":"*"}}`, "*"},
		{"whitespace trimmed", `{"context":{"repo_scope":"  github.com/acme/z  "}}`, "github.com/acme/z"},
		// Every no-information case must yield "" rather than a partial guess:
		// downstream reads "" as "say nothing", and a guess here would stamp the
		// wrong repo on somebody's chunks.
		{"whitespace-only is empty", `{"context":{"repo_scope":"   "}}`, ""},
		{"absent", `{"context":{}}`, ""},
		{"no context at all", `{"taskType":"research"}`, ""},
		{"malformed json", `{"context":{`, ""},
		{"empty payload", ``, ""},
		{"null payload literal", `null`, ""},
		// A non-string type must not panic or coerce — json.Unmarshal fails the
		// whole struct, and "" is the safe answer.
		{"wrong type", `{"context":{"repo_scope":123}}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FromPayload([]byte(c.payload)); got != c.want {
				t.Errorf("FromPayload(%s) = %q, want %q", c.payload, got, c.want)
			}
		})
	}
}

func TestResolve_PayloadWinsOverProjectDefault(t *testing.T) {
	// The per-task value always wins: the caller knew which repo the work
	// concerned; the project default is only a guess about the common case.
	got := Resolve([]byte(`{"context":{"repo_scope":"github.com/acme/explicit"}}`), "github.com/acme/default")
	if got != "github.com/acme/explicit" {
		t.Errorf("payload scope must win, got %q", got)
	}
}

func TestResolve_ProjectDefaultFillsSilence(t *testing.T) {
	// THE REGRESSION. Before the default existed, this returned "" and the chunk
	// landed NULL — the whole reason assistant/janka/vornik-marketing/ibkr-trader
	// were 100% uncategorized.
	for _, payload := range []string{``, `{}`, `{"context":{}}`, `{"context":{"repo_scope":"  "}}`, `{"bad":`} {
		if got := Resolve([]byte(payload), "github.com/acme/default"); got != "github.com/acme/default" {
			t.Errorf("payload %q: got %q, want the project default", payload, got)
		}
	}
}

func TestResolve_NoDefaultStaysEmpty(t *testing.T) {
	// A project that is genuinely not repo-bound sets no default and keeps NULL.
	// That is correct for it, not a gap: NULL chunks surface under every scoped
	// recall. This test exists so nobody "fixes" the empty case by inventing a
	// fallback scope.
	if got := Resolve([]byte(`{"context":{}}`), ""); got != "" {
		t.Errorf("no payload scope and no default must stay empty, got %q", got)
	}
	if got := Resolve([]byte(`{"context":{}}`), "   "); got != "" {
		t.Errorf("a whitespace-only default is not a default, got %q", got)
	}
}

func TestPtr(t *testing.T) {
	if Ptr("") != nil {
		t.Error(`Ptr("") must be nil so the column gets NULL, not an empty string`)
	}
	if Ptr("   ") != nil {
		t.Error("Ptr(whitespace) must be nil")
	}
	p := Ptr("github.com/acme/x")
	if p == nil || *p != "github.com/acme/x" {
		t.Errorf("Ptr lost the value: %v", p)
	}
	// Each call must yield an INDEPENDENT copy. The failure this guards is the
	// batch loop: if Ptr returned &argument, every chunk in a batch would end up
	// pointing at whichever scope the loop finished on. Exercised the way the
	// ingest sites actually call it — accumulating pointers across iterations —
	// rather than by mutating a local, which the compiler can prove is dead.
	var got []*string
	for _, sc := range []string{"github.com/acme/first", "github.com/acme/second"} {
		got = append(got, Ptr(sc))
	}
	if len(got) != 2 || *got[0] != "github.com/acme/first" || *got[1] != "github.com/acme/second" {
		t.Errorf("Ptr aliased across iterations: %q, %q", *got[0], *got[1])
	}
}
