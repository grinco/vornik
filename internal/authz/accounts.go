package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Accounts is the COMMUNITY account-management service: an authorized
// operator creates, grants, revokes, disables and unlinks accounts over the
// shared identity repository. It is deliberately NOT the Enterprise
// first-contact provisioner (`Provision`): federated first contact creates
// an account because an identity provider vouched for it; this creates one
// because an operator did (review R1 — "CE account creation is a separate
// authorized service over the shared repository").
//
// Every mutation is audited BEFORE it returns and REFUSED when no audit
// sink is wired: an access change nobody can see is a compliance gap, not
// a convenience. Last-admin protection is enforced by the repository inside
// its transaction (persistence.ErrLastAdmin); this layer surfaces it.
type Accounts struct {
	repo  persistence.IdentityRepository
	audit persistence.AdminAuditRepository
	now   func() time.Time
}

// Actor names who performed an account-management action, for the audit
// row. Principal is a stable, non-secret id (a key id, a session user id,
// "auth-disabled"); never a bearer secret.
type Actor struct {
	Principal string
	Source    string // "api" | "cli" | "ui"
	IP        string
	UserAgent string
}

// ErrUnauditable is returned when no audit repository is wired; no
// mutation happens.
var ErrUnauditable = errors.New("authz: admin audit log not wired; refusing unauditable access change")

// ErrInvalidRole is returned for a role outside admin|user.
var ErrInvalidRole = errors.New("authz: role must be admin or user")

// ErrIdentityNotOwned is returned when an unlink names a binding that
// does not belong to the target account — a crafted request must not
// unlink someone else's identity.
var ErrIdentityNotOwned = errors.New("authz: that identity does not belong to this account")

// NewAccounts constructs the service. audit may be nil, in which case every
// mutation returns ErrUnauditable.
func NewAccounts(repo persistence.IdentityRepository, audit persistence.AdminAuditRepository) *Accounts {
	return &Accounts{repo: repo, audit: audit, now: func() time.Time { return time.Now().UTC() }}
}

// CreateRequest describes a new account.
type CreateRequest struct {
	DisplayName string
	// Role is admin|user; empty creates an account that is AWAITING access
	// (authenticated once linked, sees nothing) until granted.
	Role     string
	Projects []string // user role only; ["*"] = all
}

// Create makes an account and, when a role is given, grants it. Returns
// the created user.
func (a *Accounts) Create(ctx context.Context, req CreateRequest, actor Actor) (*persistence.User, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.DisplayName)
	if name == "" {
		return nil, errors.New("authz: display name is required")
	}
	if req.Role != "" && req.Role != RoleAdmin && req.Role != RoleUser {
		return nil, ErrInvalidRole
	}
	u := &persistence.User{ID: persistence.GenerateID("user"), DisplayName: name, CreatedAt: a.now()}
	if err := a.repo.CreateUser(ctx, u); err != nil {
		return nil, fmt.Errorf("authz: create account: %w", err)
	}
	if req.Role != "" {
		if err := a.repo.SetUserAccess(ctx, u.ID, req.Role, normalizeProjects(req.Projects)); err != nil {
			return nil, fmt.Errorf("authz: grant on create: %w", err)
		}
	}
	if err := a.record(ctx, actor, "account.create", u.ID, map[string]any{"display_name": name, "role": req.Role, "projects": req.Projects}); err != nil {
		return u, err
	}
	return u, nil
}

// Grant sets an account's role and project scope (user role) through its
// backing group; the repository revokes the account's sessions in the same
// transaction so the new principal is resolved fresh.
func (a *Accounts) Grant(ctx context.Context, userID, role string, projects []string, actor Actor) error {
	if err := a.ready(); err != nil {
		return err
	}
	if role != RoleAdmin && role != RoleUser {
		return ErrInvalidRole
	}
	if err := a.repo.SetUserAccess(ctx, userID, role, normalizeProjects(projects)); err != nil {
		return fmt.Errorf("authz: grant: %w", err)
	}
	return a.record(ctx, actor, "account.grant", userID, map[string]any{"role": role, "projects": projects})
}

