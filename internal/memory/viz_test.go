package memory

import (
	"context"
	"math"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestNewVizSource(t *testing.T) {
	r, _, cleanup := newRepo(t)
	defer cleanup()
	v := NewVizSource(r)
	if v == nil || v.Repo != r {
		t.Fatal()
	}
}

func TestSampleProjection_NilGuards(t *testing.T) {
	var nilV *VizSource
	got, err := nilV.SampleProjection(context.Background(), "p", nil, 10)
	if got != nil || err != nil {
		t.Fatalf("nil receiver: %v %v", got, err)
	}
	v := &VizSource{}
	got, err = v.SampleProjection(context.Background(), "p", nil, 10)
	if got != nil || err != nil {
		t.Fatalf("nil repo: %v %v", got, err)
	}
	r, _, cleanup := newRepo(t)
	defer cleanup()
	v = NewVizSource(r)
	if got, _ := v.SampleProjection(context.Background(), "", nil, 10); got != nil {
		t.Fatalf("empty project: %v", got)
	}
}

func TestSampleProjection_NonContextCtxLikeFallsBack(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	v := NewVizSource(r)
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "source_name", "content_title", "content_class",
			"validation_status", "producer_role", "preview", "content_size", "embedding",
		}))
	// Custom Done-only stub — exercises the type-assertion fallback to background ctx.
	stub := doneStub{}
	if _, err := v.SampleProjection(stub, "p", nil, 5); err != nil {
		t.Fatal(err)
	}
}

type doneStub struct{}

func (doneStub) Done() <-chan struct{} { return nil }

func TestSampleProjection_RepoErr(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	v := NewVizSource(r)
	mock.ExpectQuery("FROM project_memory_chunks").WillReturnError(errOnce)
	if _, err := v.SampleProjection(context.Background(), "p", nil, 5); err == nil {
		t.Fatal("want err")
	}
}

func TestSampleProjection_FewerThan2Chunks(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	v := NewVizSource(r)
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "source_name", "content_title", "content_class",
			"validation_status", "producer_role", "preview", "content_size", "embedding",
		}).AddRow("c1", "s", "", "c", "v", "r", "preview", 10, "[1,2,3]"))
	got, err := v.SampleProjection(context.Background(), "p", nil, 5)
	if got != nil || err != nil {
		t.Fatalf("expected nil result, got %v %v", got, err)
	}
}

func TestSampleProjection_NoEmbeddingsFilteredOut(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	v := NewVizSource(r)
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "source_name", "content_title", "content_class",
			"validation_status", "producer_role", "preview", "content_size", "embedding",
		}).
			AddRow("c1", "s", "", "c", "v", "r", "preview", 10, nil).
			AddRow("c2", "s", "", "c", "v", "r", "preview", 10, nil))
	got, _ := v.SampleProjection(context.Background(), "p", nil, 5)
	if got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}

func TestSampleProjection_HappyPath(t *testing.T) {
	r, mock, cleanup := newRepo(t)
	defer cleanup()
	v := NewVizSource(r)
	rows := sqlmock.NewRows([]string{
		"id", "source_name", "content_title", "content_class",
		"validation_status", "producer_role", "preview", "content_size", "embedding",
	})
	// Six 4-dim vectors with variance so PCA produces meaningful output.
	rows.AddRow("c1", "s1", "Title1", "research", "verified", "scout", "preview", 100, "[1,2,3,4]")
	rows.AddRow("c2", "s2", "Title2", "research", "verified", "scout", "preview", 110, "[1.1,2.1,3.1,4.1]")
	rows.AddRow("c3", "s3", "Title3", "research", "verified", "scout", "preview", 120, "[2,3,1,4]")
	rows.AddRow("c4", "s4", "Title4", "research", "verified", "scout", "preview", 130, "[4,1,2,3]")
	rows.AddRow("c5", "s5", "Title5", "research", "verified", "scout", "preview", 140, "[3,4,1,2]")
	mock.ExpectQuery("FROM project_memory_chunks").WillReturnRows(rows)

	got, err := v.SampleProjection(context.Background(), "p", []string{"e1"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("len: %d", len(got))
	}
	for _, p := range got {
		if math.IsNaN(float64(p.X)) || math.IsInf(float64(p.X), 0) {
			t.Fatalf("bad x: %v", p.X)
		}
	}
}

func TestAutoscale2D_Pure(t *testing.T) {
	autoscale(nil) // no-op
	pts := [][2]float32{{0, 0}, {10, 20}, {-5, 5}}
	autoscale(pts)
	for _, p := range pts {
		if p[0] < -1 || p[0] > 1 || p[1] < -1 || p[1] > 1 {
			t.Fatalf("out of range: %v", p)
		}
	}
	// Degenerate (all same) → recover without NaN.
	same := [][2]float32{{1, 1}, {1, 1}}
	autoscale(same)
	for _, p := range same {
		if math.IsNaN(float64(p[0])) || math.IsInf(float64(p[0]), 0) {
			t.Fatalf("nan/inf: %v", p)
		}
	}
}

func TestPCA2(t *testing.T) {
	if got := pca2(nil); got != nil {
		t.Fatal("nil input")
	}
	if got := pca2([][]float32{{1, 2}}); got != nil {
		t.Fatal("single row")
	}
	if got := pca2([][]float32{{1}, {2}}); got != nil {
		t.Fatal("dim<2")
	}
	if got := pca2([][]float32{{1, 2}, {3}}); got != nil {
		t.Fatal("mismatched dim")
	}
	got := pca2([][]float32{
		{1, 2, 3, 4},
		{2, 3, 4, 5},
		{4, 3, 2, 1},
		{5, 4, 3, 2},
	})
	if len(got) != 4 {
		t.Fatalf("got %d", len(got))
	}
}

// errOnce is a sentinel for one-shot repo error injection.
var errOnce = sqlErrSentinel("inject")

type sqlErrSentinel string

func (e sqlErrSentinel) Error() string { return string(e) }
