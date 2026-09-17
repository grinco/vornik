package telegram

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chatauth"
)

// Phase 4 on the Telegram door (oidc-identity-permissions-design §5.1/§5.3).
//
// The bot's authorization questions — may this user interact, which projects
// may they reach — now route through the compat shim, so a LINKED identity is
// authorized by the resolver and an unlinked one keeps working off the legacy
// allowlist during the migration window.

// fakeResolver answers from a fixed set of linked identities.
type fakeResolver struct{ linked map[string]*authz.Principal }

func (f fakeResolver) Resolve(_ context.Context, channel, externalID string) (*authz.Principal, error) {
	p, ok := f.linked[channel+":"+externalID]
	if !ok {
		return nil, authz.ErrUnknownIdentity
	}
	return p, nil
}

func newShimmedBot(t *testing.T, allowed map[int64]UserAccess, linked map[string]*authz.Principal) (*Bot, *prometheus.Registry, *strings.Builder) {
	t.Helper()
	reg := prometheus.NewRegistry()
	logs := &strings.Builder{}
	b := &Bot{config: BotConfig{AllowedUsers: allowed}}
	b.SetIdentityShim(chatauth.New(
		fakeResolver{linked: linked},
		chatauth.NewMetrics(reg),
		zerolog.New(logs),
	))
	return b, reg, logs
}

// TestIsAllowed_LinkedIdentityIsAuthorizedWithoutTheLegacyList is the property
// the whole phase exists for: once someone links, the hand-maintained list
// stops being what grants them access.
func TestIsAllowed_LinkedIdentityIsAuthorizedWithoutTheLegacyList(t *testing.T) {
	b, _, _ := newShimmedBot(t,
		map[int64]UserAccess{}, // legacy list denies everyone
		map[string]*authz.Principal{"telegram:42": {UserID: "u1", Role: authz.RoleUser, Projects: []string{"alpha"}}},
	)
	if !b.IsAllowed(42) {
		t.Error("a linked identity must be authorized by the resolver, with no allowlist entry")
	}
	if !b.UserCanAccessProject(42, "alpha") {
		t.Error("the principal's own project scope must apply")
	}
	if b.UserCanAccessProject(42, "beta") {
		t.Error("a project outside the principal's scope must be refused")
	}
}

// TestIsAllowed_UnlinkedIdentityKeepsWorkingDuringTheWindow is the reason the
// shim exists: on this deployment nobody is linked yet, and a cutover that
// locked out the operator would be the defect, not the fix.
func TestIsAllowed_UnlinkedIdentityKeepsWorkingDuringTheWindow(t *testing.T) {
	b, reg, logs := newShimmedBot(t,
		map[int64]UserAccess{7: {Allowed: true, Projects: []string{"*"}}},
		map[string]*authz.Principal{}, // nobody linked
	)
	if !b.IsAllowed(7) {
		t.Fatal("a legacy-listed operator must keep working during the migration window")
	}
	if !strings.Contains(logs.String(), "legacy allowlist") {
		t.Error("the legacy-only grant must appear on the migration worklist")
	}
	if !strings.Contains(logs.String(), "telegram.allowed_users") {
		t.Errorf("the WARN must name the config key to remove: %s", logs.String())
	}
	if countLegacyGrants(t, reg) == 0 {
		t.Error("the removal gate's metric must count this grant")
	}
}

// TestIsAllowed_UnknownStaysDenied — the shim widens nothing.
func TestIsAllowed_UnknownStaysDenied(t *testing.T) {
	b, _, _ := newShimmedBot(t,
		map[int64]UserAccess{7: {Allowed: true}},
		map[string]*authz.Principal{},
	)
	if b.IsAllowed(999) {
		t.Error("an identity neither linked nor listed must stay denied")
	}
}

// TestIsAllowed_NoShimWiredIsTheOldBehaviour — a daemon without Phase 4
// wiring behaves exactly as before, including the empty-list-denies rule.
func TestIsAllowed_NoShimWiredIsTheOldBehaviour(t *testing.T) {
	b := &Bot{config: BotConfig{AllowedUsers: map[int64]UserAccess{7: {Allowed: true}}}}
	if !b.IsAllowed(7) {
		t.Error("listed user denied with no shim wired")
	}
	if b.IsAllowed(8) {
		t.Error("unlisted user allowed with no shim wired")
	}
	empty := &Bot{config: BotConfig{AllowedUsers: map[int64]UserAccess{}}}
	if empty.IsAllowed(7) {
		t.Error("an empty allowlist must still DENY (2026-08-05); the shim must not have re-opened it")
	}
}

func countLegacyGrants(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, mf := range mfs {
		if mf.GetName() != "vornik_auth_legacy_allowlist_grants_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}
