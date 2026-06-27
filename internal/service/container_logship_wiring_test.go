package service

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/contracts"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
)

// minimal fakes (the service package has no shared audit fake).
type lsFakeAdmin struct {
	persistence.AdminAuditRepository
}
type lsFakeTool struct {
	persistence.ToolAuditRepository
}

func (lsFakeAdmin) Insert(context.Context, *persistence.AdminAuditEntry) error { return nil }
func (lsFakeAdmin) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}
func (lsFakeTool) Log(context.Context, *persistence.ToolAuditEntry) error { return nil }
func (lsFakeTool) List(context.Context, persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	return nil, nil
}
func (lsFakeTool) CountByTool(context.Context, string) (map[string]int64, error) { return nil, nil }

func enabledSyslogForwardCfg() config.LogForwardConfig {
	return config.LogForwardConfig{
		Enabled: true,
		Scopes:  []string{"http"},
		Syslog:  config.LogForwardSyslogConfig{Enabled: true, Address: "127.0.0.1:1", Protocol: "udp"},
	}
}

// fakeForwarder is a CE-side test double for contracts.LogForwarder. The real
// adapter lives in internal/enterprise/logship (stripped from CE), so the
// service-package tests exercise the container's seam wiring through this fake
// — they assert the container CALLS the seam correctly, not what the EE router
// does internally (that is tested in internal/enterprise/logship).
type fakeForwarder struct {
	mu              sync.Mutex
	started         bool
	drained         bool
	scopes          []string
	decoratedRepos  *storage.Repositories
	decorateCalls   int
	appWriterCalled bool
}

func (f *fakeForwarder) Start() { f.mu.Lock(); f.started = true; f.mu.Unlock() }
func (f *fakeForwarder) Drain(context.Context) error {
	f.mu.Lock()
	f.drained = true
	f.mu.Unlock()
	return nil
}
func (f *fakeForwarder) SetScopes(s []string) { f.mu.Lock(); f.scopes = s; f.mu.Unlock() }
func (f *fakeForwarder) AppWriter(out io.Writer) io.Writer {
	f.mu.Lock()
	f.appWriterCalled = true
	f.mu.Unlock()
	return out // tee that is just the original writer for the test
}
func (f *fakeForwarder) DecorateAuditRepos(repos *storage.Repositories) {
	f.mu.Lock()
	f.decoratedRepos = repos
	f.decorateCalls++
	f.mu.Unlock()
}

var _ contracts.LogForwarder = (*fakeForwarder)(nil)

// fakeFactory returns a LogForwarderFactory that builds fwd when cfg.Enabled,
// mirroring the EE factory contract: (nil, nil) when disabled, (nil, err) on a
// forced error, (fwd, nil) otherwise.
func fakeFactory(fwd *fakeForwarder, forceErr error) func(config.LogForwardConfig, func(string) string) (contracts.LogForwarder, error) {
	return func(cfg config.LogForwardConfig, _ func(string) string) (contracts.LogForwarder, error) {
		if forceErr != nil {
			return nil, forceErr
		}
		if !cfg.Enabled {
			return nil, nil
		}
		return fwd, nil
	}
}

// Edition-gate tests for initLogship (Phase 2c: relocate behind
// contracts.LogForwarder).
//
// # Design
//
// initLogship is directly callable from a thin Container, so we test the
// Logship edition gate end-to-end: call initLogship with providers.Logship
// false/true and a nil/non-nil LogForwarderFactory and assert
// c.logshipForwarder nil / non-nil. The router internals are tested in
// internal/enterprise/logship; here we test only the container seam wiring.

