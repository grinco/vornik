package repotest

import (
	"context"

	"vornik.io/vornik/internal/persistence/misscontract"
)

// TB is the subset of *testing.T that the miss-contract helpers need. It is
// an interface rather than *testing.T so the helpers' own failure paths are
// testable.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// AssertMiss checks one lookup's result for an absent row against the
// behaviour declared in misscontract.Contract for key.
//
// It works against anything implementing the method — a production
// repository, or a three-field in-package fake — which is the point: a
// double that disagrees with production about what absence looks like will
// certify a broken path rather than fail. See the package comment on
// internal/persistence/misscontract for the incident.
func AssertMiss[T any](t TB, key string, call func() (*T, error)) {
	t.Helper()
	v, err := call()
	if cerr := misscontract.Check(key, v == nil, err); cerr != nil {
		t.Fatalf("miss contract: %v", cerr)
	}
}

// AssertMissRepo is the form the shared suites use. It drives the real
// lookup with an id that cannot exist, so what is proven is the
// implementation's own miss path rather than a pair assembled by hand.
//
// The probe id is unique per call so that a suite which asserts a miss and
// later writes a row cannot collide with itself, and says what it is so an
// unexpected hit is diagnosable from the failure alone.
func AssertMissRepo[T any](t TB, key string, get func(ctx context.Context, id string) (*T, error)) {
	t.Helper()
	id := uniqueID("absent")
	AssertMiss(t, key, func() (*T, error) {
		return get(context.Background(), id)
	})
}
