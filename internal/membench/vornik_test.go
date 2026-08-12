package membench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The vornik adapter (design §5.3, §5.5). Drives the companion MCP surface —
// remember for ingest, recall for retrieval — over HTTP, which is the same
// transport the external adapter uses, so no asymmetry can hide in the client.

// mcpCall is one captured JSON-RPC tools/call request.
type mcpCall struct {
	Name string
	Args map[string]any
}

// fakeCompanion is a stand-in for the daemon's /api/v1/mcp/companion endpoint.
// It records what it was asked and replies with whatever the test queued.
type fakeCompanion struct {
	calls   []mcpCall
	reply   func(name string, args map[string]any) (any, error)
	status  int
	rawBody string
}

func (f *fakeCompanion) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.rawBody))
			return
		}
		var req struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("adapter sent undecodable JSON-RPC: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.calls = append(f.calls, mcpCall{Name: req.Params.Name, Args: req.Params.Arguments})

		payload, err := f.reply(req.Params.Name, req.Params.Arguments)
		if err != nil {
			writeMCPError(w, err.Error())
			return
		}
		writeMCPResult(w, payload)
	}
}

func writeMCPResult(w http.ResponseWriter, payload any) {
	inner, _ := json.Marshal(payload)
	out := map[string]any{
		"jsonrpc": "2.0", "id": 1,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(inner)}},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func writeMCPError(w http.ResponseWriter, msg string) {
	out := map[string]any{
		"jsonrpc": "2.0", "id": 1,
		"result": map[string]any{
			"isError": true,
			"content": []map[string]any{{"type": "text", "text": msg}},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func newVornikFixture(t *testing.T, f *fakeCompanion) *VornikSystem {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return NewVornikSystem(VornikConfig{
		BaseURL: srv.URL,
		Token:   "test-token",
		Client:  srv.Client(),
	})
}

// TestVornik_RecallPinsStrictScope is the isolation invariant. Without
// strict_scope the default scoped query includes NULL-scoped chunks via the
// migration-grace `OR repo_scope IS NULL` clause, which leaks one benchmark
// item's haystack into another's recall (design §5.5).
func TestVornik_RecallPinsStrictScope(t *testing.T) {
	f := &fakeCompanion{reply: func(string, map[string]any) (any, error) {
		return map[string]any{"hits": []any{}}, nil
	}}
	sys := newVornikFixture(t, f)

	if _, err := sys.Recall(context.Background(), "lme/q0001", Query{Text: "who?", MaxTokens: 4096}); err != nil {
		t.Fatalf("Recall: %v", err)
	}

	if len(f.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(f.calls))
	}
	args := f.calls[0].Args
	if got := args["repo_scope"]; got != "lme/q0001" {
		t.Errorf("repo_scope = %v, want lme/q0001", got)
	}
	strict, ok := args["strict_scope"].(bool)
	if !ok || !strict {
		t.Errorf("strict_scope = %v (%T), want true — without it, one item's "+
			"haystack leaks into another's recall via the NULL-scope grace clause",
			args["strict_scope"], args["strict_scope"])
	}
}

// TestVornik_IngestSendsEventTimeAndScope — the whole reason Phase 0 shipped
// first. A dated haystack must arrive with its dates.
func TestVornik_IngestSendsEventTimeAndScope(t *testing.T) {
	f := &fakeCompanion{reply: func(string, map[string]any) (any, error) {
		return map[string]any{"decision": "ALLOW", "admitted": 1}, nil
	}}
	sys := newVornikFixture(t, f)

	when := time.Date(2023, 5, 14, 9, 30, 0, 0, time.UTC)
	_, err := sys.Ingest(context.Background(), "lme/q0001", []Item{
		{DocumentID: "q1_s1", Content: "Alice moved to Berlin", Context: "Session 1", EventTime: when},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if len(f.calls) != 1 {
		t.Fatalf("expected 1 deposit, got %d", len(f.calls))
	}
	args := f.calls[0].Args
	if got := args["repo_scope"]; got != "lme/q0001" {
		t.Errorf("repo_scope = %v, want lme/q0001", got)
	}
	if got, _ := args["event_time"].(string); !strings.HasPrefix(got, "2023-05-14T09:30:00") {
		t.Errorf("event_time = %q, want the item's event time in RFC3339", got)
	}
	content, _ := args["content"].(string)
	if !strings.Contains(content, "Alice moved to Berlin") {
		t.Errorf("content %q lost the item body", content)
	}
	if !strings.Contains(content, "Session 1") {
		t.Errorf("content %q dropped the framing context, so the two systems "+
			"would receive different provenance", content)
	}
}

// TestVornik_IngestSplitsOversizedDeposit — remember caps a deposit at 64 KiB,
// so a larger session must be split on a boundary rather than truncated. Losing
// the tail would silently remove distractors and make the item easier.
func TestVornik_IngestSplitsOversizedDeposit(t *testing.T) {
	f := &fakeCompanion{reply: func(string, map[string]any) (any, error) {
		return map[string]any{"decision": "ALLOW", "admitted": 1}, nil
	}}
	sys := newVornikFixture(t, f)

	// Three times the cap, so at least three deposits are required.
	big := strings.Repeat("x", 3*rememberMaxContentBytes)
	stats, err := sys.Ingest(context.Background(), "s", []Item{{DocumentID: "d1", Content: big}})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if len(f.calls) < 3 {
		t.Errorf("a %d-byte item produced %d deposits; it must be split, not truncated",
			len(big), len(f.calls))
	}
	if stats.Splits == 0 {
		t.Error("Splits not recorded; the split count is a reported methodological " +
			"difference versus a single-document retain elsewhere (§5.6)")
	}
	// Every byte must reach the store.
	var sent int
	for _, c := range f.calls {
		s, _ := c.Args["content"].(string)
		sent += len(s)
	}
	if sent < len(big) {
		t.Errorf("sent %d bytes of a %d-byte item; the tail was dropped", sent, len(big))
	}
}

// TestVornik_IngestRecordsRejection — a quarantined or blocked deposit is
// haystack loss, and must be counted so AssessTrust can see it.
func TestVornik_IngestRecordsRejection(t *testing.T) {
	f := &fakeCompanion{reply: func(string, map[string]any) (any, error) {
		return map[string]any{"decision": "REJECTED", "admitted": 0, "rejected": 1}, nil
	}}
	sys := newVornikFixture(t, f)

	stats, err := sys.Ingest(context.Background(), "s", []Item{{DocumentID: "d1", Content: "body"}})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if stats.Rejected != 1 {
		t.Errorf("Rejected = %d, want 1", stats.Rejected)
	}
	if stats.RejectedBytes != len("body") {
		t.Errorf("RejectedBytes = %d, want %d — without it, haystack loss is invisible "+
			"to the trust check", stats.RejectedBytes, len("body"))
	}
}

// TestVornik_RecallMapsHitsToDocumentIdentity — tier-2 metrics compare against
// GOLD DOCUMENT ids, so a hit must carry the document identity, not a chunk id.
// Getting this wrong makes recall score 0 everywhere for a reason unrelated to
// retrieval.
func TestVornik_RecallMapsHitsToDocumentIdentity(t *testing.T) {
	f := &fakeCompanion{reply: func(string, map[string]any) (any, error) {
		return map[string]any{"hits": []any{
			map[string]any{"chunk_id": "c1", "source_name": "q1_s3", "content": "text", "score": 0.9},
			map[string]any{"chunk_id": "c2", "source_name": "q1_s7", "content": "more", "score": 0.5},
		}}, nil
	}}
	sys := newVornikFixture(t, f)

	got, err := sys.Recall(context.Background(), "s", Query{Text: "q", MaxTokens: 100})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	ids := got.SourceIDs()
	if len(ids) != 2 || ids[0] != "q1_s3" || ids[1] != "q1_s7" {
		t.Errorf("SourceIDs = %v, want [q1_s3 q1_s7] in rank order", ids)
	}
	if got.Latency <= 0 {
		t.Error("Latency not measured; tier-3 reporting needs it")
	}
}

// TestVornik_RecallSendsTokenBudget — both systems must be asked for the same
// budget, or the comparison is unfair in a way the numbers won't show.
func TestVornik_RecallSendsTokenBudget(t *testing.T) {
	f := &fakeCompanion{reply: func(string, map[string]any) (any, error) {
		return map[string]any{"hits": []any{}}, nil
	}}
	sys := newVornikFixture(t, f)

	if _, err := sys.Recall(context.Background(), "s", Query{Text: "q", MaxTokens: 2048}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	args := f.calls[0].Args
	if args["limit"] == nil && args["max_tokens"] == nil {
		t.Error("recall sent neither a limit nor a token budget; the two systems " +
			"would be asked for different amounts of context")
	}
}

// TestVornik_HTTP429IsQuotaExhausted — a rate limit must surface as the terminal
// sentinel so the runner aborts rather than retrying into the limit.
func TestVornik_HTTP429IsQuotaExhausted(t *testing.T) {
	f := &fakeCompanion{status: http.StatusTooManyRequests, rawBody: "slow down"}
	sys := newVornikFixture(t, f)

	_, err := sys.Recall(context.Background(), "s", Query{Text: "q"})
	if err == nil {
		t.Fatal("a 429 returned no error")
	}
	if !isQuotaErr(err) {
		t.Errorf("429 produced %v, want it to match ErrQuotaExhausted so the "+
			"runner aborts instead of retrying", err)
	}
}

// TestVornik_HTTP500IsPlainError — a server fault is NOT quota exhaustion.
// Conflating them would abort a whole run over one transient blip.
func TestVornik_HTTP500IsPlainError(t *testing.T) {
	f := &fakeCompanion{status: http.StatusInternalServerError, rawBody: "boom"}
	sys := newVornikFixture(t, f)

	_, err := sys.Recall(context.Background(), "s", Query{Text: "q"})
	if err == nil {
		t.Fatal("a 500 returned no error")
	}
	if isQuotaErr(err) {
		t.Error("a 500 was reported as quota exhaustion; one transient fault would " +
			"abort the entire run")
	}
}

// TestVornik_ToolErrorSurfaces — an MCP-level isError reply must not be mistaken
// for an empty result. Silently scoring it as "retrieved nothing" would report a
// harness fault as a retrieval failure.
func TestVornik_ToolErrorSurfaces(t *testing.T) {
	f := &fakeCompanion{reply: func(string, map[string]any) (any, error) {
		return nil, fmt.Errorf("this key lacks memory_read")
	}}
	sys := newVornikFixture(t, f)

	if _, err := sys.Recall(context.Background(), "s", Query{Text: "q"}); err == nil {
		t.Error("an MCP isError reply was treated as a successful empty recall")
	}
}

// TestVornik_Name — identifies the system in manifests.
func TestVornik_Name(t *testing.T) {
	sys := NewVornikSystem(VornikConfig{})
	if sys.Name() != "vornik" {
		t.Errorf("Name() = %q, want vornik", sys.Name())
	}
}

func isQuotaErr(err error) bool {
	type unwrapper interface{ Unwrap() []error }
	if err == nil {
		return false
	}
	if err == ErrQuotaExhausted {
		return true
	}
	if u, ok := err.(unwrapper); ok {
		for _, e := range u.Unwrap() {
			if isQuotaErr(e) {
				return true
			}
		}
	}
	if u, ok := err.(interface{ Unwrap() error }); ok {
		return isQuotaErr(u.Unwrap())
	}
	return false
}

// TestVornik_PrepareTeardownConfig — the three trivial MemorySystem members.
// Prepare and Teardown are deliberately no-ops (a repo_scope needs no creation,
// and per-item teardown would dominate a 500-item run), but they must satisfy
// the interface without error so the runner treats both systems identically.
func TestVornik_PrepareTeardownConfig(t *testing.T) {
	sys := NewVornikSystem(VornikConfig{ExtractionModel: "qwen3.6:35b"})
	ctx := context.Background()

	if err := sys.Prepare(ctx, "s"); err != nil {
		t.Errorf("Prepare: %v", err)
	}
	if err := sys.Teardown(ctx, "s"); err != nil {
		t.Errorf("Teardown: %v", err)
	}
	cfg, err := sys.Config(ctx)
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg != "qwen3.6:35b" {
		t.Errorf("Config() = %q, want the configured extraction model", cfg)
	}
}

// TestVornik_ConfigEmptyWhenUnset — an unknown model must report empty rather
// than a plausible-looking guess, so ComparabilityFields.Partial() can see the
// gap instead of a fabricated value passing as verified.
func TestVornik_ConfigEmptyWhenUnset(t *testing.T) {
	cfg, err := NewVornikSystem(VornikConfig{}).Config(context.Background())
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cfg != "" {
		t.Errorf("Config() = %q on an unconfigured adapter, want empty", cfg)
	}
}

// TestVornik_TemporalBoundsSent — the dataset's dated questions need the window
// to reach the daemon, or the temporal categories are unanswerable for a reason
// that has nothing to do with retrieval.
func TestVornik_TemporalBoundsSent(t *testing.T) {
	f := &fakeCompanion{reply: func(string, map[string]any) (any, error) {
		return map[string]any{"hits": []any{}}, nil
	}}
	sys := newVornikFixture(t, f)

	from := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2023, 12, 31, 0, 0, 0, 0, time.UTC)
	if _, err := sys.Recall(context.Background(), "s", Query{Text: "q", From: from, To: to}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	args := f.calls[0].Args
	if got, _ := args["from_date"].(string); !strings.HasPrefix(got, "2023-01-01") {
		t.Errorf("from_date = %q, want 2023-01-01", got)
	}
	if got, _ := args["to_date"].(string); !strings.HasPrefix(got, "2023-12-31") {
		t.Errorf("to_date = %q, want 2023-12-31", got)
	}
}

// TestVornik_UnreachableDaemonIsPlainError — a connection failure is not quota
// exhaustion, and must not abort a run that a retry could survive.
func TestVornik_UnreachableDaemonIsPlainError(t *testing.T) {
	// Port 0 on the loopback is never listening.
	sys := NewVornikSystem(VornikConfig{
		BaseURL: "http://127.0.0.1:0",
		Client:  &http.Client{Timeout: time.Second},
	})
	_, err := sys.Recall(context.Background(), "s", Query{Text: "q"})
	if err == nil {
		t.Fatal("an unreachable daemon returned no error")
	}
	if isQuotaErr(err) {
		t.Error("a connection failure was reported as quota exhaustion")
	}
}

// TestVornik_MalformedPayloadErrors — a tool reply whose inner JSON doesn't
// decode must fail rather than yield a silently empty recall.
func TestVornik_MalformedPayloadErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		out := map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "not json at all"}},
			},
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	sys := NewVornikSystem(VornikConfig{BaseURL: srv.URL, Client: srv.Client()})
	if _, err := sys.Recall(context.Background(), "s", Query{Text: "q"}); err == nil {
		t.Error("an undecodable payload was treated as a successful empty recall")
	}
}

// TestVornik_HTTP402IsQuotaExhausted — payment-required is the other capacity
// refusal, and gets the same terminal handling as a 429.
func TestVornik_HTTP402IsQuotaExhausted(t *testing.T) {
	f := &fakeCompanion{status: http.StatusPaymentRequired, rawBody: "out of credit"}
	sys := newVornikFixture(t, f)

	_, err := sys.Recall(context.Background(), "s", Query{Text: "q"})
	if !isQuotaErr(err) {
		t.Errorf("402 produced %v, want ErrQuotaExhausted", err)
	}
}

// TestVornik_EmptyContentReplyErrors — a well-formed envelope with no content
// block is a protocol violation, not an empty result.
func TestVornik_EmptyContentReplyErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"content": []map[string]any{}},
		})
	}))
	defer srv.Close()

	sys := NewVornikSystem(VornikConfig{BaseURL: srv.URL, Client: srv.Client()})
	if _, err := sys.Recall(context.Background(), "s", Query{Text: "q"}); err == nil {
		t.Error("an empty content block was treated as a successful recall")
	}
}

