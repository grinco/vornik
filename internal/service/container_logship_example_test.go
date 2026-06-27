package service

// Tests that the SHIPPED example config (configs/vornik.yaml.example) parses
// AND that its server.real_ip block survives the daemon's wiring funcs. After
// Phase 2c the logging.forward → logship.Config translation is an EE concern
// (internal/enterprise/logship), so the example's forward-block translation is
// asserted there; here we keep the CE-visible portion (real_ip) plus a CE
// initLogship no-op smoke (Community wires no factory, so it must stay nil).

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/httpx/realip"
)

// loadExampleConfig loads the shipped example via the flag-free LoadFromPath
// loader (no global os.Args mutation, so it is safe to run in parallel with
// the config-package example test).
func loadExampleConfig(t *testing.T) *config.Config {
	t.Helper()
	path := filepath.Join("..", "..", "configs", "vornik.yaml.example")
	cfg, err := config.LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath(%s): %v", path, err)
	}
	return cfg
}

// Test #4: the example's real_ip block parses and validates, and the
// forward.scopes allowlist round-trips into the parsed config block (it is
// what the operator edits to gate forwarding).
func TestExampleConfig_NewBlocksTranslateAndValidate(t *testing.T) {
	cfg := loadExampleConfig(t)

	// (a) real_ip: every shipped CIDR must parse through realip.NewConfig.
	// A typo'd trusted_proxies entry in the example would surface here.
	r := cfg.ResolveRealIP()
	if !r.Enabled {
		t.Fatalf("example real_ip should be enabled, got %+v", r)
	}
	if len(r.TrustedProxies) == 0 {
		t.Fatal("example real_ip should ship a non-empty trusted_proxies list")
	}
	rc, err := realip.NewConfig(r.Enabled, r.TrustedProxies, r.Header)
	if err != nil {
		t.Fatalf("realip.NewConfig rejected a CIDR shipped in the example: %v", err)
	}
	if len(rc.TrustedProxies) != len(r.TrustedProxies) {
		t.Fatalf("realip parsed %d CIDRs, example lists %d", len(rc.TrustedProxies), len(r.TrustedProxies))
	}

	// (b) The forward.scopes allowlist should round-trip into the parsed
	// config block (it is what the operator edits to gate forwarding). The
	// config→logship.Config translation is asserted in
	// internal/enterprise/logship (Phase 2c relocation).
	if len(cfg.Logging.Forward.Scopes) == 0 {
		t.Fatal("example forward.scopes should be a non-empty allowlist")
	}
}

// Test #5: the real_ip wiring func accepts the parsed example without an init
// error, and the CE initLogship is a no-op on the example (Community wires no
// LogForwarderFactory, so even with forward.enabled the forwarder stays nil).
func TestExampleConfig_WiringFuncsAcceptIt(t *testing.T) {
	cfg := loadExampleConfig(t)

	// real_ip wiring (mirrors what NewContainer does at boot).
	r := cfg.ResolveRealIP()
	if _, err := realip.NewConfig(r.Enabled, r.TrustedProxies, r.Header); err != nil {
		t.Fatalf("real_ip wiring rejected the example: %v", err)
	}

	// CE initLogship against the example: with no factory wired (Community)
	// this is a true no-op — exactly the boot path a CE binary takes.
	c := &Container{Config: cfg, Logger: zerolog.Nop()}
	c.applyOptions(nil) // CommunityProviders — Logship=false, factory nil
	if err := c.initLogship(); err != nil {
		t.Fatalf("CE initLogship rejected the example config: %v", err)
	}
	if c.logshipForwarder != nil {
		t.Fatal("Community initLogship must not build a forwarder")
	}
}

// Test #8: CE forward path = zero overhead. With no factory wired, initLogship
// leaves the root logger untouched (the disabled path must NOT install the
// MultiWriter app-tap wrapper). zerolog's Logger is not == comparable, so we
// prove "untouched" behaviourally: back the logger with a single buffer and
// assert exactly ONE copy of a probe line lands there.
func TestForwardDisabled_ZeroOverhead(t *testing.T) {
	var buf bytes.Buffer
	cfg := loadExampleConfig(t)
	c := &Container{Config: cfg, Logger: zerolog.New(&buf)}
	c.applyOptions(nil) // CommunityProviders — no factory
	if err := c.initLogship(); err != nil {
		t.Fatalf("initLogship(CE): %v", err)
	}
	if c.logshipForwarder != nil {
		t.Fatal("CE path must not build a forwarder")
	}
	c.Logger.Info().Str("component", "http").Msg("probe")
	got := buf.String()
	if n := strings.Count(got, `"probe"`); n != 1 {
		t.Fatalf("CE path must leave the logger writing once to its original writer; got %d copies in %q", n, got)
	}
}
