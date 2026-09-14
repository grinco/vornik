package authz

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// stubIdentityRepo serves canned resolver rows. Unimplemented methods panic
// via the embedded nil interface (a test bug, not a production path).
type stubIdentityRepo struct {
	persistence.IdentityRepository
	rows           map[string][]persistence.PrincipalRow
	userRows       map[string][]persistence.PrincipalRow
	resolveErr     error
	resolveUserErr error
}

func key(channel, externalID string) string { return channel + ":" + externalID }

func (s *stubIdentityRepo) ResolvePrincipalRows(_ context.Context, channel, externalID string) ([]persistence.PrincipalRow, error) {
	if s.resolveErr != nil {
		return nil, s.resolveErr
	}
	return s.rows[key(channel, externalID)], nil
}

func (s *stubIdentityRepo) ResolveUserPrincipalRows(_ context.Context, userID string) ([]persistence.PrincipalRow, error) {
	if s.resolveUserErr != nil {
		return nil, s.resolveUserErr
	}
	return s.userRows[userID], nil
}

func strp(s string) *string { return &s }

// Ported from the Enterprise package on extraction (2026-09-13, R1): the
// resolver's semantics are CE now and this is their pin.
func TestResolve_Semantics(t *testing.T) {
	cases := []struct {
		name      string
		rows      []persistence.PrincipalRow
		wantRole  string
		wantProjs []string
		wantErr   error
	}{
		{name: "unknown identity", rows: nil, wantErr: ErrUnknownIdentity},
		{name: "disabled user", rows: []persistence.PrincipalRow{{UserID: "u1", Disabled: true, Role: strp("admin")}}, wantErr: ErrUserDisabled},
		{name: "admin wins, projects collapse to *", rows: []persistence.PrincipalRow{
			{UserID: "u1", Role: strp("user"), ProjectID: strp("proj-a")},
			{UserID: "u1", Role: strp("admin")},
		}, wantRole: "admin", wantProjs: []string{"*"}},
		{name: "multi-group union, deduped, sorted", rows: []persistence.PrincipalRow{
			{UserID: "u1", Role: strp("user"), ProjectID: strp("proj-b")},
			{UserID: "u1", Role: strp("user"), ProjectID: strp("proj-a")},
			{UserID: "u1", Role: strp("user"), ProjectID: strp("proj-a")},
		}, wantRole: "user", wantProjs: []string{"proj-a", "proj-b"}},
		{name: "star entry wins the union", rows: []persistence.PrincipalRow{
			{UserID: "u1", Role: strp("user"), ProjectID: strp("proj-a")},
			{UserID: "u1", Role: strp("user"), ProjectID: strp("*")},
		}, wantRole: "user", wantProjs: []string{"*"}},
		{name: "zero groups: authenticated, no access", rows: []persistence.PrincipalRow{{UserID: "u1"}}, wantRole: "user"},
		{name: "user group with zero projects grants nothing", rows: []persistence.PrincipalRow{{UserID: "u1", Role: strp("user")}}, wantRole: "user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &stubIdentityRepo{rows: map[string][]persistence.PrincipalRow{key("telegram", "42"): tc.rows}}
			p, err := NewService(repo).Resolve(context.Background(), "telegram", "42")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Role != tc.wantRole {
				t.Errorf("Role = %q, want %q", p.Role, tc.wantRole)
			}
			if len(p.Projects) != len(tc.wantProjs) {
				t.Fatalf("Projects = %v, want %v", p.Projects, tc.wantProjs)
			}
			for i := range tc.wantProjs {
				if p.Projects[i] != tc.wantProjs[i] {
					t.Errorf("Projects = %v, want %v", p.Projects, tc.wantProjs)
				}
			}
		})
	}
}

func TestResolve_TransportErrorIsNotUnknown(t *testing.T) {
	_, err := NewService(&stubIdentityRepo{resolveErr: errors.New("db down")}).Resolve(context.Background(), "slack", "x")
	if err == nil || errors.Is(err, ErrUnknownIdentity) {
		t.Fatalf("transport error must not masquerade as unknown identity: %v", err)
	}
	_, err = NewService(&stubIdentityRepo{resolveUserErr: errors.New("db down")}).ResolveUser(context.Background(), "u1")
	if err == nil || errors.Is(err, ErrUnknownIdentity) {
		t.Fatalf("transport error must not masquerade as unknown identity: %v", err)
	}
}

// Design test 23 (the door half is asserted at the door): a nil resolver
// is UNAVAILABLE, a distinct sentinel from unknown — a door must refuse to
// serve rather than fall back to an allowlist.
func TestResolve_NilResolverIsUnavailable(t *testing.T) {
	var s *Service
	_, err := s.Resolve(context.Background(), "telegram", "1")
	if !errors.Is(err, ErrResolverUnavailable) || errors.Is(err, ErrUnknownIdentity) {
		t.Fatalf("nil service must be unavailable, not unknown: %v", err)
	}
	_, err = NewService(nil).ResolveUser(context.Background(), "u1")
	if !errors.Is(err, ErrResolverUnavailable) {
		t.Fatalf("nil repo must be unavailable: %v", err)
	}
}

func TestResolveUser_Semantics(t *testing.T) {
	repo := &stubIdentityRepo{userRows: map[string][]persistence.PrincipalRow{
		"gone":     nil,
		"disabled": {{UserID: "disabled", Disabled: true, Role: strp("admin")}},
		"admin":    {{UserID: "admin", Role: strp("admin")}},
	}}
	svc := NewService(repo)
	if _, err := svc.ResolveUser(context.Background(), "gone"); !errors.Is(err, ErrUnknownIdentity) {
		t.Fatalf("gone user: %v", err)
	}
	if _, err := svc.ResolveUser(context.Background(), "disabled"); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("disabled user: %v", err)
	}
	p, err := svc.ResolveUser(context.Background(), "admin")
	if err != nil || p.Role != RoleAdmin || !p.AllProjects() || !p.CanAccessProject("anything") {
		t.Fatalf("admin: %+v %v", p, err)
	}
	scoped := &Principal{Role: RoleUser, Projects: []string{"a"}}
	if !scoped.CanAccessProject("a") || scoped.CanAccessProject("b") || scoped.AllProjects() {
		t.Fatal("scoped principal reach")
	}
	var nilP *Principal
	if nilP.CanAccessProject("a") || nilP.AllProjects() {
		t.Fatal("nil principal reaches nothing")
	}
}
