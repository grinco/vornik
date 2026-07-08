package persistence

import "testing"

func TestP95Seconds(t *testing.T) {
	if got := P95Seconds(nil); got != 0 {
		t.Errorf("empty → 0, got %v", got)
	}
	// 1..100: nearest-rank p95 = ceil(0.95*100)=95th value = 95.
	xs := make([]float64, 100)
	for i := range xs {
		xs[i] = float64(i + 1)
	}
	if got := P95Seconds(xs); got != 95 {
		t.Errorf("p95 of 1..100 = %v, want 95", got)
	}
	// Single sample → that value.
	if got := P95Seconds([]float64{42}); got != 42 {
		t.Errorf("single → 42, got %v", got)
	}
	// Unsorted input handled.
	if got := P95Seconds([]float64{5, 1, 4, 2, 3}); got != 5 {
		t.Errorf("p95 of {1..5} = %v, want 5", got)
	}
}
