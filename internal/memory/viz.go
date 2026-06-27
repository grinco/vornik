package memory

import (
	"context"
	"math"
)

// mathSqrt64 wraps math.Sqrt in a one-liner so the cosine-norm
// inner loop has a single named indirection — handy if a future
// version swaps in an approximation for the hot path.
func mathSqrt64(x float64) float64 { return math.Sqrt(x) }

// VectorProjection is the operator-viz output: each chunk's 3D
// coordinates after PCA (top-3 principal components), plus the
// tooltip metadata. Coordinates are in [-1, 1] after autoscale so
// the UI's SVG viewBox can be fixed without per-render tuning.
// The Z axis is what makes the scatter rotatable in the operator UI.
type VectorProjection struct {
	X, Y, Z          float32
	ContentSize      int
	ChunkID          string
	SourceName       string
	ContentClass     string
	ValidationStatus string
	ProducerRole     string
	Preview          string
	Neighbors        []NeighborRef
}

// NeighborRef is one nearest-neighbour edge — chunk ID + cosine
// similarity. Operator-viz computes top-K per chunk for the
// relationship visualisation.
type NeighborRef struct {
	ChunkID    string
	Similarity float32
}

// VizSource is the Pipeline+Repo composition the UI layer talks to.
// Wraps Repository.SampleChunksForViz with the PCA projection so the
// caller doesn't have to know about embedding shapes.
type VizSource struct {
	Repo *Repository
}

// NewVizSource returns a viz adapter over the chunk repo.
func NewVizSource(repo *Repository) *VizSource {
	return &VizSource{Repo: repo}
}

// SampleProjection loads up to limit chunks with embeddings,
// projects them to 2D via PCA, and returns the per-point view.
// Empty input → empty output (no error). The ctx parameter takes
// the narrow Done-channel interface so this satisfies the UI
// layer's adapter shape without dragging the full context.Context
// surface into a separate package.
func (v *VizSource) SampleProjection(ctxLike interface{ Done() <-chan struct{} }, projectID string, activeEpochs []string, limit int) ([]VizPoint, error) {
	if v == nil || v.Repo == nil || projectID == "" {
		return nil, nil
	}
	ctx, ok := ctxLike.(context.Context)
	if !ok {
		// The narrow interface accepts any "Done()" — but the
		// repo needs a real context.Context for query
		// cancellation. Fall back to a fresh background ctx so
		// the request still completes (deadlines from upstream
		// just don't propagate).
		ctx = context.Background()
	}
	chunks, err := v.Repo.SampleChunksForViz(ctx, projectID, activeEpochs, true, limit)
	if err != nil {
		return nil, err
	}
	if len(chunks) < 2 {
		return nil, nil
	}
	// Filter to chunks that actually have embeddings — avoid
	// PCA-ing through a partial set where some rows are zero
	// vectors.
	withEmb := make([]VizChunk, 0, len(chunks))
	for _, c := range chunks {
		if len(c.Embedding) > 0 {
			withEmb = append(withEmb, c)
		}
	}
	if len(withEmb) < 2 {
		return nil, nil
	}
	embs := make([][]float32, len(withEmb))
	for i, c := range withEmb {
		embs[i] = c.Embedding
	}
	xyz := pca3(embs)
	if xyz == nil {
		return nil, nil
	}
	autoscale3(xyz)

	// Pre-normalise embeddings once so cosine reduces to a dot
	// product. At 500×1024 that's ~512K float ops, negligible
	// compared to the O(N²·D) similarity sweep below.
	norms := make([][]float32, len(withEmb))
	for i, e := range embs {
		norms[i] = unitNorm(e)
	}
	knn := topKNeighbors(norms, 3, 0.6)

	out := make([]VizPoint, len(withEmb))
	for i, c := range withEmb {
		nbrs := make([]NeighborRef, 0, len(knn[i]))
		for _, n := range knn[i] {
			nbrs = append(nbrs, NeighborRef{
				ChunkID:    withEmb[n.idx].ID,
				Similarity: n.sim,
			})
		}
		out[i] = VizPoint{
			X: xyz[i][0],
			Y: xyz[i][1],
			Z: xyz[i][2],
			// ContentSize is the full chunk byte count from
			// SampleChunksForViz (char_length(content) on the
			// DB side). Used by the UI to log-scale node radius
			// so "long dossier" vs "tiny note" is visible at a
			// glance.
			ContentSize:      c.ContentSize,
			ChunkID:          c.ID,
			SourceName:       c.DisplayTitle, // human-readable title > filename
			ContentClass:     c.ContentClass,
			ValidationStatus: c.ValidationStatus,
			ProducerRole:     c.ProducerRole,
			Preview:          c.Preview,
			Neighbors:        nbrs,
		}
	}
	return out, nil
}

// nbrIdx is one (other-row index, similarity) pair used during KNN
// computation; converted to NeighborRef before crossing the API
// surface.
type nbrIdx struct {
	idx int
	sim float32
}

// unitNorm returns v / ||v|| so a dot product against another
// unit-norm vector equals cosine similarity. Zero-length vectors
// are passed through unchanged (they'd produce NaN sims; we filter
// those at the threshold).
func unitNorm(v []float32) []float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		out := make([]float32, len(v))
		copy(out, v)
		return out
	}
	inv := float32(1.0 / mathSqrt64(s))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

