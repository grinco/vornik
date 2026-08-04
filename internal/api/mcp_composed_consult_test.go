package api

import (
	"context"
	"strings"
	"testing"

	a2aclient "vornik.io/vornik/internal/a2a/client"
	"vornik.io/vornik/internal/a2a/consult"
	"vornik.io/vornik/internal/config"
)

// The ComposedMCPExecutor must surface the consult provider's tools alongside
// the external set, and route mcp__consult__* execution to it (leaving other
// names to External/Builtin).
func TestComposedMCPExecutor_ConsultRouting(t *testing.T) {
	consultProvider := consult.New(
		map[string]config.A2APeer{"vornik_architecture": {URL: "https://h/a", Description: "arch expert"}},
		config.A2AConsultConfig{},
		a2aclient.New(),
		nil,
	)
	c := &ComposedMCPExecutor{Consult: consultProvider}

	// Tools surfaced.
	var found bool
	for _, tl := range c.Tools("proj") {
		if tl.Function.Name == "mcp__consult__vornik_architecture" {
			found = true
		}
	}
	if !found {
		t.Fatal("composed executor did not surface the consult tool")
	}

	// Execute routes to the consult provider (unknown peer name that IS a
	// consult-prefixed name → the provider's own not-configured message, proving
	// it reached the provider rather than falling through to External).
	out, err := c.Execute(context.Background(), "proj", "mcp__consult__ghost", `{"question":"q"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "ghost") {
		t.Fatalf("consult-prefixed name must route to the consult provider, got %q", out)
	}
}

// A non-consult name with no External wired falls through to the built-in
// "no external" error (i.e. it is NOT captured by the consult provider).
func TestComposedMCPExecutor_NonConsultFallsThrough(t *testing.T) {
	c := &ComposedMCPExecutor{Consult: consult.New(
		map[string]config.A2APeer{"x": {URL: "https://h/a"}}, config.A2AConsultConfig{}, a2aclient.New(), nil,
	)}
	if _, err := c.Execute(context.Background(), "proj", "mcp__pagedrop__publish", `{}`); err == nil {
		t.Fatal("a non-consult name with no External should error, not be swallowed by consult")
	}
}