// TestInitLogship_NilFactory_NoForwarder is the Phase-2c CE invariant: a
// Community build (LogForwarderFactory nil) constructs no forwarder, so
// c.logshipForwarder stays nil. This is the seam-level replacement for the
// pre-relocation "*logship.Router stays nil" assertion.
func TestInitLogship_NilFactory_NoForwarder(t *testing.T) {
	c := newTestLogshipContainer(t, ProviderSet{Logship: false}) // CommunityProviders default
	if err := c.initLogship(); err != nil {
		t.Fatalf("initLogship: %v", err)
	}
	if c.logshipForwarder != nil {
		t.Fatal("Community build must not construct a log forwarder")
	}
}

// newTestLogshipContainer builds a minimal Container for the logship seam
// tests with an enabled syslog forward config (so the edition/factory gate is
// the only thing that can suppress the forwarder).
func newTestLogshipContainer(t *testing.T, ps ProviderSet) *Container {
	t.Helper()
	cfg := &config.Config{}
	cfg.Logging.Forward = enabledSyslogForwardCfg()
	return &Container{
		Config:    cfg,
		Logger:    zerolog.Nop(),
		providers: ps,
		repos:     &storage.Repositories{AdminAudit: lsFakeAdmin{}, ToolAudit: lsFakeTool{}},
	}
}

// TestInitLogship_LogshipGate_FalseFlag_NoForwarder asserts the core CE
// invariant: when providers.Logship is false, initLogship returns nil and
// c.logshipForwarder remains nil even when Logging.Forward.Enabled is true and
// a factory is present. The edition flag is the OUTER gate (checked before the
// factory).
func TestInitLogship_LogshipGate_FalseFlag_NoForwarder(t *testing.T) {
	fwd := &fakeForwarder{}
	c := newTestLogshipContainer(t, ProviderSet{
		Logship:             false, // Community: logship omitted
		LogForwarderFactory: fakeFactory(fwd, nil),
	})
	if err := c.initLogship(); err != nil {
		t.Fatalf("initLogship with Logship=false: unexpected error: %v", err)
	}
	if c.logshipForwarder != nil {
		t.Error("providers.Logship=false → c.logshipForwarder must remain nil (edition gate must block before factory)")
	}
	if fwd.started {
		t.Error("factory output must not be started when the edition gate is closed")
	}
}

// TestInitLogship_LogshipGate_TrueFlag_BuildsForwarder asserts the EE happy
// path: Logship=true + a factory + Logging.Forward.Enabled wires the forwarder
// (Start called, repos decorated, app-writer installed).
func TestInitLogship_LogshipGate_TrueFlag_BuildsForwarder(t *testing.T) {
	fwd := &fakeForwarder{}
	c := newTestLogshipContainer(t, ProviderSet{
		Logship:             true, // Enterprise: logship included
		LogForwarderFactory: fakeFactory(fwd, nil),
	})
	if err := c.initLogship(); err != nil {
		t.Fatalf("initLogship with Logship=true: %v", err)
	}
	if c.logshipForwarder == nil {
		t.Fatal("Logship=true + factory + enabled → expected a forwarder")
	}
	if !fwd.started {
		t.Error("initLogship must Start the forwarder")
	}
	if !fwd.appWriterCalled {
		t.Error("initLogship must install the app-writer tap")
	}
	if fwd.decoratedRepos == nil {
		t.Error("initLogship must decorate the audit repos")
	}
	c.drainLogship(context.Background())
	if !fwd.drained {
		t.Error("drainLogship must drain the forwarder")
	}
}

// TestInitLogship_CommunityDefault_NoForwarder confirms the CommunityProviders()
// default leaves Logship=false / factory nil and therefore suppresses the
// forwarder even with forwarding enabled.
func TestInitLogship_CommunityDefault_NoForwarder(t *testing.T) {
	cfg := &config.Config{}
	cfg.Logging.Forward = enabledSyslogForwardCfg()
	c := &Container{Config: cfg, Logger: zerolog.Nop()}
	c.applyOptions(nil) // sets CommunityProviders — Logship=false, factory nil
	if err := c.initLogship(); err != nil {
		t.Fatalf("initLogship community default: unexpected error: %v", err)
	}
	if c.logshipForwarder != nil {
		t.Error("CommunityProviders default → c.logshipForwarder must remain nil")
	}
}

