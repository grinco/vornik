package memory

import (
	"math"
	"testing"
)

func TestPCA3RejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name string
		rows [][]float32
	}{
		{name: "nil_input", rows: nil},
		{name: "one_row", rows: [][]float32{{1, 2, 3, 4}}},
		{name: "too_few_dimensions", rows: [][]float32{{1, 2}, {3, 4}, {5, 6}}},
		{name: "ragged_rows", rows: [][]float32{{1, 2, 3}, {4, 5}, {6, 7, 8}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pca3(tc.rows); got != nil {
				t.Fatalf("pca3(%v) = %v, want nil", tc.rows, got)
			}
		})
	}
}

func TestPCA3TwoRowsProducesValidProjection(t *testing.T) {
	// Two rows must produce a valid (non-nil) projection so the UI
	// scatter renders when a project has only 2 non-superseded chunks.
	rows := [][]float32{
		{1, 0, 0, 0, 1},
		{0, 1, 0, 1, 0},
	}
	got := pca3(rows)
	if got == nil {
		t.Fatal("pca3 with 2 rows returned nil; want valid projection")
	}
	if len(got) != 2 {
		t.Fatalf("got %d projected rows, want 2", len(got))
	}
}

func TestPCA3IsDeterministicAndCentered(t *testing.T) {
	rows := [][]float32{
		{3, 0, 0, 1},
		{0, 4, 0, 2},
		{0, 0, 5, 3},
		{2, 2, 2, 4},
	}

	first := pca3(rows)
	second := pca3(rows)
	if len(first) != len(rows) {
		t.Fatalf("got %d projected rows, want %d", len(first), len(rows))
	}
	if len(second) != len(first) {
		t.Fatalf("second projection length = %d, want %d", len(second), len(first))
	}

	var sums [3]float64
	var nonZero bool
	for i := range first {
		for ax := 0; ax < 3; ax++ {
			if first[i][ax] != second[i][ax] {
				t.Fatalf("projection not deterministic at row %d axis %d: %v vs %v", i, ax, first[i][ax], second[i][ax])
			}
			if math.IsNaN(float64(first[i][ax])) || math.IsInf(float64(first[i][ax]), 0) {
				t.Fatalf("projection contains invalid coordinate at row %d axis %d: %v", i, ax, first[i][ax])
			}
			sums[ax] += float64(first[i][ax])
			if math.Abs(float64(first[i][ax])) > 1e-6 {
				nonZero = true
			}
		}
	}
	if !nonZero {
		t.Fatalf("projection collapsed all coordinates to zero: %v", first)
	}
	for ax, sum := range sums {
		if math.Abs(sum) > 1e-4 {
			t.Fatalf("axis %d is not centered: sum=%f projection=%v", ax, sum, first)
		}
	}
}

func TestAutoscale3ScalesEachAxisAndSanitizesInvalidValues(t *testing.T) {
	pts := [][3]float32{
		{10, 5, 1},
		{20, 5, 2},
		{30, 5, 3},
		{float32(math.Inf(1)), float32(math.NaN()), 4},
	}

	autoscale3(pts)

	for i, p := range pts {
		for ax, v := range p {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				t.Fatalf("point %d axis %d still invalid: %v", i, ax, v)
			}
			if v < -0.950001 || v > 0.950001 {
				t.Fatalf("point %d axis %d = %v outside scaled range", i, ax, v)
			}
		}
	}
	if pts[0][1] != -0.95 || pts[1][1] != -0.95 || pts[2][1] != -0.95 {
		t.Fatalf("constant axis should map finite inputs to -0.95, got %v", pts)
	}
}

func TestUnitNormDotAndTopKNeighbors(t *testing.T) {
	unit := [][]float32{
		unitNorm([]float32{3, 4}),
		unitNorm([]float32{6, 8}),
		unitNorm([]float32{0, 5}),
		unitNorm([]float32{-4, 3}),
	}

	if got := dot(unit[0], unit[1]); math.Abs(float64(got-1)) > 1e-6 {
		t.Fatalf("parallel vectors dot = %f, want 1", got)
	}
	if got := unitNorm([]float32{0, 0, 0}); len(got) != 3 || got[0] != 0 || got[1] != 0 || got[2] != 0 {
		t.Fatalf("zero vector normalization = %v, want unchanged zero vector", got)
	}

	neighbors := topKNeighbors(unit, 2, 0.7)
	if len(neighbors) != len(unit) {
		t.Fatalf("got %d neighbor rows, want %d", len(neighbors), len(unit))
	}
	if len(neighbors[0]) != 2 {
		t.Fatalf("row 0 got %d neighbors, want 2: %v", len(neighbors[0]), neighbors[0])
	}
	if neighbors[0][0].idx != 1 || math.Abs(float64(neighbors[0][0].sim-1)) > 1e-6 {
		t.Fatalf("row 0 best neighbor = %+v, want row 1 with sim 1", neighbors[0][0])
	}
	if neighbors[0][1].sim > neighbors[0][0].sim {
		t.Fatalf("neighbors not sorted descending: %v", neighbors[0])
	}
	for _, n := range neighbors[0] {
		if n.idx == 0 {
			t.Fatalf("self neighbor should be excluded: %v", neighbors[0])
		}
	}
}

func TestInsertTopKMaintainsDescendingCap(t *testing.T) {
	var list []nbrIdx
	for _, n := range []nbrIdx{
		{idx: 1, sim: 0.5},
		{idx: 2, sim: 0.9},
		{idx: 3, sim: 0.7},
		{idx: 4, sim: 0.1},
		{idx: 5, sim: 0.8},
	} {
		list = insertTopK(list, n, 3)
	}

	want := []nbrIdx{{idx: 2, sim: 0.9}, {idx: 5, sim: 0.8}, {idx: 3, sim: 0.7}}
	if len(list) != len(want) {
		t.Fatalf("got %d items, want %d: %v", len(list), len(want), list)
	}
	for i := range want {
		if list[i] != want[i] {
			t.Fatalf("item %d = %+v, want %+v (full list %v)", i, list[i], want[i], list)
		}
	}
}
