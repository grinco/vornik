package memory

import (
	"math"
	"math/rand"
)

// pca3 projects N D-dimensional vectors down to 3D using three
// rounds of power iteration with deflation. Same algorithm as
// pca2 (see comment there) plus one more eigenvector. Used by the
// 3D-rotatable scatter — operators drag to rotate around the
// resulting (x, y, z) basis. Returns nil for malformed input.
//
// With 2 rows the first eigenvector is the axis between the two
// points; the 2nd/3rd are arbitrary orthogonal directions (random
// power-iteration seed). Both points land at ±X, Y/Z≈0 after
// autoscale — sparse but valid. Min is 2 rather than 3 so a project
// with few non-superseded chunks still shows a scatter.
func pca3(rows [][]float32) [][3]float32 {
	if len(rows) < 2 {
		return nil
	}
	dim := len(rows[0])
	if dim < 3 {
		return nil
	}
	for _, r := range rows {
		if len(r) != dim {
			return nil
		}
	}

	// Center the data.
	mean := make([]float64, dim)
	for _, r := range rows {
		for j, v := range r {
			mean[j] += float64(v)
		}
	}
	invN := 1.0 / float64(len(rows))
	for j := range mean {
		mean[j] *= invN
	}
	centered := make([][]float64, len(rows))
	for i, r := range rows {
		c := make([]float64, dim)
		for j, v := range r {
			c[j] = float64(v) - mean[j]
		}
		centered[i] = c
	}

	// Three power-iteration passes; each deflates against the
	// previous eigenvectors so we converge on the next axis.
	v1 := powerIter(centered, dim, nil, 30)
	v2 := powerIter3(centered, dim, [][]float64{v1}, 30)
	v3 := powerIter3(centered, dim, [][]float64{v1, v2}, 30)

	out := make([][3]float32, len(rows))
	for i, r := range centered {
		var x, y, z float64
		for j := 0; j < dim; j++ {
			x += r[j] * v1[j]
			y += r[j] * v2[j]
			z += r[j] * v3[j]
		}
		out[i] = [3]float32{float32(x), float32(y), float32(z)}
	}
	return out
}

// powerIter3 generalises powerIter to deflate against multiple
// previous eigenvectors. Same convergence path as powerIter — one
// more orthogonalisation step per iteration when len(orths) > 0.
func powerIter3(X [][]float64, dim int, orths [][]float64, iters int) []float64 {
	rng := newSeededRand()
	v := make([]float64, dim)
	for j := range v {
		v[j] = rng.Float64()*2 - 1
	}
	for _, o := range orths {
		project(v, o)
	}
	normalise(v)

	work := make([]float64, dim)
	for iter := 0; iter < iters; iter++ {
		temp := make([]float64, len(X))
		for i, row := range X {
			var s float64
			for j := 0; j < dim; j++ {
				s += row[j] * v[j]
			}
			temp[i] = s
		}
		for j := range work {
			work[j] = 0
		}
		for i, row := range X {
			t := temp[i]
			for j := 0; j < dim; j++ {
				work[j] += row[j] * t
			}
		}
		v = work
		work = make([]float64, dim)
		for _, o := range orths {
			project(v, o)
		}
		normalise(v)
	}
	return v
}

// newSeededRand returns the same fixed-seed RNG used by pca2 and
// powerIter so a 3D projection of the same data yields the same
// orientation across page loads. Centralised so a future swap to
// a non-deterministic seed only touches one place.
func newSeededRand() *rand.Rand { return rand.New(rand.NewSource(42)) }

