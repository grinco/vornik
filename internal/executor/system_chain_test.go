package executor

import (
	"encoding/json"
	"strings"
	"testing"
)

// systemResultChain's own contract (design amendment 2026-09-09).

func chainOf(t *testing.T, steps ...[3]string) *systemResultChain {
	t.Helper()
	c := &systemResultChain{}
	for _, s := range steps {
		body, _ := json.Marshal(map[string]string{"message": s[2]})
		c.Append(s[0], s[1], body)
	}
	return c
}

// ONE SECTION RENDERS BARE. Every deployed workflow with a single system→agent
// boundary must be byte-identical to before this existed — that is what makes
// the change safe to ship.
func TestSystemChain_OneSectionIsByteIdenticalToTheBareBody(t *testing.T) {
	got := chainOf(t, [3]string{"fetch", "forge.fetch_diff", "the diff"}).Render()
	if got != "the diff" {
		t.Errorf("Render() = %q, want the bare body with no header", got)
	}
}

func TestSystemChain_EmptyRendersEmpty(t *testing.T) {
	if got := (&systemResultChain{}).Render(); got != "" {
		t.Errorf("Render() = %q, want empty", got)
	}
}

// A handler with no message adds no section at all — not an empty one.
func TestSystemChain_NoMessageAddsNoSection(t *testing.T) {
	c := &systemResultChain{}
	c.Append("a", "h.a", json.RawMessage(`{"message":"real"}`))
	c.Append("b", "h.b", json.RawMessage(`{"detail":"structured only"}`))
	c.Append("c", "h.c", nil)
	c.Append("d", "h.d", json.RawMessage(`not json`))
	if len(c.sections) != 1 {
		t.Fatalf("sections = %d, want 1", len(c.sections))
	}
	if got := c.Render(); got != "real" {
		t.Errorf("Render() = %q — a section-less handler must leave one section rendering bare", got)
	}
}

// THE BUDGET IS A TOTAL, and it is spent first-come so the EARLIER section
// survives whole. The claim under test is not "it was capped" — it is "the diff
// survived and the enrichment gave way".
func TestSystemChain_BudgetIsTotalAndLaterSectionsDegrade(t *testing.T) {
	// Leave room for the marker plus a little content, so the later section is
	// TRUNCATED rather than dropped. The "not even room for the marker" case is
	// its own test below.
	big := strings.Repeat("d", systemResultMaxBytes-len(systemResultTruncationMarker)-500)
	c := &systemResultChain{}
	c.Append("fetch_diff", "forge.fetch_diff", mustMsg(big))
	c.Append("fetch_ci", "forge.fetch_ci", mustMsg(strings.Repeat("c", 5000)))

	if len(c.sections) != 2 {
		t.Fatalf("sections = %d, want 2", len(c.sections))
	}
	if c.sections[0].body != big {
		t.Errorf("the FIRST section was truncated; the primary input must survive whole")
	}
	if !strings.Contains(c.sections[1].body, systemResultTruncationMarker) {
		t.Error("the later section must be truncated WITH the marker — a silent cut is the same defect one level down")
	}
	if c.used > systemResultMaxBytes {
		t.Errorf("used = %d, over the total budget of %d", c.used, systemResultMaxBytes)
	}
}

// A chain that has already spent the budget adds nothing further, rather than
// growing unbounded.
func TestSystemChain_AnExhaustedBudgetAddsNothing(t *testing.T) {
	c := &systemResultChain{}
	c.Append("a", "h.a", mustMsg(strings.Repeat("x", systemResultMaxBytes)))
	c.Append("b", "h.b", mustMsg("more"))
	if len(c.sections) != 1 {
		t.Errorf("sections = %d, want 1 — the budget was already spent", len(c.sections))
	}
}

func TestSystemChain_ResetClearsEverything(t *testing.T) {
	c := chainOf(t, [3]string{"a", "h.a", "one"}, [3]string{"b", "h.b", "two"})
	c.Reset()
	if c.Render() != "" || c.used != 0 || len(c.sections) != 0 {
		t.Errorf("Reset left state behind: sections=%d used=%d", len(c.sections), c.used)
	}
	// And it is reusable afterwards.
	c.Append("c", "h.c", mustMsg("three"))
	if got := c.Render(); got != "three" {
		t.Errorf("Render() after reset = %q, want %q", got, "three")
	}
}

func mustMsg(s string) json.RawMessage {
	b, err := json.Marshal(map[string]string{"message": s})
	if err != nil {
		panic(err)
	}
	return b
}

// When the remaining budget cannot hold the marker AND some content, the
// section is dropped rather than reduced to a bare truncation notice — a
// section that says only "this was cut" gives the agent nothing to use.
func TestSystemChain_NoRoomForTheMarkerDropsTheSection(t *testing.T) {
	c := &systemResultChain{}
	c.Append("a", "h.a", mustMsg(strings.Repeat("x", systemResultMaxBytes-10)))
	c.Append("b", "h.b", mustMsg(strings.Repeat("y", 1000)))
	if len(c.sections) != 1 {
		t.Errorf("sections = %d, want 1 — 10 bytes cannot hold the marker", len(c.sections))
	}
	if c.used > systemResultMaxBytes {
		t.Errorf("used = %d, over budget %d", c.used, systemResultMaxBytes)
	}
}
