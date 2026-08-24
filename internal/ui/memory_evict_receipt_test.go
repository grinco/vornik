package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The eviction panel used to report nothing at all.
//
// MemoryEvictAction discarded HardEvict's return (`_, err :=`) and redirected,
// so an operator erasing chunks — the panel names GDPR Art 17 as a use case —
// got no confirmation of any kind. That mattered more once eviction started
// removing what the chunks DERIVED: the memory_eviction_audit tombstones below
// the form account for the chunks, and nothing anywhere records the graph
// entities, graph edges, quarantined pre-ingest copies and cached embeddings
// that went with them. The redirect is the last moment anyone can be told.

func TestMemoryEvictAction_redirectCarriesWhatWasRemoved(t *testing.T) {
	ev := &stubMemoryEvictor{
		deleted: 2,
		derived: MemoryEvictionResult{
			GraphEntities: 4, GraphEdges: 7, QuarantinedCopies: 1, CachedEmbeddings: 3,
		},
	}
	srv := NewServer(WithMemoryEvictor(ev))

	form := url.Values{}
	form.Set("chunks", "chunk_1, chunk_2")
	form.Set("reason", "GDPR DSAR 2026-08-21")
	form.Set("confirm", "yes")
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/janka/evict", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryEvictAction(rec, req, "janka")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303; body=%q", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Path != "/ui/memory/janka" {
		t.Errorf("redirect path = %q, want the project's memory page", loc.Path)
	}
	q := loc.Query()
	for key, want := range map[string]string{
		"notice": "evicted", "chunks": "2", "entities": "4",
		"edges": "7", "quarantined": "1", "cached": "3",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("redirect %s = %q, want %q — the operator is the last person "+
				"who can be told what the derived sweep removed", key, got, want)
		}
	}
}

