package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
)

// INCIDENT 2026-07-30, customer deployment. Six memory-pipeline models were failing 100%
// of their calls for hours — roughly 500 `context deadline exceeded` per hour — and
// `vornikctl doctor` reported "all 11 role-pinned model(s) healthy". The one check an
// operator trusts said everything was fine while memory ingestion was completely stalled.
//
// Root cause of the BLIND SPOT (distinct from the incident's own cause, which was
// congestion): model_health enumerates models pinned by swarm ROLES and reads
// execution_step_outcomes + task_llm_usage. The memory workers are daemon-level config,
// not swarm roles, so they were never enumerated — and task_llm_usage is a spend table
// with no error column, so a timeout writes nothing at all. There was no queryable
// record that those calls were failing.
//
// CallStats closes that: LoggingProvider already wraps every provider and already sees
// model, call_site, latency and error on every call. It just threw the outcome away.
func TestCallStats_RecordsFailuresPerModelAndCallSite(t *testing.T) {
	s := NewCallStats()

	s.Record("gpt-oss-20b", "memory.classifier", errors.New("context deadline exceeded"))
	s.Record("gpt-oss-20b", "memory.classifier", errors.New("context deadline exceeded"))
	s.Record("gpt-oss-20b", "memory.classifier", nil)
	s.Record("gpt-oss-20b", "dispatcher", nil)

	snap := s.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot entries = %d, want 2 — (model, call_site) is the key", len(snap))
	}

	var classifier CallStat
	for _, e := range snap {
		if e.CallSite == "memory.classifier" {
			classifier = e
		}
	}
	if classifier.Calls != 3 || classifier.Failures != 2 {
		t.Fatalf("classifier stat = %+v, want 3 calls / 2 failures", classifier)
	}
	if classifier.Model != "gpt-oss-20b" {
		t.Errorf("model = %q", classifier.Model)
	}
	if classifier.FailureRate() < 0.66 || classifier.FailureRate() > 0.67 {
		t.Errorf("failure rate = %v, want ~0.667", classifier.FailureRate())
	}
	if classifier.LastError == "" {
		t.Error("the last error text is what tells an operator WHY; it must be retained")
	}
}

// A cancelled caller context is teardown — config reload, shutdown — not a model
// failure. LoggingProvider already makes that distinction for logging; the stats must
// make the same one or every restart would look like an outage.
func TestCallStats_CancellationIsNotAFailure(t *testing.T) {
	s := NewCallStats()
	s.Record("m", "site", context.Canceled)
	s.Record("m", "site", nil)

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("entries = %d, want 1", len(snap))
	}
	if snap[0].Failures != 0 {
		t.Errorf("failures = %d, want 0 — context.Canceled is teardown, not an outage",
			snap[0].Failures)
	}
	if snap[0].Calls != 2 {
		t.Errorf("calls = %d, want 2 — a cancelled call still happened", snap[0].Calls)
	}
}

// A deadline exceeded IS a failure — that is the customer's exact error, and the whole
// reason this type exists.
func TestCallStats_DeadlineExceededIsAFailure(t *testing.T) {
	s := NewCallStats()
	s.Record("m", "memory.titler", context.DeadlineExceeded)
	if snap := s.Snapshot(); len(snap) != 1 || snap[0].Failures != 1 {
		t.Fatalf("snapshot = %+v, want one entry with 1 failure", snap)
	}
}

// Unbounded growth would be a slow leak on a deployment with many models: the map is
// keyed by (model, call_site), both of which are bounded in practice, but a misbehaving
// caller could still inject unbounded call sites.
func TestCallStats_BoundsItsCardinality(t *testing.T) {
	s := NewCallStats()
	for i := 0; i < callStatsMaxEntries+50; i++ {
		s.Record("m", string(rune('a'+i%26))+string(rune('a'+i/26)), errors.New("x"))
	}
	if got := len(s.Snapshot()); got > callStatsMaxEntries {
		t.Fatalf("entries = %d, want <= %d", got, callStatsMaxEntries)
	}
}

// Nil receiver must be safe: the sink is optional and every call site passes it
// unconditionally.
func TestCallStats_NilIsSafe(t *testing.T) {
	var s *CallStats
	s.Record("m", "s", errors.New("x"))
	if snap := s.Snapshot(); snap != nil {
		t.Errorf("nil snapshot = %+v, want nil", snap)
	}
}

// The provider wrapper is where this has to happen: it is the one place every model call
// already passes through, so no future call site can forget to report.
func TestLoggingProvider_RecordsIntoCallStats(t *testing.T) {
	stats := NewCallStats()
	inner := &stubProvider{err: context.DeadlineExceeded, model: "gpt-oss-20b"}
	p := NewLoggingProviderWithStats(inner, testLogger(), stats)

	ctx := WithCallSite(context.Background(), "memory.classifier")
	_, _ = p.Complete(ctx, []Message{{Role: "user", Content: "hi"}})

	snap := stats.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("entries = %d, want 1 — the wrapper must report every call", len(snap))
	}
	if snap[0].Failures != 1 || snap[0].CallSite != "memory.classifier" || snap[0].Model != "gpt-oss-20b" {
		t.Fatalf("stat = %+v, want 1 failure attributed to memory.classifier/gpt-oss-20b", snap[0])
	}
}

// --- test doubles -----------------------------------------------------------

type stubProvider struct {
	model string
	err   error
	resp  *ChatResponse
}

func (s *stubProvider) Complete(context.Context, []Message) (*ChatResponse, error) {
	return s.resp, s.err
}

func (s *stubProvider) CompleteWithTools(context.Context, []Message, []Tool) (*ChatResponse, error) {
	return s.resp, s.err
}

func (s *stubProvider) CompleteWithToolsStream(context.Context, []Message, []Tool, StreamCallback) (*ChatResponse, error) {
	return s.resp, s.err
}

func (s *stubProvider) Model() string       { return s.model }
func (s *stubProvider) SetMetrics(*Metrics) {}

func testLogger() zerolog.Logger { return zerolog.Nop() }