// TestInitLogship_FlagSetButFactoryNil_NoForwarder — a misassembled build
// (Logship=true but no factory) must behave like Community: no forwarding, no
// error.
func TestInitLogship_FlagSetButFactoryNil_NoForwarder(t *testing.T) {
	c := newTestLogshipContainer(t, ProviderSet{Logship: true /* LogForwarderFactory: nil */})
	if err := c.initLogship(); err != nil {
		t.Fatalf("initLogship: %v", err)
	}
	if c.logshipForwarder != nil {
		t.Error("Logship=true but nil factory → no forwarder")
	}
}

// TestInitLogship_FactoryReturnsNil_DisabledInConfig — the factory returning
// (nil, nil) (config disables forwarding) leaves the forwarder nil with no
// error: the zero-overhead disabled path.
func TestInitLogship_FactoryReturnsNil_DisabledInConfig(t *testing.T) {
	c := &Container{Config: &config.Config{}, Logger: zerolog.Nop()}
	c.providers = ProviderSet{Logship: true, LogForwarderFactory: fakeFactory(&fakeForwarder{}, nil)}
	if err := c.initLogship(); err != nil {
		t.Fatalf("initLogship disabled: %v", err)
	}
	if c.logshipForwarder != nil {
		t.Error("factory (nil,nil) → forwarder must remain nil")
	}
	// Drain/attach on the disabled path are safe no-ops.
	c.drainLogship(context.Background())
	c.attachLogshipMetrics()
}

// TestInitLogship_FactoryError_FailsBoot — a factory error (misconfigured
// enabled sink) propagates out of initLogship (fail closed).
func TestInitLogship_FactoryError_FailsBoot(t *testing.T) {
	c := newTestLogshipContainer(t, ProviderSet{
		Logship:             true,
		LogForwarderFactory: fakeFactory(nil, io.ErrUnexpectedEOF),
	})
	if err := c.initLogship(); err == nil {
		t.Fatal("expected boot error to propagate from the factory")
	}
	if c.logshipForwarder != nil {
		t.Error("a failed factory must leave the forwarder nil")
	}
}

// TestDecorateAuditRepos_NilForwarderIsNoOp — no forwarder → no-op (mirrors the
// pre-relocation nil-router guard).
func TestDecorateAuditRepos_NilForwarderIsNoOp(t *testing.T) {
	c := &Container{Logger: zerolog.Nop(), repos: &storage.Repositories{AdminAudit: lsFakeAdmin{}}}
	c.decorateAuditRepos() // must not panic, must not decorate
	if _, ok := c.repos.AdminAudit.(lsFakeAdmin); !ok {
		t.Error("repos must be untouched when no forwarder is wired")
	}
}

// TestDecorateAuditRepos_DelegatesToForwarder — when a forwarder is present,
// decorateAuditRepos delegates to it with the live repos.
func TestDecorateAuditRepos_DelegatesToForwarder(t *testing.T) {
	fwd := &fakeForwarder{}
	repos := &storage.Repositories{AdminAudit: lsFakeAdmin{}, ToolAudit: lsFakeTool{}}
	c := &Container{Logger: zerolog.Nop(), repos: repos, logshipForwarder: fwd}
	c.decorateAuditRepos()
	if fwd.decoratedRepos != repos {
		t.Error("decorateAuditRepos must pass the live repos to the forwarder")
	}
}

// TestDrainLogship_DeadlineCtx — draining with a cancelled ctx is safe (the
// forwarder's Drain decides; the container just forwards the ctx).
func TestDrainLogship_DeadlineCtx(t *testing.T) {
	fwd := &fakeForwarder{}
	c := &Container{Logger: zerolog.Nop(), logshipForwarder: fwd}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.drainLogship(ctx)
	if !fwd.drained {
		t.Error("drainLogship must call the forwarder's Drain")
	}
}
