package auditredact

import (
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog"
)

// Status names what happened to one tool-audit row on its way into
// tool_audit_log.
//
// A control has three answers — pass, fail, and I COULD NOT EVALUATE THIS — and
// this seam used to publish only the first two, by publishing nothing at all
// for a row it never scanned. `secret_redaction_audit` having no rows was
// therefore consistent with both "clean fleet" and "the decorator was never
// installed" (Finding D of docs/audits/2026-08-26-silent-controls-audit.md).
//
// Design of record:
// https://docs.vornik.io
type Status string

const (
	// StatusScanned — examined, nothing found.
	StatusScanned Status = "scanned"
	// StatusRedacted — examined, findings, the row was cleaned before persist.
	StatusRedacted Status = "redacted"
	// StatusDetectOnly — examined, findings, and the row was stored INTACT by
	// operator choice. Its own status rather than part of scanned because it is
	// the one non-skipped outcome that deliberately leaves a credential at
	// rest: "which rows may hold a live secret" is detect_only + skipped, and
	// that question is unanswerable if this folds into scanned.
	StatusDetectOnly Status = "detect_only"
	// StatusSkipped — NOT examined. The fail-open, always carrying a Reason.
	StatusSkipped Status = "skipped"
)

// Reason names WHY a row was written without being scanned.
//
// Enumerated rather than free-form for the same reason mcpGapReason is: the
// distinctions are the point. "The operator turned scanning off" is a
// deployment behaving as configured; "the detector failed to construct" is the
// daemon not honouring the config it was given. One aggregate count would
// answer neither.
type Reason string

const (
	// ReasonNone is the empty label carried by every non-skipped status.
	ReasonNone Reason = ""
	// ReasonSecretsDisabled — secrets.enabled=false. An operator decision.
	ReasonSecretsDisabled Reason = "secrets_disabled"
	// ReasonDetectorUnavailable — buildSecretsDetector returned an error or a
	// nil detector while secrets.enabled=true. A failure, and the reason most
	// worth alerting on.
	ReasonDetectorUnavailable Reason = "detector_unavailable"
	// ReasonDetectorNil — a Repo constructed with no detector by some path
	// other than the two above: CE builds, tests, and anything written later.
	ReasonDetectorNil Reason = "detector_nil"
)

// Metrics is the shared census for the tool-audit redaction seam.
//
// WHY A SHARED HOLDER RATHER THAN A COUNTER PER Repo. The container ends up
// with TWO live decorator instances on postgres: initScheduler (container.go
// :990) and initDispatcher (:1165) capture the Repo built at :834, while the
// post-observability rebuild at :1421 decorates fresh repo handles that only
// the re-run initHTTPServer (:1433) picks up. The executor's batch persist and
// the realtime POST handler therefore write through different instances for the
// life of the daemon. Per-instance counters would publish a denominator over
// roughly half the rows while presenting as complete — the exact defect this
// census exists to retire. One holder, injected by reference into every Repo,
// makes the denominator whole regardless of instance topology.
//
// WHY Attach RATHER THAN A REGISTERER AT CONSTRUCTION. The seam is wired at
// container.go:834; the observability registry does not exist until :1383. And
// registering on prometheus.DefaultRegisterer instead would make the series
// invisible: internal/observability/metrics.go:25 builds a bare
// prometheus.NewRegistry() that does not gather the default one. That is the
// 2026-06-06 invisible-metric incident recorded at container_http.go:551-571 —
// do not copy the package-level promauto shape from
// internal/mcp/block_notify.go:247 here.
type Metrics struct {
	// vec is read without the lock on the hot path; it is written exactly once,
	// under the lock, by Attach.
	vec atomic.Pointer[prometheus.CounterVec]

	mu      sync.Mutex
	pending map[[2]string]int64 // pre-attach tallies, drained by Attach

	// preAttachRows counts rows seen before the registry existed. Non-zero is
	// not a normal state — see Attach.
	preAttachRows int64
}

// NewMetrics builds the registry-less holder. Safe to share across goroutines
// and across Repo instances.
func NewMetrics() *Metrics {
	return &Metrics{pending: map[[2]string]int64{}}
}

// Attach registers the counter on the SERVED registry and drains anything
// counted before it existed. Idempotent: a second call is a no-op, which
// matters because initHTTPServer runs twice.
//
// A non-zero pre-attach tally is reported at WARN, naming the count. Rows reach
// this seam only over HTTP, and the listener starts after the observability
// phase, so the expected value is zero. If it is ever not zero, some writer now
// reaches tool_audit_log before the metrics exist — the late flush keeps the
// numbers honest, but the WARN is what says the ordering assumption broke,
// rather than letting the flush quietly paper over it.
func (m *Metrics) Attach(registerer prometheus.Registerer, logger *zerolog.Logger) {
	if m == nil || registerer == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vec.Load() != nil {
		return
	}
	vec := promauto.With(registerer).NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "vornik",
			Name:      "tool_audit_rows_total",
			Help: "Rows written to tool_audit_log, by {status, reason}. The SUM is the " +
				"coverage denominator: every row is counted whether or not it was scanned " +
				"for secrets, so zero-findings and zero-coverage are distinguishable. " +
				"status=skipped is a fail-open census, NOT a refusal count — those rows " +
				"were persisted unscanned, deliberately. status = scanned|redacted|" +
				"detect_only|skipped; reason (skipped only) = secrets_disabled|" +
				"detector_unavailable|detector_nil.",
		},
		[]string{"status", "reason"},
	)
	for labels, n := range m.pending {
		if n > 0 {
			vec.WithLabelValues(labels[0], labels[1]).Add(float64(n))
		}
	}
	if m.preAttachRows > 0 && logger != nil {
		logger.Warn().
			Int64("pre_attach_rows", m.preAttachRows).
			Msg("secrets: tool-audit rows were counted before the metrics registry existed — " +
				"a writer now reaches tool_audit_log before initHTTPServer; the counts were " +
				"flushed, but the boot ordering this seam assumes no longer holds")
	}
	m.pending = map[[2]string]int64{}
	m.vec.Store(vec)
}

// PreAttachRows reports how many rows were counted before the registry
// existed. Zero is the expected value: rows reach this seam only over HTTP, and
// the listener starts after the observability phase. Non-zero means some writer
// now reaches tool_audit_log earlier than that.
func (m *Metrics) PreAttachRows() int64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.preAttachRows
}

// record counts one row. Lock-free once attached; the pre-attach path takes the
// lock and re-checks, so a row cannot land in a tally that has already drained.
func (m *Metrics) record(status Status, reason Reason) {
	if m == nil {
		return
	}
	if vec := m.vec.Load(); vec != nil {
		vec.WithLabelValues(string(status), string(reason)).Inc()
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-check under the lock: Attach may have drained between the load above
	// and this point, and a row added to a drained tally would be lost.
	if vec := m.vec.Load(); vec != nil {
		vec.WithLabelValues(string(status), string(reason)).Inc()
		return
	}
	m.pending[[2]string{string(status), string(reason)}]++
	m.preAttachRows++
}
