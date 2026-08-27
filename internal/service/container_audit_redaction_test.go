package service

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
)

type nopToolAudit struct {
	persistence.ToolAuditRepository
}

func newRedactionContainer(enabled bool) *Container {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	c.Config.Secrets.Enabled = enabled
	c.repos = &storage.Repositories{ToolAudit: &nopToolAudit{}}
	return c
}

func TestInitToolAuditRedactionDecorates(t *testing.T) {
	c := newRedactionContainer(true)
	c.initToolAuditRedaction()
	if _, ok := c.repos.ToolAudit.(*auditredact.Repo); !ok {
		t.Fatalf("tool-audit repo was not decorated: %T", c.repos.ToolAudit)
	}
}

// The observability rebuild replaces c.repos with fresh undecorated handles and
// then re-runs this. It must re-decorate, or the realtime tool-audit handler
// built by the SECOND initHTTPServer writes plaintext credentials again — the
// defect the seam exists to close, reintroduced by a rebuild that has nothing
// to do with secrets.
func TestInitToolAuditRedactionSurvivesRepoRebuild(t *testing.T) {
	c := newRedactionContainer(true)
	c.initToolAuditRedaction()

	c.repos = &storage.Repositories{ToolAudit: &nopToolAudit{}} // the rebuild
	if _, ok := c.repos.ToolAudit.(*auditredact.Repo); ok {
		t.Fatal("precondition: the rebuilt repo must start undecorated")
	}
	c.initToolAuditRedaction()
	if _, ok := c.repos.ToolAudit.(*auditredact.Repo); !ok {
		t.Fatal("the rebuilt repo was left undecorated — plaintext would reach tool_audit_log")
	}
}

// Double-wrapping would scan every row twice for no benefit.
func TestInitToolAuditRedactionIsIdempotent(t *testing.T) {
	c := newRedactionContainer(true)
	c.initToolAuditRedaction()
	first := c.repos.ToolAudit
	c.initToolAuditRedaction()
	if c.repos.ToolAudit != first {
		t.Error("a second call must not wrap the decorator in another decorator")
	}
}

// secrets.enabled=false is an explicit operator bypass. Until 2026-08-27 it
// left the repo UNDECORATED, so nothing could count the rows it let through and
// the bypass announced itself once, at boot. This test previously asserted that
// old contract; it is the specification being changed (Finding C of the
// 2026-08-26 silent-controls audit).
func TestInitToolAuditRedactionWrapsBypassWhenSecretsDisabled(t *testing.T) {
	c := newRedactionContainer(false)
	c.initToolAuditRedaction()
	repo, ok := c.repos.ToolAudit.(*auditredact.Repo)
	if !ok {
		t.Fatalf("a bypass must still be decorated so it can be counted: %T", c.repos.ToolAudit)
	}
	if got := repo.BypassReason(); got != auditredact.ReasonSecretsDisabled {
		t.Errorf("BypassReason() = %q, want %q", got, auditredact.ReasonSecretsDisabled)
	}
}

// A detector that fails to construct while secrets.enabled=true is the daemon
// failing to honour the config it was given — distinct from the operator having
// turned scanning off, and counted separately for exactly that reason.
func TestInitToolAuditRedactionCountsDetectorFailure(t *testing.T) {
	c := newRedactionContainer(true)
	// An unparseable custom pattern is the cheapest real construction failure.
	c.Config.Secrets.Patterns.Custom = []config.SecretsPatternConfig{{Name: "broken", Regex: "("}}
	c.initToolAuditRedaction()
	repo, ok := c.repos.ToolAudit.(*auditredact.Repo)
	if !ok {
		t.Fatalf("a failed detector must still be decorated so it can be counted: %T", c.repos.ToolAudit)
	}
	if got := repo.BypassReason(); got != auditredact.ReasonDetectorUnavailable {
		t.Errorf("BypassReason() = %q, want %q", got, auditredact.ReasonDetectorUnavailable)
	}
}

// D3: the guard must tell a real seam from a pass-through. A bypass wrapper
// built before the config could produce a detector would otherwise pin the
// fleet to unscanned writes, and the counter would honestly report a bypass the
// rebuild should have fixed.
func TestInitToolAuditRedactionUpgradesABypass(t *testing.T) {
	c := newRedactionContainer(false)
	c.initToolAuditRedaction()
	if c.repos.ToolAudit.(*auditredact.Repo).BypassReason() == auditredact.ReasonNone {
		t.Fatal("precondition: the first wrap must be a bypass")
	}

	c.Config.Secrets.Enabled = true
	c.initToolAuditRedaction()
	repo, ok := c.repos.ToolAudit.(*auditredact.Repo)
	if !ok {
		t.Fatalf("upgrade lost the decorator: %T", c.repos.ToolAudit)
	}
	if got := repo.BypassReason(); got != auditredact.ReasonNone {
		t.Errorf("bypass was not upgraded: BypassReason() = %q, want empty", got)
	}
	if _, nested := repo.Inner().(*auditredact.Repo); nested {
		t.Error("the upgrade nested the real seam over the bypass — every row would be counted twice")
	}
}

// D3 fact 3: instance A (captured by initScheduler/initDispatcher) and instance
// B (built by the post-observability rebuild) must count into the SAME holder,
// or the denominator covers roughly half the rows while presenting as whole.
func TestBothWriterPathsShareOneHolder(t *testing.T) {
	c := newRedactionContainer(true)
	c.initToolAuditRedaction()
	instanceA := c.repos.ToolAudit.(*auditredact.Repo)

	c.repos = &storage.Repositories{ToolAudit: &nopToolAudit{}} // the rebuild
	c.initToolAuditRedaction()
	instanceB := c.repos.ToolAudit.(*auditredact.Repo)

	if instanceA == instanceB {
		t.Fatal("precondition: the rebuild must produce a second instance")
	}
	if c.auditRedactMetrics == nil {
		t.Fatal("no shared holder was created")
	}
	if instanceA.Metrics() != c.auditRedactMetrics || instanceB.Metrics() != c.auditRedactMetrics {
		t.Error("the two live decorator instances do not share one census holder")
	}
}

// The ordering invariant behind the pre-attach tally (round-1 review F2): no
// row may reach the seam between the wrap at container.go:834 and Attach in
// initHTTPServer. Rows arrive only over HTTP and the listener starts after the
// observability phase, so the expected pre-attach count is zero.
//
// This guards the wiring phase specifically: it fails if a future init step
// writes a tool-audit row while the container is still being built. It cannot
// prove the invariant for a writer added outside that phase — that case is
// covered in production by the WARN Attach emits when the tally is non-zero
// (TestAttachFlushesPreAttachTally).
func TestNoRowsBeforeAttach(t *testing.T) {
	c := newRedactionContainer(true)
	c.initToolAuditRedaction()

	reg := prometheus.NewRegistry()
	c.auditRedactMetrics.Attach(reg, &c.Logger)

	if got := c.auditRedactMetrics.PreAttachRows(); got != 0 {
		t.Errorf("PreAttachRows() = %d, want 0 — a writer now reaches tool_audit_log "+
			"before the metrics registry exists; the boot ordering this seam assumes has changed", got)
	}
}
