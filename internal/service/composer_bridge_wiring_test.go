package service

// End-to-end regression test for the compose_automation wiring site
// (task 1.4): container_http.go's initHTTPServer builds the shared
// *projectwizard.Wizard, wraps it as a ComposerBridge, and calls
// c.Dispatcher.SetComposerBridge — but only when c.Dispatcher is
// non-nil (chat configured). Unit tests elsewhere pin
// newComposerBridge and Agent.SetComposerBridge in isolation; this
// test drives the REAL NewContainer boot path (same DB-free SQLite
// recipe as container_newcontainer_order_test.go) so a future refactor
// that moves this wiring out of initHTTPServer (or drops the
// c.Dispatcher nil-guard) fails a real test instead of only the
// isolated unit tests, which would keep passing against a mis-wired
// container.

import (
	"testing"

	"vornik.io/vornik/internal/config"
)

// newComposerWiringTestConfig mirrors newContainerForOrderTest's
// DB-free recipe (SQLite in-memory, "ui" profile) plus a Chat config
// that satisfies initChatHTTP's non-empty-field guards without ever
// making a network call — chat.NewClient only builds an HTTP client
// wrapper at construction time.
func newComposerWiringTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Database.Driver = "sqlite"
	cfg.Database.Path = ":memory:"
	cfg.Node.Profile = "ui"
	cfg.Storage.ArtifactsPath = t.TempDir()
	cfg.Chat.Enabled = true
	cfg.Chat.Provider = "http"
	cfg.Chat.Endpoint = "http://127.0.0.1:0/v1"
	cfg.Chat.APIKey = "test-key"
	cfg.Chat.Model = "test-model"
	return cfg
}

// TestNewContainer_WiresComposerBridgeOntoDispatcher_WhenComposerEnabled
// asserts that a full boot with chat configured + composer.enabled=true
// ends with compose_automation reporting Available=true in the
// dispatcher's InventoryTools() — proving SetComposerBridge actually
// ran with a non-nil bridge and enabled=true, not just that the
// isolated helpers behave correctly in unit tests.
func TestNewContainer_WiresComposerBridgeOntoDispatcher_WhenComposerEnabled(t *testing.T) {
	cfg := newComposerWiringTestConfig(t)
	cfg.Composer.Enabled = true

	c, err := NewContainer(cfg, isolatedConfigPath(t))
	if err != nil {
		t.Fatalf("NewContainer: unexpected error: %v", err)
	}
	if c.Dispatcher == nil {
		t.Fatal("c.Dispatcher is nil — chat was configured, so initDispatcher should have built it")
	}

	rows := c.Dispatcher.InventoryTools()
	for _, r := range rows {
		if r.Name == "compose_automation" {
			if !r.Available {
				t.Error("compose_automation Available = false with a chat client wired and composer.enabled=true")
			}
			return
		}
	}
	t.Fatal("compose_automation not present in dispatcher inventory")
}

// TestNewContainer_ComposerBridgeStaysDisabled_WhenComposerConfigDisabled
// is the soak-default regression lock: composer.enabled defaults to
// false, and a container built without flipping it must NOT offer
// compose_automation even though the bridge itself (the Wizard) is
// fully wired.
func TestNewContainer_ComposerBridgeStaysDisabled_WhenComposerConfigDisabled(t *testing.T) {
	cfg := newComposerWiringTestConfig(t)
	// cfg.Composer.Enabled left at its zero value (false) — the
	// Phase 3 soak default.

	c, err := NewContainer(cfg, isolatedConfigPath(t))
	if err != nil {
		t.Fatalf("NewContainer: unexpected error: %v", err)
	}
	if c.Dispatcher == nil {
		t.Fatal("c.Dispatcher is nil — chat was configured, so initDispatcher should have built it")
	}

	rows := c.Dispatcher.InventoryTools()
	for _, r := range rows {
		if r.Name == "compose_automation" {
			if r.Available {
				t.Error("compose_automation Available = true with composer.enabled left at its false default")
			}
			return
		}
	}
	t.Fatal("compose_automation not present in dispatcher inventory")
}
