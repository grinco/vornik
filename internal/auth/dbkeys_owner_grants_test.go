package auth

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// ownerAccessSpy returns a canned access record and remembers the keys asked
// about, so a test can prove the check ran rather than infer it.
type ownerAccessSpy struct {
	access OwnerAccess
	err    error
	calls  []string
}

func (s *ownerAccessSpy) check(_ context.Context, keyID string) (OwnerAccess, error) {
	s.calls = append(s.calls, keyID)
	return s.access, s.err
}

// Regression: audit 2026-09-15 CA-04 — "Account Grants Do Not Narrow a Mapped
// Key".
//
// The mapped-owner check only asked whether the owner was DISABLED.
// Authentication then built the principal from the key's own project without
// consulting the owner's grants at all, so removing a person's access through
// Accounts.RemoveUserAccess left disabled_at unset and their claimed key kept
// working — including for projects the person was never granted. Normative
// amendment R2 requires both the project intersection and denial for an owner
// with no grants.
//
// Mapping a key never EXPANDS its authority; the defect was that reducing the
// person's authority did not reduce the mapped credential's.
func TestDBKeys_MappedKeyIsNarrowedByOwnerGrants(t *testing.T) {
	cases := []struct {
		name    string
		access  OwnerAccess
		wantErr error
		why     string
	}{
		{
			name:    "owner granted this project",
			access:  OwnerAccess{Mapped: true, Projects: []string{"proj", "other"}},
			wantErr: nil,
			why:     "the key's own project is within the owner's grants",
		},
		{
			name:    "owner granted only another project",
			access:  OwnerAccess{Mapped: true, Projects: []string{"other"}},
			wantErr: ErrUnauthorized,
			why:     "the key's project is outside the owner's current grants",
		},
		{
			name:    "owner has no grants at all",
			access:  OwnerAccess{Mapped: true, Projects: nil},
			wantErr: ErrUnauthorized,
			why:     "R2: a zero-grant owner denies; awaiting-access is not access",
		},
		{
			name:    "owner is an admin",
			access:  OwnerAccess{Mapped: true, AllProjects: true},
			wantErr: nil,
			why:     "an admin owner narrows nothing",
		},
		{
			name:    "owner is disabled",
			access:  OwnerAccess{Mapped: true, Disabled: true, Projects: []string{"proj"}},
			wantErr: ErrUnauthorized,
			why:     "the existing §5.0 door must not regress",
		},
		{
			name:    "key is not mapped to any person",
			access:  OwnerAccess{Mapped: false},
			wantErr: nil,
			why:     "an unclaimed machine key keeps its own authority",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, row := newKeyCred(t)
			spy := &ownerAccessSpy{access: tc.access}
			b := NewDBKeysBackend(&stubLookup{hash: apikey.Hash(raw), row: row}, nil)
			b.OwnerAccess = spy.check

			id, err := b.Authenticate(context.Background(), Credential{BearerToken: raw})
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("Authenticate = %v, want success — %s", err, tc.why)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("Authenticate = %v, want %v — %s", err, tc.wantErr, tc.why)
			}
			if len(spy.calls) != 1 {
				t.Fatalf("owner access checked %d times, want exactly 1", len(spy.calls))
			}
			if err == nil {
				// Mapping must never WIDEN the key: whatever the owner may
				// reach, the key still reaches only its own project.
				if len(id.Projects) != 1 || id.Projects[0] != row.ProjectID {
					t.Fatalf("mapped key projects = %v, want only its own %q: mapping must not promote a key", id.Projects, row.ProjectID)
				}
			}
		})
	}
}

// An owner lookup that ERRORS must fail closed. This is an access control:
// treating a database hiccup as "no restriction" makes the door openable by
// breaking the lookup it depends on.
func TestDBKeys_OwnerAccessErrorFailsClosed(t *testing.T) {
	raw, row := newKeyCred(t)
	spy := &ownerAccessSpy{err: errors.New("database is gone")}
	b := NewDBKeysBackend(&stubLookup{hash: apikey.Hash(raw), row: row}, nil)
	b.OwnerAccess = spy.check

	if _, err := b.Authenticate(context.Background(), Credential{BearerToken: raw}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Authenticate = %v, want ErrUnauthorized", err)
	}
}

// Regression: audit 2026-09-15 CA-11, third path — persistence.APIKey's
// OwnerUserID is documented as the account that owns a key, and the config
// assistant's REST actor builder reads it to attribute a proposal to a
// person. No driver ever populated the column (the mapping lives in
// user_identities), so the field was a documented behaviour nothing
// implemented, and attribution silently reported "no person".
func TestDBKeys_StampsTheResolvedOwnerOntoTheKeyRow(t *testing.T) {
	raw, row := newKeyCred(t)
	spy := &ownerAccessSpy{access: OwnerAccess{Mapped: true, Projects: []string{"proj"}, UserID: "user_alice"}}
	b := NewDBKeysBackend(&stubLookup{hash: apikey.Hash(raw), row: row}, nil)
	b.OwnerAccess = spy.check

	id, err := b.Authenticate(context.Background(), Credential{BearerToken: raw})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := id.Extra[ExtraDBKeyRow].(*persistence.APIKey)
	if !ok {
		t.Fatalf("Extra[%s] = %T, want *persistence.APIKey", ExtraDBKeyRow, id.Extra[ExtraDBKeyRow])
	}
	if got.OwnerUserID != "user_alice" {
		t.Fatalf("OwnerUserID = %q, want the resolved owner: attribution has no person to name", got.OwnerUserID)
	}
	// The stamp must not leak back into the caller's stored row.
	if row.OwnerUserID != "" {
		t.Fatal("the looked-up row was mutated in place")
	}
}

// An UNOWNED key keeps an empty owner — executor-minted per-task keys are
// always unowned and must not acquire a person.
func TestDBKeys_UnownedKeyHasNoOwner(t *testing.T) {
	raw, row := newKeyCred(t)
	spy := &ownerAccessSpy{access: OwnerAccess{Mapped: false}}
	b := NewDBKeysBackend(&stubLookup{hash: apikey.Hash(raw), row: row}, nil)
	b.OwnerAccess = spy.check

	id, err := b.Authenticate(context.Background(), Credential{BearerToken: raw})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := id.Extra[ExtraDBKeyRow].(*persistence.APIKey)
	if got == nil || got.OwnerUserID != "" {
		t.Fatalf("an unowned key must have no owner, got %+v", got)
	}
}
