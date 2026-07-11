package ui

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewInboxMetrics_RegistersOnGivenRegistry locks the metric name the
// design (§5.8) and scripts/lld-lint-allowlist.txt's now-removed
// [PLANNED] entry both name: vornik_ui_inbox_views_total. A CounterVec
// exposes no series in Gather() until at least one label combination has
// been observed, so this records one before gathering.
func TestNewInboxMetrics_RegistersOnGivenRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewInboxMetrics(reg)
	require.NotNil(t, m)
	m.RecordView("admin")

	mfs, err := reg.Gather()
	require.NoError(t, err)
	names := make(map[string]bool, len(mfs))
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	assert.True(t, names["vornik_ui_inbox_views_total"], "vornik_ui_inbox_views_total must be registered on the given registry")
}

func TestInboxMetrics_RecordView_IncrementsLabelledCounter(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewInboxMetrics(reg)
	m.RecordView("admin")
	m.RecordView("admin")
	m.RecordView("user")

	assert.Equal(t, float64(2), testutil.ToFloat64(m.ViewsTotal.WithLabelValues("admin")))
	assert.Equal(t, float64(1), testutil.ToFloat64(m.ViewsTotal.WithLabelValues("user")))
}

// TestInboxMetrics_RecordView_EmptyRoleNoop guards against a spurious
// empty-label series if a caller ever passes "" directly (inboxMetricsRole
// in inbox.go never does — it normalizes to "none" — but RecordView
// itself must not trust that).
func TestInboxMetrics_RecordView_EmptyRoleNoop(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewInboxMetrics(reg)
	m.RecordView("")
	assert.Equal(t, 0, testutil.CollectAndCount(m.ViewsTotal))
}

// --- nil-safety: RecordView must be a no-op on a nil *InboxMetrics, so
// Inbox() never needs a "s.inboxMetrics != nil" guard at the call site
// (idiom shared with IntegrationsMetrics/DeliverableMetrics). ---

func TestInboxMetrics_RecordView_NilReceiverNoPanic(t *testing.T) {
	var m *InboxMetrics
	assert.NotPanics(t, func() { m.RecordView("admin") })
}

// --- WithInboxMetrics ServerOption ---

func TestWithInboxMetrics_WiresMetrics(t *testing.T) {
	m := NewInboxMetrics(prometheus.NewRegistry())
	s := NewServer(WithInboxMetrics(m))
	assert.Same(t, m, s.inboxMetrics)
}

func TestWithInboxMetrics_NilIsHarmlessNoop(t *testing.T) {
	s := NewServer(WithInboxMetrics(nil))
	assert.Nil(t, s.inboxMetrics)
}