// Readiness and pending-ingest now come from the companion `whoami` payload, not
// from /api/v1/memory/stats.
//
// That route is ADMIN-only and this adapter authenticates with a COMPANION key, so
// the old implementation returned 403 on every run ever made with one. The error was
// mapped to "readiness unknown", unknown was treated as acceptable, and the signal
// built in §13.11 to catch a partially-embedded corpus never fired once. It took a
// fabricated head-to-head result to notice.

func whoamiServer(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/v1/mcp/companion") {
			t.Errorf("requested %q, want the companion MCP route — readiness must use a "+
				"surface the companion key can actually reach", r.URL.Path)
		}
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": payload}}},
		})
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestVornik_EmbeddingReadiness — the fraction that tells a reader whether the
// tier-2 numbers describe semantic or keyword-dominant retrieval.
func TestVornik_EmbeddingReadiness(t *testing.T) {
	srv := whoamiServer(t, `{"project_id":"p","embedding_readiness":0.25,
		"memory_chunks_total":200,"memory_chunks_embedded":50,"memory_embed_queue_depth":150}`)

	sys := NewVornikSystem(VornikConfig{BaseURL: srv.URL, Client: srv.Client()})
	got, err := sys.EmbeddingReadiness(context.Background())
	if err != nil {
		t.Fatalf("EmbeddingReadiness: %v", err)
	}
	if got != 0.25 {
		t.Errorf("readiness = %v, want 0.25", got)
	}
}

