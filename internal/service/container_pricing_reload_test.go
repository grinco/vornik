package service

import (
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/pricing"
)

// 2026-09-17. The corrected pricing.yaml was deployed to both trees,
// `vornikctl config reload` answered cleanly on each — "Pending activation:
// false", no validation errors — and the old rates kept billing, because
// pricing.Load ran once in NewContainer and the reload path never touched it.
// The arm that started three minutes later recorded $13.758 where the
// corrected table gives $1.817.

func writePricing(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "pricing.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPricingReload_NewRatesAreInForceAfterActivation(t *testing.T) {
	dir := t.TempDir()
	path := writePricing(t, dir, "models:\n  \"m1\": { input: 1.00, output: 3.00 }\ndefault: { input: 1.00, output: 3.00 }\n")

	table, err := pricing.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c := &Container{pricingTable: table, pricingPath: path}

	// A consumer that captured the pointer at construction, as the executor's
	// cost recorder and the judge runner do.
	holder := c.pricingTable

	writePricing(t, dir, "models:\n  \"m1\": { input: 0.35, output: 2.75 }\ndefault: { input: 1.00, output: 3.00 }\n")
	if err := c.stagePricingTable(); err != nil {
		t.Fatalf("stage: %v", err)
	}
	c.applyStagedPricing()

	if got := holder.CostUSD("m1", 1_000_000, 0); got != 0.35 {
		t.Fatalf("the reload did not reach a pointer holder: want 0.35, got %v", got)
	}
}

// Fail closed: a malformed pricing.yaml aborts the reload at the LOADER, before
// anything activates, and the running rates keep serving. This matches boot,
// where malformed pricing YAML is fatal precisely because silent cost
// miscounting is worse than a loud failure.
func TestPricingReload_MalformedFileAbortsAndKeepsOldRates(t *testing.T) {
	dir := t.TempDir()
	path := writePricing(t, dir, "models:\n  \"m1\": { input: 1.00, output: 3.00 }\ndefault: {}\n")
	table, err := pricing.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c := &Container{pricingTable: table, pricingPath: path}

	writePricing(t, dir, "models:\n  m1: { input: [this is not a rate\n")
	if err := c.stagePricingTable(); err == nil {
		t.Fatal("a malformed pricing.yaml must abort the reload")
	}

	c.applyStagedPricing() // nothing staged; must be a no-op
	if got := c.pricingTable.CostUSD("m1", 1_000_000, 0); got != 1.00 {
		t.Fatalf("a failed parse changed the live rates: %v", got)
	}
}

// An absent pricing.yaml is the documented "no file, no cost metric" case at
// boot, and a reload must not turn it into a reload failure.
func TestPricingReload_AbsentFileIsNotAReloadFailure(t *testing.T) {
	c := &Container{pricingPath: filepath.Join(t.TempDir(), "pricing.yaml")}
	if err := c.stagePricingTable(); err != nil {
		t.Fatalf("absent pricing.yaml must not fail a reload: %v", err)
	}
}

// A daemon with no pricing path wired must not panic on the reload path.
func TestPricingReload_NoPathIsANoOp(t *testing.T) {
	c := &Container{}
	if err := c.stagePricingTable(); err != nil {
		t.Fatalf("want no error, got %v", err)
	}
	c.applyStagedPricing()
}

// Applying twice must not re-apply a stale staged table: the slot is cleared,
// the same way applyHotConfig clears its staged config.
func TestPricingReload_StagedSlotIsClearedAfterApply(t *testing.T) {
	dir := t.TempDir()
	path := writePricing(t, dir, "models:\n  \"m1\": { input: 1.00, output: 1.00 }\ndefault: {}\n")
	table, _ := pricing.Load(path)
	c := &Container{pricingTable: table, pricingPath: path}

	writePricing(t, dir, "models:\n  \"m1\": { input: 2.00, output: 2.00 }\ndefault: {}\n")
	if err := c.stagePricingTable(); err != nil {
		t.Fatal(err)
	}
	c.applyStagedPricing()

	// A later reload whose loader never ran must not re-apply anything.
	writePricing(t, dir, "models:\n  \"m1\": { input: 3.00, output: 3.00 }\ndefault: {}\n")
	c.applyStagedPricing()

	if got := c.pricingTable.CostUSD("m1", 1_000_000, 0); got != 2.00 {
		t.Fatalf("stale staged table re-applied or slot not cleared: %v", got)
	}
}