// A failed eviction must not redirect with a receipt claiming success.
func TestMemoryEvictAction_failureCarriesNoReceipt(t *testing.T) {
	ev := &stubMemoryEvictor{hardErr: context.DeadlineExceeded}
	srv := NewServer(WithMemoryEvictor(ev))

	form := url.Values{}
	form.Set("chunks", "chunk_1")
	form.Set("confirm", "yes")
	req := httptest.NewRequest(http.MethodPost, "/ui/memory/janka/evict", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.MemoryEvictAction(rec, req, "janka")

	if rec.Code == http.StatusSeeOther {
		t.Fatal("a failed eviction must surface, not redirect to a success banner")
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("no redirect expected, got %q", loc)
	}
}

// The receipt is SIGNED and time-bounded, so these go through the same
// evictionRedirect the handler builds rather than hand-writing a query — a test
// that constructed the URL itself would pass while real receipts failed to
// verify.
func TestEvictionReceipt_roundTrips(t *testing.T) {
	srv := NewServer()
	now := time.Now()
	want := evictionNotice{Chunks: 2, Entities: 4, Edges: 7, Quarantine: 1, Cached: 3}

	loc, err := url.Parse(srv.evictionRedirect("p1", want, now))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	got := srv.parseEvictionNotice("p1", loc.Query(), now)
	if got == nil {
		t.Fatal("a receipt this server just signed must verify")
	}
	if got.Chunks != 2 || got.Entities != 4 || got.Edges != 7 ||
		got.Quarantine != 1 || got.Cached != 3 {
		t.Errorf("counts not round-tripped: %+v", got)
	}
	if !got.Derived() {
		t.Error("Derived() must be true when derived rows went")
	}
	if got.At.Unix() != now.Unix() {
		t.Errorf("completion time not carried: %v", got.At)
	}
}

func TestEvictionReceipt_absentOnAnOrdinaryPageLoad(t *testing.T) {
	srv := NewServer()
	if n := srv.parseEvictionNotice("p1", url.Values{}, time.Now()); n != nil {
		t.Errorf("no banner without notice=evicted, got %+v", n)
	}
	if n := srv.parseEvictionNotice("p1", url.Values{"notice": {"other"}}, time.Now()); n != nil {
		t.Errorf("only the evicted token renders this banner, got %+v", n)
	}
}

// A forged link must render NOTHING. Query parameters are attacker-controllable,
// and this surface names Article 17 in its own help: a fabricated confirmation
// that an erasure happened is worse than no confirmation, because the operator
// stops looking. The banner even says the derived counts are "recorded nowhere
// else", which discourages the one check that would disprove it.
func TestEvictionReceipt_forgedLinkRendersNothing(t *testing.T) {
	srv := NewServer()
	now := time.Now()

	t.Run("no signature at all", func(t *testing.T) {
		q := url.Values{
			"notice": {"evicted"}, "chunks": {"500"},
			"at": {strconv.FormatInt(now.Unix(), 10)},
		}
		if n := srv.parseEvictionNotice("p1", q, now); n != nil {
			t.Fatalf("an unsigned receipt must not render, got %+v", n)
		}
	})

	t.Run("counts altered after signing", func(t *testing.T) {
		loc, _ := url.Parse(srv.evictionRedirect("p1", evictionNotice{Chunks: 1}, now))
		q := loc.Query()
		q.Set("chunks", "500") // the lie
		if n := srv.parseEvictionNotice("p1", q, now); n != nil {
			t.Fatalf("tampered counts must not render, got %+v", n)
		}
	})

	t.Run("replayed onto another project", func(t *testing.T) {
		loc, _ := url.Parse(srv.evictionRedirect("p1", evictionNotice{Chunks: 9}, now))
		if n := srv.parseEvictionNotice("p2", loc.Query(), now); n != nil {
			t.Fatalf("a receipt is bound to its project, got %+v", n)
		}
	})

	t.Run("signed by a different server", func(t *testing.T) {
		other := NewServer()
		loc, _ := url.Parse(other.evictionRedirect("p1", evictionNotice{Chunks: 9}, now))
		if n := srv.parseEvictionNotice("p1", loc.Query(), now); n != nil {
			t.Fatalf("another key's signature must not verify, got %+v", n)
		}
	})
}

// Replay: a bookmarked or back-buttoned URL must stop showing "Erased N chunks"
// rather than look fresh forever.
func TestEvictionReceipt_expires(t *testing.T) {
	srv := NewServer()
	issued := time.Now()
	loc, _ := url.Parse(srv.evictionRedirect("p1", evictionNotice{Chunks: 3}, issued))

	if n := srv.parseEvictionNotice("p1", loc.Query(), issued.Add(receiptTTL-time.Minute)); n == nil {
		t.Error("a receipt must survive a reload or a detour into another tab")
	}
	if n := srv.parseEvictionNotice("p1", loc.Query(), issued.Add(receiptTTL+time.Minute)); n != nil {
		t.Errorf("an expired receipt must stop rendering, got %+v", n)
	}
	// A future timestamp is a forgery attempt or an unreasonable clock.
	if n := srv.parseEvictionNotice("p1", loc.Query(), issued.Add(-time.Hour)); n != nil {
		t.Errorf("a receipt from the future must not render, got %+v", n)
	}
}

// Unparseable counts drop the whole receipt rather than rendering zeroes: a
// banner an operator cannot trust is worse than none.
func TestEvictionReceipt_garbageCountsDropTheBanner(t *testing.T) {
	srv := NewServer()
	now := time.Now()
	loc, _ := url.Parse(srv.evictionRedirect("p1", evictionNotice{Chunks: 1}, now))

	for _, bad := range []string{"<script>alert(1)</script>", "-5", "1e9999"} {
		q := loc.Query()
		q.Set("entities", bad)
		if n := srv.parseEvictionNotice("p1", q, now); n != nil {
			t.Errorf("count %q must drop the receipt, got %+v", bad, n)
		}
	}
}

// Zero chunks is a real outcome — every id stale, wrong-project or already
// evicted — and must still produce a receipt saying so.
func TestEvictionReceipt_zeroChunksStillReports(t *testing.T) {
	srv := NewServer()
	now := time.Now()
	loc, _ := url.Parse(srv.evictionRedirect("p1", evictionNotice{}, now))

	n := srv.parseEvictionNotice("p1", loc.Query(), now)
	if n == nil {
		t.Fatal("a zero-chunk eviction must still produce a receipt")
	}
	if n.Derived() {
		t.Error("nothing derived went either")
	}
}

func TestEvictionNotice_nilReceiver(t *testing.T) {
	var n *evictionNotice
	if n.Derived() {
		t.Error("a nil notice has no derived rows")
	}
}

// The banner must actually RENDER. The handler tests above prove the counts
// reach the page; this proves the page shows them, which is the whole point —
// a template that silently dropped the block would leave the operator exactly
// as uninformed as before.
func TestMemoryProject_rendersTheEvictionReceipt(t *testing.T) {
	srv := NewServer(WithMemoryEvictor(&stubMemoryEvictor{}))

	loc := srv.evictionRedirect("p1", evictionNotice{
		Chunks: 2, Entities: 4, Edges: 7, Quarantine: 2, Cached: 3,
	}, time.Now())
	rec := httptest.NewRecorder()
	srv.MemoryProject(rec, httptest.NewRequest(http.MethodGet, loc, nil), "p1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Erased 2 chunks",
		"at ", // the completion time, so a replay cannot pass as fresh
		"Derived data removed with them",
		"knowledge entities",
		"graph edges",
		"quarantined pre-ingest copies",
		"cached embeddings",
		"recorded nowhere else",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("receipt missing %q", want)
		}
	}
}

