package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

type capturingTradingOrderRepo struct {
	row *persistence.TradingOrder
	err error
}

func (r *capturingTradingOrderRepo) Record(_ context.Context, row *persistence.TradingOrder) error {
	r.row = row
	return r.err
}

func (r *capturingTradingOrderRepo) List(context.Context, persistence.TradingOrderFilter) ([]*persistence.TradingOrder, error) {
	return nil, nil
}

func (r *capturingTradingOrderRepo) Count(context.Context, persistence.TradingOrderFilter) (int64, error) {
	return 0, nil
}

type capturingTradingSafetyRepo struct {
	row *persistence.TradingSafetyEvent
	err error
}

func (r *capturingTradingSafetyRepo) Record(_ context.Context, row *persistence.TradingSafetyEvent) error {
	r.row = row
	return r.err
}

func (r *capturingTradingSafetyRepo) List(context.Context, persistence.TradingSafetyEventFilter) ([]*persistence.TradingSafetyEvent, error) {
	return nil, nil
}

func (r *capturingTradingSafetyRepo) Count(context.Context, persistence.TradingSafetyEventFilter) (int64, error) {
	return 0, nil
}

type capturingTradingFillRepo struct {
	row       *persistence.TradingFill
	shadowRow *persistence.TradingFill
	err       error
	shadowErr error
}

func (r *capturingTradingFillRepo) Record(_ context.Context, row *persistence.TradingFill) error {
	r.row = row
	return r.err
}

func (r *capturingTradingFillRepo) RecordShadow(_ context.Context, row *persistence.TradingFill) error {
	r.shadowRow = row
	return r.shadowErr
}

func (r *capturingTradingFillRepo) List(context.Context, persistence.TradingFillFilter) ([]*persistence.TradingFill, error) {
	return nil, nil
}

func (r *capturingTradingFillRepo) SumVolume(context.Context, persistence.TradingFillFilter) (float64, error) {
	return 0, nil
}

func (r *capturingTradingFillRepo) MaxFilledAt(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}

func (r *capturingTradingFillRepo) PatchCommission(context.Context, string, float64) error {
	return nil
}

func (r *capturingTradingFillRepo) ListNullCommission(_ context.Context, _ time.Time) ([]*persistence.TradingFill, error) {
	return nil, nil
}

type capturingFillNotifier struct {
	fill *persistence.TradingFill
}

func (n *capturingFillNotifier) NotifyFill(_ context.Context, fill *persistence.TradingFill) {
	n.fill = fill
}

func scopedRequest(method, target, body string, projects ...string) *http.Request {
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	if projects != nil {
		req = req.WithContext(context.WithValue(req.Context(), projectIDKey, projects))
	}
	return req
}

func TestIngestTradingOrderRecordsOptionalFields(t *testing.T) {
	repo := &capturingTradingOrderRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(repo))
	body := `{
		"id":"ord_1",
		"project_id":"proj-a",
		"task_id":"task-1",
		"execution_id":"exec-1",
		"broker_order_id":"ib-7",
		"idempotency_key":"idem-1",
		"mode":"paper",
		"symbol":"AAPL",
		"action":"BUY",
		"order_type":"LMT",
		"qty":2,
		"limit_price":195.5,
		"stop_price":190,
		"time_in_force":"DAY",
		"status":"submitted",
		"last_status_reason":"accepted",
		"submitted_at":"2026-05-05T13:45:00Z",
		"terminal_at":"2026-05-05T13:50:00Z"
	}`
	rec := httptest.NewRecorder()

	server.IngestTradingOrder(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", body, "proj-a"))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	row := repo.row
	if row == nil {
		t.Fatal("Record was not called")
	}
	if row.ID != "ord_1" || row.ProjectID != "proj-a" || row.Symbol != "AAPL" || row.Qty != 2 || row.Status != "submitted" {
		t.Fatalf("unexpected order row: %#v", row)
	}
	if row.TaskID == nil || *row.TaskID != "task-1" || row.ExecutionID == nil || *row.ExecutionID != "exec-1" || row.BrokerOrderID == nil || *row.BrokerOrderID != "ib-7" {
		t.Fatalf("optional string fields not captured: %#v", row)
	}
	if row.LimitPrice == nil || *row.LimitPrice != 195.5 || row.StopPrice == nil || *row.StopPrice != 190 {
		t.Fatalf("optional prices not captured: %#v", row)
	}
	if row.SubmittedAt.Format(time.RFC3339) != "2026-05-05T13:45:00Z" || row.TerminalAt == nil || row.TerminalAt.Format(time.RFC3339) != "2026-05-05T13:50:00Z" {
		t.Fatalf("timestamps not captured: submitted=%s terminal=%v", row.SubmittedAt, row.TerminalAt)
	}
}

