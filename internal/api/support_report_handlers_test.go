package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// supportTaskRepoStub satisfies the full persistence.TaskRepository
// (so it can be passed to WithTaskRepository) by embedding the
// interface — only Get + List are implemented; the rest panic if a
// handler path ever calls them (it doesn't for support-report).
type supportTaskRepoStub struct {
	persistence.TaskRepository
	task *persistence.Task
}

func (s supportTaskRepoStub) Get(_ context.Context, id string) (*persistence.Task, error) {
	if s.task != nil && s.task.ID == id {
		return s.task, nil
	}
	return nil, persistence.ErrNotFound
}
func (s supportTaskRepoStub) List(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
	if s.task == nil {
		return nil, nil
	}
	return []*persistence.Task{s.task}, nil
}

func newSupportTestServer(t *testing.T, taskProject string) *Server {
	t.Helper()
	det, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	return NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithTaskRepository(supportTaskRepoStub{
			task: &persistence.Task{ID: "t1", ProjectID: taskProject, CreatedAt: now, UpdatedAt: now},
		}),
		WithSecrets(det, secrets.DefaultCheckpoints()),
	)
}

// withProjectScopeContext stamps an admin key AND a project-scope
// restriction — i.e. a project-scoped admin key.
func withProjectScopeContext(r *http.Request, key string, projects []string) *http.Request {
	ctx := context.WithValue(r.Context(), apiKeyKey, key)
	ctx = context.WithValue(ctx, projectIDKey, projects)
	return r.WithContext(ctx)
}

