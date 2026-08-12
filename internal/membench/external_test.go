package membench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The external adapter (design §5.3, §5.5, §5.6). Drives a generic HTTP
// agent-memory service over the same client shape the vornik adapter uses.
//
// The service's exact route names are NOT verified — they cannot be until a live
// run — so they are configuration, and these tests pin the DEFAULTS and the
// overridability rather than pretending a particular vendor's shape is known.

// externalRequest is one captured call to the fake service.
type externalRequest struct {
	Method string
	Path   string
	Auth   string
	Body   map[string]any
}

// fakeExternal stands in for the external memory service. It records every
// request and answers with whatever the test queued.
type fakeExternal struct {
	mu       sync.Mutex
	requests []externalRequest
	// reply returns the status and the JSON payload for one request. A nil
	// payload writes the status with no body, which is how a 204-style retain and
	// an error page are both expressed.
	reply func(r externalRequest) (int, any)
}

func (f *fakeExternal) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		// A request with no body is legitimate (bank delete), so a decode failure
		// on an empty body is not an error worth failing the test over.
		_ = json.NewDecoder(r.Body).Decode(&body)

		f.mu.Lock()
		f.requests = append(f.requests, externalRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Auth:   r.Header.Get("Authorization"),
			Body:   body,
		})
		f.mu.Unlock()

		status, payload := 200, any(nil)
		if f.reply != nil {
			status, payload = f.reply(externalRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		}
		if payload == nil {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}
}

func (f *fakeExternal) all() []externalRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]externalRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// last returns the most recent request, failing the test when none was made —
// an adapter that silently makes no call would otherwise pass an assertion
// vacuously.
func (f *fakeExternal) last(t *testing.T) externalRequest {
	t.Helper()
	all := f.all()
	if len(all) == 0 {
		t.Fatal("the adapter made no HTTP request at all")
	}
	return all[len(all)-1]
}

func newExternalFixture(t *testing.T, f *fakeExternal, opts ...func(*ExternalConfig)) *ExternalSystem {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	cfg := ExternalConfig{BaseURL: srv.URL, Token: "ext-token", Client: srv.Client()}
	for _, o := range opts {
		o(&cfg)
	}
	return NewExternalSystem(cfg)
}

// okRecall is a service that answers every recall with two hits.
func okRecall(r externalRequest) (int, any) {
	if strings.Contains(r.Path, "recall") {
		return 200, map[string]any{"hits": []any{}}
	}
	return 200, map[string]any{"accepted": true}
}

// TestExternal_Name — the system's identity in results and manifests. A run
// directory named for the wrong system is unattributable after the fact.
func TestExternal_Name(t *testing.T) {
	if got := NewExternalSystem(ExternalConfig{}).Name(); got != "external" {
		t.Errorf("Name() = %q, want external", got)
	}
}

