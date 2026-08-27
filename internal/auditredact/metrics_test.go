package auditredact

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// These tests cover findings C and D of the 2026-08-26 silent-controls audit:
// a deliberate fail-open that is documented but not counted, and a control
// whose coverage boundary is not published so "zero findings" and "zero
// coverage" render identically. Design of record:
// https://docs.vornik.io

// seriesValue reads one {status,reason} child of vornik_tool_audit_rows_total
// off the registry. Gathering rather than reaching into the CounterVec keeps
// the assertion on the surface an operator actually scrapes.
func seriesValue(t *testing.T, reg *prometheus.Registry, status, reason string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "vornik_tool_audit_rows_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			var gotStatus, gotReason string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "status":
					gotStatus = l.GetValue()
				case "reason":
					gotReason = l.GetValue()
				}
			}
			if gotStatus == status && gotReason == reason {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func attached(t *testing.T) (*Metrics, *prometheus.Registry) {
	t.Helper()
	m := NewMetrics()
	reg := prometheus.NewRegistry()
	m.Attach(reg, nil)
	return m, reg
}

// G1: every row that reaches tool_audit_log is counted, whether or not it was
// scanned. The sum over statuses IS the denominator.
func TestLogCountsEveryRow(t *testing.T) {
	m, reg := attached(t)
	inner := newConflictRepo()
	r := New(inner, detector(t), nil, nil, nil, nil)
	r.SetMetrics(m)

	if err := r.Log(context.Background(), entry("a", "web_fetch", "in", "clean output")); err != nil {
		t.Fatalf("log clean: %v", err)
	}
	if err := r.Log(context.Background(), entry("b", "web_fetch", "in", "key "+syntheticKey)); err != nil {
		t.Fatalf("log dirty: %v", err)
	}

	if got := seriesValue(t, reg, "scanned", ""); got != 1 {
		t.Errorf("scanned = %v, want 1", got)
	}
	if got := seriesValue(t, reg, "redacted", ""); got != 1 {
		t.Errorf("redacted = %v, want 1", got)
	}
}

// D2: a detect-only row is stored INTACT, so it must not hide inside
// "scanned". An operator asking which rows may hold a live credential needs
// detect_only + skipped, and cannot get it if detect_only is invisible.
func TestDetectOnlyIsItsOwnStatus(t *testing.T) {
	m, reg := attached(t)
	inner := newConflictRepo()
	actions := map[string]secrets.Action{secrets.CheckpointToolAudit: secrets.ActionDetect}
	r := New(inner, detector(t), actions, nil, nil, nil)
	r.SetMetrics(m)

	if err := r.Log(context.Background(), entry("a", "web_fetch", "in", "key "+syntheticKey)); err != nil {
		t.Fatalf("log: %v", err)
	}
	if got := seriesValue(t, reg, "detect_only", ""); got != 1 {
		t.Errorf("detect_only = %v, want 1", got)
	}
	if got := seriesValue(t, reg, "scanned", ""); got != 0 {
		t.Errorf("scanned = %v, want 0 — detect_only must not be folded in", got)
	}
}

// Finding C: a bypass is a decorator carrying a reason, never an absent
// decorator. G5: the inner repo must receive the entry byte-identical.
func TestBypassDelegatesAndCounts(t *testing.T) {
	for _, reason := range []Reason{ReasonSecretsDisabled, ReasonDetectorUnavailable, ReasonDetectorNil} {
		t.Run(string(reason), func(t *testing.T) {
			m, reg := attached(t)
			inner := newConflictRepo()
			r := NewBypassed(inner, reason, nil)
			r.SetMetrics(m)

			in := entry("a", "web_fetch", "input "+syntheticKey, "output "+syntheticKey)
			if err := r.Log(context.Background(), in); err != nil {
				t.Fatalf("log: %v", err)
			}

			stored := inner.rows["a"]
			if stored == nil {
				t.Fatal("bypass did not delegate the write")
			}
			if stored.ToolInput != in.ToolInput || stored.ToolOutput != in.ToolOutput {
				t.Error("bypass altered the entry; G5 forbids any stored-byte change")
			}
			if got := seriesValue(t, reg, "skipped", string(reason)); got != 1 {
				t.Errorf("skipped{%s} = %v, want 1", reason, got)
			}
		})
	}
}

// D3 fact 3: the postgres rebuild leaves TWO live decorator instances — the
// scheduler and dispatcher hold the one built at container.go:834, the API
// server the one built at :1421. Per-instance metrics would publish a
// denominator over roughly half the rows while presenting as complete, which
// is the very defect this instrumentation exists to retire.
func TestSharedHolderAggregatesInstances(t *testing.T) {
	m, reg := attached(t)
	instanceA := New(newConflictRepo(), detector(t), nil, nil, nil, nil)
	instanceA.SetMetrics(m)
	instanceB := New(newConflictRepo(), detector(t), nil, nil, nil, nil)
	instanceB.SetMetrics(m)

	if err := instanceA.Log(context.Background(), entry("a", "web_fetch", "in", "clean")); err != nil {
		t.Fatalf("A: %v", err)
	}
	if err := instanceB.Log(context.Background(), entry("b", "web_fetch", "in", "clean")); err != nil {
		t.Fatalf("B: %v", err)
	}

	if got := seriesValue(t, reg, "scanned", ""); got != 2 {
		t.Errorf("scanned = %v, want 2 (the SUM over both writer paths)", got)
	}
}

// D3 / round-1 F2: the holder is built before the registry exists, so counts
// taken pre-attach must appear rather than vanish — and the fact that any were
// taken is itself alertable, so Attach WARNs naming the count.
func TestAttachFlushesPreAttachTally(t *testing.T) {
	m := NewMetrics()
	r := New(newConflictRepo(), detector(t), nil, nil, nil, nil)
	r.SetMetrics(m)

	for _, id := range []string{"a", "b", "c"} {
		if err := r.Log(context.Background(), entry(id, "web_fetch", "in", "clean")); err != nil {
			t.Fatalf("log %s: %v", id, err)
		}
	}

	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	reg := prometheus.NewRegistry()
	m.Attach(reg, &logger)

	if got := seriesValue(t, reg, "scanned", ""); got != 3 {
		t.Errorf("scanned = %v, want 3 flushed from the pre-attach tally", got)
	}
	if !bytes.Contains(buf.Bytes(), []byte("pre_attach_rows")) {
		t.Errorf("Attach did not WARN about pre-attach rows; log was %q", buf.String())
	}
}

func TestAttachIsIdempotent(t *testing.T) {
	m := NewMetrics()
	reg := prometheus.NewRegistry()
	m.Attach(reg, nil)
	m.Attach(reg, nil) // must not panic on duplicate registration

	r := New(newConflictRepo(), detector(t), nil, nil, nil, nil)
	r.SetMetrics(m)
	if err := r.Log(context.Background(), entry("a", "web_fetch", "in", "clean")); err != nil {
		t.Fatalf("log: %v", err)
	}
	if got := seriesValue(t, reg, "scanned", ""); got != 1 {
		t.Errorf("scanned = %v, want 1 — a second Attach must not double-count", got)
	}
}

// A Repo with no holder must log normally: CE paths and direct-construction
// tests never attach one.
func TestNilMetricsIsSafe(t *testing.T) {
	inner := newConflictRepo()
	r := New(inner, detector(t), nil, nil, nil, nil)
	if err := r.Log(context.Background(), entry("a", "web_fetch", "in", "clean")); err != nil {
		t.Fatalf("log with nil metrics: %v", err)
	}
	if inner.rows["a"] == nil {
		t.Error("row was not stored")
	}
	var nilHolder *Metrics
	nilHolder.record(StatusScanned, ReasonNone) // must not panic
}

// The bypass must not be mistaken for a wired seam by the container's
// idempotency guard, which has to be able to upgrade a detector_nil wrapper.
func TestBypassReasonIsReadable(t *testing.T) {
	r := NewBypassed(newConflictRepo(), ReasonDetectorNil, nil)
	if r.BypassReason() != ReasonDetectorNil {
		t.Errorf("BypassReason() = %q, want %q", r.BypassReason(), ReasonDetectorNil)
	}
	wired := New(newConflictRepo(), detector(t), nil, nil, nil, nil)
	if wired.BypassReason() != ReasonNone {
		t.Errorf("a wired Repo reports reason %q, want empty", wired.BypassReason())
	}
}

// A nil entry is not a row. Counting it would inflate the denominator with
// writes that never happened — the same species of wrong this census fixes.
func TestNilEntryIsNotCounted(t *testing.T) {
	m, reg := attached(t)
	// A nil-tolerant inner: conflictRepo dereferences e.ID, and passing nil
	// through to the inner repo is pre-existing behaviour this change does not
	// touch. What is under test is only whether the census counts it.
	r := NewBypassed(&nilTolerantRepo{}, ReasonSecretsDisabled, nil)
	r.SetMetrics(m)

	if err := r.Log(context.Background(), nil); err != nil {
		t.Fatalf("log nil: %v", err)
	}
	if got := seriesValue(t, reg, "skipped", string(ReasonSecretsDisabled)); got != 0 {
		t.Errorf("skipped = %v, want 0 — a nil entry is not a row", got)
	}
}

// The holder is written by every writer path and attached once, mid-boot, from
// another goroutine's point of view. Code review 2026-08-27 (review-20260827-6ec8)
// asked for this under -race: the fast path reads m.vec without the lock, so a
// row recorded while Attach is draining must land in exactly one of the two
// places, never neither.
func TestRecordConcurrentWithAttach(t *testing.T) {
	const writers = 200
	m := NewMetrics()
	reg := prometheus.NewRegistry()

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			<-start
			m.record(StatusScanned, ReasonNone)
		}()
	}

	var attachWG sync.WaitGroup
	attachWG.Add(1)
	go func() {
		defer attachWG.Done()
		<-start
		m.Attach(reg, nil)
	}()

	close(start)
	wg.Wait()
	attachWG.Wait()

	// Attach may have completed before, during or after any given record. Every
	// row must still be counted exactly once: the ones that beat it flush from
	// the tally, the ones that lost it increment the counter directly.
	if got := seriesValue(t, reg, "scanned", ""); got != writers {
		t.Errorf("scanned = %v, want %d — a row was lost or double-counted in the attach race", got, writers)
	}
}

type nilTolerantRepo struct{ conflictRepo }

func (r *nilTolerantRepo) Log(context.Context, *persistence.ToolAuditEntry) error { return nil }

var _ persistence.ToolAuditRepository = (*Repo)(nil)
