package membench

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The hindsight dialect, established by driving hindsight 0.9.0 for real rather
// than by reading its docs.
//
// §12.1 recorded that the external adapter's request BODIES were "a guess at a
// conventional shape ... unverified against any live service", and said a
// body-mapping layer would be expected work if a real service disagreed. It
// disagreed in three places and agreed in two, and the split is worth pinning
// because the agreements are load-bearing:
//
//	route          conventional guess          hindsight 0.9.0
//	bank create    POST  {"bank": id}          PUT   {} (idempotent upsert)
//	ingest         {bank, document_id,         {"items":[{content, document_id,
//	               content, event_time}         timestamp}], "async": false}
//	recall         POST {query, max_tokens}    SAME
//	hits key       "hits"                      "results"  (already tolerated)
//	doc identity   hits[].document_id          results[].document_id  (same)
//
// The last two are why this is a dialect and not a second adapter: the recall
// contract was already right, and recall is where the measurement happens.

// hindsightStub records what the adapter sent so the shape can be asserted
// against the real service's schema rather than against our own assumption.
type hindsightStub struct {
	bankCreateMethod string
	bankCreateBody   map[string]any
	retainBodies     []map[string]any
	recallBody       map[string]any
}

func (h *hindsightStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	decode := func(r *http.Request) map[string]any {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &m)
		}
		return m
	}
	mux.HandleFunc("/v1/default/banks/", func(w http.ResponseWriter, r *http.Request) {
		// The bank id carries a scope hash suffix, so match on the ROUTE tail
		// rather than on a literal id.
		tail := strings.TrimPrefix(r.URL.Path, "/v1/default/banks/")
		if i := strings.Index(tail, "/"); i >= 0 {
			tail = tail[i:]
		} else {
			tail = ""
		}
		switch tail {
		case "":
			h.bankCreateMethod = r.Method
			h.bankCreateBody = decode(r)
			_, _ = w.Write([]byte(`{"bank_id":"probe-bank"}`))
		case "/memories":
			h.retainBodies = append(h.retainBodies, decode(r))
			_, _ = w.Write([]byte(`{"success":true,"items_count":1,"async":false}`))
		case "/memories/recall":
			h.recallBody = decode(r)
			// The real 0.9.0 shape: `results`, document_id, and a NESTED score.
			_, _ = w.Write([]byte(`{"results":[
				{"document_id":"embed-queue-lease.md","text":"the lease is 30s",
				 "scores":{"final":1.09,"reranker":0.99}},
				{"document_id":"rrf-tie-order.md","text":"ties break by chunk id",
				 "scores":{"final":0.0001}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func hindsightSystem(t *testing.T, base string) *ExternalSystem {
	t.Helper()
	return NewExternalSystem(ExternalConfig{
		BaseURL:        base,
		Dialect:        DialectHindsight,
		BankPrefix:     "probe",
		BankCreatePath: "/v1/default/banks/{bank}",
		IngestPath:     "/v1/default/banks/{bank}/memories",
		RecallPath:     "/v1/default/banks/{bank}/memories/recall",
	})
}

// TestHindsight_IngestUsesTheItemsEnvelope pins the difference that actually broke
// the guess. A flat {content, document_id} body is a 422 against 0.9.0: the route
// takes a BATCH, so the document identity lives one level down.
func TestHindsight_IngestUsesTheItemsEnvelope(t *testing.T) {
	stub := &hindsightStub{}
	sys := hindsightSystem(t, stub.server(t).URL)

	when := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	_, err := sys.Ingest(context.Background(), "bank", []Item{
		{DocumentID: "embed-queue-lease.md", Content: "the lease is 30s", EventTime: when},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if len(stub.retainBodies) != 1 {
		t.Fatalf("expected 1 retain call, got %d", len(stub.retainBodies))
	}
	body := stub.retainBodies[0]

	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("body has no `items` array: %v — a flat document body is a 422 here", body)
	}
	it, _ := items[0].(map[string]any)
	if got := it["document_id"]; got != "embed-queue-lease.md" {
		t.Errorf("items[0].document_id = %v; without it, recall cannot return the "+
			"identity tier-2 scores against and recall would read 0 everywhere", got)
	}
	if got := it["content"]; got != "the lease is 30s" {
		t.Errorf("items[0].content = %v", got)
	}
	// `timestamp`, not `event_time`. Sending the wrong key loses the temporal
	// signal silently — the document ingests fine and simply has no date, which
	// would quietly break exactly the temporal-reasoning ability we most want to
	// measure.
	if got := it["timestamp"]; got != "2026-08-01T10:00:00Z" {
		t.Errorf("items[0].timestamp = %v, want the RFC3339 event time", got)
	}
	if _, wrong := it["event_time"]; wrong {
		t.Error("sent `event_time`, which hindsight ignores")
	}
	// Synchronous, or the run recalls against a haystack still being written.
	if got, ok := body["async"].(bool); !ok || got {
		t.Errorf("async = %v, want explicit false: an async retain lets the run "+
			"score before ingest completes, which measures an emptier corpus", body["async"])
	}
}

// TestHindsight_OmitsTimestampWhenUnknown keeps the existing rule: a fabricated
// date is worse than none, because it places the document outside every temporal
// window rather than leaving it unfiltered.
func TestHindsight_OmitsTimestampWhenUnknown(t *testing.T) {
	stub := &hindsightStub{}
	sys := hindsightSystem(t, stub.server(t).URL)

	if _, err := sys.Ingest(context.Background(), "bank", []Item{
		{DocumentID: "d.md", Content: "no date"},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	items := stub.retainBodies[0]["items"].([]any)
	if ts, present := items[0].(map[string]any)["timestamp"]; present {
		t.Errorf("timestamp sent as %v for an item with no event time", ts)
	}
}

// TestHindsight_BankCreateIsAnIdempotentPut pins the second difference. POST to
// the bank route is not how 0.9.0 creates a bank; PUT is, and it upserts — which
// matters because Prepare runs per item scope and a second call must not fail.
func TestHindsight_BankCreateIsAnIdempotentPut(t *testing.T) {
	stub := &hindsightStub{}
	sys := hindsightSystem(t, stub.server(t).URL)

	if err := sys.Prepare(context.Background(), "bank"); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if stub.bankCreateMethod != http.MethodPut {
		t.Errorf("bank create used %s, want PUT", stub.bankCreateMethod)
	}
	if _, sent := stub.bankCreateBody["bank"]; sent {
		t.Error("sent a `bank` field in the create body; 0.9.0 takes the id from the " +
			"path and rejects unknown fields")
	}
	// Idempotent: preparing twice is a normal consequence of a retried run.
	if err := sys.Prepare(context.Background(), "bank"); err != nil {
		t.Errorf("second prepare failed: %v", err)
	}
}

// TestHindsight_RecallReadsResultsAndDocumentIDs is the agreement half, and the
// reason this is a dialect rather than a separate adapter: the recall contract was
// already correct, and recall is where the measurement happens.
func TestHindsight_RecallReadsResultsAndDocumentIDs(t *testing.T) {
	stub := &hindsightStub{}
	sys := hindsightSystem(t, stub.server(t).URL)

	got, err := sys.Recall(context.Background(), "bank", Query{Text: "how long is the lease", MaxTokens: 4096})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	ids := got.SourceIDs()
	if len(ids) != 2 || ids[0] != "embed-queue-lease.md" || ids[1] != "rrf-tie-order.md" {
		t.Fatalf("SourceIDs() = %v, want the two document ids in RANK ORDER", ids)
	}
	if stub.recallBody["query"] != "how long is the lease" {
		t.Errorf("recall body = %v", stub.recallBody)
	}
	if got.Tokens <= 0 {
		t.Error("Tokens = 0; hindsight reports no token count, so the estimate must " +
			"stand in or budget utilisation reads as zero")
	}
}

// TestHindsight_NestedScoreDoesNotBreakRanking states a real asymmetry rather than
// hiding it. Hindsight reports `scores: {final: …}`, not a flat `score`, so per-hit
// Score arrives as 0. Tier-2 metrics rank by ARRAY ORDER, so they are unaffected —
// but anything that thresholds on score would silently see zeros, and a future
// reader needs to know that before adding one.
func TestHindsight_NestedScoreDoesNotBreakRanking(t *testing.T) {
	stub := &hindsightStub{}
	sys := hindsightSystem(t, stub.server(t).URL)

	got, err := sys.Recall(context.Background(), "bank", Query{Text: "q", MaxTokens: 4096})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	// Order is preserved, which is all the metrics consume.
	if ContextRecall(got.SourceIDs(), []string{"embed-queue-lease.md"}) != 1.0 {
		t.Error("gold document not scored as retrieved despite being first")
	}
	if MRR(got.SourceIDs(), []string{"embed-queue-lease.md"}) != 1.0 {
		t.Error("MRR did not see the gold document at rank 1")
	}
}

// TestExternal_ReportsItsWriteTarget keeps the destructive-run guard meaningful
// for a system that has no database.
//
// VerifyWriteTarget fails closed on any system that cannot name where its writes
// land — correctly, since a guard that passes when it learned nothing is what let
// twelve fixture documents into the production corpus. Applied to the external
// adapter that rule would refuse every external run, and the tempting fix is to
// skip the guard for external systems. That is the wrong repair: it reintroduces
// exactly the hole, just for the arm we understand least.
//
// The right one is to answer the question in the terms that system actually has.
// For a remote service the blast radius is the SERVICE, so it reports its base
// URL and the operator names that in --database. The denylist keeps working: a
// production endpoint can be listed there like any database name.
func TestExternal_ReportsItsWriteTarget(t *testing.T) {
	sys := NewExternalSystem(ExternalConfig{BaseURL: "http://127.0.0.1:8888"})

	got, err := sys.WriteTargetDatabase(context.Background())
	if err != nil {
		t.Fatalf("WriteTargetDatabase: %v", err)
	}
	if got != "http://127.0.0.1:8888" {
		t.Errorf("write target = %q, want the base URL — the operator has to be able to "+
			"name it, and a service address is the only thing they know", got)
	}
	if err := VerifyWriteTarget(context.Background(), sys, "http://127.0.0.1:8888"); err != nil {
		t.Errorf("naming the base URL was refused: %v", err)
	}
}

// TestExternal_WriteTargetRefusesADifferentService is the same protection the
// vornik arm gets: naming one endpoint while writing to another must not run.
func TestExternal_WriteTargetRefusesADifferentService(t *testing.T) {
	sys := NewExternalSystem(ExternalConfig{BaseURL: "https://memory.production.example"})

	err := VerifyWriteTarget(context.Background(), sys, "http://127.0.0.1:8888")
	if err == nil {
		t.Fatal("a run naming a local endpoint while pointed at a production service " +
			"was allowed to bulk-write")
	}
	if !strings.Contains(err.Error(), "memory.production.example") {
		t.Errorf("error %q must name the service actually being written", err)
	}
}
