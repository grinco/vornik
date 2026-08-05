package repotest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunMCPOAuthTokenSuite exercises the MCP OAuth token store (MCP server authentication design
// §6) against whichever backend the caller wires. Run from BOTH sqlite and postgres: the
// dialects diverge on TIMESTAMPTZ vs RFC3339 TEXT, BOOLEAN vs INTEGER, and the conditional
// UPDATE's row count, and `go test ./...` is sqlite-only.
//
// The properties under test are the ones the design leans on rather than CRUD for its own sake:
//   - project_id = "" IS the daemon scope, and must not collide with a project of any name;
//   - SwapRefreshToken is the ROTATION GUARD: it must apply only when the stored refresh token
//     is still the one the caller used, so a concurrent refresh loses cleanly instead of
//     clobbering the winner's rotated value;
//   - a swap must not overwrite connected_at / connected_by — a refresh is not a new consent;
//   - needs_reconnect survives a round trip, because the UI and the tool-call error path use it
//     to tell "will refresh itself" apart from "a human must consent again";
//   - WithRefreshLock actually serialises.
func RunMCPOAuthTokenSuite(t *testing.T, repo persistence.MCPOAuthTokenRepository) {
	t.Helper()
	runMCPOAuthUpsertGet(t, repo)
	runMCPOAuthDaemonScopeIsItsOwnKey(t, repo)
	runMCPOAuthSwapRotationGuard(t, repo)
	runMCPOAuthSwapPreservesConsent(t, repo)
	runMCPOAuthNeedsReconnect(t, repo)
	runMCPOAuthDeleteAndList(t, repo)
	runMCPOAuthRefreshLockSerialises(t, repo)
	runMCPOAuthInvalidateStaleRedirectURIs(t, repo)
}

func mcpToken(projectID, serverName string) *persistence.MCPOAuthToken {
	exp := time.Now().Add(time.Hour).UTC()
	return &persistence.MCPOAuthToken{
		ProjectID:    projectID,
		ServerName:   serverName,
		Resource:     "https://mcp.atlassian.com/v1/mcp/authv2",
		ClientID:     "client-abc",
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		ExpiresAt:    &exp,
		Scopes:       "read:jira-work offline_access",
		RedirectURI:  "https://vornik.example.com/auth/mcp/callback",
		ConnectedBy:  "operator@example.com",
		ConnectedAt:  time.Now().Add(-time.Minute).UTC(),
	}
}

func runMCPOAuthUpsertGet(t *testing.T, repo persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	tok := mcpToken("proj-upsert", "atlassian")
	if err := repo.Upsert(ctx, tok); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.Get(ctx, "proj-upsert", "atlassian")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil for a stored grant")
	}
	if got.AccessToken != "at-1" || got.RefreshToken != "rt-1" {
		t.Errorf("tokens = %q/%q", got.AccessToken, got.RefreshToken)
	}
	if got.Resource != tok.Resource {
		t.Errorf("Resource = %q; the audience is a property of the grant (F5)", got.Resource)
	}
	if got.ClientID != "client-abc" {
		t.Errorf("ClientID = %q; a DCR client must survive a restart", got.ClientID)
	}
	if got.Scopes != tok.Scopes {
		t.Errorf("Scopes = %q, want the GRANTED set", got.Scopes)
	}
	if got.ConnectedBy != "operator@example.com" {
		t.Errorf("ConnectedBy = %q", got.ConnectedBy)
	}
	if got.ExpiresAt == nil || !timesCloseToSecond(*got.ExpiresAt, *tok.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want ~%v", got.ExpiresAt, tok.ExpiresAt)
	}
	if got.NeedsReconnect {
		t.Error("a freshly stored grant must not be flagged needs_reconnect")
	}

	// A missing pair is nil-nil, not an error: the injection path asks about
	// every oauth server on every wiring pass.
	missing, err := repo.Get(ctx, "proj-upsert", "never-connected")
	if err != nil {
		t.Fatalf("Get(missing): %v", err)
	}
	if missing != nil {
		t.Errorf("Get(missing) = %+v, want nil", missing)
	}

	// Upsert replaces rather than duplicating (the pair is the primary key).
	tok.AccessToken = "at-2"
	tok.Scopes = "read:jira-work"
	if err := repo.Upsert(ctx, tok); err != nil {
		t.Fatalf("Upsert(replace): %v", err)
	}
	got, err = repo.Get(ctx, "proj-upsert", "atlassian")
	if err != nil {
		t.Fatalf("Get after replace: %v", err)
	}
	if got.AccessToken != "at-2" || got.Scopes != "read:jira-work" {
		t.Errorf("replace did not take: %q / %q", got.AccessToken, got.Scopes)
	}
}

