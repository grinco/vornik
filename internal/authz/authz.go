// Package authz is the COMMUNITY identity-core resolver: ONE function answers
// "who is this channel identity and what may they do" for every door — web
// sessions, Telegram and Slack senders, API keys via their owner. Extracted
// from the Enterprise package on 2026-09-13 (config-assistant review R1,
// plan §2 "R1's deliverables, by file") so that account and channel/key
// mapping ship in CE while federated login (OIDC/SSO providers, org-member
// defaults, bootstrap provisioning) stays Enterprise and imports this.
//
// Symbol manifest (review R1): CE owns Principal, RoleAdmin, RoleUser,
// ErrUnknownIdentity, ErrUserDisabled, Resolve, ResolveUser and
// principalFromRows, with a repository-only constructor. Everything
// federated — Provision, the org-member defaults, the bootstrap helpers and
// the two group constants — lives in internal/enterprise/identity/authz.
//
// The package owns no caching. Cache policy is a caller concern — the EE
// session backend keeps a 60s TTL; chat dispatchers want per-message
// freshness so revocation revokes NOW (review R4).
package authz

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

// Roles. Admin is instance-wide; user is scoped by Projects. The vocabulary
// is internal/auth's (re-homed to CE in Phase 2c); aliased here so callers
// of the resolver read one name.
const (
	RoleAdmin = auth.RoleAdmin
	RoleUser  = auth.RoleUser
)

// Sentinel errors.
var (
	// ErrUnknownIdentity: no active binding for (channel, external_id), or
	// the user row is gone.
	ErrUnknownIdentity = errors.New("authz: unknown identity")
	// ErrUserDisabled: the binding resolves to a disabled user.
	ErrUserDisabled = errors.New("authz: user disabled")
)

// Principal is the effective authenticated human (OIDC design §3.2).
type Principal struct {
	UserID      string
	DisplayName string
	Role        string   // RoleAdmin | RoleUser
	Projects    []string // ["*"] when Role == RoleAdmin; sorted, deduped
}

// AllProjects reports whether the principal may reach every project.
func (p *Principal) AllProjects() bool {
	return p != nil && len(p.Projects) == 1 && p.Projects[0] == "*"
}

// CanAccessProject reports whether the principal may reach projectID.
func (p *Principal) CanAccessProject(projectID string) bool {
	if p == nil {
		return false
	}
	if p.AllProjects() {
		return true
	}
	for _, id := range p.Projects {
		if id == projectID {
			return true
		}
	}
	return false
}

// Resolver is the narrow read surface every door consumes. It is an
// interface so a channel can be handed exactly this and nothing that
// mutates.
type Resolver interface {
	Resolve(ctx context.Context, channel, externalID string) (*Principal, error)
	ResolveUser(ctx context.Context, userID string) (*Principal, error)
}

// Service resolves channel identities and user ids to principals over the
// shared identity repository. Construct with NewService; the Enterprise
// package embeds it and adds provisioning.
type Service struct {
	repo persistence.IdentityRepository
}

// NewService constructs the resolver over the repository. The repository
// is the ONLY dependency: the authentication service accepts
// persistence.IdentityRepository and never a profile-link repository
// (review R4 — profile aliases must not become auth bindings).
func NewService(repo persistence.IdentityRepository) *Service {
	return &Service{repo: repo}
}

// Repo exposes the shared repository to the Enterprise extension (which
// provisions over it). CE callers should not need it.
func (s *Service) Repo() persistence.IdentityRepository { return s.repo }

// Resolve maps an active channel binding to its effective Principal. Role
// = admin if ANY membership is admin-role (then Projects = ["*"]);
// otherwise Projects is the deduped, sorted union of the user-role
// groups' projects, with a literal "*" entry collapsing the union. Zero
// groups → RoleUser with empty Projects ("awaiting access": authenticated,
// sees nothing).
func (s *Service) Resolve(ctx context.Context, channel, externalID string) (*Principal, error) {
	if s == nil || s.repo == nil {
		return nil, fmt.Errorf("authz: resolve %s: %w", channel, ErrResolverUnavailable)
	}
	rows, err := s.repo.ResolvePrincipalRows(ctx, channel, externalID)
	if err != nil {
		return nil, fmt.Errorf("authz: resolve %s:%s: %w", channel, externalID, err)
	}
	return principalFromRows(rows)
}

// ResolveUser computes the principal for a known user id (the session
// path — the binding was resolved at login). Same semantics as Resolve:
// ErrUnknownIdentity when the user row is gone, ErrUserDisabled when
// disabled.
func (s *Service) ResolveUser(ctx context.Context, userID string) (*Principal, error) {
	if s == nil || s.repo == nil {
		return nil, fmt.Errorf("authz: resolve user %s: %w", userID, ErrResolverUnavailable)
	}
	rows, err := s.repo.ResolveUserPrincipalRows(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("authz: resolve user %s: %w", userID, err)
	}
	return principalFromRows(rows)
}

// ErrResolverUnavailable is returned when no identity repository is wired.
// It is deliberately distinct from ErrUnknownIdentity: a door that cannot
// resolve at all must refuse to serve (design test 23), never treat the
// caller as an unknown sender and fall back to an allowlist.
var ErrResolverUnavailable = errors.New("authz: identity resolver unavailable")

// UserDisabled is THE predicate for "may this principal act right now".
//
// It reads `users.disabled_at` (surfaced as PrincipalRow.Disabled) and
// nothing else — notably NOT `users.access_revoked_at`, which answers a
// different question at a different time: whether a bootstrap login may
// silently re-grant access (oidc-identity-permissions-design §5.0's
// two-column table).
//
// It is exported and pure on purpose. The disabled invariant has two
// enforcement sites — this resolver, and the API-key door of §5.4 — and the
// design requires them not to drift (round-4 finding F4-2). A shared pure
// function is a stronger guarantee than the injectable seam that finding
// asked for: an injected double proves the sites CALL something, whereas one
// function they both call cannot diverge at all. See §5.0.
//
// No rows is NOT disabled: that is "unknown identity", which the caller
// distinguishes, and collapsing the two would deny where it should ask.
func UserDisabled(rows []persistence.PrincipalRow) bool {
	return len(rows) > 0 && rows[0].Disabled
}

func principalFromRows(rows []persistence.PrincipalRow) (*Principal, error) {
	if len(rows) == 0 {
		return nil, ErrUnknownIdentity
	}
	if UserDisabled(rows) {
		return nil, ErrUserDisabled
	}
	p := &Principal{
		UserID:      rows[0].UserID,
		DisplayName: rows[0].DisplayName,
		Role:        RoleUser,
	}
	projects := map[string]bool{}
	star := false
	for _, r := range rows {
		if r.Role == nil {
			continue // zero-group LEFT JOIN row
		}
		if *r.Role == RoleAdmin {
			p.Role = RoleAdmin
			p.Projects = []string{"*"}
			return p, nil
		}
		if r.ProjectID != nil {
			if *r.ProjectID == "*" {
				star = true
			} else {
				projects[*r.ProjectID] = true
			}
		}
	}
	if star {
		p.Projects = []string{"*"}
		return p, nil
	}
	for proj := range projects {
		p.Projects = append(p.Projects, proj)
	}
	sort.Strings(p.Projects)
	return p, nil
}
