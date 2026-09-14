package authz

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// memIdentityRepo is an in-memory IdentityRepository slice sufficient for
// the account service: users, access grants, bindings, revocations.
type memIdentityRepo struct {
	persistence.IdentityRepository
	users     map[string]*persistence.User
	access    map[string]string // userID → role
	projects  map[string][]string
	bindings  map[string]string // channel:ext → userID
	revoked   map[string]bool   // channel:ext
	disabled  map[string]bool
	lastAdmin bool // when true, any admin-removing mutation returns ErrLastAdmin
}

func newMemRepo() *memIdentityRepo {
	return &memIdentityRepo{users: map[string]*persistence.User{}, access: map[string]string{}, projects: map[string][]string{},
		bindings: map[string]string{}, revoked: map[string]bool{}, disabled: map[string]bool{}}
}

func (m *memIdentityRepo) CreateUser(_ context.Context, u *persistence.User) error {
	m.users[u.ID] = u
	return nil
}
func (m *memIdentityRepo) SetUserAccess(_ context.Context, userID, role string, projects []string) error {
	if _, ok := m.users[userID]; !ok {
		return persistence.ErrUserNotFound
	}
	if role == "user" && len(projects) == 0 {
		return persistence.ErrNoProjects
	}
	if m.lastAdmin && role != "admin" {
		return persistence.ErrLastAdmin
	}
	m.access[userID] = role
	m.projects[userID] = projects
	return nil
}
func (m *memIdentityRepo) RemoveUserAccess(_ context.Context, userID string) error {
	if m.lastAdmin {
		return persistence.ErrLastAdmin
	}
	delete(m.access, userID)
	return nil
}
func (m *memIdentityRepo) SetUserDisabled(_ context.Context, userID string, disabled bool) error {
	if _, ok := m.users[userID]; !ok {
		return persistence.ErrUserNotFound
	}
	if m.lastAdmin && disabled {
		return persistence.ErrLastAdmin
	}
	m.disabled[userID] = disabled
	return nil
}
func (m *memIdentityRepo) RevokeIdentity(_ context.Context, channel, ext string) error {
	k := channel + ":" + ext
	if _, ok := m.bindings[k]; !ok || m.revoked[k] {
		return persistence.ErrIdentityNotFound
	}
	m.revoked[k] = true
	return nil
}
func (m *memIdentityRepo) ListUsers(context.Context) ([]persistence.UserAdminView, error) {
	var out []persistence.UserAdminView
	for id, u := range m.users {
		v := persistence.UserAdminView{UserID: id, DisplayName: u.DisplayName, Role: m.access[id], Projects: m.projects[id], Disabled: m.disabled[id]}
		for k, uid := range m.bindings {
			if uid == id && !m.revoked[k] {
				ch, ext, _ := cut(k)
				v.Identities = append(v.Identities, persistence.UserIdentityRef{Channel: ch, ExternalID: ext})
			}
		}
		out = append(out, v)
	}
	return out, nil
}

func cut(k string) (string, string, bool) {
	for i := 0; i < len(k); i++ {
		if k[i] == ':' {
			return k[:i], k[i+1:], true
		}
	}
	return k, "", false
}

type memAudit struct {
	rows []*persistence.AdminAuditEntry
	err  error
}

func (a *memAudit) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	if a.err != nil {
		return a.err
	}
	a.rows = append(a.rows, e)
	return nil
}
func (a *memAudit) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return a.rows, nil
}

var actor = Actor{Principal: "api_key:k1", Source: "api"}

