// Tests for the Runner — verifies that an extraction is persisted
// end-to-end (sections on disk, metadata.json + outline.json next
// to them, and the extracted_documents row Upserted).
package extractor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"vornik.io/vornik/internal/persistence"
)

// fakeRepo captures Upsert calls so the test can assert on the row
// shape without standing up a real DB. Implements only the methods
// Runner.Run actually uses.
type fakeRepo struct {
	mu      sync.Mutex
	upserts []*persistence.ExtractedDocument
	err     error
}

func (f *fakeRepo) Upsert(_ context.Context, d *persistence.ExtractedDocument) error {
	if f.err != nil {
		return f.err
	}
	// Defensive copy — Runner.Run shouldn't mutate after Upsert, but
	// the assertion targets are the values at write time. Guard the
	// slice append: the concurrency tests drive parallel Run calls.
	clone := *d
	f.mu.Lock()
	f.upserts = append(f.upserts, &clone)
	f.mu.Unlock()
	return nil
}
func (*fakeRepo) Get(context.Context, string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}
func (*fakeRepo) GetByArtifact(context.Context, string) (*persistence.ExtractedDocument, error) {
	return nil, nil
}
func (*fakeRepo) ListByProject(context.Context, string, int) ([]*persistence.ExtractedDocument, error) {
	return nil, nil
}
func (*fakeRepo) Delete(context.Context, string) error { return nil }

// canned implements Extractor — no real parsing, just returns the
// preset Result. Lets the Runner test focus on persistence, not
// parser correctness (the epub package has its own tests for that).
type canned struct {
	name    string
	version string
	res     Result
	err     error
}

func (c *canned) Name() string                                    { return c.name }
func (c *canned) Version() string                                 { return c.version }
func (c *canned) Extract(context.Context, Source) (Result, error) { return c.res, c.err }

