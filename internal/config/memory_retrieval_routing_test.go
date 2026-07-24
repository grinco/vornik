package config

import "testing"

// Confidence-based retrieval routing (P3): the one hard config invariant is
// min_results <= k, enforced at config load (§3.2).

func TestMemoryRetrievalRoutingConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     MemoryRetrievalRoutingConfig
		wantErr bool
	}{
		{"all-default (both unset)", MemoryRetrievalRoutingConfig{}, false},
		{"min_results==k", MemoryRetrievalRoutingConfig{K: 5, MinResults: 5}, false},
		{"min_results<k", MemoryRetrievalRoutingConfig{K: 5, MinResults: 3}, false},
		{"min_results>k", MemoryRetrievalRoutingConfig{K: 5, MinResults: 6}, true},
		{"min_results>default_k(5)", MemoryRetrievalRoutingConfig{MinResults: 7}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}