// TestVornik_PendingIngest — the signal settle actually waits on. Distinct from
// readiness: a corpus can be 25% embedded and climbing, or 25% embedded and stable
// because no embedder is wired, and only the first is worth waiting for.
func TestVornik_PendingIngest(t *testing.T) {
	srv := whoamiServer(t, `{"embedding_readiness":0.25,"memory_embed_queue_depth":150}`)

	sys := NewVornikSystem(VornikConfig{BaseURL: srv.URL, Client: srv.Client()})
	got, err := sys.PendingIngest(context.Background())
	if err != nil {
		t.Fatalf("PendingIngest: %v", err)
	}
	if got != 150 {
		t.Errorf("pending = %d, want 150", got)
	}
}

// TestVornik_PendingIngest_UnreportedIsAnError — a daemon that reports no memory
// stats would otherwise present as queue depth 0, i.e. "settled", which is exactly
// the false-clear that let a mid-drain corpus be scored.
func TestVornik_PendingIngest_UnreportedIsAnError(t *testing.T) {
	srv := whoamiServer(t, `{"project_id":"p","database":"bench"}`)

	sys := NewVornikSystem(VornikConfig{BaseURL: srv.URL, Client: srv.Client()})
	if _, err := sys.PendingIngest(context.Background()); err == nil {
		t.Error("a daemon reporting no memory stats was treated as a settled corpus")
	}
}

