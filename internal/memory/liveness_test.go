package memory

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestExtractURLs covers the happy-path regex behaviour: pull http/https
// URLs out of mixed content, dedupe, and strip trailing punctuation.
func TestExtractURLs(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "single bare url",
			content: "see https://example.com for details",
			want:    []string{"https://example.com"},
		},
		{
			name:    "trailing period stripped",
			content: "Job posting at https://example.com/jobs/42.",
			want:    []string{"https://example.com/jobs/42"},
		},
		{
			name:    "dedupe identical urls",
			content: "https://example.com first then https://example.com second",
			want:    []string{"https://example.com"},
		},
		{
			name:    "http and https kept distinct",
			content: "old http://x.test and new https://x.test",
			want:    []string{"http://x.test", "https://x.test"},
		},
		{
			name:    "markdown parens stripped",
			content: "[link](https://example.com/x)",
			want:    []string{"https://example.com/x"},
		},
		{
			name:    "no urls present",
			content: "plain prose with no links",
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractURLs(tc.content)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %v want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("index %d: got %q want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestCheckURL_StatusMapping pins the HEAD-status decision table.
func TestCheckURL_StatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		alive  bool
	}{
		{"200 alive", 200, true},
		{"301 alive (redirect)", 301, true},
		{"403 dead", 403, false},
		{"404 dead", 404, false},
		{"410 dead", 410, false},
		{"451 dead (legal)", 451, false},
		{"405 alive (head rejected)", 405, true},
		{"501 alive (not implemented)", 501, true},
		{"500 alive (upstream hiccup)", 500, true},
		{"503 alive (transient)", 503, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			checker := NewURLLivenessChecker(nil)
			checker.SetHTTPClient(srv.Client())
			checker.SetAllowPrivateNetworksForTest(true)
			checker.SetTimeout(2 * time.Second)
			alive := checker.checkURL(context.Background(), srv.URL)
			if alive != tc.alive {
				t.Fatalf("status %d: alive=%v want %v", tc.status, alive, tc.alive)
			}
		})
	}
}

// TestCheckURL_TransportErrorIsDead — DNS/connect failures must mark
// the URL dead so the chunk gets flagged.
func TestCheckURL_TransportErrorIsDead(t *testing.T) {
	checker := NewURLLivenessChecker(nil)
	checker.SetTimeout(500 * time.Millisecond)
	// 127.0.0.0/8 black-hole — connect refused.
	alive := checker.checkURL(context.Background(), "http://127.0.0.1:1") // port 1 won't have a listener
	if alive {
		t.Fatal("transport error must yield alive=false")
	}
}

