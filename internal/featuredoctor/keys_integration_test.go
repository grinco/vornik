package featuredoctor_test

import (
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/featuredoctor"
)

// TestRegistryGateKeysUnique ensures no two registered features declare the
// same Gate.Key. A shared gate key means enabling one feature and disabling
// another would fight over the same config value, and Diagnose's gatesOn
// would couple their states — a silent cross-feature bug. Adding a feature
// that reuses an existing gate key becomes a build-time-caught failure.
func TestRegistryGateKeysUnique(t *testing.T) {
	owner := map[string]string{} // gate key -> first feature ID that declared it
	for _, f := range featuredoctor.Registry() {
		for _, g := range f.Gates {
			if prev, dup := owner[g.Key]; dup {
				t.Errorf("gate key %q is declared by both %q and %q — gate keys must be unique per feature",
					g.Key, prev, f.ID)
				continue
			}
			owner[g.Key] = f.ID
		}
	}
}

// TestRegistryGateKeysResolve ensures that every Gate.Key declared in the
// Registry actually resolves against a real *config.Config via
// config.LookupByPath. A gate key that doesn't resolve silently reads as
// "gate off", making the feature appear permanently disabled without any
// error. This test makes a wrong key a build-time-caught test failure.
func TestRegistryGateKeysResolve(t *testing.T) {
	cfg := config.DefaultConfig()
	for _, f := range featuredoctor.Registry() {
		for _, g := range f.Gates {
			_, found := config.LookupByPath(cfg, g.Key)
			if !found {
				t.Errorf("feature %q: gate key %q does not resolve against *config.Config — "+
					"check the dotted yaml-tag path in the feature definition", f.ID, g.Key)
			}
		}
	}
}