// runMCPOAuthDaemonScopeIsItsOwnKey — project_id = "" is the daemon scope, a real key rather
// than a missing value. A daemon-scope grant and a project grant for the same server name are
// different rows, and neither may be returned for the other.
func runMCPOAuthDaemonScopeIsItsOwnKey(t *testing.T, repo persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	daemon := mcpToken("", "shared-server")
	daemon.AccessToken = "daemon-at"
	project := mcpToken("proj-scope", "shared-server")
	project.AccessToken = "project-at"

	if err := repo.Upsert(ctx, daemon); err != nil {
		t.Fatalf("Upsert(daemon): %v", err)
	}
	if err := repo.Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert(project): %v", err)
	}

	got, err := repo.Get(ctx, "", "shared-server")
	if err != nil || got == nil {
		t.Fatalf("Get(daemon scope): %v / %v", got, err)
	}
	if got.AccessToken != "daemon-at" {
		t.Errorf("daemon-scope Get returned %q", got.AccessToken)
	}
	got, err = repo.Get(ctx, "proj-scope", "shared-server")
	if err != nil || got == nil {
		t.Fatalf("Get(project scope): %v / %v", got, err)
	}
	if got.AccessToken != "project-at" {
		t.Errorf("project-scope Get returned %q — the scopes collided", got.AccessToken)
	}
}

// runMCPOAuthSwapRotationGuard is the important one. Two daemons refresh the same grant; the
// loser must not clobber the winner.
func runMCPOAuthSwapRotationGuard(t *testing.T, repo persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	tok := mcpToken("proj-swap", "linear")
	if err := repo.Upsert(ctx, tok); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Daemon A refreshes: it used rt-1, which is still stored, so it wins.
	next := mcpToken("proj-swap", "linear")
	next.AccessToken = "at-winner"
	next.RefreshToken = "rt-2"
	won, err := repo.SwapRefreshToken(ctx, "rt-1", next)
	if err != nil {
		t.Fatalf("SwapRefreshToken(winner): %v", err)
	}
	if !won {
		t.Fatal("the refresh that used the STORED refresh token must win")
	}

	// Daemon B refreshes concurrently: it also used rt-1, but the store has
	// moved on. It must lose — not overwrite at-winner with a value derived
	// from a refresh token the authorization server has already rotated away.
	stale := mcpToken("proj-swap", "linear")
	stale.AccessToken = "at-loser"
	stale.RefreshToken = "rt-3"
	won, err = repo.SwapRefreshToken(ctx, "rt-1", stale)
	if err != nil {
		t.Fatalf("SwapRefreshToken(loser): %v", err)
	}
	if won {
		t.Fatal("a refresh that used a ROTATED-AWAY refresh token must lose")
	}

	got, err := repo.Get(ctx, "proj-swap", "linear")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AccessToken != "at-winner" || got.RefreshToken != "rt-2" {
		t.Errorf("loser clobbered the winner: %q / %q", got.AccessToken, got.RefreshToken)
	}
}

