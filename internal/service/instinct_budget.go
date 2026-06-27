package service

// instinct_budget.go — CE container method that resolves the active
// contracts.InstinctBudgetResolver without importing internal/instinct.
//
// In EE builds, enterprise.Providers() sets providers.InstinctBudgetFactory to
// enterprise/instinct.NewInstinctBudgetResolver; the method calls that factory
// the first time the instinct repo is available. Community builds leave the
// factory nil — the method returns nil and the executor skips budget resolution.

import (
	"vornik.io/vornik/internal/contracts"
)

// instinctBudgetResolver returns the active contracts.InstinctBudgetResolver for
// the container. On EE builds (providers.InstinctBudgetFactory non-nil), if the
// field has not been upgraded yet and the instinct repo is ready, the method
// constructs and caches a real DB-backed resolver via the factory. Community
// builds return nil; the executor nil-guards on every call path.
func (c *Container) instinctBudgetResolver() contracts.InstinctBudgetResolver {
	if c.providers.InstinctBudget != nil {
		return c.providers.InstinctBudget
	}
	// Community: no factory → no resolver.
	if c.providers.InstinctBudgetFactory == nil {
		return nil
	}
	if c.repos == nil || c.repos.Instincts == nil {
		return nil
	}
	// Upgrade the EE placeholder with a real DB-backed resolver for this container.
	c.providers.InstinctBudget = c.providers.InstinctBudgetFactory(c.repos.Instincts)
	return c.providers.InstinctBudget
}
