package featuredoctor

import (
	"testing"

	"vornik.io/vornik/internal/version"
)

func TestEveryFeatureHasValidEdition(t *testing.T) {
	for _, f := range Registry() {
		switch f.Edition {
		case version.EditionCommunity, version.EditionEnterprise:
			// ok
		default:
			t.Errorf("feature %q has invalid Edition %q (must be %q or %q)",
				f.ID, f.Edition, version.EditionCommunity, version.EditionEnterprise)
		}
	}
}

func TestFeatureEditionMapping(t *testing.T) {
	want := map[string]string{
		"instinct":       version.EditionEnterprise,
		"cluster":        version.EditionEnterprise,
		"trading-series": version.EditionEnterprise,
		"auth":           version.EditionCommunity,
		"memory-rag":     version.EditionCommunity,
	}
	got := map[string]string{}
	for _, f := range Registry() {
		got[f.ID] = f.Edition
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("feature %q edition = %q, want %q", id, got[id], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("registry has %d features, test covers %d — update the mapping", len(got), len(want))
	}
}

func TestIsEnterprise(t *testing.T) {
	if !(Feature{Edition: version.EditionEnterprise}).IsEnterprise() {
		t.Error("enterprise feature should report IsEnterprise() == true")
	}
	if (Feature{Edition: version.EditionCommunity}).IsEnterprise() {
		t.Error("community feature should report IsEnterprise() == false")
	}
}