func TestSupportReport_NonAdmin403(t *testing.T) {
	s := newSupportTestServer(t, "p1")
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost, "/api/v1/support-report", strings.NewReader(`{"task_id":"t1"}`)),
		"sk-user")
	rec := httptest.NewRecorder()
	s.SupportReport(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestSupportReport_MethodNotAllowed(t *testing.T) {
	s := newSupportTestServer(t, "p1")
	req := withAdminKeyContext(httptest.NewRequest(http.MethodGet, "/api/v1/support-report", nil), "sk-admin")
	rec := httptest.NewRecorder()
	s.SupportReport(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
}

func TestSupportReport_XORValidation(t *testing.T) {
	s := newSupportTestServer(t, "p1")
	for _, body := range []string{`{}`, `{"task_id":"t1","since":"2h"}`} {
		req := withAdminKeyContext(
			httptest.NewRequest(http.MethodPost, "/api/v1/support-report", strings.NewReader(body)), "sk-admin")
		rec := httptest.NewRecorder()
		s.SupportReport(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: want 400, got %d", body, rec.Code)
		}
	}
}

func TestSupportReport_CrossProjectTask404(t *testing.T) {
	// Task lives in "secret-proj"; caller is scoped to "other-proj".
	s := newSupportTestServer(t, "secret-proj")
	req := withProjectScopeContext(
		httptest.NewRequest(http.MethodPost, "/api/v1/support-report", strings.NewReader(`{"task_id":"t1"}`)),
		"sk-admin", []string{"other-proj"})
	rec := httptest.NewRecorder()
	s.SupportReport(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-project task: want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestSupportReport_WindowRequiresGlobalAdmin(t *testing.T) {
	s := newSupportTestServer(t, "p1")
	// Project-scoped admin key → window mode refused.
	req := withProjectScopeContext(
		httptest.NewRequest(http.MethodPost, "/api/v1/support-report", strings.NewReader(`{"since":"2h"}`)),
		"sk-admin", []string{"p1"})
	rec := httptest.NewRecorder()
	s.SupportReport(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("window+project-key: want 403, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "global admin") {
		t.Errorf("expected global-admin error, got %q", rec.Body.String())
	}
}

// extractBundle reads a gzip tarball response into a name->bytes map.
func extractBundle(t *testing.T, body []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip: %v (len=%d)", err, len(body))
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, _ := io.ReadAll(tr)
		out[hdr.Name] = data
	}
	return out
}

func TestSupportReport_TaskBundleHappyPath(t *testing.T) {
	s := newSupportTestServer(t, "p1")
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost, "/api/v1/support-report", strings.NewReader(`{"task_id":"t1"}`)),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.SupportReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("content-type = %q", ct)
	}
	files := extractBundle(t, rec.Body.Bytes())
	for _, want := range []string{"MANIFEST.json", "REDACTION.txt", "task/task.json", "version.txt"} {
		if _, ok := files[want]; !ok {
			t.Errorf("bundle missing %s; have %v", want, keysOf(files))
		}
	}
}

func TestSupportReport_WindowBundleGlobalAdmin(t *testing.T) {
	s := newSupportTestServer(t, "p1")
	// Global admin (no project scope stamped).
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost, "/api/v1/support-report", strings.NewReader(`{"since":"24h"}`)),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.SupportReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	files := extractBundle(t, rec.Body.Bytes())
	if _, ok := files["window/tasks.json"]; !ok {
		t.Errorf("window bundle missing window/tasks.json; have %v", keysOf(files))
	}
}

func TestSupportReport_BadWindow(t *testing.T) {
	s := newSupportTestServer(t, "p1")
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost, "/api/v1/support-report", strings.NewReader(`{"since":"not-a-time"}`)),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.SupportReport(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad window: want 400, got %d", rec.Code)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestParseTimeOrDuration(t *testing.T) {
	ref := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	got, err := parseTimeOrDuration("2h", ref)
	if err != nil || !got.Equal(ref.Add(-2*time.Hour)) {
		t.Fatalf("duration: %v %v", got, err)
	}
	got, err = parseTimeOrDuration("2026-06-20T10:00:00Z", ref)
	if err != nil || got.Hour() != 10 {
		t.Fatalf("rfc3339: %v %v", got, err)
	}
	if _, err := parseTimeOrDuration("garbage", ref); err == nil {
		t.Fatal("garbage should error")
	}
}

func TestParseWindow_UntilBeforeSince(t *testing.T) {
	_, _, err := parseWindow("2026-06-20T12:00:00Z", "2026-06-20T10:00:00Z")
	if err == nil {
		t.Fatal("until<since should error")
	}
}

// stubLogSource + stubOpener satisfy the Server's TaskLogSource +
// ArtifactOpener so the handler's container-log injection + the opener
// adapter are exercised.
type stubLogSource struct{ secret string }

func (s stubLogSource) TaskLogs(_ context.Context, _ string, _ int) (string, error) {
	return "container stderr token=" + s.secret + "\n", nil
}

type stubOpener struct{ data []byte }

func (s stubOpener) Open(_ context.Context, _ string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

func TestSupportReport_ContainerLogsAndMetrics(t *testing.T) {
	det, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	logSecret := "sk-CONTAINERLOGSECRET0000000000000000000000000000"
	reg := prometheusNewRegistry()
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithTaskRepository(supportTaskRepoStub{task: &persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: now, UpdatedAt: now}}),
		WithSecrets(det, secrets.DefaultCheckpoints()),
		WithTaskLogSource(stubLogSource{secret: logSecret}),
		WithMetricsRegistry(reg),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost, "/api/v1/support-report", strings.NewReader(`{"task_id":"t1"}`)),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.SupportReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	files := extractBundle(t, rec.Body.Bytes())
	cl, ok := files["task/container_logs.txt"]
	if !ok {
		t.Fatalf("missing container_logs.txt; have %v", keysOf(files))
	}
	if bytes.Contains(cl, []byte(logSecret)) {
		t.Errorf("container log secret leaked:\n%s", cl)
	}
	if !bytes.Contains(cl, []byte("[REDACTED:")) {
		t.Errorf("container log not redacted:\n%s", cl)
	}
}

func TestSupportArtifactOpenerAdapter(t *testing.T) {
	a := supportArtifactOpenerAdapter{o: stubOpener{data: []byte("hi")}}
	rc, err := a.Open(context.Background(), "x")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != "hi" {
		t.Errorf("adapter read = %q", got)
	}
}

func prometheusNewRegistry() *prometheus.Registry { return prometheus.NewRegistry() }

func TestSupportReport_TaskRepoNotWired(t *testing.T) {
	det, _ := secrets.NewMultiDetector(secrets.Config{})
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithSecrets(det, secrets.DefaultCheckpoints()),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodPost, "/api/v1/support-report", strings.NewReader(`{"task_id":"t1"}`)),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.SupportReport(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no task repo: want 503, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestSanitizeForFilename(t *testing.T) {
	if got := sanitizeForFilename(""); got != "bundle" {
		t.Errorf("empty -> %q", got)
	}
	if got := sanitizeForFilename("a/../b"); strings.ContainsAny(got, "/") || strings.Contains(got, "..") {
		t.Errorf("unsafe -> %q", got)
	}
}

func TestRedactedConfigYAML_MapInput(t *testing.T) {
	// A plain map exercises the marshal path without a full Config.
	yml, err := redactedConfigYAML(map[string]any{"password": "hunter2", "name": "ok"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Contains(yml, "hunter2") {
		t.Errorf("password leaked:\n%s", yml)
	}
}
