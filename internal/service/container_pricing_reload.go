package service

import (
	"fmt"

	"vornik.io/vornik/internal/pricing"
)

// Pricing table hot-reload (17-config-and-hot-reload.md, "Pricing table
// hot-reload and saying what a reload did NOT cover", 2026-09-19).
//
// THE INCIDENT. A corrected pricing.yaml was deployed to both trees and
// `vornikctl config reload` was run against each. Both answered cleanly — Last
// reload 1s ago, Pending activation false, no validation errors — and the new
// rates were not in force. `pricing.Load` ran once in NewContainer and nothing
// in the reload path touched it, so the table was restart-only in the way
// daemon-level MCP config is, and, unlike that one, nothing said so anywhere.
// The divergence surfaced only because someone compared a recorded cost against
// the provider's own invoice.
//
// The split below mirrors the config.yaml path exactly: the LOADER parses and
// fails the whole reload before activation, the ACTIVATOR applies. Two phases,
// because a parse failure must never leave the deployment half-applied.

// stagePricingTable re-parses pricing.yaml for the pending reload. A parse
// failure aborts the WHOLE reload before activation and the running rates keep
// serving — the same fail-closed rule boot uses, where malformed pricing YAML
// is fatal precisely because silent cost miscounting is worse than a loud
// failure.
//
// An ABSENT file is not an error: "no file, no cost metric" is the documented
// opt-in at boot, and a reload must not convert it into a reload failure.
func (c *Container) stagePricingTable() error {
	if c == nil || c.pricingPath == "" {
		return nil
	}
	staged, err := pricing.Load(c.pricingPath)
	if err != nil {
		return fmt.Errorf("reload: re-parse pricing.yaml: %w", err)
	}
	c.stagedPricing = staged
	return nil
}

// applyStagedPricing swaps the staged rates into the LIVE table and clears the
// staging slot.
//
// Into the live table, not over the pointer: c.pricingTable is captured at
// construction by the executor's cost recorder, the judge runner and the chat
// model catalogs, and two of those three have no setter at all. Executor.
// SetPricing exists and calls itself reload-safe, but it has no callers and
// assigns a plain field, so calling it from the reload goroutine would be a
// data race rather than a swap. pricing.Table.Replace moves the numbers behind
// an atomic snapshot instead, which every holder sees and no reader can tear.
func (c *Container) applyStagedPricing() {
	if c == nil {
		return
	}
	staged := c.stagedPricing
	c.stagedPricing = nil
	if staged == nil || c.pricingTable == nil {
		return
	}
	c.pricingTable.Replace(staged)
	if c.Logger.GetLevel() <= 1 { // Debug/Info
		c.Logger.Info().Str("path", c.pricingPath).Int("models", len(staged.IDs())).
			Msg("hot-reload: pricing table swapped; new rates apply from the next call")
	}
}
