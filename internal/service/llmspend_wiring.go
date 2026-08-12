package service

import (
	"vornik.io/vornik/internal/llmspend"
)

// llmSpend builds the billing recorder for one component.
//
// One helper rather than four lines per component, because the four-line shape is
// what let a billing wiring line be deleted without anything noticing: it was
// post-construction, optional, and identical everywhere. Now the recorder is a
// constructor argument, so a component cannot be built without one, and this
// helper makes supplying it a single expression.
//
// source and role are the task_llm_usage identity. Passing them here — at the
// wiring site, next to the component being built — keeps the mapping from
// component to ledger identity visible in one file rather than buried in each
// component's private constants.
//
// A nil repo yields llmspend.Disabled() via llmspend.New, which is the honest
// outcome for a deployment with no ledger: deliberately not billing, rather than
// an enabled recorder holding nil.
func (c *Container) llmSpend(source, role string) llmspend.Recorder {
	return llmspend.New(
		c.repos.LLMUsage,
		c.pricingTable,
		source,
		role,
		llmspend.WithLogger(c.Logger),
		llmspend.WithFailureSink(c.llmSpendFailureSink()),
	)
}

// llmSpendFailureSink registers the shared failure counter on first use.
//
// Registered once per process, not once per component: promauto panics on a
// duplicate registration, and every component asking for its own counter would
// turn a metric into a startup crash.
func (c *Container) llmSpendFailureSink() llmspend.FailureSink {
	if c.llmSpendFailures == nil {
		c.llmSpendFailures = llmspend.NewPrometheusFailureSink(nil)
	}
	return c.llmSpendFailures
}
