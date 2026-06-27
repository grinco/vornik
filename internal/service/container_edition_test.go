package service

import (
	"testing"

	"vornik.io/vornik/internal/version"
)

func TestContainerEditionDefault(t *testing.T) {
	c := &Container{}
	if got := c.Edition(); got != version.DefaultEdition {
		t.Errorf("zero-value Container Edition() = %q, want %q", got, version.DefaultEdition)
	}
}

func TestContainerSetEditionNormalizes(t *testing.T) {
	c := &Container{}
	c.SetEdition("enterprise")
	if got := c.Edition(); got != version.EditionEnterprise {
		t.Errorf("Edition() = %q, want enterprise", got)
	}
	c.SetEdition("garbage")
	if got := c.Edition(); got != version.EditionCommunity {
		t.Errorf("unknown edition should normalize to community, got %q", got)
	}
}