// TestExternal_DefaultRoutesAreConventional — the default paths are a documented
// conventional REST shape, not a claim about the real service. They are pinned so
// an operator can see from the tests what will be called when nothing is
// configured, and so a silent change of default is a test failure.
func TestExternal_DefaultRoutesAreConventional(t *testing.T) {
	f := &fakeExternal{reply: okRecall}
	sys := newExternalFixture(t, f)
	ctx := context.Background()

	if _, err := sys.Ingest(ctx, "lme/q0001", []Item{{DocumentID: "d1", Content: "x"}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got := f.last(t).Path; got != "/v1/retain" {
		t.Errorf("ingest path = %q, want the default /v1/retain", got)
	}
	if _, err := sys.Recall(ctx, "lme/q0001", Query{Text: "q"}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if got := f.last(t).Path; got != "/v1/recall" {
		t.Errorf("recall path = %q, want the default /v1/recall", got)
	}
}

// TestExternal_PathTemplatesOverridable — we cannot verify the service's real
// routes until a live run, so hardcoding a guess would be a lie in the code. The
// paths must be configuration, including the {bank} form for a service that
// namespaces in the URL rather than the body.
func TestExternal_PathTemplatesOverridable(t *testing.T) {
	f := &fakeExternal{reply: okRecall}
	sys := newExternalFixture(t, f, func(c *ExternalConfig) {
		c.IngestPath = "/api/banks/{bank}/documents"
		c.RecallPath = "/api/banks/{bank}/search"
	})
	ctx := context.Background()

	if _, err := sys.Ingest(ctx, "lme/q0001", []Item{{DocumentID: "d1", Content: "x"}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	bank := sys.BankID("lme/q0001")
	want := "/api/banks/" + bank + "/documents"
	if got := f.last(t).Path; got != want {
		t.Errorf("ingest path = %q, want %q — an unconfigurable path pins a guess at "+
			"the vendor's routes into the code", got, want)
	}
	if _, err := sys.Recall(ctx, "lme/q0001", Query{Text: "q"}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if got, want := f.last(t).Path, "/api/banks/"+bank+"/search"; got != want {
		t.Errorf("recall path = %q, want %q", got, want)
	}
}

// TestExternal_BankIDIsPerScopeAndInjective is the isolation invariant, the
// counterpart of the vornik adapter's strict_scope pinning. Two benchmark items
// sharing a bank means item A's haystack answers item B's question, and the run
// silently scores cross-contaminated recall — worse than no benchmark at all
// (§5.5).
func TestExternal_BankIDIsPerScopeAndInjective(t *testing.T) {
	sys := NewExternalSystem(ExternalConfig{})

	a, b := sys.BankID("lme/q0001"), sys.BankID("lme/q0002")
	if a == b {
		t.Fatalf("BankID collapsed two scopes onto %q; one item's haystack would be "+
			"recallable under another item's scope", a)
	}
	if sys.BankID("lme/q0001") != a {
		t.Error("BankID is not deterministic; ingest and recall would address different banks")
	}
	// Two scopes that sanitise to the same characters must STILL differ: '/' and
	// '-' both have to survive as distinct namespaces, or a dataset whose item ids
	// mix the two silently shares a haystack.
	if sys.BankID("lme/q1") == sys.BankID("lme-q1") {
		t.Error("scopes differing only in a sanitised character share a bank; " +
			"sanitisation alone is not injective, so the id must carry a digest")
	}
	if !strings.Contains(sys.BankID("lme/q0001"), "membench") {
		t.Errorf("BankID %q lacks the default prefix; a bank not recognisable as "+
			"ours cannot be safely torn down", sys.BankID("lme/q0001"))
	}
}

// TestExternal_BankPrefixConfigurable — two concurrent runs against one service
// account must not share banks, and the prefix is the only thing separating them.
func TestExternal_BankPrefixConfigurable(t *testing.T) {
	a := NewExternalSystem(ExternalConfig{BankPrefix: "run-a"})
	b := NewExternalSystem(ExternalConfig{BankPrefix: "run-b"})
	if a.BankID("s") == b.BankID("s") {
		t.Error("the same scope produced the same bank under two prefixes; two runs " +
			"would write into each other's haystacks")
	}
	if !strings.HasPrefix(a.BankID("s"), "run-a") {
		t.Errorf("BankID %q ignored the configured prefix", a.BankID("s"))
	}
}

// TestExternal_IngestAndRecallAddressTheSameBank — the isolation invariant seen
// from the wire: a recall for a scope must name the bank that scope's haystack
// was written to, and no other. A mismatch scores zero retrieval for a
// namespacing reason that looks exactly like a retrieval failure.
func TestExternal_IngestAndRecallAddressTheSameBank(t *testing.T) {
	f := &fakeExternal{reply: okRecall}
	sys := newExternalFixture(t, f)
	ctx := context.Background()

	if _, err := sys.Ingest(ctx, "lme/q0001", []Item{{DocumentID: "d1", Content: "x"}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	ingestBank, _ := f.last(t).Body["bank"].(string)

	if _, err := sys.Recall(ctx, "lme/q0001", Query{Text: "q"}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	recallBank, _ := f.last(t).Body["bank"].(string)

	if ingestBank == "" || recallBank == "" {
		t.Fatalf("bank absent from the wire (ingest %q, recall %q); the service would "+
			"fall back to a shared default namespace", ingestBank, recallBank)
	}
	if ingestBank != recallBank {
		t.Errorf("ingest wrote to %q but recall queried %q", ingestBank, recallBank)
	}
	if recallBank != sys.BankID("lme/q0001") {
		t.Errorf("recall bank %q is not the scope's bank %q", recallBank, sys.BankID("lme/q0001"))
	}

	// A different scope must reach a different bank on the wire, not merely a
	// different BankID() return value.
	if _, err := sys.Recall(ctx, "lme/q0002", Query{Text: "q"}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if other, _ := f.last(t).Body["bank"].(string); other == recallBank {
		t.Errorf("two scopes queried the same bank %q", other)
	}
}

// TestExternal_IngestSendsEventTimeAndFraming — the whole reason Phase 0 shipped
// first: a dated haystack must arrive with its dates, or the dataset's temporal
// categories are unanswerable for a reason unrelated to retrieval. The Context
// framing must be prepended for the same reason the vornik adapter prepends it —
// both systems receive identical provenance or the comparison is unfair (§5.6).
func TestExternal_IngestSendsEventTimeAndFraming(t *testing.T) {
	f := &fakeExternal{reply: okRecall}
	sys := newExternalFixture(t, f)

	when := time.Date(2023, 5, 14, 9, 30, 0, 0, time.UTC)
	stats, err := sys.Ingest(context.Background(), "lme/q0001", []Item{
		{DocumentID: "q1_s1", Content: "Alice moved to Berlin", Context: "Session 1", EventTime: when},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	req := f.last(t)
	if got, _ := req.Body["event_time"].(string); !strings.HasPrefix(got, "2023-05-14T09:30:00") {
		t.Errorf("event_time = %q, want the item's event time in RFC3339", got)
	}
	if got, _ := req.Body["document_id"].(string); got != "q1_s1" {
		t.Errorf("document_id = %q, want q1_s1 — the label tier-2 metrics score against", got)
	}
	content, _ := req.Body["content"].(string)
	if !strings.Contains(content, "Alice moved to Berlin") {
		t.Errorf("content %q lost the item body", content)
	}
	if !strings.Contains(content, "Session 1") {
		t.Errorf("content %q dropped the framing context, so the two systems would "+
			"receive different provenance", content)
	}
	if req.Auth != "Bearer ext-token" {
		t.Errorf("Authorization = %q, want the configured bearer token", req.Auth)
	}
	if stats.Deposits != 1 || stats.Bytes != len(content) {
		t.Errorf("stats = %+v, want 1 deposit of %d bytes", stats, len(content))
	}
	if stats.Latency <= 0 {
		t.Error("ingest latency not measured; tier-3 reporting needs it")
	}
}

// TestExternal_IngestOmitsUnknownEventTime — a zero event time must not be sent
// as year 0001. A fabricated date is worse than no date: it would place the whole
// haystack outside every temporal window instead of leaving it unfiltered.
func TestExternal_IngestOmitsUnknownEventTime(t *testing.T) {
	f := &fakeExternal{reply: okRecall}
	sys := newExternalFixture(t, f)

	if _, err := sys.Ingest(context.Background(), "s", []Item{{DocumentID: "d1", Content: "x"}}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got, ok := f.last(t).Body["event_time"]; ok {
		t.Errorf("event_time = %v on an item with no event time, want the field absent", got)
	}
}

// TestExternal_FramingMatchesVornikAdapter is the fairness invariant stated as an
// equality: the bytes the two adapters send for one Item must be identical. If
// they ever drift — one prepending the framing, the other not, or a different
// separator — the head-to-head measures the framing difference and reports it as
// a retrieval difference.
func TestExternal_FramingMatchesVornikAdapter(t *testing.T) {
	item := Item{
		DocumentID: "q1_s1",
		Content:    "line one\nline two",
		Context:    "Session 7 — you are the assistant in this conversation — happened on 2023-05-14 UTC.",
		EventTime:  time.Date(2023, 5, 14, 9, 30, 0, 0, time.UTC),
	}

	comp := &fakeCompanion{reply: func(string, map[string]any) (any, error) {
		return map[string]any{"decision": "ALLOW", "admitted": 1}, nil
	}}
	vsys := newVornikFixture(t, comp)
	if _, err := vsys.Ingest(context.Background(), "s", []Item{item}); err != nil {
		t.Fatalf("vornik Ingest: %v", err)
	}
	vornikBody, _ := comp.calls[0].Args["content"].(string)

	f := &fakeExternal{reply: okRecall}
	esys := newExternalFixture(t, f)
	if _, err := esys.Ingest(context.Background(), "s", []Item{item}); err != nil {
		t.Fatalf("external Ingest: %v", err)
	}
	externalBody, _ := f.last(t).Body["content"].(string)

	if vornikBody != externalBody {
		t.Errorf("the two adapters framed one item differently:\n vornik:   %q\n external: %q\n"+
			"identical provenance framing on both sides is what makes the head-to-head fair",
			vornikBody, externalBody)
	}
}

// TestExternal_RecallSendsTheTokenBudgetVerbatim — equal budgets are a named
// fairness control (§5.6). Sending a different number, or the same number in
// different units, makes one system answer from more context than the other and
// the numbers will not show it.
func TestExternal_RecallSendsTheTokenBudgetVerbatim(t *testing.T) {
	f := &fakeExternal{reply: okRecall}
	sys := newExternalFixture(t, f)

	if _, err := sys.Recall(context.Background(), "s", Query{Text: "q", MaxTokens: 2048}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	got, ok := f.last(t).Body["max_tokens"].(float64)
	if !ok || int(got) != 2048 {
		t.Errorf("max_tokens = %v (%T), want the 2048 it was given", f.last(t).Body["max_tokens"], f.last(t).Body["max_tokens"])
	}
	if _, converted := f.last(t).Body["top_k"]; converted {
		t.Error("a top_k conversion was sent to a service not declared top-k-only; " +
			"the budget must reach a budget-capable service in its own units")
	}
	if notes := sys.Notes(); len(notes) != 0 {
		t.Errorf("Notes() = %v with no conversion performed; a spurious methodological "+
			"note on the manifest is a claim about a difference that does not exist", notes)
	}
}

// TestExternal_TopKConversionIsRecorded — a service that only accepts top-k gets
// a converted request, but the conversion is a methodological difference between
// the two systems and must be REPORTED rather than silently changing units
// (§5.3, §5.6).
func TestExternal_TopKConversionIsRecorded(t *testing.T) {
	f := &fakeExternal{reply: okRecall}
	sys := newExternalFixture(t, f, func(c *ExternalConfig) { c.TopKOnly = true })

	if _, err := sys.Recall(context.Background(), "s", Query{Text: "q", MaxTokens: 2048}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	body := f.last(t).Body
	topK, ok := body["top_k"].(float64)
	if !ok || int(topK) != tokenBudgetToLimit(2048) {
		t.Errorf("top_k = %v, want %d", body["top_k"], tokenBudgetToLimit(2048))
	}
	if got, _ := body["max_tokens"].(float64); int(got) != 2048 {
		t.Errorf("max_tokens = %v; the budget is still sent alongside the conversion so "+
			"the request records what was asked for, not only what it was reduced to", body["max_tokens"])
	}

	notes := sys.Notes()
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, " "), "top_k") {
		t.Fatalf("Notes() = %v, want the token-budget conversion recorded; an unrecorded "+
			"unit change makes an unequal-budget comparison look like an equal one", notes)
	}
	// A 500-item run must not accumulate 500 copies of one note, or the manifest
	// becomes unreadable and the difference gets skimmed past.
	if _, err := sys.Recall(context.Background(), "s", Query{Text: "q", MaxTokens: 2048}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if got := len(sys.Notes()); got != len(notes) {
		t.Errorf("Notes() grew to %d entries over two identical conversions, want %d", got, len(notes))
	}
}

// TestExternal_RecallOmitsBudgetWhenUnset — a zero budget means "unset", not
// "zero tokens". Sending 0 would ask a service for nothing and score the item as
// a retrieval miss.
func TestExternal_RecallOmitsBudgetWhenUnset(t *testing.T) {
	f := &fakeExternal{reply: okRecall}
	sys := newExternalFixture(t, f, func(c *ExternalConfig) { c.TopKOnly = true })

	if _, err := sys.Recall(context.Background(), "s", Query{Text: "q"}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	body := f.last(t).Body
	if _, ok := body["max_tokens"]; ok {
		t.Errorf("max_tokens = %v with no budget set, want the field absent", body["max_tokens"])
	}
	if _, ok := body["top_k"]; ok {
		t.Errorf("top_k = %v with no budget to convert, want the field absent", body["top_k"])
	}
}

// TestExternal_RecallMapsHitsToDocumentIdentity — tier-2 metrics compare against
// GOLD DOCUMENT ids, so a hit must carry the document identity the dataset
// labels, never an internal chunk id. Getting this wrong makes recall score 0
// everywhere for a reason unrelated to retrieval.
func TestExternal_RecallMapsHitsToDocumentIdentity(t *testing.T) {
	cost := 0.0042
	f := &fakeExternal{reply: func(externalRequest) (int, any) {
		return 200, map[string]any{
			"hits": []any{
				map[string]any{"chunk_id": "chunk-88", "document_id": "q1_s3", "text": "text", "score": 0.9},
				map[string]any{"chunk_id": "chunk-91", "document_id": "q1_s7", "content": "more", "score": 0.5},
			},
			"tokens":   123,
			"cost_usd": cost,
		}
	}}
	sys := newExternalFixture(t, f)

	got, err := sys.Recall(context.Background(), "s", Query{Text: "q", MaxTokens: 100})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	ids := got.SourceIDs()
	if len(ids) != 2 || ids[0] != "q1_s3" || ids[1] != "q1_s7" {
		t.Fatalf("SourceIDs = %v, want [q1_s3 q1_s7] in rank order — chunk ids are not "+
			"comparable to gold document ids", ids)
	}
	if got.Hits[1].Text != "more" {
		t.Errorf("hit text = %q; the alternate content field was dropped, so the answer "+
			"generator would receive an empty context", got.Hits[1].Text)
	}
	if got.Tokens != 123 {
		t.Errorf("Tokens = %d, want the 123 the service reported — its own count beats "+
			"our estimate for budget-utilisation reporting", got.Tokens)
	}
	if got.CostUSD == nil || *got.CostUSD != cost {
		t.Errorf("CostUSD = %v, want %v reported through for tier-3", got.CostUSD, cost)
	}
	if got.Latency <= 0 {
		t.Error("Latency not measured on recall; tier-3 reporting needs it")
	}
}

// TestExternal_RecallPrefersSourceIDWhenDocumentIDAbsent — the service's field
// name for document identity is unverified, so the fallback is deliberate. It
// must never fall back to the chunk id, which would score 0 recall while looking
// like a populated result.
func TestExternal_RecallPrefersSourceIDWhenDocumentIDAbsent(t *testing.T) {
	f := &fakeExternal{reply: func(externalRequest) (int, any) {
		return 200, map[string]any{"results": []any{
			map[string]any{"chunk_id": "chunk-1", "source_id": "q1_s3", "text": "t"},
		}}
	}}
	sys := newExternalFixture(t, f)

	got, err := sys.Recall(context.Background(), "s", Query{Text: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got.Hits) != 1 {
		t.Fatalf("hits = %d, want 1 — the alternate results envelope was dropped entirely", len(got.Hits))
	}
	if got.Hits[0].SourceID != "q1_s3" {
		t.Errorf("SourceID = %q, want q1_s3; a chunk id here scores 0 recall everywhere",
			got.Hits[0].SourceID)
	}
	if got.Tokens <= 0 {
		t.Error("Tokens = 0 with an unreported token count; the estimate must stand in " +
			"so budget utilisation is not silently reported as zero")
	}
}

// TestExternal_RecallSendsTemporalBounds — the dataset's dated questions need the
// window to reach the service, or the temporal categories are unanswerable for a
// reason that has nothing to do with retrieval.
func TestExternal_RecallSendsTemporalBounds(t *testing.T) {
	f := &fakeExternal{reply: okRecall}
	sys := newExternalFixture(t, f)

	from := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2023, 12, 31, 0, 0, 0, 0, time.UTC)
	if _, err := sys.Recall(context.Background(), "s", Query{Text: "q", From: from, To: to}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	body := f.last(t).Body
	if got, _ := body["from"].(string); !strings.HasPrefix(got, "2023-01-01") {
		t.Errorf("from = %q, want 2023-01-01", got)
	}
	if got, _ := body["to"].(string); !strings.HasPrefix(got, "2023-12-31") {
		t.Errorf("to = %q, want 2023-12-31", got)
	}
	if got, _ := body["query"].(string); got != "q" {
		t.Errorf("query = %q, want the question text", got)
	}
}

// TestExternal_QuotaStatusesAreTerminal — 429 and 402 are capacity refusals. They
// must join ErrQuotaExhausted so the runner aborts instead of retrying into the
// limit or, worse, continuing and scoring later items zero for a billing reason
// that reads as a retrieval result (§5.3).
func TestExternal_QuotaStatusesAreTerminal(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusPaymentRequired} {
		f := &fakeExternal{reply: func(externalRequest) (int, any) { return status, nil }}
		sys := newExternalFixture(t, f)

		if _, err := sys.Recall(context.Background(), "s", Query{Text: "q"}); !isQuotaErr(err) {
			t.Errorf("recall on http %d produced %v, want ErrQuotaExhausted", status, err)
		}
		if _, err := sys.Ingest(context.Background(), "s", []Item{{DocumentID: "d", Content: "x"}}); !isQuotaErr(err) {
			t.Errorf("ingest on http %d produced %v, want ErrQuotaExhausted — a quota hit "+
				"during ingest is just as terminal as one during recall", status, err)
		}
	}
}

// TestExternal_OtherFailuresAreNotQuota — every other non-2xx is a plain error.
// Conflating a transient 500 with quota exhaustion would abort a whole run over
// one blip.
func TestExternal_OtherFailuresAreNotQuota(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusUnauthorized,
		http.StatusNotFound,
		http.StatusBadRequest,
		http.StatusForbidden,
	} {
		f := &fakeExternal{reply: func(externalRequest) (int, any) { return status, nil }}
		sys := newExternalFixture(t, f)

		_, err := sys.Recall(context.Background(), "s", Query{Text: "q"})
		if err == nil {
			t.Errorf("http %d returned no error", status)
			continue
		}
		if isQuotaErr(err) {
			t.Errorf("http %d was reported as quota exhaustion; a transient fault would "+
				"abort the entire run", status)
		}
		if !strings.Contains(err.Error(), "http") {
			t.Errorf("error %q does not name the status, leaving the failure undiagnosable", err)
		}
	}
}

// TestExternal_UnreachableServiceIsPlainError — a connection failure is not quota
// exhaustion, and must not abort a run that a retry could survive.
func TestExternal_UnreachableServiceIsPlainError(t *testing.T) {
	// Port 0 on the loopback is never listening.
	sys := NewExternalSystem(ExternalConfig{
		BaseURL: "http://127.0.0.1:0",
		Client:  &http.Client{Timeout: time.Second},
	})
	_, err := sys.Recall(context.Background(), "s", Query{Text: "q"})
	if err == nil {
		t.Fatal("an unreachable service returned no error")
	}
	if isQuotaErr(err) {
		t.Error("a connection failure was reported as quota exhaustion")
	}
}

// TestExternal_MalformedRecallPayloadErrors — an undecodable reply must fail
// rather than yield a silently empty recall, which the metrics would score as a
// retrieval miss.
func TestExternal_MalformedRecallPayloadErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json at all"))
	}))
	defer srv.Close()

	sys := NewExternalSystem(ExternalConfig{BaseURL: srv.URL, Client: srv.Client()})
	if _, err := sys.Recall(context.Background(), "s", Query{Text: "q"}); err == nil {
		t.Error("an undecodable payload was treated as a successful empty recall")
	}
}

// TestExternal_IngestRecordsRejection — a refused document is haystack loss, and
// >50% loss forces the run untrustworthy (§5.9). Counted in BYTES, because one
// rejected 60 KiB session matters far more than one rejected 200-byte one.
func TestExternal_IngestRecordsRejection(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"explicit rejected count", map[string]any{"rejected": 1}},
		{"accepted false", map[string]any{"accepted": false}},
		{"status string", map[string]any{"status": "Rejected"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeExternal{reply: func(externalRequest) (int, any) { return 200, tc.payload }}
			sys := newExternalFixture(t, f)

			stats, err := sys.Ingest(context.Background(), "s", []Item{{DocumentID: "d1", Content: "body"}})
			if err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			if stats.Rejected != 1 {
				t.Errorf("Rejected = %d, want 1", stats.Rejected)
			}
			if stats.RejectedBytes != len("body") {
				t.Errorf("RejectedBytes = %d, want %d — without it, haystack loss is "+
					"invisible to the trust check", stats.RejectedBytes, len("body"))
			}
			if stats.HaystackLoss() != 1 {
				t.Errorf("HaystackLoss = %v, want 1", stats.HaystackLoss())
			}
		})
	}
}

// TestExternal_ChunksStoredUnreportedIsMinusOne — the contract's sentinel. Zero
// would read as "stored nothing", which is a very different claim from "the
// service does not tell us".
func TestExternal_ChunksStoredUnreportedIsMinusOne(t *testing.T) {
	f := &fakeExternal{reply: func(externalRequest) (int, any) { return 200, map[string]any{"accepted": true} }}
	sys := newExternalFixture(t, f)

	stats, err := sys.Ingest(context.Background(), "s", []Item{{DocumentID: "d1", Content: "x"}})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if stats.ChunksStored != -1 {
		t.Errorf("ChunksStored = %d with no service report, want -1", stats.ChunksStored)
	}
}

// TestExternal_ChunksStoredAccumulatesWhenReported — chunk counts are reported
// asymmetry (§5.6): our 64 KiB split chunks differently from a single-document
// retain, and the comparison is only honest if both counts are on the manifest.
func TestExternal_ChunksStoredAccumulatesWhenReported(t *testing.T) {
	f := &fakeExternal{reply: func(externalRequest) (int, any) {
		return 200, map[string]any{"accepted": true, "chunks_stored": 3}
	}}
	sys := newExternalFixture(t, f)

	stats, err := sys.Ingest(context.Background(), "s", []Item{
		{DocumentID: "d1", Content: "x"},
		{DocumentID: "d2", Content: "y"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if stats.ChunksStored != 6 {
		t.Errorf("ChunksStored = %d, want 6 across two documents", stats.ChunksStored)
	}
	if stats.Deposits != 2 {
		t.Errorf("Deposits = %d, want 2", stats.Deposits)
	}
	if stats.Splits != 0 {
		t.Errorf("Splits = %d, want 0 — the external side retains whole documents; a "+
			"nonzero split count here would misattribute our 64 KiB cap to them", stats.Splits)
	}
}

// TestExternal_IngestEmptyBodyReplyIsNotAFailure — a 204-style retain is a
// plausible shape for a service we have not yet run against, and must not be read
// as a refused document.
func TestExternal_IngestEmptyBodyReplyIsNotAFailure(t *testing.T) {
	f := &fakeExternal{reply: func(externalRequest) (int, any) { return http.StatusNoContent, nil }}
	sys := newExternalFixture(t, f)

	stats, err := sys.Ingest(context.Background(), "s", []Item{{DocumentID: "d1", Content: "x"}})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if stats.Rejected != 0 || stats.Deposits != 1 {
		t.Errorf("stats = %+v, want one accepted deposit", stats)
	}
}

// TestExternal_IngestStopsAtFirstFailure — a partially-ingested haystack must not
// be scored as if it were whole. Naming the failing document is what makes the
// failure diagnosable rather than just fatal.
func TestExternal_IngestStopsAtFirstFailure(t *testing.T) {
	f := &fakeExternal{reply: func(r externalRequest) (int, any) {
		if id, _ := r.Body["document_id"].(string); id == "d2" {
			return http.StatusInternalServerError, nil
		}
		return 200, map[string]any{"accepted": true}
	}}
	sys := newExternalFixture(t, f)

	_, err := sys.Ingest(context.Background(), "s", []Item{
		{DocumentID: "d1", Content: "x"},
		{DocumentID: "d2", Content: "y"},
		{DocumentID: "d3", Content: "z"},
	})
	if err == nil {
		t.Fatal("a failed retain was swallowed; the item would be scored against a partial haystack")
	}
	if !strings.Contains(err.Error(), "d2") {
		t.Errorf("error %q does not name the failing document", err)
	}
	if got := len(f.all()); got != 2 {
		t.Errorf("%d requests made, want 2 — ingest must stop at the failure rather than "+
			"pressing on with a known-incomplete haystack", got)
	}
}

// TestExternal_PrepareAndTeardownAreNoOpsWithoutRoutes — we cannot verify the
// service exposes bank lifecycle routes, so the adapter must work without them:
// the bank comes into existence with the first retain, exactly as a repo_scope
// does on our side. Firing a request at a guessed route would fail every run.
func TestExternal_PrepareAndTeardownAreNoOpsWithoutRoutes(t *testing.T) {
	f := &fakeExternal{reply: okRecall}
	sys := newExternalFixture(t, f)
	ctx := context.Background()

	if err := sys.Prepare(ctx, "lme/q0001"); err != nil {
		t.Errorf("Prepare: %v", err)
	}
	if err := sys.Teardown(ctx, "lme/q0001"); err != nil {
		t.Errorf("Teardown: %v", err)
	}
	if got := len(f.all()); got != 0 {
		t.Errorf("%d requests made with no lifecycle routes configured, want 0", got)
	}
}

// TestExternal_PrepareAndTeardownUseTheScopeBank — where the service does offer
// isolation primitives, they must be applied to the SCOPE's bank. A prepare that
// cleared the wrong bank would wipe another item's haystack mid-run; a teardown
// that cleared the wrong one would do the same.
func TestExternal_PrepareAndTeardownUseTheScopeBank(t *testing.T) {
	f := &fakeExternal{reply: func(externalRequest) (int, any) { return 200, map[string]any{"ok": true} }}
	sys := newExternalFixture(t, f, func(c *ExternalConfig) {
		c.BankCreatePath = "/v1/banks"
		c.BankDeletePath = "/v1/banks/{bank}"
	})
	ctx := context.Background()
	bank := sys.BankID("lme/q0001")

	if err := sys.Prepare(ctx, "lme/q0001"); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	req := f.last(t)
	if req.Method != http.MethodPost || req.Path != "/v1/banks" {
		t.Errorf("Prepare sent %s %s, want POST /v1/banks", req.Method, req.Path)
	}
	if got, _ := req.Body["bank"].(string); got != bank {
		t.Errorf("Prepare bank = %q, want %q", got, bank)
	}

	if err := sys.Teardown(ctx, "lme/q0001"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	req = f.last(t)
	if req.Method != http.MethodDelete || req.Path != "/v1/banks/"+bank {
		t.Errorf("Teardown sent %s %s, want DELETE /v1/banks/%s", req.Method, req.Path, bank)
	}
}

// TestExternal_PrepareFailureSurfaces — an un-prepared bank means the haystack
// lands somewhere unintended, so unlike teardown this cannot be best-effort.
func TestExternal_PrepareFailureSurfaces(t *testing.T) {
	f := &fakeExternal{reply: func(externalRequest) (int, any) { return http.StatusInternalServerError, nil }}
	sys := newExternalFixture(t, f, func(c *ExternalConfig) { c.BankCreatePath = "/v1/banks" })

	if err := sys.Prepare(context.Background(), "s"); err == nil {
		t.Error("a failed bank create was swallowed; the item would be ingested into an " +
			"unprepared namespace")
	}
}

// TestExternal_TeardownFailureIsReported — best-effort at the RUNNER, not
// silently discarded here: a leaked bank costs the operator money and the runner
// can only log what it is told.
func TestExternal_TeardownFailureIsReported(t *testing.T) {
	f := &fakeExternal{reply: func(externalRequest) (int, any) { return http.StatusInternalServerError, nil }}
	sys := newExternalFixture(t, f, func(c *ExternalConfig) { c.BankDeletePath = "/v1/banks/{bank}" })

	if err := sys.Teardown(context.Background(), "s"); err == nil {
		t.Error("a failed bank delete returned nil; the leak would be invisible")
	}
}

// TestExternal_ConfigEmptyWithNoEndpoint — empty marks the comparability key
// PARTIAL (§5.6). "Could not verify" is not "unchanged", and this distinction was
// a specific review finding: a fabricated value would let the service swap its
// embedding model between runs while the key still matched.
func TestExternal_ConfigEmptyWithNoEndpoint(t *testing.T) {
	sys := NewExternalSystem(ExternalConfig{ExtractionModel: "their-model-v2"})
	got, err := sys.Config(context.Background())
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if got != "" {
		t.Errorf("Config() = %q with no config endpoint, want empty so the key is marked partial", got)
	}

	// The empty value must actually reach Partial() as a partial key, which is the
	// consumer this behaviour exists for.
	fields := ComparabilityFields{ExternalConfigSHA256: got}
	if !fields.Partial() {
		t.Error("an unread external config did not mark the comparability key partial")
	}
}

// TestExternal_ConfigReadsEndpointCanonically — the value is hashed into the
// comparability key, so two identical configs must produce the same string
// regardless of the service's key ordering or whitespace. Otherwise every run
// looks incomparable to every other and `compare` refuses forever.
func TestExternal_ConfigReadsEndpointCanonically(t *testing.T) {
	bodies := []string{
		`{"embedding_model":"e5-large","reranker":"bge","top_k":8}`,
		"{\n  \"top_k\": 8,\n  \"reranker\": \"bge\",\n  \"embedding_model\": \"e5-large\"\n}",
	}
	var seen []string
	for _, body := range bodies {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/config" {
				t.Errorf("Config hit %s, want the configured /v1/config", r.URL.Path)
			}
			if r.Method != http.MethodGet {
				t.Errorf("Config used %s, want GET", r.Method)
			}
			_, _ = w.Write([]byte(body))
		}))
		sys := NewExternalSystem(ExternalConfig{
			BaseURL: srv.URL, Client: srv.Client(), ConfigPath: "/v1/config",
		})
		got, err := sys.Config(context.Background())
		srv.Close()
		if err != nil {
			t.Fatalf("Config: %v", err)
		}
		if !strings.Contains(got, "e5-large") {
			t.Errorf("Config() = %q, want the service's reported configuration", got)
		}
		seen = append(seen, got)
	}
	if seen[0] != seen[1] {
		t.Errorf("the same configuration serialised two ways produced %q and %q; the "+
			"comparability key would differ between two identical runs", seen[0], seen[1])
	}
}

// TestExternal_ConfigEmptyWhenEndpointFails — the load-bearing case. On any
// failure the adapter reports EMPTY, never a plausible-looking substitute such as
// the configured extraction model: an unverifiable config must be visible as a
// gap, not pass as verified.
func TestExternal_ConfigEmptyWhenEndpointFails(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
		{"unauthorised", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}},
		{"undecodable body", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>not json</html>"))
		}},
		{"empty body", func(http.ResponseWriter, *http.Request) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			sys := NewExternalSystem(ExternalConfig{
				BaseURL:         srv.URL,
				Client:          srv.Client(),
				ConfigPath:      "/v1/config",
				ExtractionModel: "their-model-v2",
			})
			got, err := sys.Config(context.Background())
			if got != "" {
				t.Errorf("Config() = %q on a failed read, want empty — a fabricated value "+
					"would let a swapped model pass as an unchanged one", got)
			}
			if err == nil {
				t.Error("Config() reported no error on a failed read; the operator would " +
					"have no way to tell an unexposed config from a broken one")
			}
		})
	}
}

// TestExternal_ConfigQuotaIsTerminal — a config read that hits the quota is the
// same terminal condition as any other, and must not be reported as a mere
// "config unavailable".
func TestExternal_ConfigQuotaIsTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	sys := NewExternalSystem(ExternalConfig{
		BaseURL: srv.URL, Client: srv.Client(), ConfigPath: "/v1/config",
	})
	if _, err := sys.Config(context.Background()); !isQuotaErr(err) {
		t.Errorf("Config on a 429 produced %v, want ErrQuotaExhausted", err)
	}
}

// TestExternal_DefaultClientIsUsedWhenUnset — a nil client must not panic; the
// adapter is constructed from config a CLI assembles, where omitting the client
// is normal.
func TestExternal_DefaultClientIsUsedWhenUnset(t *testing.T) {
	sys := NewExternalSystem(ExternalConfig{BaseURL: "http://127.0.0.1:0"})
	if sys.client == nil {
		t.Fatal("no default HTTP client; every call would panic")
	}
	if sys.client.Timeout <= 0 {
		t.Error("the default client has no timeout; a hung service would stall the run forever")
	}
}

// TestExternal_BaseURLTrailingSlashDoesNotDoubleUp — an operator-supplied base
// URL with a trailing slash is normal, and "//v1/recall" is a 404 on most
// routers, which would present as a total retrieval failure.
func TestExternal_BaseURLTrailingSlashDoesNotDoubleUp(t *testing.T) {
	f := &fakeExternal{reply: okRecall}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	sys := NewExternalSystem(ExternalConfig{BaseURL: srv.URL + "/", Client: srv.Client()})
	if _, err := sys.Recall(context.Background(), "s", Query{Text: "q"}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if got := f.last(t).Path; got != "/v1/recall" {
		t.Errorf("path = %q, want /v1/recall", got)
	}
}

// TestExternal_PathTemplateWithoutLeadingSlash — an operator writing
// "v1/retain" must reach the same route as "/v1/retain". Concatenating it raw
// would produce "https://hostv1/retain", a DNS failure that reads as an outage.
func TestExternal_PathTemplateWithoutLeadingSlash(t *testing.T) {
	f := &fakeExternal{reply: okRecall}
	sys := newExternalFixture(t, f, func(c *ExternalConfig) { c.RecallPath = "v1/search" })

	if _, err := sys.Recall(context.Background(), "s", Query{Text: "q"}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if got := f.last(t).Path; got != "/v1/search" {
		t.Errorf("path = %q, want /v1/search", got)
	}
}

// TestExternal_BankIDIsBoundedAndStillInjective — a dataset id can be long, and
// services cap identifier length. The readable part is truncated, so the digest
// is what has to keep two long scopes apart; without it, truncation would merge
// every scope sharing a prefix into one shared haystack.
func TestExternal_BankIDIsBoundedAndStillInjective(t *testing.T) {
	sys := NewExternalSystem(ExternalConfig{})
	long := strings.Repeat("lme/question-", 20)

	a, b := sys.BankID(long+"0001"), sys.BankID(long+"0002")
	if len(a) > 96 {
		t.Errorf("BankID is %d chars; an unbounded id risks rejection by the service", len(a))
	}
	if a == b {
		t.Errorf("two long scopes sharing a prefix collapsed onto %q, so truncation "+
			"merged two haystacks", a)
	}
}

// TestExternal_MalformedBaseURLIsPlainError — a typo'd base URL must fail as a
// configuration error, not as quota exhaustion, which would be recorded as a
// terminal capacity event and abort the run with a misleading manifest.
func TestExternal_MalformedBaseURLIsPlainError(t *testing.T) {
	sys := NewExternalSystem(ExternalConfig{BaseURL: "http://a b c"})
	_, err := sys.Recall(context.Background(), "s", Query{Text: "q"})
	if err == nil {
		t.Fatal("an unparseable base URL returned no error")
	}
	if isQuotaErr(err) {
		t.Error("a malformed base URL was reported as quota exhaustion")
	}
}

// TestExternal_ContextCancellationPropagates — a cancelled run must stop calling
// a metered service immediately, not finish the item first.
func TestExternal_ContextCancellationPropagates(t *testing.T) {
	f := &fakeExternal{reply: okRecall}
	sys := newExternalFixture(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sys.Recall(ctx, "s", Query{Text: "q"}); err == nil {
		t.Error("a cancelled context still performed the recall")
	}
	if _, err := sys.Ingest(ctx, "s", []Item{{DocumentID: "d", Content: "x"}}); err == nil {
		t.Error("a cancelled context still performed the ingest")
	}
}

// TestExternal_ConcurrentRecallIsSafe — §5.10 gives the runner a --parallel N
// flag, and one adapter instance is shared across the items. A data race on the
// notes list under -race is a crash mid-run at best and a silently lost
// methodological note at worst, so the recording must be safe for concurrent use
// before anything drives it in parallel.
func TestExternal_ConcurrentRecallIsSafe(t *testing.T) {
	f := &fakeExternal{reply: okRecall}
	sys := newExternalFixture(t, f, func(c *ExternalConfig) { c.TopKOnly = true })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct budgets so each goroutine records a DIFFERENT note, which is
			// what actually contends on the list rather than short-circuiting on the
			// dedup check.
			q := Query{Text: "q", MaxTokens: 1024 * (i + 1)}
			if _, err := sys.Recall(context.Background(), fmt.Sprintf("s%d", i), q); err != nil {
				t.Errorf("Recall: %v", err)
			}
			_ = sys.Notes()
		}(i)
	}
	wg.Wait()

	if got := len(sys.Notes()); got != 8 {
		t.Errorf("Notes() = %d entries after 8 distinct conversions, want 8 — a lost "+
			"note is a methodological difference missing from the manifest", got)
	}
}

// TestExternal_ImplementsMemorySystem — the seam is the whole point of §5.3: the
// runner drives both systems through one interface, so a signature drift here
// would be caught at the call site rather than here, far from the cause.
func TestExternal_ImplementsMemorySystem(t *testing.T) {
	var _ MemorySystem = (*ExternalSystem)(nil)
	var sys MemorySystem = NewExternalSystem(ExternalConfig{})
	if sys.Name() == "" {
		t.Error("Name() is empty through the interface")
	}
}
