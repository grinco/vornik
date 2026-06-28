package onboarding

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/config"
)

type stubCommittedChecker struct {
	committed bool
	err       error
}

func (s stubCommittedChecker) HasCommitted(context.Context) (bool, error) {
	return s.committed, s.err
}

func TestDetector_UsesCommittedRowAsSourceOfTruth(t *testing.T) {
	d := Detector{
		Sessions: stubCommittedChecker{committed: true},
		Config:   &config.Config{},
	}
	status := d.Detect(context.Background())
	if !status.Onboarded || status.FreshInstall {
		t.Fatalf("expected committed install to be treated as onboarded, got %+v", status)
	}
	if status.Source != "durable" {
		t.Fatalf("expected durable source, got %q", status.Source)
	}
}

func TestDetector_CommittedRowWinsOverConfig(t *testing.T) {
	// Even when the heuristic would say "fresh" (empty config),
	// a committed row overrides.
	d := Detector{
		Sessions: stubCommittedChecker{committed: true},
		Config:   &config.Config{},
	}
	status := d.Detect(context.Background())
	if status.FreshInstall {
		t.Fatalf("committed row should override heuristic, got %+v", status)
	}
}

func TestDetector_FallsBackToHeuristicOnDBError(t *testing.T) {
	d := Detector{
		Sessions: stubCommittedChecker{err: errors.New("db unavailable")},
		Config:   &config.Config{},
	}
	status := d.Detect(context.Background())
	if !status.FreshInstall {
		t.Fatalf("expected heuristic fallback on DB error, got %+v", status)
	}
	if status.Source != "heuristic" {
		t.Fatalf("expected heuristic source, got %q", status.Source)
	}
}

func TestDetector_HeuristicDetectsUnwiredChat(t *testing.T) {
	d := Detector{Config: &config.Config{}}
	status := d.Detect(context.Background())
	if !status.FreshInstall {
		t.Fatalf("expected fresh install when chat is unwired, got %+v", status)
	}
	if len(status.Reasons) == 0 {
		t.Fatal("expected heuristic reasons")
	}
}

func TestDetector_HeuristicAcceptsHealthyConfig(t *testing.T) {
	d := Detector{
		Config: &config.Config{
			Chat: config.ChatConfig{
				Endpoint: "http://chat.example",
				Model:    "gpt-4.1",
			},
			Telegram: config.TelegramConfig{
				DispatcherProjectID: "assistant",
			},
			Memory: config.MemoryConfig{
				Enabled:           true,
				EmbeddingModel:    "text-embedding-3-small",
				EmbeddingEndpoint: "http://embed.example",
			},
		},
	}
	status := d.Detect(context.Background())
	if status.FreshInstall || !status.Onboarded {
		t.Fatalf("expected healthy config to not be fresh, got %+v", status)
	}
}

func TestDetector_MemoryEnabledWithoutEmbeddingModel(t *testing.T) {
	d := Detector{
		Config: &config.Config{
			Chat: config.ChatConfig{
				Endpoint: "http://chat.example",
				Model:    "gpt-4.1",
			},
			Telegram: config.TelegramConfig{
				DispatcherProjectID: "assistant",
			},
			Memory: config.MemoryConfig{
				Enabled:           true,
				EmbeddingModel:    "", // missing
				EmbeddingEndpoint: "http://embed.example",
			},
		},
	}
	status := d.Detect(context.Background())
	if !status.FreshInstall {
		t.Fatalf("expected fresh install when embedding model is missing, got %+v", status)
	}
	found := false
	for _, r := range status.Reasons {
		if r == "memory enabled without embedding model" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'memory enabled without embedding model' reason, got %v", status.Reasons)
	}
}

func TestDetector_MemoryEnabledWithoutEmbeddingEndpoint(t *testing.T) {
	d := Detector{
		Config: &config.Config{
			Chat: config.ChatConfig{
				Endpoint: "http://chat.example",
				Model:    "gpt-4.1",
			},
			Telegram: config.TelegramConfig{
				DispatcherProjectID: "assistant",
			},
			Memory: config.MemoryConfig{
				Enabled:           true,
				EmbeddingModel:    "text-embedding-3-small",
				EmbeddingEndpoint: "", // missing
			},
		},
	}
	status := d.Detect(context.Background())
	if !status.FreshInstall {
		t.Fatalf("expected fresh install when embedding endpoint is missing, got %+v", status)
	}
	found := false
	for _, r := range status.Reasons {
		if r == "memory enabled without embedding endpoint" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'memory enabled without embedding endpoint' reason, got %v", status.Reasons)
	}
}

func TestDetector_MemoryDisabledIsFine(t *testing.T) {
	d := Detector{
		Config: &config.Config{
			Chat: config.ChatConfig{
				Endpoint: "http://chat.example",
				Model:    "gpt-4.1",
			},
			Telegram: config.TelegramConfig{
				DispatcherProjectID: "assistant",
			},
			Memory: config.MemoryConfig{
				Enabled: false,
			},
		},
	}
	status := d.Detect(context.Background())
	if status.FreshInstall {
		t.Fatalf("expected non-fresh when memory is disabled and other config is fine, got %+v", status)
	}
}

func TestDetector_NilConfig_ReturnsFreshWithReason(t *testing.T) {
	d := Detector{}
	status := d.Detect(context.Background())
	if !status.FreshInstall {
		t.Fatalf("expected fresh install when config is nil, got %+v", status)
	}
	if status.Source != "heuristic" {
		t.Fatalf("expected heuristic source, got %q", status.Source)
	}
	found := false
	for _, r := range status.Reasons {
		if r == "config unavailable" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'config unavailable' reason, got %v", status.Reasons)
	}
}

func TestDetector_NilSessions_SkipsDurableCheck(t *testing.T) {
	d := Detector{
		Sessions: nil, // no DB repo wired
		Config: &config.Config{
			Chat: config.ChatConfig{
				Endpoint: "http://chat.example",
				Model:    "gpt-4.1",
			},
			Telegram: config.TelegramConfig{
				DispatcherProjectID: "assistant",
			},
		},
	}
	status := d.Detect(context.Background())
	if status.Source != "heuristic" {
		t.Fatalf("expected heuristic source when sessions is nil, got %q", status.Source)
	}
}