func TestIngestTradingOrderDefaultsInvalidTimestamps(t *testing.T) {
	repo := &capturingTradingOrderRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(repo))
	body := `{"id":"ord_2","project_id":"proj-a","idempotency_key":"idem-2","symbol":"AAPL","status":"submitted","qty":1,"submitted_at":"bad","terminal_at":"also-bad"}`
	rec := httptest.NewRecorder()

	server.IngestTradingOrder(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", body, "proj-a"))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if repo.row == nil || repo.row.SubmittedAt.IsZero() {
		t.Fatalf("submitted_at should default to now: %#v", repo.row)
	}
	if repo.row.TerminalAt != nil {
		t.Fatalf("invalid terminal_at should be ignored: %#v", repo.row.TerminalAt)
	}
}

func TestIngestTradingOrderRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name   string
		server *Server
		req    *http.Request
		want   int
	}{
		{"method", NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(&capturingTradingOrderRepo{})), scopedRequest(http.MethodGet, "/api/v1/internal/trading-orders", ""), http.StatusMethodNotAllowed},
		{"missing repo", NewServer(WithLogger(zerolog.Nop())), scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", `{}`), http.StatusServiceUnavailable},
		{"bad json", NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(&capturingTradingOrderRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", `{`, "proj-a"), http.StatusBadRequest},
		{"missing required", NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(&capturingTradingOrderRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", `{"id":"ord_1"}`, "proj-a"), http.StatusBadRequest},
		{"invalid qty", NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(&capturingTradingOrderRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", `{"id":"ord_1","project_id":"proj-a","idempotency_key":"idem","symbol":"AAPL","status":"submitted","qty":0}`, "proj-a"), http.StatusBadRequest},
		{"negative limit", NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(&capturingTradingOrderRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", `{"id":"ord_1","project_id":"proj-a","idempotency_key":"idem","symbol":"AAPL","status":"submitted","qty":1,"limit_price":-1}`, "proj-a"), http.StatusBadRequest},
		{"scope mismatch", NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(&capturingTradingOrderRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", `{"id":"ord_1","project_id":"proj-b","idempotency_key":"idem","symbol":"AAPL","status":"submitted","qty":1}`, "proj-a"), http.StatusForbidden},
		{"repo failure", NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(&capturingTradingOrderRepo{err: errors.New("db down")})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", `{"id":"ord_1","project_id":"proj-a","idempotency_key":"idem","symbol":"AAPL","status":"submitted","qty":1}`, "proj-a"), http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.server.IngestTradingOrder(rec, tt.req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), tt.want)
			}
		})
	}
}

func TestIngestTradingOrderRejectsOversizedBody(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(&capturingTradingOrderRepo{}))
	rec := httptest.NewRecorder()
	server.IngestTradingOrder(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", strings.Repeat("x", 64*1024+1), "proj-a"))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body=%s, want 413", rec.Code, rec.Body.String())
	}
}

func TestIngestTradingFillRecordsAndNotifies(t *testing.T) {
	repo := &capturingTradingFillRepo{}
	notifier := &capturingFillNotifier{}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTradingFillRepository(repo),
		WithFillNotifier(notifier),
	)
	body := `{
		"id":"fill-1",
		"order_id":"ord-1",
		"project_id":"proj-a",
		"symbol":"MSFT",
		"qty":1.5,
		"price":407.25,
		"commission_usd":0.35,
		"filled_at":"2026-05-05T14:00:00Z"
	}`
	rec := httptest.NewRecorder()

	server.IngestTradingFill(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills", body, "proj-a"))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if repo.row == nil || repo.row.ID != "fill-1" || repo.row.OrderID != "ord-1" || repo.row.Qty != 1.5 || repo.row.Price != 407.25 {
		t.Fatalf("unexpected fill row: %#v", repo.row)
	}
	if repo.row.CommissionUSD == nil || *repo.row.CommissionUSD != 0.35 || repo.row.FilledAt.Format(time.RFC3339) != "2026-05-05T14:00:00Z" {
		t.Fatalf("optional fill fields not captured: %#v", repo.row)
	}
	if notifier.fill != repo.row {
		t.Fatalf("notifier fill = %#v, want recorded fill", notifier.fill)
	}
}

func TestIngestTradingFillRecordsExecKeyedFields(t *testing.T) {
	// The exec-keyed reconcile path (ReconcileExecutions) posts
	// exec_id/account_id/source/source_detail. The ingest handler must
	// map them onto the persisted row, otherwise the AAPL stop-out
	// booking lands without its provenance columns.
	repo := &capturingTradingFillRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(repo))
	body := `{
		"id":"exec-0001.01",
		"order_id":"aapl-open_stop",
		"project_id":"proj-a",
		"symbol":"AAPL",
		"qty":8,
		"price":286,
		"exec_id":"0001.01",
		"account_id":"DUH1",
		"source":"broker_reconcile",
		"source_detail":"order_ref=aapl-open_stop perm=2001",
		"filled_at":"2026-06-25T13:32:00Z"
	}`
	rec := httptest.NewRecorder()

	server.IngestTradingFill(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills", body, "proj-a"))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	row := repo.row
	if row == nil {
		t.Fatal("Record was not called")
	}
	if row.ExecID == nil || *row.ExecID != "0001.01" {
		t.Fatalf("exec_id not captured: %#v", row.ExecID)
	}
	if row.AccountID == nil || *row.AccountID != "DUH1" {
		t.Fatalf("account_id not captured: %#v", row.AccountID)
	}
	if row.Source != "broker_reconcile" {
		t.Fatalf("source not captured: %q", row.Source)
	}
	if row.SourceDetail == nil || *row.SourceDetail != "order_ref=aapl-open_stop perm=2001" {
		t.Fatalf("source_detail not captured: %#v", row.SourceDetail)
	}
}

func TestIngestTradingFillDefaultsInvalidTimestamp(t *testing.T) {
	repo := &capturingTradingFillRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(repo))
	body := `{"id":"fill-2","order_id":"ord-2","project_id":"proj-a","symbol":"MSFT","qty":1,"price":400,"filled_at":"bad"}`
	rec := httptest.NewRecorder()

	server.IngestTradingFill(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills", body, "proj-a"))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if repo.row == nil || repo.row.FilledAt.IsZero() {
		t.Fatalf("filled_at should default to now: %#v", repo.row)
	}
	if repo.row.CommissionUSD != nil {
		t.Fatalf("commission should stay nil when omitted: %#v", repo.row.CommissionUSD)
	}
}

func TestIngestTradingFillRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name   string
		server *Server
		req    *http.Request
		want   int
	}{
		{"method", NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{})), scopedRequest(http.MethodGet, "/api/v1/internal/trading-fills", ""), http.StatusMethodNotAllowed},
		{"missing repo", NewServer(WithLogger(zerolog.Nop())), scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills", `{}`), http.StatusServiceUnavailable},
		{"bad json", NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills", `{`, "proj-a"), http.StatusBadRequest},
		{"missing required", NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills", `{"id":"fill-1"}`, "proj-a"), http.StatusBadRequest},
		{"non positive qty", NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills", `{"id":"fill-1","order_id":"ord-1","project_id":"proj-a","symbol":"MSFT","qty":0}`, "proj-a"), http.StatusBadRequest},
		{"negative price", NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills", `{"id":"fill-1","order_id":"ord-1","project_id":"proj-a","symbol":"MSFT","qty":1,"price":-1}`, "proj-a"), http.StatusBadRequest},
		{"scope mismatch", NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills", `{"id":"fill-1","order_id":"ord-1","project_id":"proj-b","symbol":"MSFT","qty":1}`, "proj-a"), http.StatusForbidden},
		{"repo failure", NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{err: errors.New("db down")})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills", `{"id":"fill-1","order_id":"ord-1","project_id":"proj-a","symbol":"MSFT","qty":1}`, "proj-a"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.server.IngestTradingFill(rec, tt.req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), tt.want)
			}
		})
	}
}

func TestIngestTradingSafetyEventRejectsOversizedBody(t *testing.T) {
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingSafetyEventRepository(&capturingTradingSafetyRepo{}))
	rec := httptest.NewRecorder()
	server.IngestTradingSafetyEvent(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-safety-events", strings.Repeat("x", 64*1024+1), "proj-a"))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body=%s, want 413", rec.Code, rec.Body.String())
	}
}