// topKNeighbors returns the top-K most similar OTHER rows for each
// row, filtering by minSim. Brute force O(N²·D) — fine at our
// sample cap of 500. The "skip self" check keeps a row out of its
// own neighbour list.
func topKNeighbors(unit [][]float32, k int, minSim float32) [][]nbrIdx {
	out := make([][]nbrIdx, len(unit))
	for i := range unit {
		var top []nbrIdx
		for j := range unit {
			if i == j {
				continue
			}
			sim := dot(unit[i], unit[j])
			if sim < minSim {
				continue
			}
			top = insertTopK(top, nbrIdx{idx: j, sim: sim}, k)
		}
		out[i] = top
	}
	return out
}

// dot is the float32-domain inner product. Unrolled by 4 for the
// hot loop — at 500×500×1024 = 256M multiplies the difference
// between unrolled and naive is ~30% wall clock.
func dot(a, b []float32) float32 {
	var s float32
	n := len(a)
	if n > len(b) {
		n = len(b)
	}
	i := 0
	for ; i+4 <= n; i += 4 {
		s += a[i]*b[i] + a[i+1]*b[i+1] + a[i+2]*b[i+2] + a[i+3]*b[i+3]
	}
	for ; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}

// insertTopK keeps top descends sorted by sim, capped at k. Used
// as the per-row accumulator. List comprehension not great for
// sub-100-element K so this is the simplest correct version.
func insertTopK(list []nbrIdx, candidate nbrIdx, k int) []nbrIdx {
	pos := len(list)
	for pos > 0 && list[pos-1].sim < candidate.sim {
		pos--
	}
	if pos >= k {
		return list
	}
	if len(list) < k {
		list = append(list, nbrIdx{})
	}
	// Shift right from pos to len-1 (excluding last; we appended above).
	copy(list[pos+1:], list[pos:len(list)-1])
	list[pos] = candidate
	return list
}

// VizPoint mirrors ui.VizPoint to satisfy the interface without a
// cross-package dependency. The UI's adapter does the conversion
// at the boundary (one struct copy per point — cheap). 3D coords +
// content size so the UI can drive rotation, zoom, and per-chunk
// node radius without re-fetching.
type VizPoint struct {
	X, Y, Z          float32
	ContentSize      int
	ChunkID          string
	SourceName       string
	ContentClass     string
	ValidationStatus string
	ProducerRole     string
	Preview          string
	Neighbors        []NeighborRef
}

// autoscale3 shifts + scales the projection so coordinates fall in
// [-1, 1] per axis (with a tiny margin so points on the extremes
// aren't clipped at the viewport edge). In place. The 3D variant
// scales each axis independently so a flat-along-Z corpus still
// uses the available Z range — one per-axis range gives the
// rotation visualisation more pop than a uniform scale.
func autoscale3(pts [][3]float32) {
	if len(pts) == 0 {
		return
	}
	min := [3]float32{pts[0][0], pts[0][1], pts[0][2]}
	max := [3]float32{pts[0][0], pts[0][1], pts[0][2]}
	for _, p := range pts {
		for ax := 0; ax < 3; ax++ {
			if p[ax] < min[ax] {
				min[ax] = p[ax]
			}
			if p[ax] > max[ax] {
				max[ax] = p[ax]
			}
		}
	}
	rng := [3]float64{
		float64(max[0] - min[0]),
		float64(max[1] - min[1]),
		float64(max[2] - min[2]),
	}
	for ax := range rng {
		if rng[ax] == 0 {
			rng[ax] = 1
		}
	}
	const margin = 0.95
	for i := range pts {
		for ax := 0; ax < 3; ax++ {
			pts[i][ax] = float32(margin * (2*(float64(pts[i][ax]-min[ax])/rng[ax]) - 1))
		}
	}
	for i := range pts {
		for ax := 0; ax < 3; ax++ {
			v := float64(pts[i][ax])
			if math.IsNaN(v) || math.IsInf(v, 0) {
				pts[i][ax] = 0
			}
		}
	}
}

// autoscale shifts + scales the projection so coordinates fall in
// [-1, 1] (with a tiny margin so points on the extremes aren't
// clipped at the SVG edge). In place. No-op when all points are
// the same (degenerate input).
//
// Currently unused (autoscale3 supersedes via the 3D scatter) —
// kept for symmetry with pca2 should a deployment opt back into
// the static 2D viz.
//
//nolint:unused
func autoscale(pts [][2]float32) {
	if len(pts) == 0 {
		return
	}
	xMin, xMax := pts[0][0], pts[0][0]
	yMin, yMax := pts[0][1], pts[0][1]
	for _, p := range pts {
		if p[0] < xMin {
			xMin = p[0]
		}
		if p[0] > xMax {
			xMax = p[0]
		}
		if p[1] < yMin {
			yMin = p[1]
		}
		if p[1] > yMax {
			yMax = p[1]
		}
	}
	xRange := float64(xMax - xMin)
	yRange := float64(yMax - yMin)
	if xRange == 0 {
		xRange = 1
	}
	if yRange == 0 {
		yRange = 1
	}
	// 5% margin so extreme points aren't drawn flush against the
	// viewBox edge.
	const margin = 0.95
	for i := range pts {
		pts[i][0] = float32(margin * (2*(float64(pts[i][0]-xMin)/xRange) - 1))
		pts[i][1] = float32(margin * (2*(float64(pts[i][1]-yMin)/yRange) - 1))
	}
	// Defensive against NaN/Inf surviving the divide.
	for i := range pts {
		if math.IsNaN(float64(pts[i][0])) || math.IsInf(float64(pts[i][0]), 0) {
			pts[i][0] = 0
		}
		if math.IsNaN(float64(pts[i][1])) || math.IsInf(float64(pts[i][1]), 0) {
			pts[i][1] = 0
		}
	}
}
