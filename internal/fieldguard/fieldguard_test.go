package fieldguard

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestAllowlist_AllowsAndRejects(t *testing.T) {
	g := Allowlist("displayName", "autonomy", "", "autonomy") // empty + dup ignored
	if !g.Allows("displayName") || !g.Allows("autonomy") {
		t.Fatal("listed fields should be allowed")
	}
	if g.Allows("projectId") {
		t.Error("unlisted field projectId must be rejected")
	}
	if g.Allows("") {
		t.Error("empty field name must not be allowed (it was skipped at construction)")
	}
}

func TestDenyAll_EmptyAllowlist(t *testing.T) {
	g := Allowlist()
	if g.Allows("anything") {
		t.Error("empty allowlist must deny everything")
	}
	if err := g.Check([]string{"a"}); err == nil {
		t.Error("deny-all guard must reject a non-empty field set")
	}
}

func TestNilGuard_AllowsEverything(t *testing.T) {
	var g *Guard
	if !g.Allows("projectId") {
		t.Error("nil guard must allow everything (opt-in semantics)")
	}
	if err := g.Check([]string{"projectId", "tenant_id"}); err != nil {
		t.Errorf("nil guard Check must be nil, got %v", err)
	}
	if rej := g.Rejected([]string{"x"}); rej != nil {
		t.Errorf("nil guard rejects nothing, got %v", rej)
	}
}

func TestRejected_OrderAndDedup(t *testing.T) {
	g := Allowlist("ok")
	got := g.Rejected([]string{"b", "a", "b", "ok", "a"})
	want := []string{"b", "a"} // input order, deduped, allowed dropped
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Rejected = %v, want %v", got, want)
	}
	if g.Rejected([]string{"ok"}) != nil {
		t.Error("all-allowed input must yield nil")
	}
}

func TestCheck_ViolationMessageNamesFields(t *testing.T) {
	g := Allowlist("displayName")
	err := g.Check([]string{"displayName", "projectId", "tenant_id"})
	if err == nil {
		t.Fatal("expected a violation")
	}
	var v *Violation
	if !errors.As(err, &v) {
		t.Fatalf("expected *Violation, got %T", err)
	}
	if !reflect.DeepEqual(v.Fields, []string{"projectId", "tenant_id"}) {
		t.Errorf("violation fields = %v", v.Fields)
	}
	for _, want := range []string{"projectId", "tenant_id", "protected field"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message %q missing %q", err.Error(), want)
		}
	}
	// All-allowed → nil.
	if g.Check([]string{"displayName"}) != nil {
		t.Error("all-allowed Check must return nil")
	}
}

func TestFilter_StripMode(t *testing.T) {
	g := Allowlist("a", "c")
	in := map[string]int{"a": 1, "b": 2, "c": 3, "d": 4}
	allowed, rejected := Filter(g, in)
	if !reflect.DeepEqual(allowed, map[string]int{"a": 1, "c": 3}) {
		t.Errorf("allowed = %v", allowed)
	}
	if !reflect.DeepEqual(rejected, []string{"b", "d"}) { // sorted
		t.Errorf("rejected = %v, want [b d]", rejected)
	}
	// Input map must be untouched.
	if len(in) != 4 {
		t.Error("Filter must not mutate the input map")
	}
	// Empty input.
	a, r := Filter(g, map[string]int{})
	if a != nil || r != nil {
		t.Errorf("empty input → nil,nil; got %v,%v", a, r)
	}
}

func TestFilter_NilGuardAllowsAll(t *testing.T) {
	allowed, rejected := Filter[int](nil, map[string]int{"x": 1, "y": 2})
	if len(allowed) != 2 || rejected != nil {
		t.Errorf("nil guard Filter should allow all: allowed=%v rejected=%v", allowed, rejected)
	}
}