func TestIngestTradingSafetyEventRecordsDetail(t *testing.T) {
	repo := &capturingTradingSafetyRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingSafetyEventRepository(repo))
	body := `{
		"id":"safe-1",
		"project_id":"proj-a",
		"kind":"cap_refused",
		"severity":"warn",
		"symbol":"TSLA",
		"detail":{"cap_kind":"max_position_usd","attempted_value":2000},
		"recorded_at":"2026-05-05T15:00:00Z"
	}`
	rec := httptest.NewRecorder()

	server.IngestTradingSafetyEvent(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-safety-events", body, "proj-a"))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	row := repo.row
	if row == nil || row.ID != "safe-1" || row.ProjectID != "proj-a" || row.Kind != "cap_refused" || row.Severity != "warn" {
		t.Fatalf("unexpected safety row: %#v", row)
	}
	if row.Symbol == nil || *row.Symbol != "TSLA" || row.RecordedAt.Format(time.RFC3339) != "2026-05-05T15:00:00Z" {
		t.Fatalf("optional safety fields not captured: %#v", row)
	}
	var detail map[string]any
	if err := json.Unmarshal(row.Detail, &detail); err != nil {
		t.Fatalf("detail is not JSON: %v", err)
	}
	if detail["cap_kind"] != "max_position_usd" || detail["attempted_value"] != float64(2000) {
		t.Fatalf("unexpected detail: %v", detail)
	}
}