func TestAccounts_CreateGrantRevokeAudited(t *testing.T) {
	repo, audit := newMemRepo(), &memAudit{}
	acc := NewAccounts(repo, audit)
	ctx := context.Background()

	u, err := acc.Create(ctx, CreateRequest{DisplayName: "Ada", Role: "user", Projects: []string{"a", "*"}}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if repo.access[u.ID] != "user" || len(repo.projects[u.ID]) != 1 || repo.projects[u.ID][0] != "*" {
		t.Fatalf("create must grant with '*' collapsed: %v %v", repo.access, repo.projects)
	}
	if err := acc.Grant(ctx, u.ID, "admin", nil, actor); err != nil {
		t.Fatal(err)
	}
	if err := acc.Revoke(ctx, u.ID, actor); err != nil {
		t.Fatal(err)
	}
	if _, ok := repo.access[u.ID]; ok {
		t.Fatal("revoke must return the account to awaiting")
	}
	if err := acc.SetDisabled(ctx, u.ID, true, actor); err != nil || !repo.disabled[u.ID] {
		t.Fatalf("disable: %v", err)
	}
	want := []string{"account.create", "account.grant", "account.revoke_access", "account.disable"}
	if len(audit.rows) != len(want) {
		t.Fatalf("audit rows = %d, want %d", len(audit.rows), len(want))
	}
	for i, w := range want {
		if audit.rows[i].Action != w || audit.rows[i].Principal != "api_key:k1" || audit.rows[i].Target != u.ID {
			t.Errorf("row %d = %+v, want action %s", i, audit.rows[i], w)
		}
	}
}

// R1/R2: an unauditable access change is refused outright — nothing is
// mutated when no audit sink is wired.
func TestAccounts_RefusesWithoutAudit(t *testing.T) {
	repo := newMemRepo()
	acc := NewAccounts(repo, nil)
	if _, err := acc.Create(context.Background(), CreateRequest{DisplayName: "x"}, actor); !errors.Is(err, ErrUnauditable) {
		t.Fatalf("want ErrUnauditable, got %v", err)
	}
	if len(repo.users) != 0 {
		t.Fatal("no user may be created when unauditable")
	}
	if err := acc.Grant(context.Background(), "u", "admin", nil, actor); !errors.Is(err, ErrUnauditable) {
		t.Fatalf("grant: %v", err)
	}
}

func TestAccounts_AuditWriteFailureIsSurfaced(t *testing.T) {
	repo := newMemRepo()
	acc := NewAccounts(repo, &memAudit{err: errors.New("disk full")})
	u, err := acc.Create(context.Background(), CreateRequest{DisplayName: "x"}, actor)
	if err == nil || u == nil {
		t.Fatalf("mutation applied but audit failed must return BOTH the user and an error, got %v %v", u, err)
	}
}

func TestAccounts_LastAdminSurfaces(t *testing.T) {
	repo := newMemRepo()
	repo.lastAdmin = true
	acc := NewAccounts(repo, &memAudit{})
	repo.users["a"] = &persistence.User{ID: "a"}
	for _, f := range []func() error{
		func() error { return acc.Revoke(context.Background(), "a", actor) },
		func() error { return acc.SetDisabled(context.Background(), "a", true, actor) },
		func() error { return acc.Grant(context.Background(), "a", "user", []string{"p"}, actor) },
	} {
		if err := f(); !errors.Is(err, persistence.ErrLastAdmin) {
			t.Fatalf("want ErrLastAdmin, got %v", err)
		}
	}
}

func TestAccounts_UnlinkOwnershipAndInvalidRole(t *testing.T) {
	repo := newMemRepo()
	acc := NewAccounts(repo, &memAudit{})
	repo.users["a"] = &persistence.User{ID: "a"}
	repo.users["b"] = &persistence.User{ID: "b"}
	repo.bindings["telegram:1"] = "a"
	ctx := context.Background()
	if err := acc.Unlink(ctx, "b", "telegram", "1", actor); !errors.Is(err, ErrIdentityNotOwned) {
		t.Fatalf("crafted unlink must be refused, got %v", err)
	}
	if err := acc.Unlink(ctx, "a", "telegram", "1", actor); err != nil || !repo.revoked["telegram:1"] {
		t.Fatalf("owned unlink: %v", err)
	}
	if err := acc.Grant(ctx, "a", "root", nil, actor); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("invalid role: %v", err)
	}
	if _, err := acc.Create(ctx, CreateRequest{DisplayName: " "}, actor); err == nil {
		t.Fatal("empty display name must be refused")
	}
	if _, err := acc.Get(ctx, "nope"); !errors.Is(err, persistence.ErrUserNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	if v, err := acc.Get(ctx, "a"); err != nil || v.UserID != "a" {
		t.Fatalf("get: %v %v", v, err)
	}
}
