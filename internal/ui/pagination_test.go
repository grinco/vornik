package ui

import (
	"net/url"
	"testing"
)

func TestNewPaginator_WindowAndControls(t *testing.T) {
	q := url.Values{"status": {"all"}}
	p := NewPaginator(57, 2, 25, "/ui/projects/x/keys", q)

	if p.Pages != 3 {
		t.Errorf("Pages = %d; want 3 (57/25)", p.Pages)
	}
	if p.Start != 26 || p.End != 50 {
		t.Errorf("window = %d–%d; want 26–50", p.Start, p.End)
	}
	if !p.HasPrev || !p.HasNext {
		t.Errorf("page 2 of 3 should have prev AND next; got prev=%v next=%v", p.HasPrev, p.HasNext)
	}
	// Navigation must preserve the status filter and set the right page.
	pu, _ := url.Parse(p.NextURL)
	if pu.Query().Get("status") != "all" {
		t.Errorf("NextURL dropped the status filter: %s", p.NextURL)
	}
	if pu.Query().Get("page") != "3" {
		t.Errorf("NextURL page = %q; want 3", pu.Query().Get("page"))
	}
	if pp, _ := url.Parse(p.PrevURL); pp.Query().Get("page") != "1" {
		t.Errorf("PrevURL page = %q; want 1", pp.Query().Get("page"))
	}
}

func TestNewPaginator_LastPagePartialWindow(t *testing.T) {
	p := NewPaginator(57, 3, 25, "/x", nil)
	if p.Start != 51 || p.End != 57 {
		t.Errorf("last page window = %d–%d; want 51–57", p.Start, p.End)
	}
	if p.HasNext {
		t.Error("last page must not have next")
	}
}

func TestNewPaginator_ClampsOverflowAndEmpty(t *testing.T) {
	// page past the end clamps to the last page.
	if p := NewPaginator(10, 99, 25, "/x", nil); p.Page != 1 || p.Pages != 1 {
		t.Errorf("overflow clamp = page %d/%d; want 1/1", p.Page, p.Pages)
	}
	// zero items: one empty page, no controls, no 0–0 lie.
	e := NewPaginator(0, 1, 25, "/x", nil)
	if e.Total != 0 || e.Start != 0 || e.HasPrev || e.HasNext {
		t.Errorf("empty paginator = %+v; want total 0, start 0, no controls", e)
	}
}

func TestPageWindow_SlicesAndClamps(t *testing.T) {
	items := make([]int, 57)
	for i := range items {
		items[i] = i
	}
	if got := pageWindow(items, 1, 25); len(got) != 25 || got[0] != 0 {
		t.Errorf("page 1 = %d items starting %d; want 25 starting 0", len(got), got[0])
	}
	if got := pageWindow(items, 3, 25); len(got) != 7 || got[0] != 50 {
		t.Errorf("page 3 = %d items starting %d; want 7 starting 50", len(got), got[0])
	}
	// stale page past the end clamps to the last window, never empty.
	if got := pageWindow(items, 99, 25); len(got) != 7 || got[0] != 50 {
		t.Errorf("overflow page = %d items starting %d; want last window", len(got), got[0])
	}
	if got := pageWindow([]int{}, 1, 25); len(got) != 0 {
		t.Errorf("empty input should stay empty; got %d", len(got))
	}
}

func TestParsePageParam(t *testing.T) {
	cases := map[string]int{"": 1, "0": 1, "-3": 1, "abc": 1, "4": 4}
	for in, want := range cases {
		if got := parsePageParam(url.Values{"page": {in}}); got != want {
			t.Errorf("parsePageParam(%q) = %d; want %d", in, got, want)
		}
	}
}