func TestRunner_PersistsRowAndFiles(t *testing.T) {
	repo := &fakeRepo{}
	base := t.TempDir()
	clock := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	r := &Runner{
		Repo:     repo,
		BasePath: base,
		Clock:    func() time.Time { return clock },
		IDMint:   func() string { return "extdoc_test_1" },
	}
	ext := &canned{
		name:    "vornik-extract-test",
		version: "1.0.0",
		res: Result{
			Metadata: Metadata{Title: "Test", Author: "Author"},
			Outline: []OutlineEntry{
				{SectionID: "001-intro", Title: "Intro", TextBytes: 11},
				{SectionID: "002-body", Title: "Body", TextBytes: 9},
			},
			Sections: []Section{
				{SectionID: "001-intro", Title: "Intro", Content: "hello world"},
				{SectionID: "002-body", Title: "Body", Content: "good\nday."},
			},
		},
	}

	row, err := r.Run(context.Background(), "assistant", "art-1", ext, Source{
		FilePath: "/dev/null", // canned extractor ignores it
		MimeType: "application/epub+zip",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if row == nil || row.ID != "extdoc_test_1" {
		t.Fatalf("row ID = %v; want extdoc_test_1", row)
	}
	if row.SectionCount != 2 {
		t.Errorf("SectionCount = %d; want 2", row.SectionCount)
	}
	if row.TotalTextBytes != int64(len("hello world")+len("good\nday.")) {
		t.Errorf("TotalTextBytes = %d; want %d", row.TotalTextBytes, len("hello world")+len("good\nday."))
	}
	if row.Status != persistence.ExtractedDocumentStatusOK {
		t.Errorf("Status = %q; want OK", row.Status)
	}
	if row.ExtractorName != "vornik-extract-test" || row.ExtractorVersion != "1.0.0" {
		t.Errorf("extractor identity = %q/%q", row.ExtractorName, row.ExtractorVersion)
	}

	// On-disk layout.
	storageDir := filepath.Join(base, "assistant", "extracted", "extdoc_test_1")
	if row.StoragePath != storageDir {
		t.Errorf("StoragePath = %q; want %q", row.StoragePath, storageDir)
	}
	for _, want := range []string{
		filepath.Join(storageDir, "metadata.json"),
		filepath.Join(storageDir, "outline.json"),
		filepath.Join(storageDir, "sections", "001-intro.md"),
		filepath.Join(storageDir, "sections", "002-body.md"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("expected %q to exist: %v", want, err)
		}
	}

	// Section bytes round-trip.
	body, err := ReadSection(row, "001-intro")
	if err != nil {
		t.Fatalf("ReadSection: %v", err)
	}
	if body != "hello world" {
		t.Errorf("section content = %q; want %q", body, "hello world")
	}

	// metadata.json round-trip.
	var m Metadata
	mb, err := os.ReadFile(filepath.Join(storageDir, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if m.Title != "Test" || m.Author != "Author" {
		t.Errorf("metadata round-trip lost fields: %+v", m)
	}

	// One Upsert recorded.
	if len(repo.upserts) != 1 {
		t.Errorf("Upsert called %d times; want 1", len(repo.upserts))
	}
}

func TestRunner_ExtractError_ReturnsErrorNoUpsert(t *testing.T) {
	repo := &fakeRepo{}
	r := &Runner{Repo: repo, BasePath: t.TempDir()}
	ext := &canned{name: "x", version: "1", err: errors.New("kaboom")}
	_, err := r.Run(context.Background(), "p", "a", ext, Source{FilePath: "/dev/null"})
	if err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("expected wrapped extract error; got %v", err)
	}
	if len(repo.upserts) != 0 {
		t.Errorf("Upsert should NOT fire on extract error; got %d calls", len(repo.upserts))
	}
}

func TestRunner_ZeroSections_RejectedBeforePersist(t *testing.T) {
	repo := &fakeRepo{}
	r := &Runner{Repo: repo, BasePath: t.TempDir()}
	ext := &canned{name: "x", version: "1", res: Result{Metadata: Metadata{Title: "Empty"}}}
	_, err := r.Run(context.Background(), "p", "a", ext, Source{FilePath: "/dev/null"})
	if err == nil || !strings.Contains(err.Error(), "zero sections") {
		t.Errorf("expected zero-sections error; got %v", err)
	}
	if len(repo.upserts) != 0 {
		t.Errorf("Upsert should NOT fire on empty extraction; got %d calls", len(repo.upserts))
	}
}

func TestRunner_UpsertError_Propagates(t *testing.T) {
	repo := &fakeRepo{err: errors.New("db down")}
	r := &Runner{Repo: repo, BasePath: t.TempDir()}
	ext := &canned{
		name: "x", version: "1",
		res: Result{
			Sections: []Section{{SectionID: "001", Title: "t", Content: "c"}},
			Outline:  []OutlineEntry{{SectionID: "001", Title: "t", TextBytes: 1}},
		},
	}
	_, err := r.Run(context.Background(), "p", "a", ext, Source{FilePath: "/dev/null"})
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Errorf("expected upsert error to surface; got %v", err)
	}
}

func TestRunner_Metrics_SuccessAndErrorOutcomes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	base := t.TempDir()
	clock := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	r := &Runner{
		Repo:     &fakeRepo{},
		BasePath: base,
		Clock:    func() time.Time { return clock },
		IDMint:   func() string { return "extdoc_m_1" },
		Metrics:  m,
	}

	okExt := &canned{
		name: "vornik-extract-pdf", version: "1",
		res: Result{
			Outline:  []OutlineEntry{{SectionID: "001", Title: "t", TextBytes: 1}},
			Sections: []Section{{SectionID: "001", Title: "t", Content: "c"}},
		},
	}
	if _, err := r.Run(context.Background(), "assistant", "art-ok", okExt, Source{FilePath: "/dev/null"}); err != nil {
		t.Fatalf("Run(ok): %v", err)
	}

	// Extract failure — same extractor label, counted as error, no
	// document recorded.
	errExt := &canned{name: "vornik-extract-pdf", version: "1", err: errors.New("boom")}
	if _, err := r.Run(context.Background(), "assistant", "art-err", errExt, Source{FilePath: "/dev/null"}); err == nil {
		t.Fatal("Run(err): expected error")
	}

	if got := testutil.ToFloat64(m.ExtractionsTotal.WithLabelValues("vornik-extract-pdf", "ok")); got != 1 {
		t.Errorf("extractions_total{ok} = %v; want 1", got)
	}
	if got := testutil.ToFloat64(m.ExtractionsTotal.WithLabelValues("vornik-extract-pdf", "error")); got != 1 {
		t.Errorf("extractions_total{error} = %v; want 1", got)
	}
	if got := testutil.ToFloat64(m.ExtractedDocumentsTotal.WithLabelValues("assistant")); got != 1 {
		t.Errorf("extracted_documents_total{assistant} = %v; want 1 (only the ok run)", got)
	}
	// Duration observed on both runs (the Extract work happened either way).
	if n := testutil.CollectAndCount(reg, "vornik_extraction_duration_seconds"); n == 0 {
		t.Error("extraction_duration_seconds not observed")
	}
}

func TestRunner_Metrics_PersistFailureCountedAsError(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	r := &Runner{
		Repo:     &fakeRepo{err: errors.New("db down")},
		BasePath: t.TempDir(),
		Metrics:  m,
	}
	ext := &canned{
		name: "vornik-extract-pdf", version: "1",
		res: Result{
			Outline:  []OutlineEntry{{SectionID: "001", Title: "t", TextBytes: 1}},
			Sections: []Section{{SectionID: "001", Title: "t", Content: "c"}},
		},
	}
	if _, err := r.Run(context.Background(), "assistant", "art", ext, Source{FilePath: "/dev/null"}); err == nil {
		t.Fatal("expected upsert error")
	}
	// A post-Extract persistence failure is still an extraction error,
	// but produces no persisted-document tick.
	if got := testutil.ToFloat64(m.ExtractionsTotal.WithLabelValues("vornik-extract-pdf", "error")); got != 1 {
		t.Errorf("extractions_total{error} = %v; want 1", got)
	}
	if got := testutil.ToFloat64(m.ExtractedDocumentsTotal.WithLabelValues("assistant")); got != 0 {
		t.Errorf("extracted_documents_total = %v; want 0 (persist failed)", got)
	}
}

func TestRunner_Metrics_ValidationErrorNotCounted(t *testing.T) {
	// Pre-Extract validation failures are config/programmer errors, not
	// extraction attempts — they must not tick extractions_total.
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	r := &Runner{Repo: &fakeRepo{}, BasePath: t.TempDir(), Metrics: m}
	if _, err := r.Run(context.Background(), "", "art", &canned{name: "vornik-extract-pdf", version: "1"}, Source{}); err == nil {
		t.Fatal("expected validation error for empty projectID")
	}
	if n := testutil.CollectAndCount(reg, "vornik_extractions_total"); n != 0 {
		t.Errorf("extractions_total has %d series; want 0 (validation error pre-Extract)", n)
	}
}

func TestRunner_RequiresFields(t *testing.T) {
	// Nil-safety on the construction surface — every required field
	// should produce a clear error rather than nil-deref deeper in.
	ctx := context.Background()
	cases := []struct {
		name string
		r    *Runner
		want string
	}{
		{"nil runner", nil, "runner is nil"},
		{"missing repo", &Runner{BasePath: "/tmp"}, "Repo is required"},
		{"missing base", &Runner{Repo: &fakeRepo{}}, "BasePath is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.r.Run(ctx, "p", "a", &canned{name: "x", version: "1"}, Source{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v; want error containing %q", err, c.want)
			}
		})
	}
}