// runMCPOAuthSwapPreservesConsent — a refresh must not rewrite who consented or when. Without
// this the only record of the human behind a grant (design §9) would be overwritten hourly.
func runMCPOAuthSwapPreservesConsent(t *testing.T, repo persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	tok := mcpToken("proj-consent", "notion")
	tok.ConnectedBy = "alice@example.com"
	tok.ConnectedAt = time.Now().Add(-72 * time.Hour).UTC()
	if err := repo.Upsert(ctx, tok); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	next := mcpToken("proj-consent", "notion")
	next.ConnectedBy = "somebody-else@example.com" // must be ignored by a swap
	next.RefreshToken = "rt-rotated"
	if _, err := repo.SwapRefreshToken(ctx, "rt-1", next); err != nil {
		t.Fatalf("SwapRefreshToken: %v", err)
	}

	got, err := repo.Get(ctx, "proj-consent", "notion")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ConnectedBy != "alice@example.com" {
		t.Errorf("ConnectedBy = %q; a refresh is not a new consent", got.ConnectedBy)
	}
	if !timesCloseToSecond(got.ConnectedAt, tok.ConnectedAt) {
		t.Errorf("ConnectedAt = %v, want the original %v", got.ConnectedAt, tok.ConnectedAt)
	}
}

func runMCPOAuthNeedsReconnect(t *testing.T, repo persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	tok := mcpToken("proj-reconnect", "slack")
	if err := repo.Upsert(ctx, tok); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := repo.MarkNeedsReconnect(ctx, "proj-reconnect", "slack"); err != nil {
		t.Fatalf("MarkNeedsReconnect: %v", err)
	}
	got, err := repo.Get(ctx, "proj-reconnect", "slack")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.NeedsReconnect {
		t.Fatal("needs_reconnect did not survive the round trip")
	}
	if got.Usable(time.Now(), 0) {
		t.Error("a needs_reconnect grant must never be presented to a server")
	}

	// A successful swap clears the flag: the grant works again, and leaving it
	// set would keep prompting a human who no longer needs to do anything.
	next := mcpToken("proj-reconnect", "slack")
	next.RefreshToken = "rt-new"
	won, err := repo.SwapRefreshToken(ctx, "rt-1", next)
	if err != nil {
		t.Fatalf("SwapRefreshToken: %v", err)
	}
	if !won {
		t.Fatal("swap should have won")
	}
	got, err = repo.Get(ctx, "proj-reconnect", "slack")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.NeedsReconnect {
		t.Error("a successful refresh must clear needs_reconnect")
	}
}

