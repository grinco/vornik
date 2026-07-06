package service

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/templates"
)

func TestTemplateOptionsResolver_RoutesSources(t *testing.T) {
	r := NewTemplateOptionsResolver(
		func(_ context.Context) ([]string, error) { return []string{"slack", "github"}, nil },
		func(_ context.Context) ([]string, error) { return []string{"m1"}, nil },
	)
	got, err := r.ResolveOptions(templates.OptionsSourceMCPRegistry)
	if err != nil || len(got) != 2 || got[0] != "slack" {
		t.Fatalf("mcp_registry: %v %v", got, err)
	}
	got, err = r.ResolveOptions(templates.OptionsSourceModels)
	if err != nil || len(got) != 1 || got[0] != "m1" {
		t.Fatalf("models: %v %v", got, err)
	}
	if _, err := r.ResolveOptions("nonsense"); err == nil {
		t.Fatal("unknown source must error")
	}
}

func TestTemplateOptionsResolver_PropagatesSourceError(t *testing.T) {
	r := NewTemplateOptionsResolver(
		func(_ context.Context) ([]string, error) { return nil, errors.New("mcp down") },
		nil, // nil models fn = source unavailable
	)
	if _, err := r.ResolveOptions(templates.OptionsSourceMCPRegistry); err == nil {
		t.Fatal("source error must propagate")
	}
	if _, err := r.ResolveOptions(templates.OptionsSourceModels); err == nil {
		t.Fatal("nil source fn must error, not panic")
	}
}
