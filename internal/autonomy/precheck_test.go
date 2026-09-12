package autonomy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBrokerReachable_DetectsErrorFieldsInBody — pins the
// 2026-05-06 audit fix: a /caps response that returns HTTP 200
// but carries a `*_error` field with non-empty content means the
// broker MCP's "front door" is up but the IBKR pipeline behind
// it is broken. Pre-fix the precheck only looked at status code
// and let 6 trading ticks fire against a degraded pipeline; one
// of them produced a fabricated TSM @ $150 trade proposal
// (TSM trades at ~$395) — see the audit notes for full context.
func TestBrokerReachable_DetectsErrorFieldsInBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "clean caps body — reachable",
			body: `{"configured":{"mode":"paper"},"portfolio":{"account":"DUH"}}`,
			want: true,
		},
		{
			name: "portfolio_error present — NOT reachable",
			body: `{"configured":{"mode":"paper"},"portfolio":null,"portfolio_error":"sidecar request failed: dial tcp ...: connect: connection refused"}`,
			want: false,
		},
		{
			name: "future error field (any *_error key) — NOT reachable",
			body: `{"configured":{"mode":"paper"},"feed_error":"streaming feed disconnected"}`,
			want: false,
		},
		{
			name: "empty error string — reachable (lenient on transient nulls)",
			body: `{"portfolio_error":""}`,
			want: true,
		},
		{
			name: "non-string error value — reachable (defensive parse)",
			body: `{"portfolio_error":null}`,
			want: true,
		},
		{
			name: "malformed JSON body — reachable (lenient on shape changes)",
			body: `{not-json`,
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			got := brokerReachable(context.Background(), srv.URL)
			assert.Equal(t, tc.want, got, "body: %s", tc.body)
		})
	}
}

// TestBrokerReachable_StatusGate — pre-existing 5xx / connection-
// refused behaviour preserved. A non-2xx status overrides the
// body inspection: there's no body to trust.
func TestBrokerReachable_StatusGate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"portfolio":{"account":"DUH"}}`))
	}))
	defer srv.Close()
	assert.False(t, brokerReachable(context.Background(), srv.URL))
}
