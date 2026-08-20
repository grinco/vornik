package service

import (
	"testing"

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

// secrets.enabled=false is an explicit operator bypass, warned about loudly in
// initScheduler. Half-applying it here would put the bypass in two places.
func TestInitToolAuditRedactionSkippedWhenSecretsDisabled(t *testing.T) {
	c := newRedactionContainer(false)
	c.initToolAuditRedaction()
	if _, ok := c.repos.ToolAudit.(*auditredact.Repo); ok {
		t.Error("secrets.enabled=false must leave the repo undecorated")
	}
}
