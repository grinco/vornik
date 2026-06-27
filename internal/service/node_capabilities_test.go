package service

import (
	"testing"

	"vornik.io/vornik/internal/config"
)

func TestContainerCapabilities_DefaultRunsWorkers(t *testing.T) {
	c := &Container{Config: &config.Config{}}
	if !c.capabilities().RunWorkers {
		t.Fatal("default (absent node block) must run workers")
	}
}

func TestContainerCapabilities_WebhookProfileNoWorkers(t *testing.T) {
	c := &Container{Config: &config.Config{Node: config.NodeConfig{Profile: "webhook",
		Relay: config.RelayConfig{Upstream: "https://w:8443", ClientCert: "c", ClientKey: "k", CA: "ca"}}}}
	caps := c.capabilities()
	if caps.RunWorkers {
		t.Fatal("webhook profile must NOT run workers")
	}
	if !caps.ServeWebhooks || !caps.RelayMode {
		t.Fatalf("webhook profile must serve webhooks in relay mode, got %+v", caps)
	}
}
