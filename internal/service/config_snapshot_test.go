package service

import (
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
)

// Regression for "config show shows a config the daemon is no longer running":
// the reload activator publishes the staged config and its provenance into
// the snapshot the API serves, even though c.Config is never swapped
// (resolved-config provenance design §4.1).
func TestApplyHotConfig_PublishesTheReloadedSnapshot(t *testing.T) {
	boot := config.DefaultConfig()
	boot.Logging.Level = "boot"
	staged := config.DefaultConfig()
	staged.Logging.Level = "reloaded"
	prov := &config.Provenance{Path: "/tmp/config.yaml", Values: map[string]config.ValueOrigin{
		"logging.level": {Origin: config.OriginFile, Source: "config.yaml"},
	}}
	c := &Container{Config: boot, Logger: zerolog.Nop(), stagedConfig: staged, stagedProvenance: prov}
	c.configSnapshot.Store(boot, nil)

	c.applyHotConfig()

	snap := c.configSnapshot.Load()
	if snap == nil || snap.Config.Logging.Level != "reloaded" || snap.Provenance != prov {
		t.Fatalf("snapshot after reload = %+v, want the staged config and its provenance", snap)
	}
	if c.Config.Logging.Level != "boot" {
		t.Error("c.Config must not be swapped — many goroutines hold the pointer")
	}
	if c.stagedConfig != nil || c.stagedProvenance != nil {
		t.Error("staged state must be consumed")
	}
	// A reload with nothing staged leaves the snapshot alone.
	c.applyHotConfig()
	if c.configSnapshot.Load().Config.Logging.Level != "reloaded" {
		t.Error("an empty apply must not blank the snapshot")
	}
}
