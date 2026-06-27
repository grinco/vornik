package service

// CE-side tests for the logging.forward.scopes hot-reload seam
// (applyHotConfig). After Phase 2c the live logship router is an EE type, so
// these tests assert the container's hot-reload DECISION logic (when to call
// SetScopes, when to refuse, the nil-forwarder guard, the staging-slot clear)
// against a fake contracts.LogForwarder. The live router's actual scope-swap
// behaviour is tested in internal/enterprise/logship.

import (
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
)

// scopeRecorder is a contracts.LogForwarder that records SetScopes calls so a
// hot-reload test can assert whether (and with what) the live allowlist was
// updated. It embeds fakeForwarder for the other (unused) methods.
type scopeRecorder struct {
	fakeForwarder
	setCalls [][]string
}

func (s *scopeRecorder) SetScopes(scopes []string) {
	s.setCalls = append(s.setCalls, scopes)
	s.fakeForwarder.SetScopes(scopes)
}

// TestApplyHotConfig_HotSwapsLogshipScopes — a logging.forward.scopes edit on a
// running forwarder hot-swaps the live allowlist via SetScopes (no restart).
func TestApplyHotConfig_HotSwapsLogshipScopes(t *testing.T) {
	rec := &scopeRecorder{}
	c := &Container{
		Logger:           zerolog.Nop(),
		logshipForwarder: rec,
		stagedConfig: &config.Config{
			Logging: config.LoggingConfig{
				Forward: config.LogForwardConfig{Enabled: true, Scopes: []string{"llm"}},
			},
		},
	}
	c.applyHotConfig()

	if c.stagedConfig != nil {
		t.Error("applyHotConfig must clear the staging slot after applying")
	}
	if len(rec.setCalls) != 1 {
		t.Fatalf("expected exactly one SetScopes call, got %d", len(rec.setCalls))
	}
	if len(rec.setCalls[0]) != 1 || rec.setCalls[0][0] != "llm" {
		t.Errorf("SetScopes called with %v, want [llm]", rec.setCalls[0])
	}
}

// TestApplyHotConfig_LogshipDisableEditIsNoOpLive — an enabled:false edit on
// hot-reload must NOT touch the scope allowlist (a disable edit could otherwise
// covertly widen what ships). No SetScopes call.
func TestApplyHotConfig_LogshipDisableEditIsNoOpLive(t *testing.T) {
	rec := &scopeRecorder{}
	c := &Container{
		Logger:           zerolog.Nop(),
		logshipForwarder: rec,
		stagedConfig: &config.Config{
			Logging: config.LoggingConfig{
				Forward: config.LogForwardConfig{Enabled: false, Scopes: []string{"llm"}},
			},
		},
	}
	c.applyHotConfig()
	if len(rec.setCalls) != 0 {
		t.Errorf("disable edit must not call SetScopes; got %v", rec.setCalls)
	}
}

// TestApplyHotConfig_LogshipEmptyScopesRefusedLive — empty scopes means
// ship-ALL; on hot-reload that must be REFUSED live (no SetScopes) to avoid
// covertly expanding what's forwarded. ship-all requires a restart.
func TestApplyHotConfig_LogshipEmptyScopesRefusedLive(t *testing.T) {
	rec := &scopeRecorder{}
	c := &Container{
		Logger:           zerolog.Nop(),
		logshipForwarder: rec,
		stagedConfig: &config.Config{
			Logging: config.LoggingConfig{
				Forward: config.LogForwardConfig{Enabled: true, Scopes: nil},
			},
		},
	}
	c.applyHotConfig()
	if len(rec.setCalls) != 0 {
		t.Errorf("empty scopes (ship-all) must be refused live; got %v", rec.setCalls)
	}
}

// TestApplyHotConfig_NilLogshipForwarderIsNoOp — a scopes edit when forwarding
// is disabled (no forwarder wired) must be a no-op, never a panic.
func TestApplyHotConfig_NilLogshipForwarderIsNoOp(t *testing.T) {
	c := &Container{
		Logger: zerolog.Nop(),
		stagedConfig: &config.Config{
			Logging: config.LoggingConfig{
				Forward: config.LogForwardConfig{Scopes: []string{"llm"}},
			},
		},
	}
	c.applyHotConfig() // must not panic on a nil forwarder
	if c.stagedConfig != nil {
		t.Error("applyHotConfig must clear the staging slot even with no logship forwarder")
	}
}

// TestReload_BadConfigEdit_FailsClosed — the fail-closed half of the Tier 2
// config-hot-reload item. A bad config.yaml edit (one that fails to re-parse in
// the loader) must abort the reload cycle BEFORE activation, so the live
// subsystems keep their last-good config. Drives the real ConfigReloader
// loader→validator→activator seam with fakes.
func TestReload_BadConfigEdit_FailsClosed(t *testing.T) {
	reloader := config.NewConfigReloader(nil, zerolog.Nop())

	var validatorRan, activatorRan bool
	parseErr := errors.New("reload: re-parse config.yaml: yaml: line 3: did not find expected key")

	reloader.SetLoader(func() error { return parseErr })
	reloader.SetValidator(func() error { validatorRan = true; return nil })
	reloader.SetActivator(func() error { activatorRan = true; return nil })

	err := reloader.Reload()
	if err == nil {
		t.Fatal("Reload must return an error when the loader rejects a bad config edit")
	}
	if !errors.Is(err, parseErr) {
		t.Errorf("Reload error must wrap the loader parse error, got %v", err)
	}
	if validatorRan {
		t.Error("validator must NOT run after the loader fails (fail closed before validation)")
	}
	if activatorRan {
		t.Error("activator must NOT run after the loader fails (fail closed before activation)")
	}

	st := reloader.Status()
	if !st.HasErrors || len(st.Errors) == 0 {
		t.Error("reload status must record the loader error after a fail-closed reject")
	}
}