// pca2 projects N D-dimensional vectors down to 2D using two
// rounds of power iteration over the covariance matrix.
//
// Currently unused (the 3D-rotatable scatter superseded it via
// pca3) — kept here as the canonical reference + documentation
// for the algorithm. Operators wanting a non-rotatable 2D scatter
// can wire it back via the same VizSource shape.
//
// We don't bring in gonum or a full SVD library because:
//   - one production caller (the operator viz endpoint) — full
//     SVD is overkill for "show me a 2D scatter to eyeball
//     clustering",
//   - chunks×embedding_dim is bounded (we cap chunks at 500 for
//     the viz; embedding_dim is ~1024 today), so the O(N·D)
//     per-iteration cost is ~500K float ops × ~30 iters = trivially
//     fast,
//   - keeping the dep tree shallow matters for the daemon's binary
//     size + startup time.
//
// Algorithm:
//  1. Center the data column-wise (per-dimension mean = 0).
//  2. Power iteration to find the top-1 eigenvector v1 of XᵀX.
//  3. Deflate (project rows orthogonal to v1) and repeat for v2.
//  4. Project every row onto (v1, v2) → 2D coords.
//
// The resulting axes are not labeled — operators read clustering
// not absolute positions, so we deliberately don't expose
// "principal component 1" semantics. Same data → same projection
// (deterministic when the seed is fixed; we use seed=42 for the
// initial random vector so reloading the page doesn't reshuffle
// the scatter).
//
// Returns nil when input is malformed (dim mismatch, fewer than 2
// rows). Caller treats nil as "viz unavailable".
//
//nolint:unused
func pca2(rows [][]float32) [][2]float32 {
	if len(rows) < 2 {
		return nil
	}
	dim := len(rows[0])
	if dim < 2 {
		return nil
	}
	for _, r := range rows {
		if len(r) != dim {
			return nil
		}
	}

	// 1. Center.
	mean := make([]float64, dim)
	for _, r := range rows {
		for j, v := range r {
			mean[j] += float64(v)
		}
	}
	invN := 1.0 / float64(len(rows))
	for j := range mean {
		mean[j] *= invN
	}
	centered := make([][]float64, len(rows))
	for i, r := range rows {
		c := make([]float64, dim)
		for j, v := range r {
			c[j] = float64(v) - mean[j]
		}
		centered[i] = c
	}

	// 2-3. Power iteration for top-2 eigenvectors.
	v1 := powerIter(centered, dim, nil, 30)
	v2 := powerIter(centered, dim, v1, 30)

	// 4. Project each row onto (v1, v2).
	out := make([][2]float32, len(rows))
	for i, r := range centered {
		var x, y float64
		for j := 0; j < dim; j++ {
			x += r[j] * v1[j]
			y += r[j] * v2[j]
		}
		out[i] = [2]float32{float32(x), float32(y)}
	}
	return out
}

// powerIter runs power iteration on the implicit covariance matrix
// XᵀX (we never materialise the dim×dim matrix — we just compute
// XᵀXv via two passes over X). When `orth` is non-nil, every
// iterate is projected orthogonal to it so we converge on the
// second eigenvector instead of the first.
func powerIter(X [][]float64, dim int, orth []float64, iters int) []float64 {
	// Seeded so the projection is reproducible across page loads.
	rng := rand.New(rand.NewSource(42))
	v := make([]float64, dim)
	for j := range v {
		v[j] = rng.Float64()*2 - 1
	}
	if orth != nil {
		project(v, orth)
	}
	normalise(v)

	work := make([]float64, dim)
	for iter := 0; iter < iters; iter++ {
		// XᵀX v = Xᵀ(Xv). First pass: temp = Xv (length N).
		temp := make([]float64, len(X))
		for i, row := range X {
			var s float64
			for j := 0; j < dim; j++ {
				s += row[j] * v[j]
			}
			temp[i] = s
		}
		// Second pass: work = Xᵀ temp (length dim).
		for j := range work {
			work[j] = 0
		}
		for i, row := range X {
			t := temp[i]
			for j := 0; j < dim; j++ {
				work[j] += row[j] * t
			}
		}
		v = work
		work = make([]float64, dim)
		if orth != nil {
			project(v, orth)
		}
		normalise(v)
	}
	return v
}

// project removes the component of v along orth in place.
func project(v, orth []float64) {
	var dot float64
	for j := range v {
		dot += v[j] * orth[j]
	}
	for j := range v {
		v[j] -= dot * orth[j]
	}
}

// normalise scales v to unit length in place. Zero vectors are
// left alone (callers that hit this case won't crash; the projection
// just collapses to the origin which is honest behaviour).
func normalise(v []float64) {
	var s float64
	for _, x := range v {
		s += x * x
	}
	if s == 0 {
		return
	}
	inv := 1.0 / math.Sqrt(s)
	for j := range v {
		v[j] *= inv
	}
}
