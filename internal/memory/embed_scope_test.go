package memory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Slice 1 of the embed-spend-attribution design
// (https://docs.vornik.io §4.1).
//
// No spend is recorded yet. What is enforced here is that every embedding call
// STATES who it is billed to, because the failure this whole item exists to
// remove is a call nobody thought about: a nil recorder records nothing without
// complaining, and an ambient default is how a caller forgets.
//
// Attribution is a required parameter rather than a context value on purpose.
// Round-1 review of the design rejected the context shape on evidence from this
// codebase — graph.completeWithRetry already overwrites the ctx call-site — so
// attribution a lower layer can silently rewrite is not attribution.

func TestEmbedScope_Validate(t *testing.T) {
	cases := []struct {
		name    string
		scope   EmbedScope
		wantErr bool
		because string
	}{
		{
			name:    "project and call site set",
			scope:   EmbedScope{ProjectID: "janka", CallSite: "memory.ingest"},
			wantErr: false,
			because: "the ordinary case: attributable work",
		},
		{
			name:    "missing call site",
			scope:   EmbedScope{ProjectID: "janka"},
			wantErr: true,
			because: "a project alone cannot say WHICH path spent the money",
		},
		{
			name:    "missing project on a non-infra call site",
			scope:   EmbedScope{CallSite: "memory.ingest"},
			wantErr: true,
			because: "unattributed project spend is the defect being fixed, not a default",
		},
		{
			name:    "infra probe may omit the project",
			scope:   EmbedScope{CallSite: EmbedCallSiteInfraProbe},
			wantErr: false,
			because: "a reachability probe has no project; it is billed to infrastructure",
		},
		{
			name:    "wholly empty scope",
			scope:   EmbedScope{},
			wantErr: true,
			because: "the zero value must never be silently acceptable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.scope.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() = nil, want error — %s", tc.because)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil — %s", err, tc.because)
			}
		})
	}
}

// TestEmbed_InvalidScopeIsAnErrorNotADegrade draws the line the design's §4.2
// depends on. Embed returns (nil, nil) for a genuine degrade — network error,
// non-200, unparseable body — so callers can carry on without vectors. An
// invalid scope is NOT that: it is a programming error at the call site, and
// collapsing it into the degrade path would let an unattributed caller ship
// looking exactly like a flaky endpoint.
//
// It must also spend nothing: rejecting after the provider call would bill money
// the ledger then cannot attribute.
func TestEmbed_InvalidScopeIsAnErrorNotADegrade(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]}]}`))
	}))
	defer srv.Close()

	e := NewEmbedder(Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "m"})
	vecs, err := e.Embed(context.Background(), EmbedScope{}, []string{"hello"})

	if err == nil {
		t.Error("Embed with a zero scope returned nil error — an unattributed call must fail loudly")
	}
	if vecs != nil {
		t.Errorf("Embed with an invalid scope returned %d vectors, want none", len(vecs))
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("provider was called %d times on an invalid scope — validation must precede spend", n)
	}
	// The error has to name the offending call site, or a failure in CI tells
	// nobody which caller to fix.
	if err != nil && !strings.Contains(err.Error(), "scope") {
		t.Errorf("error %q does not mention the scope — it must say what is wrong", err)
	}
}

// TestEmbed_ValidScopeLeavesBehaviourUnchanged is the regression half of slice 1:
// threading a scope through must not alter what Embed does. Slice 1 records no
// spend, so a passing embed here is byte-identical to the pre-change behaviour.
func TestEmbed_ValidScopeLeavesBehaviourUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.5,0.6]},{"index":1,"embedding":[0.7,0.8]}]}`))
	}))
	defer srv.Close()

	e := NewEmbedder(Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "m"})
	vecs, err := e.Embed(context.Background(),
		EmbedScope{ProjectID: "janka", CallSite: "memory.ingest"},
		[]string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 2 || vecs[0][0] != 0.5 || vecs[1][1] != 0.8 {
		t.Errorf("vectors did not survive the scope plumbing: %v", vecs)
	}
}

// TestEmbedQuery_CarriesAScope closes the gap the round-2 review flagged as its
// one optional catch: the internal query helper at embedder.go:398 delegates to
// Embed, so it must take a scope through rather than assume its callers have
// one — otherwise query-time embedding is the one path that stays unattributed.
func TestEmbedQuery_CarriesAScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.9]}]}`))
	}))
	defer srv.Close()

	e := NewEmbedder(Config{EmbeddingEndpoint: srv.URL, EmbeddingModel: "m"})
	vec, err := e.EmbedQuery(context.Background(),
		EmbedScope{ProjectID: "janka", CallSite: EmbedCallSiteSearchQuery},
		"what did we decide about retries")
	if err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if len(vec) != 1 || vec[0] != 0.9 {
		t.Errorf("EmbedQuery returned %v, want [0.9]", vec)
	}

	if _, err := e.EmbedQuery(context.Background(), EmbedScope{}, "unattributed"); err == nil {
		t.Error("EmbedQuery with a zero scope must fail — query-time spend needs attribution too")
	}
}
