package repotest

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Identity-core contract suites shared by both backends (SQLite parity of
// 2026-09-13, config-assistant plan §2 / review R1). RunIdentityRepositorySuite
// (repotest.go) pins the resolver; the two suites here cover what it does
// not: browser sessions, and the admin mutations whose observable side
// effect is on sessions — which is why each takes BOTH repositories.

// identityFixture bundles the two repositories and the row factories the
// subtests share. Every id comes from uniqueID so a suite can run against
// a shared, non-reset database.
type identityFixture struct {
	identity persistence.IdentityRepository
	sessions persistence.UISessionRepository
}

// clock is a microsecond-truncated UTC now: Postgres stores timestamps at
// microsecond precision, so a nanosecond input would not round-trip
// Equal on that backend while it would on SQLite.
func clock() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func (f *identityFixture) user(t *testing.T) *persistence.User {
	t.Helper()
	u := &persistence.User{ID: uniqueID("user"), DisplayName: "suite user", CreatedAt: clock()}
	if err := f.identity.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

// session creates a session for userID with the given last-seen and
// expiry instants; ip/user-agent are set so NULL handling is exercised
// by the callers that pass empty ones explicitly.
func (f *identityFixture) session(t *testing.T, userID string, lastSeen, expires time.Time) *persistence.UISession {
	t.Helper()
	s := &persistence.UISession{
		ID: uniqueID("sess"), TokenHash: uniqueID("hash"), UserID: userID, Provider: "github",
		CreatedAt: lastSeen, LastSeenAt: lastSeen, ExpiresAt: expires,
		IP: "10.0.0.1", UserAgent: "suite/1.0",
	}
	if err := f.sessions.CreateSession(context.Background(), s); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return s
}

// activeSession is the common case: seen an hour ago, expires in a day.
func (f *identityFixture) activeSession(t *testing.T, userID string) *persistence.UISession {
	t.Helper()
	return f.session(t, userID, clock().Add(-time.Hour), clock().Add(24*time.Hour))
}

// sessionActive reports whether GetActiveByTokenHash still returns the
// session — the observable form of "was it revoked".
func (f *identityFixture) sessionActive(t *testing.T, tokenHash string) bool {
	t.Helper()
	_, err := f.sessions.GetActiveByTokenHash(context.Background(), tokenHash)
	if errors.Is(err, persistence.ErrSessionNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("GetActiveByTokenHash: %v", err)
	}
	return true
}

func (f *identityFixture) bind(t *testing.T, userID, channel, externalID string) {
	t.Helper()
	bound, err := f.identity.BindIdentity(context.Background(), &persistence.UserIdentity{
		ID: uniqueID("uident"), UserID: userID, Channel: channel, ExternalID: externalID,
		Display: "bound " + externalID, CreatedAt: clock(),
	})
	if err != nil {
		t.Fatalf("BindIdentity: %v", err)
	}
	// A fixture that silently did not bind makes every assertion built on
	// it vacuous, which is how the false success stayed invisible.
	if !bound {
		t.Fatalf("BindIdentity(%s, %s) did not bind: the identity is already held by another user", channel, externalID)
	}
}

func (f *identityFixture) group(t *testing.T, role string, projects ...string) *persistence.Group {
	t.Helper()
	g := &persistence.Group{ID: uniqueID("grp"), Name: uniqueID("grpname"), Role: role, CreatedAt: clock()}
	if err := f.identity.CreateGroup(context.Background(), g); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if len(projects) > 0 {
		if err := f.identity.SetGroupProjects(context.Background(), g.ID, projects); err != nil {
			t.Fatalf("SetGroupProjects: %v", err)
		}
	}
	return g
}

func (f *identityFixture) member(t *testing.T, groupID, userID string) {
	t.Helper()
	if err := f.identity.AddGroupMember(context.Background(), groupID, userID); err != nil {
		t.Fatalf("AddGroupMember: %v", err)
	}
}

// grant is SetUserAccess with a fatal on error.
func (f *identityFixture) grant(t *testing.T, userID, role string, projects ...string) {
	t.Helper()
	if err := f.identity.SetUserAccess(context.Background(), userID, role, projects); err != nil {
		t.Fatalf("SetUserAccess(%s, %s, %v): %v", userID, role, projects, err)
	}
}

// view returns the ListUsers row for userID.
func (f *identityFixture) view(t *testing.T, userID string) persistence.UserAdminView {
	t.Helper()
	views, err := f.identity.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	for _, v := range views {
		if v.UserID == userID {
			return v
		}
	}
	t.Fatalf("ListUsers: user %s missing from %d rows", userID, len(views))
	return persistence.UserAdminView{}
}

// principal folds ResolveUserPrincipalRows into (max role, project set,
// disabled) for assertions on the resolver's view of a user.
func (f *identityFixture) principal(t *testing.T, userID string) (role string, projects map[string]bool, disabled bool) {
	t.Helper()
	rows, err := f.identity.ResolveUserPrincipalRows(context.Background(), userID)
	if err != nil {
		t.Fatalf("ResolveUserPrincipalRows: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("ResolveUserPrincipalRows(%s): zero rows for an existing user", userID)
	}
	rank := map[string]int{"": 0, "user": 1, "admin": 2}
	projects = map[string]bool{}
	for _, r := range rows {
		disabled = r.Disabled
		if r.Role != nil && rank[*r.Role] > rank[role] {
			role = *r.Role
		}
		if r.ProjectID != nil {
			projects[*r.ProjectID] = true
		}
	}
	return role, projects, disabled
}

// wantErr asserts errors.Is(err, want).
func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want %v", what, err, want)
	}
}

// wantOK asserts a nil error.
func wantOK(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// RunUISessionSuite exercises the persistence.UISessionRepository contract
// against a backend. identity is needed to own the sessions' users.
func RunUISessionSuite(t *testing.T, repo persistence.UISessionRepository, identity persistence.IdentityRepository) {
	t.Helper()
	f := &identityFixture{identity: identity, sessions: repo}
	t.Run("miss_contract", func(t *testing.T) {
		AssertMiss(t, "UISessionRepository.GetActiveByTokenHash", func() (*persistence.UISession, error) {
			return repo.GetActiveByTokenHash(context.Background(), uniqueID("absent-hash"))
		})
	})
	t.Run("create_then_get_round_trips", func(t *testing.T) { uiSessionRoundTrip(t, f) })
	t.Run("touch_refreshes_ip_only_when_given", func(t *testing.T) { uiSessionTouch(t, f) })
	t.Run("revoke_twice_is_not_found", func(t *testing.T) { uiSessionRevoke(t, f) })
	t.Run("revoke_for_user_enforces_ownership", func(t *testing.T) { uiSessionRevokeForUser(t, f) })
	t.Run("list_active_excludes_expired_and_revoked", func(t *testing.T) { uiSessionListActive(t, f) })
	t.Run("delete_expired_counts_rows", func(t *testing.T) { uiSessionDeleteExpired(t, f) })
	t.Run("count_by_status_buckets", func(t *testing.T) { uiSessionCountByStatus(t, f) })
}

func uiSessionRoundTrip(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	u := f.user(t)
	s := f.activeSession(t, u.ID)
	got, err := f.sessions.GetActiveByTokenHash(ctx, s.TokenHash)
	wantOK(t, "GetActiveByTokenHash", err)
	if got.ID != s.ID || got.UserID != u.ID || got.Provider != s.Provider || got.TokenHash != s.TokenHash {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, s)
	}
	if got.IP != s.IP || got.UserAgent != s.UserAgent {
		t.Fatalf("ip/user-agent mismatch: got %q/%q want %q/%q", got.IP, got.UserAgent, s.IP, s.UserAgent)
	}
	if !got.CreatedAt.Equal(s.CreatedAt) || !got.LastSeenAt.Equal(s.LastSeenAt) || !got.ExpiresAt.Equal(s.ExpiresAt) {
		t.Fatalf("timestamps did not round-trip: got %v/%v/%v want %v/%v/%v",
			got.CreatedAt, got.LastSeenAt, got.ExpiresAt, s.CreatedAt, s.LastSeenAt, s.ExpiresAt)
	}
	if got.RevokedAt != nil {
		t.Fatalf("fresh session has RevokedAt=%v, want nil", got.RevokedAt)
	}
	// Empty IP / user-agent are stored as NULL and read back as "".
	bare := &persistence.UISession{
		ID: uniqueID("sess"), TokenHash: uniqueID("hash"), UserID: u.ID, Provider: "github",
		CreatedAt: clock(), LastSeenAt: clock(), ExpiresAt: clock().Add(time.Hour),
	}
	wantOK(t, "CreateSession(bare)", f.sessions.CreateSession(ctx, bare))
	got, err = f.sessions.GetActiveByTokenHash(ctx, bare.TokenHash)
	wantOK(t, "GetActiveByTokenHash(bare)", err)
	if got.IP != "" || got.UserAgent != "" {
		t.Fatalf("NULL ip/user-agent read back as %q/%q, want empty", got.IP, got.UserAgent)
	}
}

func uiSessionTouch(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	u := f.user(t)
	s := f.activeSession(t, u.ID) // last seen an hour ago
	wantOK(t, "TouchSession(empty ip)", f.sessions.TouchSession(ctx, s.ID, ""))
	got, err := f.sessions.GetActiveByTokenHash(ctx, s.TokenHash)
	wantOK(t, "GetActiveByTokenHash", err)
	if !got.LastSeenAt.After(s.LastSeenAt) {
		t.Fatalf("TouchSession did not advance last_seen_at: %v → %v", s.LastSeenAt, got.LastSeenAt)
	}
	if got.IP != s.IP {
		t.Fatalf("TouchSession with empty ip changed ip %q → %q", s.IP, got.IP)
	}
	wantOK(t, "TouchSession(new ip)", f.sessions.TouchSession(ctx, s.ID, "10.9.9.9"))
	got, err = f.sessions.GetActiveByTokenHash(ctx, s.TokenHash)
	wantOK(t, "GetActiveByTokenHash", err)
	if got.IP != "10.9.9.9" {
		t.Fatalf("TouchSession with ip did not refresh it: got %q", got.IP)
	}
	// Best-effort: an absent session is not an error.
	wantOK(t, "TouchSession(absent)", f.sessions.TouchSession(ctx, uniqueID("absent"), "1.2.3.4"))
}

func uiSessionRevoke(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	u := f.user(t)
	s := f.activeSession(t, u.ID)
	wantOK(t, "RevokeSession", f.sessions.RevokeSession(ctx, s.ID))
	if f.sessionActive(t, s.TokenHash) {
		t.Fatal("revoked session still returned by GetActiveByTokenHash")
	}
	wantErr(t, "RevokeSession(second)", f.sessions.RevokeSession(ctx, s.ID), persistence.ErrSessionNotFound)
	wantErr(t, "RevokeSession(absent)", f.sessions.RevokeSession(ctx, uniqueID("absent")), persistence.ErrSessionNotFound)
}

func uiSessionRevokeForUser(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	owner, other := f.user(t), f.user(t)
	s := f.activeSession(t, owner.ID)
	wantErr(t, "RevokeSessionForUser(other user)", f.sessions.RevokeSessionForUser(ctx, other.ID, s.ID), persistence.ErrSessionNotFound)
	if !f.sessionActive(t, s.TokenHash) {
		t.Fatal("a foreign RevokeSessionForUser revoked the session")
	}
	wantOK(t, "RevokeSessionForUser(owner)", f.sessions.RevokeSessionForUser(ctx, owner.ID, s.ID))
	if f.sessionActive(t, s.TokenHash) {
		t.Fatal("owner RevokeSessionForUser left the session active")
	}
	wantErr(t, "RevokeSessionForUser(again)", f.sessions.RevokeSessionForUser(ctx, owner.ID, s.ID), persistence.ErrSessionNotFound)
}

func uiSessionListActive(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	u := f.user(t)
	now := clock()
	older := f.session(t, u.ID, now.Add(-2*time.Hour), now.Add(24*time.Hour))
	newer := f.session(t, u.ID, now.Add(-time.Hour), now.Add(24*time.Hour))
	expired := f.session(t, u.ID, now.Add(-3*time.Hour), now.Add(-time.Minute)) // expired, NOT revoked
	revoked := f.session(t, u.ID, now, now.Add(24*time.Hour))
	wantOK(t, "RevokeSession", f.sessions.RevokeSession(ctx, revoked.ID))
	// A different user's session must not leak in.
	f.activeSession(t, f.user(t).ID)

	list, err := f.sessions.ListActiveByUser(ctx, u.ID)
	wantOK(t, "ListActiveByUser", err)
	if len(list) != 2 {
		t.Fatalf("ListActiveByUser returned %d sessions, want 2 (expired %s / revoked %s excluded)", len(list), expired.ID, revoked.ID)
	}
	if list[0].ID != newer.ID || list[1].ID != older.ID {
		t.Fatalf("ListActiveByUser order = [%s %s], want newest last-seen first [%s %s]", list[0].ID, list[1].ID, newer.ID, older.ID)
	}
	// The expired row is still a hit for the token lookup (expiry is the
	// store's check, revocation is SQL's) — the list predicate is stricter.
	if !f.sessionActive(t, expired.TokenHash) {
		t.Fatal("expired-but-not-revoked session must still be returned by GetActiveByTokenHash")
	}
}

func uiSessionDeleteExpired(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	u := f.user(t)
	now := clock()
	stale := f.session(t, u.ID, now.Add(-72*time.Hour), now.Add(-48*time.Hour))
	live := f.activeSession(t, u.ID)
	cutoff := now.Add(-24 * time.Hour)
	n, err := f.sessions.DeleteExpiredSessions(ctx, cutoff)
	wantOK(t, "DeleteExpiredSessions", err)
	if n < 1 {
		t.Fatalf("DeleteExpiredSessions deleted %d rows, want at least the stale one", n)
	}
	if f.sessionActive(t, stale.TokenHash) {
		t.Fatal("session expired before cutoff survived DeleteExpiredSessions")
	}
	if !f.sessionActive(t, live.TokenHash) {
		t.Fatal("live session was deleted by DeleteExpiredSessions")
	}
	// Everything before cutoff is gone now, so the same cutoff deletes 0.
	n, err = f.sessions.DeleteExpiredSessions(ctx, cutoff)
	wantOK(t, "DeleteExpiredSessions(second)", err)
	if n != 0 {
		t.Fatalf("second DeleteExpiredSessions with the same cutoff deleted %d rows, want 0", n)
	}
}

func uiSessionCountByStatus(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	before, err := f.sessions.CountByStatus(ctx)
	wantOK(t, "CountByStatus(before)", err)
	u := f.user(t)
	now := clock()
	f.activeSession(t, u.ID)
	f.session(t, u.ID, now.Add(-time.Hour), now.Add(-time.Minute)) // expired, not revoked
	rev := f.activeSession(t, u.ID)
	wantOK(t, "RevokeSession", f.sessions.RevokeSession(ctx, rev.ID))
	after, err := f.sessions.CountByStatus(ctx)
	wantOK(t, "CountByStatus(after)", err)
	if d := after.Active - before.Active; d != 1 {
		t.Errorf("Active delta = %d, want 1", d)
	}
	if d := after.ExpiredNotRevoked - before.ExpiredNotRevoked; d != 1 {
		t.Errorf("ExpiredNotRevoked delta = %d, want 1", d)
	}
	if d := after.Revoked - before.Revoked; d != 1 {
		t.Errorf("Revoked delta = %d, want 1", d)
	}
}

// RunIdentityAdminSuite exercises the admin-page half of
// persistence.IdentityRepository — ListUsers, SetUserAccess,
// RemoveUserAccess, SetUserDisabled, RevokeIdentity,
// MigrateIdentityExternalID and the last-admin guard — plus the miss
// contract of GetGroupByName.
//
// PRECONDITION: the store holds no enabled admin user when the suite
// starts. The last-admin case runs first for that reason and leaves ONE
// enabled admin behind on purpose, so every later case may demote or
// remove the admins it creates without tripping the guard.
func RunIdentityAdminSuite(t *testing.T, repo persistence.IdentityRepository, sessions persistence.UISessionRepository) {
	t.Helper()
	f := &identityFixture{identity: repo, sessions: sessions}
	t.Run("last_admin_guard", func(t *testing.T) { identityLastAdminGuard(t, f) })
	t.Run("get_group_by_name_miss", func(t *testing.T) {
		AssertMissRepo(t, "IdentityRepository.GetGroupByName", repo.GetGroupByName)
	})
	t.Run("list_users_role_rank_and_projects", func(t *testing.T) { identityListUsersRoles(t, f) })
	t.Run("list_users_identities_and_sessions", func(t *testing.T) { identityListUsersIdentities(t, f) })
	t.Run("set_user_access_backing_group", func(t *testing.T) { identitySetUserAccess(t, f) })
	t.Run("set_user_access_admin_ignores_projects", func(t *testing.T) { identitySetUserAccessAdmin(t, f) })
	t.Run("set_user_access_validation", func(t *testing.T) { identitySetUserAccessValidation(t, f) })
	t.Run("remove_user_access", func(t *testing.T) { identityRemoveUserAccess(t, f) })
	t.Run("set_user_disabled", func(t *testing.T) { identitySetUserDisabled(t, f) })
	t.Run("revoke_identity", func(t *testing.T) { identityRevokeIdentity(t, f) })
	t.Run("revoke_identity_owned_by", func(t *testing.T) { identityRevokeIdentityOwnedBy(t, f) })
	t.Run("migrate_identity_external_id", func(t *testing.T) { identityMigrateExternalID(t, f) })
	t.Run("noop_on_absent_rows", func(t *testing.T) { identityNoops(t, f) })
	t.Run("both_resolvers_agree", func(t *testing.T) { identityBothResolversAgree(t, f) })
	t.Run("bind_reports_an_active_conflict", func(t *testing.T) { identityBindReportsConflict(t, f) })
	t.Run("rebind_moves_an_active_binding", func(t *testing.T) { identityRebindMovesActive(t, f) })
}

// identityBindReportsConflict pins the bool BindIdentity gained after the
// silent-write defect: it must be TRUE for a fresh insert, a same-user
// re-bind and a revoked repoint, and FALSE — with no error — when another
// user actively holds the identity.
//
// Before the bool, that last case returned nil and wrote nothing, so admin
// re-assignment and link-code redemption reported success and then audited a
// change that had not happened (review-20260914-3c36 F11).
func identityBindReportsConflict(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	owner, other := f.user(t), f.user(t)
	ext := uniqueID("ext")

	id := func(userID string) *persistence.UserIdentity {
		return &persistence.UserIdentity{
			ID: uniqueID("uident"), UserID: userID, Channel: "github",
			ExternalID: ext, Display: "bound " + ext, CreatedAt: clock(),
		}
	}
	bind := func(what, userID string) bool {
		bound, err := f.identity.BindIdentity(ctx, id(userID))
		wantOK(t, "BindIdentity("+what+")", err)
		return bound
	}
	resolvesTo := func() string {
		rows, err := f.identity.ResolvePrincipalRows(ctx, "github", ext)
		wantOK(t, "ResolvePrincipalRows", err)
		if len(rows) == 0 {
			return ""
		}
		return rows[0].UserID
	}

	if !bind("fresh insert", owner.ID) {
		t.Fatal("a fresh insert must report bound")
	}
	if !bind("same user again", owner.ID) {
		t.Error("re-binding the SAME user must report bound: that is the state the caller asked for")
	}
	if bind("active conflict", other.ID) {
		t.Error("binding over another user's ACTIVE identity must report NOT bound")
	}
	if got := resolvesTo(); got != owner.ID {
		t.Errorf("after the refused bind the identity resolves to %q, want the original owner %q", got, owner.ID)
	}

	// A revoked row is the case the guard was written for: it repoints.
	wantOK(t, "RevokeIdentity", f.identity.RevokeIdentity(ctx, "github", ext))
	if !bind("revoked repoint", other.ID) {
		t.Fatal("binding over a REVOKED identity must report bound")
	}
	if got := resolvesTo(); got != other.ID {
		t.Errorf("after the revoked repoint the identity resolves to %q, want %q", got, other.ID)
	}
}

// identityRebindMovesActive pins admin authority: RebindIdentity does what
// BindIdentity refuses, so AssignKey can correct a wrong mapping — the one
// thing §5.4 says self-claim must never be able to do.
func identityRebindMovesActive(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	owner, other := f.user(t), f.user(t)
	ext := uniqueID("ext")
	f.bind(t, owner.ID, "api_key", ext)

	wantOK(t, "RebindIdentity", f.identity.RebindIdentity(ctx, &persistence.UserIdentity{
		ID: uniqueID("uident"), UserID: other.ID, Channel: "api_key",
		ExternalID: ext, Display: "reassigned", CreatedAt: clock(),
	}))

	rows, err := f.identity.ResolvePrincipalRows(ctx, "api_key", ext)
	wantOK(t, "ResolvePrincipalRows", err)
	if len(rows) == 0 || rows[0].UserID != other.ID {
		t.Fatalf("RebindIdentity did not move the ACTIVE binding: rows=%+v, want owner %q", rows, other.ID)
	}
}

func identityLastAdminGuard(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	// A non-admin is never blocked, even in the zero-admin state.
	bystander := f.user(t)
	wantOK(t, "SetUserDisabled(non-admin, zero admins)", f.identity.SetUserDisabled(ctx, bystander.ID, true))

	sole := f.user(t)
	f.grant(t, sole.ID, "admin")
	wantErr(t, "SetUserDisabled(sole admin)", f.identity.SetUserDisabled(ctx, sole.ID, true), persistence.ErrLastAdmin)
	wantErr(t, "SetUserAccess(demote sole admin)", f.identity.SetUserAccess(ctx, sole.ID, "user", []string{"*"}), persistence.ErrLastAdmin)
	wantErr(t, "RemoveUserAccess(sole admin)", f.identity.RemoveUserAccess(ctx, sole.ID), persistence.ErrLastAdmin)
	if v := f.view(t, sole.ID); v.Role != "admin" || v.Disabled {
		t.Fatalf("refused mutations leaked: sole admin now role=%q disabled=%v", v.Role, v.Disabled)
	}

	// A second enabled admin makes each of the three succeed.
	second := f.user(t)
	f.grant(t, second.ID, "admin")
	proj := uniqueID("proj")
	wantOK(t, "SetUserAccess(demote with second admin)", f.identity.SetUserAccess(ctx, sole.ID, "user", []string{proj}))
	f.grant(t, sole.ID, "admin")
	wantOK(t, "RemoveUserAccess(with second admin)", f.identity.RemoveUserAccess(ctx, sole.ID))
	f.grant(t, sole.ID, "admin")
	wantOK(t, "SetUserDisabled(with second admin)", f.identity.SetUserDisabled(ctx, sole.ID, true))
	// A DISABLED admin does not count: `second` is now the last one.
	wantErr(t, "SetUserDisabled(second, sole enabled)", f.identity.SetUserDisabled(ctx, second.ID, true), persistence.ErrLastAdmin)
	// `second` stays enabled as the suite's standing admin (see RunIdentityAdminSuite).
}

func identityListUsersRoles(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	// admin via backing group: role admin, no projects.
	admin := f.user(t)
	f.grant(t, admin.ID, "admin")
	// admin + user groups: rank picks admin; the user-role group's projects still list.
	mixed := f.user(t)
	pm := uniqueID("proj")
	f.member(t, f.group(t, "admin").ID, mixed.ID)
	f.member(t, f.group(t, "user", pm).ID, mixed.ID)
	// two user groups: sorted union, no duplicates.
	pa, pb, pc := "a-"+uniqueID("p"), "b-"+uniqueID("p"), "c-"+uniqueID("p")
	user := f.user(t)
	f.member(t, f.group(t, "user", pa, pb).ID, user.ID)
	f.member(t, f.group(t, "user", pb, pc).ID, user.ID)
	// '*' anywhere collapses the set to exactly ["*"].
	star := f.user(t)
	f.member(t, f.group(t, "user", pa).ID, star.ID)
	f.member(t, f.group(t, "user", "*").ID, star.ID)
	// no groups: awaiting.
	awaiting := f.user(t)
	wantOK(t, "SetUserDisabled(awaiting)", f.identity.SetUserDisabled(ctx, awaiting.ID, true))

	if v := f.view(t, admin.ID); v.Role != "admin" || len(v.Projects) != 0 {
		t.Errorf("admin view = role %q projects %v, want admin/[]", v.Role, v.Projects)
	}
	if v := f.view(t, mixed.ID); v.Role != "admin" || len(v.Projects) != 1 || v.Projects[0] != pm {
		t.Errorf("mixed view = role %q projects %v, want admin/[%s]", v.Role, v.Projects, pm)
	}
	if v := f.view(t, user.ID); v.Role != "user" || len(v.Projects) != 3 || v.Projects[0] != pa || v.Projects[1] != pb || v.Projects[2] != pc {
		t.Errorf("user view = role %q projects %v, want user/[%s %s %s]", v.Role, v.Projects, pa, pb, pc)
	}
	if v := f.view(t, star.ID); len(v.Projects) != 1 || v.Projects[0] != "*" {
		t.Errorf("star view projects = %v, want [*]", v.Projects)
	}
	v := f.view(t, awaiting.ID)
	if v.Role != "" || len(v.Projects) != 0 || !v.Disabled || v.DisplayName != awaiting.DisplayName || !v.CreatedAt.Equal(awaiting.CreatedAt) {
		t.Errorf("awaiting view = %+v, want role \"\" / no projects / disabled / created %v", v, awaiting.CreatedAt)
	}
}

func identityListUsersIdentities(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	u := f.user(t)
	active, revoked := uniqueID("ext"), uniqueID("ext")
	f.bind(t, u.ID, "github", active)
	f.bind(t, u.ID, "telegram", revoked)
	wantOK(t, "RevokeIdentity", f.identity.RevokeIdentity(ctx, "telegram", revoked))
	now := clock()
	f.activeSession(t, u.ID)
	f.session(t, u.ID, now.Add(-time.Hour), now.Add(-time.Minute)) // expired: must not count
	rev := f.activeSession(t, u.ID)
	wantOK(t, "RevokeSession", f.sessions.RevokeSession(ctx, rev.ID))

	v := f.view(t, u.ID)
	if len(v.Identities) != 1 || v.Identities[0].Channel != "github" || v.Identities[0].ExternalID != active || v.Identities[0].Display != "bound "+active {
		t.Errorf("identities = %+v, want exactly the active github binding", v.Identities)
	}
	if v.ActiveSessions != 1 {
		t.Errorf("ActiveSessions = %d, want 1 (expired and revoked rows excluded)", v.ActiveSessions)
	}
	if nobody := f.view(t, f.user(t).ID); len(nobody.Identities) != 0 || nobody.ActiveSessions != 0 {
		t.Errorf("fresh user view = %+v, want no identities / zero sessions", nobody)
	}
}

func identitySetUserAccess(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	u := f.user(t)
	pa, pb, pc := uniqueID("proj"), uniqueID("proj"), uniqueID("proj")
	s1 := f.activeSession(t, u.ID)
	f.grant(t, u.ID, "user", pa, pb)
	g, err := f.identity.GetGroupByName(ctx, "auto-user-"+u.ID)
	wantOK(t, "GetGroupByName(backing group)", err)
	if g.Role != "user" {
		t.Fatalf("backing group role = %q, want user", g.Role)
	}
	if role, projects, _ := f.principal(t, u.ID); role != "user" || len(projects) != 2 || !projects[pa] || !projects[pb] {
		t.Fatalf("after grant: role %q projects %v, want user/{%s %s}", role, projects, pa, pb)
	}
	if f.sessionActive(t, s1.TokenHash) {
		t.Fatal("SetUserAccess left the pre-grant session active")
	}
	// Re-grant replaces the project set wholesale on the SAME group and
	// revokes the sessions minted since.
	s2 := f.activeSession(t, u.ID)
	f.grant(t, u.ID, "user", pc)
	g2, err := f.identity.GetGroupByName(ctx, "auto-user-"+u.ID)
	wantOK(t, "GetGroupByName(after re-grant)", err)
	if g2.ID != g.ID {
		t.Fatalf("re-grant created a second backing group %s (was %s)", g2.ID, g.ID)
	}
	if _, projects, _ := f.principal(t, u.ID); len(projects) != 1 || !projects[pc] {
		t.Fatalf("re-grant did not replace projects wholesale: %v, want {%s}", projects, pc)
	}
	if f.sessionActive(t, s2.TokenHash) {
		t.Fatal("re-grant left the session active")
	}
}

func identitySetUserAccessAdmin(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	u := f.user(t)
	ignored := uniqueID("proj")
	f.grant(t, u.ID, "admin", ignored)
	role, projects, _ := f.principal(t, u.ID)
	if role != "admin" || len(projects) != 0 {
		t.Fatalf("admin grant: role %q projects %v, want admin with no projects (instance-wide)", role, projects)
	}
	g, err := f.identity.GetGroupByName(ctx, "auto-user-"+u.ID)
	wantOK(t, "GetGroupByName", err)
	if g.Role != "admin" {
		t.Fatalf("backing group role = %q, want admin", g.Role)
	}
	// Demotion re-syncs the existing group's role (the suite's standing
	// admin keeps the guard quiet).
	pb := uniqueID("proj")
	f.grant(t, u.ID, "user", pb)
	if role, projects, _ := f.principal(t, u.ID); role != "user" || len(projects) != 1 || !projects[pb] {
		t.Fatalf("after demotion: role %q projects %v, want user/{%s}", role, projects, pb)
	}
}

func identitySetUserAccessValidation(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	u := f.user(t)
	wantErr(t, "SetUserAccess(user, no projects)", f.identity.SetUserAccess(ctx, u.ID, "user", nil), persistence.ErrNoProjects)
	if err := f.identity.SetUserAccess(ctx, u.ID, "superuser", []string{"*"}); err == nil {
		t.Fatal("SetUserAccess accepted role superuser")
	}
	// Neither rejected call may have created the backing group.
	_, err := f.identity.GetGroupByName(ctx, "auto-user-"+u.ID)
	wantErr(t, "GetGroupByName after rejected grants", err, persistence.ErrGroupNotFound)
}

func identityRemoveUserAccess(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	u := f.user(t)
	manual := uniqueID("proj")
	f.member(t, f.group(t, "user", manual).ID, u.ID) // untouched by RemoveUserAccess
	f.grant(t, u.ID, "user", uniqueID("proj"))
	s := f.activeSession(t, u.ID)
	wantOK(t, "RemoveUserAccess", f.identity.RemoveUserAccess(ctx, u.ID))
	if f.sessionActive(t, s.TokenHash) {
		t.Fatal("RemoveUserAccess left the session active")
	}
	if _, projects, _ := f.principal(t, u.ID); len(projects) != 1 || !projects[manual] {
		t.Fatalf("after RemoveUserAccess projects = %v, want only the manual group's {%s}", projects, manual)
	}
	if v := f.view(t, u.ID); v.Role != "user" {
		t.Fatalf("manual membership lost: role %q", v.Role)
	}
	// With ONLY a backing group the user returns to awaiting.
	lone := f.user(t)
	f.grant(t, lone.ID, "user", "*")
	wantOK(t, "RemoveUserAccess(lone)", f.identity.RemoveUserAccess(ctx, lone.ID))
	if v := f.view(t, lone.ID); v.Role != "" || len(v.Projects) != 0 {
		t.Fatalf("after RemoveUserAccess: role %q projects %v, want awaiting", v.Role, v.Projects)
	}
	// No backing group at all: a no-op, not an error.
	wantOK(t, "RemoveUserAccess(no backing group)", f.identity.RemoveUserAccess(ctx, f.user(t).ID))
}

func identitySetUserDisabled(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	u := f.user(t)
	s := f.activeSession(t, u.ID)
	wantOK(t, "SetUserDisabled(true)", f.identity.SetUserDisabled(ctx, u.ID, true))
	if f.sessionActive(t, s.TokenHash) {
		t.Fatal("disable left the session active")
	}
	if _, _, disabled := f.principal(t, u.ID); !disabled {
		t.Fatal("resolver does not report the user disabled")
	}
	wantOK(t, "SetUserDisabled(false)", f.identity.SetUserDisabled(ctx, u.ID, false))
	if _, _, disabled := f.principal(t, u.ID); disabled {
		t.Fatal("re-enable did not clear disabled")
	}
	if f.sessionActive(t, s.TokenHash) {
		t.Fatal("re-enable resurrected a revoked session")
	}
	ghost := uniqueID("ghost")
	wantErr(t, "SetUserDisabled(unknown, true)", f.identity.SetUserDisabled(ctx, ghost, true), persistence.ErrUserNotFound)
	wantErr(t, "SetUserDisabled(unknown, false)", f.identity.SetUserDisabled(ctx, ghost, false), persistence.ErrUserNotFound)
	// Standalone kick: revokes, and zero active sessions is fine.
	s2 := f.activeSession(t, u.ID)
	wantOK(t, "RevokeSessionsForUser", f.identity.RevokeSessionsForUser(ctx, u.ID))
	if f.sessionActive(t, s2.TokenHash) {
		t.Fatal("RevokeSessionsForUser left the session active")
	}
	wantOK(t, "RevokeSessionsForUser(none left)", f.identity.RevokeSessionsForUser(ctx, u.ID))
}

func identityRevokeIdentity(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	u := f.user(t)
	ext := uniqueID("ext")
	f.bind(t, u.ID, "github", ext)
	s := f.activeSession(t, u.ID)
	wantOK(t, "RevokeIdentity", f.identity.RevokeIdentity(ctx, "github", ext))
	if f.sessionActive(t, s.TokenHash) {
		t.Fatal("RevokeIdentity left the user's session active")
	}
	wantErr(t, "RevokeIdentity(already revoked)", f.identity.RevokeIdentity(ctx, "github", ext), persistence.ErrIdentityNotFound)
	wantErr(t, "RevokeIdentity(absent)", f.identity.RevokeIdentity(ctx, "github", uniqueID("absent")), persistence.ErrIdentityNotFound)
}

// identityRevokeIdentityOwnedBy pins the atomic ownership predicate on BOTH
// drivers. Regression: audit 2026-09-15 CA-10 — self-unlink checked ownership
// from a snapshot and then revoked by (channel, external_id) alone, so a
// binding reassigned in between was revoked by its FORMER owner, taking the
// new owner's sessions with it. Both drivers shared the unqualified
// predicate, which is why this belongs in the shared suite.
func identityRevokeIdentityOwnedBy(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	owner := f.user(t)
	other := f.user(t)
	ext := uniqueID("ext")
	f.bind(t, owner.ID, "api_key", ext)
	sess := f.activeSession(t, owner.ID)

	// A stranger's unlink of someone else's binding is refused, and leaves
	// the binding and the owner's sessions intact.
	wantErr(t, "RevokeIdentityOwnedBy(wrong owner)",
		f.identity.RevokeIdentityOwnedBy(ctx, "api_key", ext, other.ID), persistence.ErrIdentityNotFound)
	if !f.sessionActive(t, sess.TokenHash) {
		t.Fatal("a refused owner-qualified revoke still killed the real owner's session")
	}
	if rows, err := f.identity.ResolvePrincipalRows(ctx, "api_key", ext); err != nil || len(rows) == 0 {
		t.Fatalf("a refused owner-qualified revoke still revoked the binding (rows=%d, err=%v)", len(rows), err)
	}

	// The real owner succeeds, and their sessions go with it.
	wantOK(t, "RevokeIdentityOwnedBy(owner)", f.identity.RevokeIdentityOwnedBy(ctx, "api_key", ext, owner.ID))
	if f.sessionActive(t, sess.TokenHash) {
		t.Fatal("RevokeIdentityOwnedBy left the user's session active")
	}
	wantErr(t, "RevokeIdentityOwnedBy(already revoked)",
		f.identity.RevokeIdentityOwnedBy(ctx, "api_key", ext, owner.ID), persistence.ErrIdentityNotFound)
	wantErr(t, "RevokeIdentityOwnedBy(absent)",
		f.identity.RevokeIdentityOwnedBy(ctx, "api_key", uniqueID("absent"), owner.ID), persistence.ErrIdentityNotFound)
}

func identityMigrateExternalID(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	resolves := func(ext string) (string, bool) {
		rows, err := f.identity.ResolvePrincipalRows(ctx, "github", ext)
		wantOK(t, "ResolvePrincipalRows", err)
		if len(rows) == 0 {
			return "", false
		}
		return rows[0].UserID, true
	}
	// Branch 1: the new key is free → the legacy row is repointed.
	u := f.user(t)
	oldKey, newKey := uniqueID("login"), uniqueID("id")
	f.bind(t, u.ID, "github", oldKey)
	wantOK(t, "MigrateIdentityExternalID(free)", f.identity.MigrateIdentityExternalID(ctx, "github", oldKey, newKey))
	if uid, ok := resolves(newKey); !ok || uid != u.ID {
		t.Fatalf("new key resolves to (%q,%v), want %s", uid, ok, u.ID)
	}
	if _, ok := resolves(oldKey); ok {
		t.Fatal("legacy key still resolves after repoint")
	}
	// Branch 2: the new key is taken → that row is canonical, the legacy
	// row is revoked rather than repointed.
	legacy, canonical := f.user(t), f.user(t)
	oldKey2, newKey2 := uniqueID("login"), uniqueID("id")
	f.bind(t, legacy.ID, "github", oldKey2)
	f.bind(t, canonical.ID, "github", newKey2)
	wantOK(t, "MigrateIdentityExternalID(taken)", f.identity.MigrateIdentityExternalID(ctx, "github", oldKey2, newKey2))
	if uid, ok := resolves(newKey2); !ok || uid != canonical.ID {
		t.Fatalf("canonical binding changed: (%q,%v), want %s", uid, ok, canonical.ID)
	}
	if _, ok := resolves(oldKey2); ok {
		t.Fatal("legacy row was not revoked when the new key was taken")
	}
	// Absent / already-revoked legacy binding: a no-op, not an error.
	wantOK(t, "MigrateIdentityExternalID(absent)", f.identity.MigrateIdentityExternalID(ctx, "github", uniqueID("absent"), uniqueID("id")))
	wantOK(t, "MigrateIdentityExternalID(idempotent)", f.identity.MigrateIdentityExternalID(ctx, "github", oldKey2, newKey2))
}

func identityNoops(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	wantOK(t, "SetGroupRole(missing group)", f.identity.SetGroupRole(ctx, uniqueID("nogrp"), "admin"))
	wantOK(t, "RemoveGroupMember(absent)", f.identity.RemoveGroupMember(ctx, uniqueID("nogrp"), uniqueID("nouser")))
	// SetGroupRole on a real group is visible to the resolver.
	u := f.user(t)
	g := f.group(t, "user", uniqueID("proj"))
	f.member(t, g.ID, u.ID)
	wantOK(t, "SetGroupRole", f.identity.SetGroupRole(ctx, g.ID, "admin"))
	if role, _, _ := f.principal(t, u.ID); role != "admin" {
		t.Fatalf("SetGroupRole not reflected: role %q", role)
	}
	wantOK(t, "RemoveGroupMember", f.identity.RemoveGroupMember(ctx, g.ID, u.ID))
	if v := f.view(t, u.ID); v.Role != "" {
		t.Fatalf("RemoveGroupMember not reflected: role %q", v.Role)
	}
}

// identityBothResolversAgree pins the collapse — the central claim the whole
// identity feature rests on: "a linked sender resolves to the SAME Principal
// the web session resolves to."
//
// The two doors reach it by two DIFFERENT queries. The chat door calls
// ResolvePrincipalRows, keyed by (channel, external_id); the web door calls
// ResolveUserPrincipalRows, keyed by user id. Both feed the same
// principalFromRows, so a divergence would be in the SQL — a join dropped
// from one, a role or project column missing from the other — and neither
// query's own test would notice, because each would still be internally
// consistent. Two people, one account, different project scope depending on
// which door they came through.
//
// Asserted at the repository contract, on BOTH drivers, because that is where
// the divergence would live. Each resolver's semantics are tested separately
// in internal/authz; nothing compared them until 2026-09-15.
func identityBothResolversAgree(t *testing.T, f *identityFixture) {
	ctx := context.Background()
	u := f.user(t)
	ext := uniqueID("ext")
	f.bind(t, u.ID, "telegram", ext)

	// A user with real scope: two groups, one of them multi-project, so a
	// query that dropped a join or de-duplicated wrongly shows up.
	g1 := f.group(t, "user", "alpha", "beta")
	g2 := f.group(t, "user", "gamma")
	f.member(t, g1.ID, u.ID)
	f.member(t, g2.ID, u.ID)

	chatRows, err := f.identity.ResolvePrincipalRows(ctx, "telegram", ext)
	wantOK(t, "ResolvePrincipalRows", err)
	webRows, err := f.identity.ResolveUserPrincipalRows(ctx, u.ID)
	wantOK(t, "ResolveUserPrincipalRows", err)

	if got := principalShape(chatRows); got != principalShape(webRows) {
		t.Errorf("the two doors disagree about the same person:\n  chat: %s\n  web:  %s\n"+
			"a linked sender must resolve to the SAME principal the web session does — "+
			"otherwise project scope depends on which door they came through", got, principalShape(webRows))
	}
	// And the shape must be non-trivial, or the comparison above passes on
	// two empty sets and asserts nothing.
	if len(chatRows) == 0 {
		t.Fatal("the chat door resolved nothing; the agreement above is vacuous")
	}

	// Disabling must reach both doors identically — it is the one property
	// §5.0 calls the whole feature's justification.
	wantOK(t, "SetUserDisabled", f.identity.SetUserDisabled(ctx, u.ID, true))
	chatRows, err = f.identity.ResolvePrincipalRows(ctx, "telegram", ext)
	wantOK(t, "ResolvePrincipalRows(disabled)", err)
	webRows, err = f.identity.ResolveUserPrincipalRows(ctx, u.ID)
	wantOK(t, "ResolveUserPrincipalRows(disabled)", err)
	if len(chatRows) == 0 || !chatRows[0].Disabled {
		t.Errorf("the chat door does not see the disable: %+v", chatRows)
	}
	if len(webRows) == 0 || !webRows[0].Disabled {
		t.Errorf("the web door does not see the disable: %+v", webRows)
	}
	wantOK(t, "re-enable", f.identity.SetUserDisabled(ctx, u.ID, false))
}

// principalShape renders the principal-relevant content of a row set in a
// stable, order-independent form: everything principalFromRows reads, and
// nothing else. Comparing rendered shapes rather than slices keeps the
// assertion about the PRINCIPAL rather than about row ordering, which the two
// queries are not required to match on.
func principalShape(rows []persistence.PrincipalRow) string {
	if len(rows) == 0 {
		return "<none>"
	}
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		role, project := "-", "-"
		if r.Role != nil {
			role = *r.Role
		}
		if r.ProjectID != nil {
			project = *r.ProjectID
		}
		parts = append(parts, role+"/"+project)
	}
	sort.Strings(parts)
	return rows[0].UserID + " disabled=" + strconv.FormatBool(rows[0].Disabled) +
		" [" + strings.Join(parts, " ") + "]"
}
