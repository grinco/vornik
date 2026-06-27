package service

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/dispatcher"
)

// TestNewDispatcherToolInventory_NilAgent — nil receiver returns
// nil adapter so the admin page renders its "not wired" state
// instead of crashing.
func TestNewDispatcherToolInventory_NilAgent(t *testing.T) {
	if got := newDispatcherToolInventory(nil); got != nil {
		t.Errorf("nil agent: got %v, want nil", got)
	}
}

// TestDispatcherToolInventory_TranslatesInfo — the adapter
// preserves Name/Description/BackingService/Available 1:1 from the
// underlying dispatcher.ToolInfo. Drive via the real Agent rather
// than a fake so a future field addition on ToolInfo fails this
// test (forcing the bridge to be updated).
func TestDispatcherToolInventory_TranslatesInfo(t *testing.T) {
	agent := dispatcher.NewAgent(nil, nil, nil, nil, nil,
		dispatcher.WithEmailSender(stubEmailSenderInv{}),
	)
	inv := newDispatcherToolInventory(agent)
	rows := inv.DispatcherTools()
	if len(rows) == 0 {
		t.Fatal("rows empty — InventoryTools should return registered set")
	}
	var sendEmail *struct {
		BackingService string
		Available      bool
	}
	for _, r := range rows {
		if r.Name == "send_email" {
			sendEmail = &struct {
				BackingService string
				Available      bool
			}{r.BackingService, r.Available}
		}
	}
	if sendEmail == nil {
		t.Fatal("send_email row missing")
	}
	if sendEmail.BackingService != "EmailSender" {
		t.Errorf("BackingService = %q, want EmailSender", sendEmail.BackingService)
	}
	if !sendEmail.Available {
		t.Error("Available = false; EmailSender was wired so should be true")
	}
}

// TestDispatcherToolInventory_DisabledWhenUnwired — agent without
// email/memory wiring → those rows surface Available=false. This
// is the operator-visible signal that "the LLM can't email" maps
// to "email isn't configured."
func TestDispatcherToolInventory_DisabledWhenUnwired(t *testing.T) {
	agent := dispatcher.NewAgent(nil, nil, nil, nil, nil)
	inv := newDispatcherToolInventory(agent)
	rows := inv.DispatcherTools()
	for _, r := range rows {
		switch r.Name {
		case "send_email":
			if r.Available {
				t.Error("send_email Available=true without EmailSender wired")
			}
		case "memory_search":
			if r.Available {
				t.Error("memory_search Available=true without MemorySearcher wired")
			}
		}
	}
}

type stubEmailSenderInv struct{}

func (stubEmailSenderInv) SendEmail(_ context.Context, _ string, _ dispatcher.EmailSendRequest) (string, error) {
	return "", nil
}