// TestVornik_EmbeddingReadiness_UnreportedIsAnError — absent is not 0.0 and not
// 1.0. A caller must be able to tell "nothing embedded" from "cannot say".
func TestVornik_EmbeddingReadiness_UnreportedIsAnError(t *testing.T) {
	srv := whoamiServer(t, `{"project_id":"p"}`)

	sys := NewVornikSystem(VornikConfig{BaseURL: srv.URL, Client: srv.Client()})
	if _, err := sys.EmbeddingReadiness(context.Background()); err == nil {
		t.Error("an unreported readiness was returned as a fraction instead of erroring")
	}
}

// TestVornik_EmbeddingReadiness_HTTPErrorSurfaces — an unreachable companion route
// leaves readiness UNKNOWN rather than reporting a fabricated fraction.
func TestVornik_EmbeddingReadiness_HTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	sys := NewVornikSystem(VornikConfig{BaseURL: srv.URL, Client: srv.Client()})
	if _, err := sys.EmbeddingReadiness(context.Background()); err == nil {
		t.Error("a 404 produced a readiness fraction")
	}
}

// TestVornik_RecallRequestsProductionPath — the harness must ask for the retrieval
// mode agents actually use. Reranking is gated on the caller requesting it, so
// omitting this measures single-shot RRF and a gate built on it could stay green
// while the agent context-assembly path regresses.
func TestVornik_RecallRequestsProductionPath(t *testing.T) {
	f := &fakeCompanion{reply: func(string, map[string]any) (any, error) {
		return map[string]any{"hits": []any{}}, nil
	}}
	sys := newVornikFixture(t, f)

	if _, err := sys.Recall(context.Background(), "s", Query{Text: "q", MaxTokens: 4096}); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	got, ok := f.calls[0].Args["sufficient"].(bool)
	if !ok || !got {
		t.Errorf("sufficient = %v (%T), want true — without it the reranker never "+
			"fires and the benchmark measures the interactive path instead of the "+
			"agent path", f.calls[0].Args["sufficient"], f.calls[0].Args["sufficient"])
	}
}
