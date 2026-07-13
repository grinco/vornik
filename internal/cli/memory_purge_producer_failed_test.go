package cli

import "testing"

// TestCheckExpectCount pins the bulk-op safety gate for
// `vornikctl memory purge-producer-failed --confirm` (LLD §5): --confirm must
// carry an --expect-count that exactly matches the current candidate set, so a
// set that changed since the operator's --dry-run aborts rather than deleting
// a set they never previewed.
func TestCheckExpectCount(t *testing.T) {
	cases := []struct {
		name    string
		expect  int
		actual  int
		wantErr bool
	}{
		{"match", 17, 17, false},
		{"match zero", 0, 0, false},
		{"omitted flag (negative)", -1, 5, true},
		{"set grew since dry-run", 5, 8, true},
		{"set shrank since dry-run", 8, 5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkExpectCount(tc.expect, tc.actual)
			if tc.wantErr && err == nil {
				t.Fatalf("checkExpectCount(%d,%d) = nil, want error", tc.expect, tc.actual)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkExpectCount(%d,%d) = %v, want nil", tc.expect, tc.actual, err)
			}
		})
	}
}