// TestRecheckProject_FlagsDeadURL is the integration-level happy
// path: one chunk with a dead URL gets is_alive=false written. Uses
// sqlmock to verify the UPDATE fires with the right args.
func TestRecheckProject_FlagsDeadURL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	deadURL := srv.URL + "/posting/expired"

	// SELECT for the walker.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, content")).
		WithArgs("proj").
		WillReturnRows(sqlmock.NewRows([]string{"id", "content"}).
			AddRow("chunk-1", "see "+deadURL+" for the job"))
	// UPDATE for the chunk.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_memory_chunks SET is_alive")).
		WithArgs(false, sqlmock.AnyArg(), "chunk-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	repo := NewRepository(db)
	checker := NewURLLivenessChecker(repo)
	checker.SetHTTPClient(srv.Client())
	checker.SetAllowPrivateNetworksForTest(true)
	checker.SetMetrics(metrics)

	out, err := checker.RecheckProject(context.Background(), "proj", 0)
	if err != nil {
		t.Fatalf("RecheckProject: %v", err)
	}
	if out.URLsChecked != 1 || out.URLsDead != 1 || out.ChunksFlagged != 1 {
		t.Fatalf("expected 1 url checked / dead / chunk flagged, got %+v", out)
	}
	if got := testutil.ToFloat64(metrics.URLLivenessTotal.WithLabelValues("proj", "false")); got != 1 {
		t.Fatalf("URLLivenessTotal{alive=false} = %v, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestRecheckProject_AliveURLConfirmsChunk — the opposite branch:
// a 200 means is_alive=true.
func TestRecheckProject_AliveURLConfirmsChunk(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, content")).
		WithArgs("proj").
		WillReturnRows(sqlmock.NewRows([]string{"id", "content"}).
			AddRow("chunk-1", "fresh "+srv.URL+" link"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE project_memory_chunks SET is_alive")).
		WithArgs(true, sqlmock.AnyArg(), "chunk-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewRepository(db)
	checker := NewURLLivenessChecker(repo)
	checker.SetHTTPClient(srv.Client())
	checker.SetAllowPrivateNetworksForTest(true)

	out, err := checker.RecheckProject(context.Background(), "proj", 0)
	if err != nil {
		t.Fatalf("RecheckProject: %v", err)
	}
	if out.URLsAlive != 1 || out.ChunksConfirmed != 1 {
		t.Fatalf("expected 1 alive / 1 confirmed, got %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestCheckURL_RejectsPrivateNetworkTargets(t *testing.T) {
	checker := NewURLLivenessChecker(nil)
	for _, raw := range []string{
		"http://127.0.0.1:8080/secret",
		"http://localhost/admin",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/internal",
		"http://172.16.0.1/internal",
		"http://192.168.1.10/internal",
		"http://[::1]/internal",
	} {
		if checker.checkURL(context.Background(), raw) {
			t.Fatalf("private target %q must be rejected as dead", raw)
		}
	}
}

func TestValidateLivenessURL_RejectsPrivateDNSResolution(t *testing.T) {
	checker := NewURLLivenessChecker(nil)
	err := checker.validateLivenessURL(context.Background(), "http://localhost/path")
	if err == nil || !strings.Contains(err.Error(), "local hostname") {
		t.Fatalf("localhost should be rejected before request, got %v", err)
	}
}

func TestLivenessDialContext_RejectsPrivateDialAddress(t *testing.T) {
	checker := NewURLLivenessChecker(nil)
	_, err := checker.dialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil || !strings.Contains(err.Error(), "private IP") {
		t.Fatalf("private dial address should be rejected, got %v", err)
	}
}

// TestRecheckProject_ChunkWithoutURLsIsSkipped — chunks that contain
// no URL must NOT receive an UPDATE row (is_alive stays NULL so
// readers can tell apart "no URL" vs "dead URL").
func TestRecheckProject_ChunkWithoutURLsIsSkipped(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, content")).
		WithArgs("proj").
		WillReturnRows(sqlmock.NewRows([]string{"id", "content"}).
			AddRow("chunk-2", "prose with no urls in it"))
	// No UPDATE expected — sqlmock will fail ExpectationsWereMet if
	// the checker calls UPDATE for the no-URL chunk.

	repo := NewRepository(db)
	checker := NewURLLivenessChecker(repo)
	out, err := checker.RecheckProject(context.Background(), "proj", 0)
	if err != nil {
		t.Fatalf("RecheckProject: %v", err)
	}
	if out.ChunksScanned != 1 || out.ChunksWithURLs != 0 || out.URLsChecked != 0 {
		t.Fatalf("expected scanned=1 with_urls=0 checked=0, got %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestRecheckProject_RejectsEmptyProjectID — surface-level guard
// so an operator typo doesn't trigger a project-wide walk against
// an unscoped query.
func TestRecheckProject_RejectsEmptyProjectID(t *testing.T) {
	repo := &Repository{}
	checker := NewURLLivenessChecker(repo)
	_, err := checker.RecheckProject(context.Background(), "", 0)
	if err == nil {
		t.Fatal("expected error on empty projectID")
	}
}

// TestRecheckProject_NilCheckerErrors — defensive: nil receiver or
// nil repo must surface a real error, not a panic.
func TestRecheckProject_NilCheckerErrors(t *testing.T) {
	var c *URLLivenessChecker
	_, err := c.RecheckProject(context.Background(), "proj", 0)
	if err == nil {
		t.Fatal("expected error on nil checker")
	}
	c = &URLLivenessChecker{}
	_, err = c.RecheckProject(context.Background(), "proj", 0)
	if err == nil {
		t.Fatal("expected error on nil repo")
	}
}

// TestUpdateChunkLiveness_RequiresChunkID — sanity check for the
// repository helper. Empty chunk ID is operator error and must
// fail fast rather than fire a SQL UPDATE against the whole table.
func TestUpdateChunkLiveness_RequiresChunkID(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	r := NewRepository(db)
	if err := r.UpdateChunkLiveness(context.Background(), "", false, time.Now()); err == nil {
		t.Fatal("expected error on empty chunk_id")
	}
}

// TestListChunksForLivenessCheck_RequiresProjectID guards against
// the same operator-typo footgun on the read side.
func TestListChunksForLivenessCheck_RequiresProjectID(t *testing.T) {
	r := &Repository{}
	_, err := r.ListChunksForLivenessCheck(context.Background(), "", 0)
	if err == nil {
		t.Fatal("expected error on empty project_id")
	}
}

// TestRecheckProject_ContextCancelStopsWalk — long sweeps must
// respect ctx cancellation between chunks so an operator Ctrl+C
// (or shutdown signal) lands quickly.
func TestRecheckProject_ContextCancelStopsWalk(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	// SELECT returns 2 chunks but cancellation should stop after the
	// first row's URL HEAD.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, content")).
		WithArgs("proj").
		WillReturnRows(sqlmock.NewRows([]string{"id", "content"}).
			AddRow("chunk-1", "no url here").
			AddRow("chunk-2", "still no url"))

	repo := NewRepository(db)
	checker := NewURLLivenessChecker(repo)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = checker.RecheckProject(ctx, "proj", 0)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