func TestIngestTradingSafetyEventDefaultsInvalidTimestamp(t *testing.T) {
	repo := &capturingTradingSafetyRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingSafetyEventRepository(repo))
	body := `{"id":"safe-2","project_id":"proj-a","kind":"replay_hit","recorded_at":"bad"}`
	rec := httptest.NewRecorder()

	server.IngestTradingSafetyEvent(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-safety-events", body, "proj-a"))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if repo.row == nil || repo.row.RecordedAt.IsZero() {
		t.Fatalf("recorded_at should default to now: %#v", repo.row)
	}
	if repo.row.Symbol != nil || repo.row.Detail != nil {
		t.Fatalf("optional fields should stay nil when omitted: %#v", repo.row)
	}
}

func TestIngestTradingSafetyEventRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name   string
		server *Server
		req    *http.Request
		want   int
	}{
		{"method", NewServer(WithLogger(zerolog.Nop()), WithTradingSafetyEventRepository(&capturingTradingSafetyRepo{})), scopedRequest(http.MethodGet, "/api/v1/internal/trading-safety-events", ""), http.StatusMethodNotAllowed},
		{"missing repo", NewServer(WithLogger(zerolog.Nop())), scopedRequest(http.MethodPost, "/api/v1/internal/trading-safety-events", `{}`), http.StatusServiceUnavailable},
		{"bad json", NewServer(WithLogger(zerolog.Nop()), WithTradingSafetyEventRepository(&capturingTradingSafetyRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-safety-events", `{`, "proj-a"), http.StatusBadRequest},
		{"missing required", NewServer(WithLogger(zerolog.Nop()), WithTradingSafetyEventRepository(&capturingTradingSafetyRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-safety-events", `{"id":"safe-1"}`, "proj-a"), http.StatusBadRequest},
		{"scope mismatch", NewServer(WithLogger(zerolog.Nop()), WithTradingSafetyEventRepository(&capturingTradingSafetyRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-safety-events", `{"id":"safe-1","project_id":"proj-b","kind":"cap_refused"}`, "proj-a"), http.StatusForbidden},
		{"repo failure", NewServer(WithLogger(zerolog.Nop()), WithTradingSafetyEventRepository(&capturingTradingSafetyRepo{err: errors.New("db down")})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-safety-events", `{"id":"safe-1","project_id":"proj-a","kind":"cap_refused"}`, "proj-a"), http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.server.IngestTradingSafetyEvent(rec, tt.req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), tt.want)
			}
		})
	}
}

// TestIngestTradingFillShadowRecordsViaShadowRepo verifies that
// IngestTradingFillShadow calls RecordShadow (not Record) on the repo
// and maps all exec-keyed provenance fields correctly — mirroring
// TestIngestTradingFillRecordsExecKeyedFields for the shadow path.
func TestIngestTradingFillShadowRecordsViaShadowRepo(t *testing.T) {
	repo := &capturingTradingFillRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(repo))
	body := `{
		"id":"exec-shadow-01",
		"order_id":"msft-open_stop",
		"project_id":"proj-a",
		"symbol":"MSFT",
		"qty":3,
		"price":410,
		"exec_id":"shadow-01",
		"account_id":"DU2",
		"source":"reconcile",
		"source_detail":"order_ref=msft-open_stop perm=2002",
		"filled_at":"2026-06-26T10:00:00Z"
	}`
	rec := httptest.NewRecorder()

	server.IngestTradingFillShadow(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills-shadow", body, "proj-a"))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if repo.row != nil {
		t.Fatal("Record (live) must NOT be called on the shadow path")
	}
	row := repo.shadowRow
	if row == nil {
		t.Fatal("RecordShadow was not called")
	}
	if row.ID != "exec-shadow-01" || row.OrderID != "msft-open_stop" || row.Symbol != "MSFT" || row.Qty != 3 || row.Price != 410 {
		t.Fatalf("unexpected shadow fill row: %#v", row)
	}
	if row.ExecID == nil || *row.ExecID != "shadow-01" {
		t.Fatalf("exec_id not captured: %#v", row.ExecID)
	}
	if row.AccountID == nil || *row.AccountID != "DU2" {
		t.Fatalf("account_id not captured: %#v", row.AccountID)
	}
	if row.Source != "reconcile" {
		t.Fatalf("source not captured: %q", row.Source)
	}
	if row.SourceDetail == nil || *row.SourceDetail != "order_ref=msft-open_stop perm=2002" {
		t.Fatalf("source_detail not captured: %#v", row.SourceDetail)
	}
}

// TestIngestTradingFillShadowRejectsInvalidRequests mirrors the live fill
// rejection tests, confirming the shadow handler enforces the same guards.
func TestIngestTradingFillShadowRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name   string
		server *Server
		req    *http.Request
		want   int
	}{
		{"method", NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{})), scopedRequest(http.MethodGet, "/api/v1/internal/trading-fills-shadow", ""), http.StatusMethodNotAllowed},
		{"missing repo", NewServer(WithLogger(zerolog.Nop())), scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills-shadow", `{}`), http.StatusServiceUnavailable},
		{"bad json", NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills-shadow", `{`, "proj-a"), http.StatusBadRequest},
		{"missing required", NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills-shadow", `{"id":"fill-1"}`, "proj-a"), http.StatusBadRequest},
		{"non positive qty", NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills-shadow", `{"id":"fill-1","order_id":"ord-1","project_id":"proj-a","symbol":"MSFT","qty":0}`, "proj-a"), http.StatusBadRequest},
		{"negative price", NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills-shadow", `{"id":"fill-1","order_id":"ord-1","project_id":"proj-a","symbol":"MSFT","qty":1,"price":-1}`, "proj-a"), http.StatusBadRequest},
		{"scope mismatch", NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills-shadow", `{"id":"fill-1","order_id":"ord-1","project_id":"proj-b","symbol":"MSFT","qty":1}`, "proj-a"), http.StatusForbidden},
		{"repo failure", NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&capturingTradingFillRepo{shadowErr: errors.New("db down")})), scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills-shadow", `{"id":"fill-1","order_id":"ord-1","project_id":"proj-a","symbol":"MSFT","qty":1}`, "proj-a"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.server.IngestTradingFillShadow(rec, tt.req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), tt.want)
			}
		})
	}
}
