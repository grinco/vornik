package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/httpx/realip"
	"vornik.io/vornik/internal/persistence"
)

// recordingAdminRepo captures the last AdminAuditEntry handed to Insert so a
// test can assert what the handler would persist — no DB involved.
type recordingAdminRepo struct {
	last *persistence.AdminAuditEntry
}

func (r *recordingAdminRepo) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	r.last = e
	return nil
}

func (r *recordingAdminRepo) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}

// auditingHandler mirrors the production audit-row construction (see the
// memoryfirewall / cpc handlers): it reads the client IP via the same
// clientIPFromRequest helper that every admin handler uses, then persists an
// AdminAuditEntry. Composed under realip.Middleware so the context value the
// helper reads is the one the middleware actually resolved.
func auditingHandler(repo *recordingAdminRepo) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = repo.Insert(r.Context(), &persistence.AdminAuditEntry{
			Action: "test.action",
			Source: "api",
			IP:     clientIPFromRequest(r),
		})
		w.WriteHeader(http.StatusOK)
	})
}

// TestAuditEntryIP_FollowsTrustedRealIPThroughStack — the WIRED-stack proof
// for the audit-row IP: realip.Middleware (the outermost wrapper) resolves a
// trusted proxy's CF-Connecting-IP override into the request context, and the
// admin audit row built downstream via clientIPFromRequest records THAT
// resolved client IP — not the proxy's RemoteAddr. The existing
// cpc_clientip_test only proves the helper reads a pre-set context value; this
// pins the middleware → context → clientIPFromRequest → AdminAuditEntry.IP
// chain end to end.
func TestAuditEntryIP_FollowsTrustedRealIPThroughStack(t *testing.T) {
	const (
		trustedProxy = "10.0.0.5"
		realClient   = "203.0.113.42"
	)
	rcfg, err := realip.NewConfig(true, []string{trustedProxy + "/32"}, "CF-Connecting-IP")
	if err != nil {
		t.Fatalf("realip.NewConfig: %v", err)
	}

	repo := &recordingAdminRepo{}
	stack := realip.Middleware(rcfg, nil)(auditingHandler(repo))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/whatever", nil)
	req.RemoteAddr = trustedProxy + ":40000" // immediate peer = trusted proxy
	req.Header.Set("CF-Connecting-IP", realClient)

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if repo.last == nil {
		t.Fatal("handler did not persist an audit entry")
	}
	if repo.last.IP != realClient {
		t.Errorf("AdminAuditEntry.IP = %q, want the trusted-source override %q", repo.last.IP, realClient)
	}
}

// TestAuditEntryIP_IgnoresUntrustedForgedHeader — the spoof half of the same
// chain. A request from an UNTRUSTED peer carrying a forged CF-Connecting-IP
// must record the peer's RemoteAddr host in the audit row, never the forged
// header. Guards the Cloudflare-tunnel real-IP design at the audit layer
// through the actual middleware (not just the helper's direct unit test).
func TestAuditEntryIP_IgnoresUntrustedForgedHeader(t *testing.T) {
	const (
		untrustedPeer = "198.51.100.9"
		forged        = "203.0.113.42"
	)
	// Only 10.0.0.5 is trusted; the request peer is NOT.
	rcfg, err := realip.NewConfig(true, []string{"10.0.0.5/32"}, "CF-Connecting-IP")
	if err != nil {
		t.Fatalf("realip.NewConfig: %v", err)
	}

	repo := &recordingAdminRepo{}
	stack := realip.Middleware(rcfg, nil)(auditingHandler(repo))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/whatever", nil)
	req.RemoteAddr = untrustedPeer + ":40000"
	req.Header.Set("CF-Connecting-IP", forged)

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, req)

	if repo.last == nil {
		t.Fatal("handler did not persist an audit entry")
	}
	if repo.last.IP != untrustedPeer {
		t.Errorf("AdminAuditEntry.IP = %q, want untrusted peer host %q (forged header ignored)", repo.last.IP, untrustedPeer)
	}
}