func runMCPOAuthDeleteAndList(t *testing.T, repo persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	for _, name := range []string{"zeta", "alpha"} {
		if err := repo.Upsert(ctx, mcpToken("proj-list", name)); err != nil {
			t.Fatalf("Upsert(%s): %v", name, err)
		}
	}
	list, err := repo.ListForProject(ctx, "proj-list")
	if err != nil {
		t.Fatalf("ListForProject: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListForProject returned %d grants, want 2", len(list))
	}
	if list[0].ServerName != "alpha" || list[1].ServerName != "zeta" {
		t.Errorf("ordering = %q,%q; rows must be stable across refreshes",
			list[0].ServerName, list[1].ServerName)
	}

	if err := repo.Delete(ctx, "proj-list", "alpha"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	list, err = repo.ListForProject(ctx, "proj-list")
	if err != nil {
		t.Fatalf("ListForProject after delete: %v", err)
	}
	if len(list) != 1 || list[0].ServerName != "zeta" {
		t.Errorf("after delete = %+v", list)
	}

	// Deleting a grant that is not there is not an error — Disconnect is
	// idempotent, and a double-click must not surface a failure.
	if err := repo.Delete(ctx, "proj-list", "alpha"); err != nil {
		t.Errorf("Delete(missing) = %v, want nil", err)
	}
}

// runMCPOAuthRefreshLockSerialises proves the lock is real on whichever backend. Not a timing
// test: the callbacks record overlap explicitly.
func runMCPOAuthRefreshLockSerialises(t *testing.T, repo persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	var (
		mu      sync.Mutex
		inside  int
		overlap bool
		wg      sync.WaitGroup
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = repo.WithRefreshLock(ctx, "proj-lock", "server", func(context.Context) error {
				mu.Lock()
				inside++
				if inside > 1 {
					overlap = true
				}
				mu.Unlock()

				time.Sleep(5 * time.Millisecond)

				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()
	if overlap {
		t.Error("WithRefreshLock allowed two refreshes into the critical section at once")
	}

	// An error from fn propagates unchanged — the caller distinguishes a
	// refresh failure from a locking failure.
	sentinel := errors.New("boom")
	if err := repo.WithRefreshLock(ctx, "proj-lock", "server", func(context.Context) error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Errorf("WithRefreshLock swallowed fn's error: %v", err)
	}

	// …and the lock is released after an error, or every later refresh wedges.
	done := make(chan struct{})
	go func() {
		_ = repo.WithRefreshLock(ctx, "proj-lock", "server", func(context.Context) error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WithRefreshLock did not release the lock after fn returned an error")
	}
}

// runMCPOAuthInvalidateStaleRedirectURIs covers §7.2a on both backends.
//
// A DCR registration pins its redirect_uris, so when server.public_base_url changes the
// stored client_id is dead at the vendor. Three behaviours matter, and the third is the
// one that is easy to get wrong: a row whose recorded URI is EMPTY predates migration
// 151 and means "unknown", not "registered elsewhere" — dropping a working client over
// our own missing data would break connections for nothing.
func runMCPOAuthInvalidateStaleRedirectURIs(t *testing.T, repo persistence.MCPOAuthTokenRepository) {
	ctx := context.Background()
	const current = "https://now.example.com/auth/mcp/callback"

	stale := mcpToken("proj-redirect-stale", "atlassian")
	stale.RedirectURI = "https://before.example.com/auth/mcp/callback"
	matching := mcpToken("proj-redirect-match", "atlassian")
	matching.RedirectURI = current
	unknown := mcpToken("proj-redirect-unknown", "atlassian")
	unknown.RedirectURI = ""
	for _, tok := range []*persistence.MCPOAuthToken{stale, matching, unknown} {
		if err := repo.Upsert(ctx, tok); err != nil {
			t.Fatalf("Upsert %s: %v", tok.ProjectID, err)
		}
	}

	// Earlier subtests leave rows behind that also carry the fixture URI, so the sweep
	// legitimately touches more than this subtest's own row. Assert on the rows under
	// test, not a suite-wide count that any new subtest would break.
	n, err := repo.InvalidateStaleRedirectURIs(ctx, current)
	if err != nil {
		t.Fatalf("InvalidateStaleRedirectURIs: %v", err)
	}
	if n < 1 {
		t.Errorf("invalidated %d rows, want at least the stale one", n)
	}

	got, err := repo.Get(ctx, stale.ProjectID, stale.ServerName)
	if err != nil || got == nil {
		t.Fatalf("Get(stale): %v", err)
	}
	if got.ClientID != "" {
		t.Errorf("stale grant kept client_id %q — it is unusable at the vendor", got.ClientID)
	}
	if !got.NeedsReconnect {
		t.Error("stale grant was not flagged needs_reconnect, so it would fail silently at the next call")
	}
	if got.AccessToken == "" {
		t.Error("the access token was destroyed; §7.2a drops the CLIENT, not the grant")
	}

	for _, keep := range []*persistence.MCPOAuthToken{matching, unknown} {
		got, err := repo.Get(ctx, keep.ProjectID, keep.ServerName)
		if err != nil || got == nil {
			t.Fatalf("Get(%s): %v", keep.ProjectID, err)
		}
		if got.ClientID != "client-abc" || got.NeedsReconnect {
			t.Errorf("%s was invalidated (client_id=%q needs_reconnect=%v); only a RECORDED mismatch may be",
				keep.ProjectID, got.ClientID, got.NeedsReconnect)
		}
	}

	// Idempotent: a second sweep with the same current URI changes nothing, because the
	// boot path and every connect both run it.
	again, err := repo.InvalidateStaleRedirectURIs(ctx, current)
	if err != nil {
		t.Fatalf("second InvalidateStaleRedirectURIs: %v", err)
	}
	if again != 0 {
		t.Errorf("second sweep touched %d rows, want 0 — the sweep must clear redirect_uri "+
			"as well as client_id, or every boot re-flags grants it already handled", again)
	}
}
