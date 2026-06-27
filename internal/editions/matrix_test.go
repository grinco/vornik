package editions

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/version"
)

func TestRenderMatrix_EmitsHeaderAndRows(t *testing.T) {
	out := RenderMatrix()
	if !strings.Contains(out, "| Capability | Community | Enterprise |") {
		t.Errorf("expected table header; got:\n%s", out)
	}
	// A known EE row must render as Enterprise-only.
	if !strings.Contains(out, "Clustering") {
		t.Errorf("expected a Clustering row; got:\n%s", out)
	}
	// The check mark / dash vocabulary must be present.
	if !strings.Contains(out, "✅") || !strings.Contains(out, "—") {
		t.Errorf("expected ✅ and — cell markers; got:\n%s", out)
	}
}

func TestMatrix_ExcludesHiddenTrading(t *testing.T) {
	for _, r := range Matrix() {
		if r.FeatureID == "trading-series" {
			t.Errorf("matrix row %q links to the hidden trading feature", r.Capability)
		}
		if strings.Contains(strings.ToLower(r.Capability), "trading") {
			t.Errorf("matrix row %q mentions trading (hidden from public docs)", r.Capability)
		}
	}
	if strings.Contains(strings.ToLower(RenderMatrix()), "trading") {
		t.Error("rendered matrix must not mention trading")
	}
}

func TestMemoryFirewallIsCommunity(t *testing.T) {
	for _, r := range Matrix() {
		if r.Capability == "Memory firewall" {
			if !r.Community {
				t.Fatal("Memory firewall must be a Community capability")
			}
			// Pin the CrossCheck exemption: the row has no FeatureID, so
			// CrossCheck does NOT derive its truth from the featuredoctor
			// registry. If a future edit adds a FeatureID, CrossCheck would
			// look for an EE probe, find none, and fail silently — guard
			// against that here. The firewall is gated at runtime (Postgres
			// eval repo present), not via featuredoctor.
			if r.FeatureID != "" {
				t.Fatal("Memory firewall must have no FeatureID — it is CrossCheck-exempt, gated at runtime not via featuredoctor")
			}
			return
		}
	}
	t.Fatal("Memory firewall row missing")
}

func TestCrossCheck_CleanOnRealRegistry(t *testing.T) {
	if ms := CrossCheck(featuredoctor.Registry()); len(ms) != 0 {
		t.Errorf("real curated matrix is inconsistent with the registry: %v", ms)
	}
}

func TestCrossCheckRows_FlagsEEFeatureMarkedCommunity(t *testing.T) {
	rows := []Row{{Capability: "X", Community: true, Enterprise: true, FeatureID: "x"}}
	features := []featuredoctor.Feature{{ID: "x", Edition: version.EditionEnterprise}}
	ms := crossCheckRows(rows, features)
	if len(ms) == 0 {
		t.Fatal("expected a mismatch: EE feature marked Community in the matrix")
	}
}

func TestCrossCheckRows_FlagsEEFeatureWithNoRow(t *testing.T) {
	features := []featuredoctor.Feature{{ID: "x", Edition: version.EditionEnterprise}}
	ms := crossCheckRows(nil, features)
	if len(ms) == 0 {
		t.Fatal("expected a mismatch: EE feature has no Enterprise row")
	}
}

func TestCrossCheckRows_FlagsRowLinkedToHiddenFeature(t *testing.T) {
	rows := []Row{{Capability: "T", Enterprise: true, FeatureID: "trading-series"}}
	features := []featuredoctor.Feature{{ID: "trading-series", Edition: version.EditionEnterprise}}
	ms := crossCheckRows(rows, features)
	if len(ms) == 0 {
		t.Fatal("expected a mismatch: matrix links a hidden feature")
	}
}

func TestCrossCheckRows_FlagsDanglingFeatureID(t *testing.T) {
	rows := []Row{{Capability: "Ghost", Enterprise: true, FeatureID: "ghost"}}
	ms := crossCheckRows(rows, nil)
	if len(ms) == 0 {
		t.Fatal("expected a mismatch: row links an unknown feature")
	}
}

func TestCrossCheckRows_HiddenFeatureWithoutRowIsClean(t *testing.T) {
	features := []featuredoctor.Feature{{ID: "trading-series", Edition: version.EditionEnterprise}}
	if ms := crossCheckRows(nil, features); len(ms) != 0 {
		t.Errorf("hidden feature with no row should be clean; got: %v", ms)
	}
}

func TestCrossCheckRows_CleanEEFeatureWithEERow(t *testing.T) {
	rows := []Row{{Capability: "X", Enterprise: true, FeatureID: "x"}}
	features := []featuredoctor.Feature{{ID: "x", Edition: version.EditionEnterprise}}
	if ms := crossCheckRows(rows, features); len(ms) != 0 {
		t.Errorf("consistent EE feature/row should be clean; got: %v", ms)
	}
}