// serializingExtractor records the maximum number of Extract calls
// observed running concurrently. The Runner's per-target extraction
// lock must keep this at 1 for the same (project, artifact,
// extractor) key.
type serializingExtractor struct {
	name, version string
	mu            sync.Mutex
	active        int
	maxActive     int
	res           Result
}

func (s *serializingExtractor) Name() string    { return s.name }
func (s *serializingExtractor) Version() string { return s.version }
func (s *serializingExtractor) Extract(context.Context, Source) (Result, error) {
	s.mu.Lock()
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.mu.Unlock()

	// Hold the "extraction" open briefly so a racing call would
	// overlap if the lock were absent.
	time.Sleep(30 * time.Millisecond)

	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return s.res, nil
}

// TestRunner_SerializesSameTarget — batch-3 ingress/untrusted-input:
// document-extraction hardening (e). Two concurrent Run calls for the
// SAME (project, artifact, extractor, version) must not extract in
// parallel. Pre-fix both Extract bodies overlap (maxActive == 2),
// doing duplicate work and leaving an orphan storage dir; post-fix
// the per-target lock serializes them (maxActive == 1).
func TestRunner_SerializesSameTarget(t *testing.T) {
	ext := &serializingExtractor{
		name:    "vornik-extract-test",
		version: "1.0.0",
		res: Result{
			Sections: []Section{{SectionID: "001-x", Title: "X", Content: "body"}},
			Outline:  []OutlineEntry{{SectionID: "001-x", Title: "X", TextBytes: 4}},
		},
	}
	var idc int
	var idmu sync.Mutex
	r := &Runner{
		Repo:     &fakeRepo{},
		BasePath: t.TempDir(),
		IDMint: func() string {
			idmu.Lock()
			defer idmu.Unlock()
			idc++
			return "extdoc_" + strconv.Itoa(idc)
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Run(context.Background(), "assistant", "art-1", ext, Source{FilePath: "/dev/null"})
		}()
	}
	wg.Wait()

	ext.mu.Lock()
	defer ext.mu.Unlock()
	if ext.maxActive != 1 {
		t.Errorf("max concurrent Extract = %d; want 1 (extraction lock not serializing same target)", ext.maxActive)
	}
}

// TestRunner_DifferentTargetsRunConcurrently — the lock must be
// per-target, not global: two DIFFERENT artifacts extract in
// parallel (maxActive == 2), preserving throughput.
func TestRunner_DifferentTargetsRunConcurrently(t *testing.T) {
	ext := &serializingExtractor{
		name:    "vornik-extract-test",
		version: "1.0.0",
		res: Result{
			Sections: []Section{{SectionID: "001-x", Title: "X", Content: "body"}},
			Outline:  []OutlineEntry{{SectionID: "001-x", Title: "X", TextBytes: 4}},
		},
	}
	var idc int
	var idmu sync.Mutex
	r := &Runner{
		Repo:     &fakeRepo{},
		BasePath: t.TempDir(),
		IDMint: func() string {
			idmu.Lock()
			defer idmu.Unlock()
			idc++
			return "extdoc_" + strconv.Itoa(idc)
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		artifact := "art-" + strconv.Itoa(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Run(context.Background(), "assistant", artifact, ext, Source{FilePath: "/dev/null"})
		}()
	}
	wg.Wait()

	ext.mu.Lock()
	defer ext.mu.Unlock()
	if ext.maxActive != 2 {
		t.Errorf("max concurrent Extract = %d; want 2 (per-target lock should not serialize distinct targets)", ext.maxActive)
	}
}
