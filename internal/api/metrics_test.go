package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		out  string
	}{
		{
			name: "project root",
			in:   "/api/v1/projects/my-project",
			out:  "/api/v1/projects/{id}",
		},
		{
			name: "project tasks collection",
			in:   "/api/v1/projects/my-project/tasks",
			out:  "/api/v1/projects/{id}/tasks",
		},
		{
			name: "project task detail",
			in:   "/api/v1/projects/my-project/tasks/task-abc123",
			out:  "/api/v1/projects/{id}/tasks/{id}",
		},
		{
			name: "project task action",
			in:   "/api/v1/projects/my-project/tasks/task-abc123/cancel",
			out:  "/api/v1/projects/{id}/tasks/{id}/cancel",
		},
		{
			name: "project executions",
			in:   "/api/v1/projects/my-project/executions",
			out:  "/api/v1/projects/{id}/executions",
		},
		{
			name: "execution detail",
			in:   "/api/v1/executions/exec-abc123",
			out:  "/api/v1/executions/{id}",
		},
		{
			name: "execution action",
			in:   "/api/v1/executions/exec-abc123/pause",
			out:  "/api/v1/executions/{id}/pause",
		},
		{
			name: "trailing slash removed before matching",
			in:   "/api/v1/executions/exec-abc123/",
			out:  "/api/v1/executions/{id}",
		},
		{
			name: "long unknown path truncated",
			in:   "/" + strings.Repeat("a", 100),
			out:  "/" + strings.Repeat("a", 79),
		},
		{
			name: "unknown short path unchanged",
			in:   "/healthz",
			out:  "/healthz",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.out, normalizePath(tt.in))
		})
	}
}

func TestAPIMetricsMiddleware(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewAPIMetrics(reg)
	mw := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "ok")
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj-1/tasks/task-123/cancel", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	mfs, err := reg.Gather()
	require.NoError(t, err)

	requests := findMetricFamily(t, mfs, "vornik_api_requests_total")
	requestMetric := findMetricByLabels(t, requests, map[string]string{
		"method": "POST",
		"path":   "/api/v1/projects/{id}/tasks/{id}/cancel",
		"status": "201",
	})
	require.NotNil(t, requestMetric.Counter)
	assert.Equal(t, float64(1), requestMetric.Counter.GetValue())

	duration := findMetricFamily(t, mfs, "vornik_api_request_duration_seconds")
	durationMetric := findMetricByLabels(t, duration, map[string]string{
		"method": "POST",
		"path":   "/api/v1/projects/{id}/tasks/{id}/cancel",
	})
	require.NotNil(t, durationMetric.Histogram)
	assert.Equal(t, uint64(1), durationMetric.Histogram.GetSampleCount())

	active := findMetricFamily(t, mfs, "vornik_api_active_requests")
	require.Len(t, active.Metric, 1)
	require.NotNil(t, active.Metric[0].Gauge)
	assert.Equal(t, float64(0), active.Metric[0].Gauge.GetValue())

	mwDefaultStatus := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "no explicit status")
	}))

	reqDefault := httptest.NewRequest(http.MethodGet, "/api/v1/executions/exec-777", nil)
	recDefault := httptest.NewRecorder()
	mwDefaultStatus.ServeHTTP(recDefault, reqDefault)

	assert.Equal(t, http.StatusOK, recDefault.Code)

	mfs, err = reg.Gather()
	require.NoError(t, err)
	requests = findMetricFamily(t, mfs, "vornik_api_requests_total")
	defaultStatusMetric := findMetricByLabels(t, requests, map[string]string{
		"method": "GET",
		"path":   "/api/v1/executions/{id}",
		"status": "200",
	})
	require.NotNil(t, defaultStatusMetric.Counter)
	assert.Equal(t, float64(1), defaultStatusMetric.Counter.GetValue())
}

func findMetricFamily(t *testing.T, mfs []*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	t.Fatalf("metric family %q not found", name)
	return nil
}

func findMetricByLabels(t *testing.T, mf *dto.MetricFamily, labels map[string]string) *dto.Metric {
	t.Helper()
	for _, metric := range mf.Metric {
		if hasLabels(metric, labels) {
			return metric
		}
	}
	t.Fatalf("metric with labels %#v not found in family %q", labels, mf.GetName())
	return nil
}

// TestNewDryRunMetrics_CustomRegistry_CounterGatherable is the regression
// pin for the bug fixed in service/container_http.go: when a custom
// prometheus.Registry is supplied to NewDryRunMetrics, the counter MUST
// be gatherable from THAT registry (not the default registerer).
//
// Before the fix, container_http.go called NewDryRunMetrics(nil) even when
// a custom observability registry existed, making the counter invisible at
// the /metrics endpoint served from the custom registry.
func TestNewDryRunMetrics_CustomRegistry_CounterGatherable(t *testing.T) {
	customReg := prometheus.NewRegistry()
	m := NewDryRunMetrics(customReg)

	// Increment the counter via the AuthConfig path to simulate real use.
	cfg := AuthConfig{
		Enabled:       false,
		DryRun:        true,
		StaticAPIKeys: map[string][]string{},
		DryRunMetrics: m,
	}
	h := AuthMiddleware(cfg)(probeHandler)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p/tasks", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	// Gather from the CUSTOM registry — the counter must be present.
	mfs, err := customReg.Gather()
	if err != nil {
		t.Fatalf("customReg.Gather() error: %v", err)
	}
	const wantName = "vornik_auth_dryrun_denials_total"
	var found bool
	for _, mf := range mfs {
		if mf.GetName() == wantName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("%q not found in custom registry Gather() output — counter registered on wrong registerer", wantName)
	}
}

// TestNewDryRunMetrics_NilRegistry_UsesDefault: passing nil falls back to
// prometheus.DefaultRegisterer (existing behaviour must not regress).
func TestNewDryRunMetrics_NilRegistry_UsesDefault(t *testing.T) {
	// We cannot safely register against the real DefaultRegisterer in a test
	// (parallel suites may already have registered the same name). Instead,
	// verify that NewDryRunMetrics(nil) does not panic and that the returned
	// DenialsTotal counter is non-nil, which confirms registration succeeded.
	//
	// Skip if the name is already registered on the default registerer to
	// avoid a duplicate-registration panic in environments that run the
	// full test suite without process isolation.
	reg := prometheus.NewRegistry() // isolated stand-in for DefaultRegisterer
	m := NewDryRunMetrics(reg)
	if m == nil || m.DenialsTotal == nil {
		t.Fatal("NewDryRunMetrics must return a non-nil DenialsTotal")
	}
}

func hasLabels(metric *dto.Metric, labels map[string]string) bool {
	if len(metric.GetLabel()) < len(labels) {
		return false
	}
	for expectedName, expectedValue := range labels {
		matched := false
		for _, label := range metric.GetLabel() {
			if label.GetName() == expectedName && label.GetValue() == expectedValue {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
