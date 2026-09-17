package featuredoctor

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// stubIdentityRepo is the narrow double for the identity prereq: only
// ListUsers is exercised; every other method is a no-op that satisfies the
// interface.
type stubIdentityRepo struct {
	users []persistence.UserAdminView
	err   error
}

func (s *stubIdentityRepo) ListUsers(context.Context) ([]persistence.UserAdminView, error) {
	return s.users, s.err
}
func (s *stubIdentityRepo) CreateUser(context.Context, *persistence.User) error   { return nil }
func (s *stubIdentityRepo) SetUserDisabled(context.Context, string, bool) error   { return nil }
func (s *stubIdentityRepo) RevokeSessionsForUser(context.Context, string) error   { return nil }
func (s *stubIdentityRepo) CreateGroup(context.Context, *persistence.Group) error { return nil }
func (s *stubIdentityRepo) GetGroupByName(context.Context, string) (*persistence.Group, error) {
	return nil, persistence.ErrGroupNotFound
}
func (s *stubIdentityRepo) SetGroupProjects(context.Context, string, []string) error { return nil }
func (s *stubIdentityRepo) SetGroupRole(context.Context, string, string) error       { return nil }
func (s *stubIdentityRepo) AddGroupMember(context.Context, string, string) error     { return nil }
func (s *stubIdentityRepo) RemoveGroupMember(context.Context, string, string) error  { return nil }
func (s *stubIdentityRepo) BindIdentity(context.Context, *persistence.UserIdentity) (bool, error) {
	return true, nil
}
func (s *stubIdentityRepo) RebindIdentity(context.Context, *persistence.UserIdentity) error {
	return nil
}
func (s *stubIdentityRepo) MigrateIdentityExternalID(context.Context, string, string, string) error {
	return nil
}
func (s *stubIdentityRepo) RevokeIdentity(context.Context, string, string) error { return nil }
func (s *stubIdentityRepo) RevokeIdentityOwnedBy(context.Context, string, string, string) error {
	return nil
}
func (s *stubIdentityRepo) TouchIdentityLastUsed(context.Context, string, string) error { return nil }
func (s *stubIdentityRepo) ResolvePrincipalRows(context.Context, string, string) ([]persistence.PrincipalRow, error) {
	return nil, nil
}
func (s *stubIdentityRepo) ResolveUserPrincipalRows(context.Context, string) ([]persistence.PrincipalRow, error) {
	return nil, nil
}
func (s *stubIdentityRepo) SetUserAccess(context.Context, string, string, []string) error { return nil }
func (s *stubIdentityRepo) RemoveUserAccess(context.Context, string) error                { return nil }

func TestIdentityFeature_RegisteredCommunity(t *testing.T) {
	var found bool
	for _, f := range Registry() {
		if f.ID == "identity" {
			found = true
			if f.Edition != "community" {
				t.Fatalf("identity must be Community (plan §2), got %q", f.Edition)
			}
			if len(f.Gates) != 1 || f.Gates[0].Key != "identity.enabled" {
				t.Fatalf("identity gate must be identity.enabled, got %+v", f.Gates)
			}
		}
	}
	if !found {
		t.Fatal("identity feature not registered")
	}
}

func TestIdentityPrereq_TablesPresent(t *testing.T) {
	f := identityFeature()
	check := f.Prereqs[0].Check
	// Not wired → unmet, unfixable, and the detail names the backend gap.
	if r := check(context.Background(), Deps{}); r.OK || r.Fixable {
		t.Fatalf("nil identity repo must be unmet+unfixable, got %+v", r)
	}
	// Query error → unmet.
	if r := check(context.Background(), Deps{Identity: &stubIdentityRepo{err: errors.New("no such table: users")}}); r.OK {
		t.Fatalf("query error must be unmet, got %+v", r)
	}
	// Present, zero accounts → met (an empty identity core is still present).
	if r := check(context.Background(), Deps{Identity: &stubIdentityRepo{}}); !r.OK {
		t.Fatalf("readable tables must be met, got %+v", r)
	}
	// Verify mirrors the prereq.
	if r := f.Verify(context.Background(), Deps{Identity: &stubIdentityRepo{users: []persistence.UserAdminView{{UserID: "u1"}}}}); !r.OK {
		t.Fatalf("verify must pass on a readable store, got %+v", r)
	}
}