// Revoke returns an account to awaiting access (its backing-group
// membership is dropped, sessions revoked). This is a DELIBERATE
// revocation: the repository marks it so a later federated bootstrap login
// does not silently re-grant it (review R3).
func (a *Accounts) Revoke(ctx context.Context, userID string, actor Actor) error {
	if err := a.ready(); err != nil {
		return err
	}
	if err := a.repo.RemoveUserAccess(ctx, userID); err != nil {
		return fmt.Errorf("authz: revoke: %w", err)
	}
	return a.record(ctx, actor, "account.revoke_access", userID, nil)
}

// SetDisabled disables (revoking every door at once — sessions in the
// same transaction, chat bindings on their next resolve, owned keys on
// their next request) or re-enables an account.
func (a *Accounts) SetDisabled(ctx context.Context, userID string, disabled bool, actor Actor) error {
	if err := a.ready(); err != nil {
		return err
	}
	if err := a.repo.SetUserDisabled(ctx, userID, disabled); err != nil {
		return fmt.Errorf("authz: set disabled: %w", err)
	}
	action := "account.enable"
	if disabled {
		action = "account.disable"
	}
	return a.record(ctx, actor, action, userID, map[string]any{"disabled": disabled})
}

// Unlink revokes one channel binding of the account. The binding must
// belong to userID.
func (a *Accounts) Unlink(ctx context.Context, userID, channel, externalID string, actor Actor) error {
	if err := a.ready(); err != nil {
		return err
	}
	views, err := a.repo.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("authz: unlink: %w", err)
	}
	owned := false
	for _, v := range views {
		if v.UserID != userID {
			continue
		}
		for _, idn := range v.Identities {
			if idn.Channel == channel && idn.ExternalID == externalID {
				owned = true
			}
		}
	}
	if !owned {
		return ErrIdentityNotOwned
	}
	if err := a.repo.RevokeIdentity(ctx, channel, externalID); err != nil {
		return fmt.Errorf("authz: unlink: %w", err)
	}
	return a.record(ctx, actor, "account.unlink", userID, map[string]any{"channel": channel, "external_id": externalID})
}

// List returns every account with its aggregated role, projects, active
// bindings and session count.
func (a *Accounts) List(ctx context.Context) ([]persistence.UserAdminView, error) {
	if a == nil || a.repo == nil {
		return nil, ErrResolverUnavailable
	}
	return a.repo.ListUsers(ctx)
}

// Get returns one account's aggregated view, or persistence.ErrUserNotFound.
func (a *Accounts) Get(ctx context.Context, userID string) (*persistence.UserAdminView, error) {
	views, err := a.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range views {
		if views[i].UserID == userID {
			return &views[i], nil
		}
	}
	return nil, persistence.ErrUserNotFound
}

func (a *Accounts) ready() error {
	if a == nil || a.repo == nil {
		return ErrResolverUnavailable
	}
	if a.audit == nil {
		return ErrUnauditable
	}
	return nil
}

// record writes the audit row. A failed write is returned as an error so
// the caller can say "changed, but the audit write failed" rather than
// pretend the trail is complete.
func (a *Accounts) record(ctx context.Context, actor Actor, action, target string, after any) error {
	var afterJSON string
	if after != nil {
		if b, err := json.Marshal(after); err == nil {
			afterJSON = string(b)
		}
	}
	principal := actor.Principal
	if principal == "" {
		principal = "unknown"
	}
	source := actor.Source
	if source == "" {
		source = "api"
	}
	if err := a.audit.Insert(ctx, &persistence.AdminAuditEntry{
		Timestamp: a.now(),
		Principal: principal,
		Source:    source,
		Action:    action,
		Target:    target,
		After:     afterJSON,
		IP:        actor.IP,
		UserAgent: actor.UserAgent,
	}); err != nil {
		return fmt.Errorf("authz: %s applied but the AUDIT WRITE FAILED: %w", action, err)
	}
	return nil
}

// normalizeProjects collapses a set containing "*" to exactly ["*"].
func normalizeProjects(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == "*" {
			return []string{"*"}
		}
		out = append(out, p)
	}
	return out
}