// An ordinary page load shows no banner — it is a one-shot receipt, not a
// permanent panel.
func TestMemoryProject_noReceiptWithoutTheRedirect(t *testing.T) {
	srv := NewServer(WithMemoryEvictor(&stubMemoryEvictor{}))

	req := httptest.NewRequest(http.MethodGet, "/ui/memory/p1", nil)
	rec := httptest.NewRecorder()
	srv.MemoryProject(rec, req, "p1")

	if strings.Contains(rec.Body.String(), "Derived data removed with them") {
		t.Error("the receipt must appear only after an eviction, not on every page load")
	}
}

// Singular/plural must not read as a bug on the one-row case, which is the
// common one for a targeted erasure.
func TestMemoryProject_receiptReadsCorrectlyForASingleChunk(t *testing.T) {
	srv := NewServer(WithMemoryEvictor(&stubMemoryEvictor{}))

	loc := srv.evictionRedirect("p1", evictionNotice{
		Chunks: 1, Entities: 1, Edges: 1, Quarantine: 1, Cached: 1,
	}, time.Now())
	rec := httptest.NewRecorder()
	srv.MemoryProject(rec, httptest.NewRequest(http.MethodGet, loc, nil), "p1")

	body := rec.Body.String()
	for _, want := range []string{
		"Erased 1 chunk\n",
		"knowledge entity,",
		"graph edge,",
		"quarantined pre-ingest copy,",
		"cached embedding.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("singular form missing %q", want)
		}
	}
}

// Zero chunks is a real outcome and must say so rather than reading as success.
func TestMemoryProject_receiptSaysWhenNothingMatched(t *testing.T) {
	srv := NewServer(WithMemoryEvictor(&stubMemoryEvictor{}))

	loc := srv.evictionRedirect("p1", evictionNotice{}, time.Now())
	rec := httptest.NewRecorder()
	srv.MemoryProject(rec, httptest.NewRequest(http.MethodGet, loc, nil), "p1")

	if !strings.Contains(rec.Body.String(), "No chunks matched") {
		t.Error("an eviction that matched nothing must say so — the IDs were stale, " +
			"already evicted, or from another project")
	}
}

