package postgres

import (
	"strings"
	"testing"
)

// TestNewLeaseIDFormat locks in the shape of a lease identifier so the
// scheduler and the database agree on how to match it: "lease-" prefix
// followed by 32 lowercase hex characters (16 bytes, 128 bits of entropy).
func TestNewLeaseIDFormat(t *testing.T) {
	id, err := newLeaseID()
	if err != nil {
		t.Fatalf("newLeaseID failed: %v", err)
	}
	if !strings.HasPrefix(id, "lease-") {
		t.Fatalf("expected lease-id prefix; got %q", id)
	}
	body := strings.TrimPrefix(id, "lease-")
	if len(body) != 32 {
		t.Fatalf("expected 32-char hex body (16 bytes); got %d: %q", len(body), body)
	}
	for _, r := range body {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex {
			t.Fatalf("expected lowercase hex only; got %q in %q", r, id)
		}
	}
}

// TestNewLeaseIDUnique is a cheap smoke test against obvious regressions
// that would collapse the entropy source (e.g. switching back to
// time-based generation, or a buggy RNG that always returns the same
// bytes).
func TestNewLeaseIDUnique(t *testing.T) {
	const samples = 1000
	seen := make(map[string]struct{}, samples)
	for i := 0; i < samples; i++ {
		id, err := newLeaseID()
		if err != nil {
			t.Fatalf("newLeaseID failed: %v", err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("lease-id collision at sample %d: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}
