// Package fieldguard restricts which fields a mutation is permitted to
// write — the allowlist-of-mutable-fields pattern.
//
// It exists to catch the class of bug where a handler builds a dynamic
// set of writes (DB columns, config-YAML keys, a bound struct) and a
// typo or an accidentally-added entry mutates a field that should never
// be writable on that path: an identity key (projectId), a creation
// timestamp (created_at), a tenancy column (tenant_id), an audit field.
// Those writes are silent and corrupting — by the time anyone notices,
// the protected value is already overwritten.
//
// The model is deny-by-default: a Guard is constructed with the exact
// set of fields a mutation MAY touch; everything else is rejected. A
// handler either refuses the whole mutation (Check) or strips the
// offending writes (Filter), depending on whether a stray write should
// be a loud failure or quietly dropped.
//
// A nil *Guard permits everything (no-op), so the guard can be threaded
// through optional code paths and wired in incrementally without
// forcing every call site to construct one at once.
package fieldguard

import (
	"fmt"
	"sort"
	"strings"
)

// Guard is an immutable allowlist of writable field names. Construct
// it with Allowlist; the zero value is not usable (use Allowlist()).
type Guard struct {
	allowed map[string]struct{}
}

// Allowlist builds a Guard permitting exactly the named fields.
// Anything not listed is rejected. Duplicate and empty names are
// ignored. Allowlist() with no fields is a valid deny-all guard.
func Allowlist(fields ...string) *Guard {
	g := &Guard{allowed: make(map[string]struct{}, len(fields))}
	for _, f := range fields {
		if f == "" {
			continue
		}
		g.allowed[f] = struct{}{}
	}
	return g
}

// Allows reports whether field may be written. A nil guard allows
// everything.
func (g *Guard) Allows(field string) bool {
	if g == nil {
		return true
	}
	_, ok := g.allowed[field]
	return ok
}

// Rejected returns the subset of fields that are not allowed, in input
// order with duplicates removed. An empty result means every field is
// permitted. A nil guard rejects nothing.
func (g *Guard) Rejected(fields []string) []string {
	if g == nil {
		return nil
	}
	var out []string
	seen := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		if _, ok := g.allowed[f]; !ok {
			out = append(out, f)
		}
	}
	return out
}

// Check returns a *Violation naming the disallowed fields, or nil when
// every field is permitted. Use it in refuse-the-whole-mutation mode:
//
//	if err := guard.Check(touched); err != nil { return err }
func (g *Guard) Check(fields []string) error {
	rej := g.Rejected(fields)
	if len(rej) == 0 {
		return nil
	}
	return &Violation{Fields: rej}
}

// Filter splits m into the allowed subset and the rejected key names
// (strip mode). The input map is never mutated; allowed is a fresh map
// (nil when m is empty). rejected is sorted for deterministic output. A
// nil guard returns m's keys all-allowed.
func Filter[V any](g *Guard, m map[string]V) (allowed map[string]V, rejected []string) {
	if len(m) == 0 {
		return nil, nil
	}
	allowed = make(map[string]V, len(m))
	for k, v := range m {
		if g.Allows(k) {
			allowed[k] = v
		} else {
			rejected = append(rejected, k)
		}
	}
	sort.Strings(rejected)
	return allowed, rejected
}

// Violation reports the fields a mutation tried to write that the guard
// does not permit.
type Violation struct {
	Fields []string
}

func (v *Violation) Error() string {
	if v == nil || len(v.Fields) == 0 {
		return "fieldguard: no violation"
	}
	return fmt.Sprintf("fieldguard: write to protected field(s) not permitted: %s",
		strings.Join(v.Fields, ", "))
}