// The receipt lives in the eviction panel, which lives under the "operate"
// tab. Without this the redirect lands on the default tab, the banner is not
// rendered at all, and the operator presses erase and is returned to a page
// that says nothing — exactly the state this change exists to end.
func TestResolveMemoryProjectTab_evictionLandsWhereItsReceiptIs(t *testing.T) {
	if got := resolveMemoryProjectTab("", 0, 0, true); got != "operate" {
		t.Errorf("after an eviction the page must open on the tab holding the "+
			"receipt, got %q", got)
	}
	// An explicit tab still wins: the operator asked for it.
	if got := resolveMemoryProjectTab("health", 0, 0, true); got != "health" {
		t.Errorf("an explicit tab request must be honoured, got %q", got)
	}
	// And the flag changes nothing on an ordinary load.
	if got := resolveMemoryProjectTab("", 0, 0, false); got != "health" {
		t.Errorf("default tab unchanged without an eviction, got %q", got)
	}
}

// End to end: the redirect the handler builds must actually land on a page
// that shows the receipt. The two halves were wired separately and each looked
// right alone.
func TestMemoryEvict_redirectLandsOnAPageShowingTheReceipt(t *testing.T) {
	ev := &stubMemoryEvictor{
		deleted: 1,
		derived: MemoryEvictionResult{GraphEntities: 2, GraphEdges: 3},
	}
	srv := NewServer(WithMemoryEvictor(ev))

	form := url.Values{}
	form.Set("chunks", "chunk_1")
	form.Set("confirm", "yes")
	post := httptest.NewRequest(http.MethodPost, "/ui/memory/p1/evict", strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postRec := httptest.NewRecorder()
	srv.MemoryEvictAction(postRec, post, "p1")

	loc := postRec.Header().Get("Location")
	if loc == "" {
		t.Fatalf("no redirect; body=%q", postRec.Body.String())
	}
	// Follow it exactly as a browser would.
	getRec := httptest.NewRecorder()
	srv.MemoryProject(getRec, httptest.NewRequest(http.MethodGet, loc, nil), "p1")

	if getRec.Code != http.StatusOK {
		t.Fatalf("following the redirect gave %d", getRec.Code)
	}
	body := getRec.Body.String()
	if !strings.Contains(body, "Erased 1 chunk") {
		t.Error("the page the eviction redirects to must show the receipt")
	}
	if !strings.Contains(body, "Derived data removed with them") {
		t.Error("the derived counts must reach the rendered page, not just the URL")
	}
}

// End of the security argument, at the page rather than the parser: a forged
// link must render no banner at all.
func TestMemoryProject_forgedReceiptLinkRendersNoBanner(t *testing.T) {
	srv := NewServer(WithMemoryEvictor(&stubMemoryEvictor{}))

	rec := httptest.NewRecorder()
	srv.MemoryProject(rec, httptest.NewRequest(http.MethodGet,
		"/ui/memory/p1?notice=evicted&chunks=500&entities=100&at=99999999999", nil), "p1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "Erased 500") || strings.Contains(body, "Derived data removed with them") {
		t.Error("a crafted link must not produce a compliance receipt — a fabricated " +
			"confirmation is worse than none, because the operator stops looking")
	}
}

// If the receipt cannot be signed, it is not shown — it does not fall back to
// an unsigned one. Failing closed costs a convenience; failing open would
// restore the forgeable banner this signing exists to prevent.
func TestEvictionReceipt_failsClosedWithoutAKey(t *testing.T) {
	srv := NewServer()
	// Burn the sync.Once with an empty key, as the rand failure path would.
	srv.receiptKeyOnce.Do(func() { srv.receiptSigningKey = nil })

	now := time.Now()
	loc := srv.evictionRedirect("p1", evictionNotice{Chunks: 3}, now)
	if strings.Contains(loc, "sig=") {
		t.Errorf("no signature is possible without a key, got %q", loc)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n := srv.parseEvictionNotice("p1", u.Query(), now); n != nil {
		t.Fatalf("an unsignable receipt must not render, got %+v", n)
	}
}
