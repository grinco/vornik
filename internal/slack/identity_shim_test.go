package slack

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chatauth"
)

// Phase 4 on the Slack door (oidc-identity-permissions-design §5.1/§5.3).
// Same OR-matrix as Telegram, applied through the same shared shim — two
// channels with two implementations of one authorization rule is how they
// drift, and the rule here is a security boundary.

type fakeResolver struct{ linked map[string]*authz.Principal }

func (f fakeResolver) Resolve(_ context.Context, channel, externalID string) (*authz.Principal, error) {
	p, ok := f.linked[channel+":"+externalID]
	if !ok {
		return nil, authz.ErrUnknownIdentity
	}
	return p, nil
}

func shimmedChannel(t *testing.T, senders map[string]struct{}, linked map[string]*authz.Principal) (*Channel, *prometheus.Registry, *strings.Builder) {
	t.Helper()
	reg := prometheus.NewRegistry()
	logs := &strings.Builder{}
	c := &Channel{
		logger:        zerolog.Nop(),
		installations: []*installation{{senders: senders}},
	}
	c.SetIdentityShim(chatauth.New(fakeResolver{linked: linked}, chatauth.NewMetrics(reg), zerolog.New(logs)))
	return c, reg, logs
}

// TestSlack_LinkedSpeakerIsAuthorizedWithoutTheAllowlist — the migration's
// destination: the hand-maintained sender list stops being what grants access.
func TestSlack_LinkedSpeakerIsAuthorizedWithoutTheAllowlist(t *testing.T) {
	c, _, _ := shimmedChannel(t,
		map[string]struct{}{}, // empty allowlist: denies on its own
		map[string]*authz.Principal{"slack:U1": {UserID: "u1", Role: authz.RoleUser, Projects: []string{"alpha"}}},
	)
	if _, err := c.ResolveSpeaker(context.Background(), "U1"); err != nil {
		t.Errorf("a linked speaker must resolve without an allowlist entry: %v", err)
	}
}

// TestSlack_UnlinkedSpeakerKeepsWorkingDuringTheWindow — the reason the shim
// exists, on the channel where an empty list already denies.
func TestSlack_UnlinkedSpeakerKeepsWorkingDuringTheWindow(t *testing.T) {
	c, reg, logs := shimmedChannel(t,
		map[string]struct{}{"U7": {}},
		map[string]*authz.Principal{},
	)
	if _, err := c.ResolveSpeaker(context.Background(), "U7"); err != nil {
		t.Fatalf("a listed sender must keep working during the migration window: %v", err)
	}
	if !strings.Contains(logs.String(), "slack.sender_allowlist") {
		t.Errorf("the WARN must name the config key to remove: %s", logs.String())
	}
	if countLegacy(t, reg) == 0 {
		t.Error("the removal gate's metric must count this legacy-only grant")
	}
}

// TestSlack_UnknownStaysDenied — the shim widens nothing.
func TestSlack_UnknownStaysDenied(t *testing.T) {
	c, _, _ := shimmedChannel(t, map[string]struct{}{"U7": {}}, map[string]*authz.Principal{})
	if _, err := c.ResolveSpeaker(context.Background(), "U999"); err == nil {
		t.Error("a speaker neither linked nor listed must stay denied")
	}
}

// TestSlack_EmptyAllowlistStillDeniesWithoutTheShim pins the 2026-08-05
// default flip against re-opening: that fix closed a hole where an
// unconfigured installation let a whole workspace drive the dispatcher, and
// adding the shim must not undo it.
func TestSlack_EmptyAllowlistStillDeniesWithoutTheShim(t *testing.T) {
	c := &Channel{logger: zerolog.Nop(), installations: []*installation{{senders: map[string]struct{}{}}}}
	if _, err := c.ResolveSpeaker(context.Background(), "U1"); err == nil {
		t.Error("an empty allowlist must still deny with no shim wired")
	}
}

func countLegacy(t *testing.T, reg *prometheus.Registry) float64 {
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

// TestSlack_BothGatesAgreeAcrossTheWholeMatrix — Slack asks "may this speaker
// act" at two places: channel-wide (ResolveSpeaker → authorizeSpeaker) and
// per-installation, after routing (resolveSpeakerForInstallation). A speaker
// allowed by one and refused by the other is authorized and then dropped at
// dispatch, with no message anywhere saying why.
//
// The two used to re-derive the legacy rule separately — the same answer by
// coincidence, since both omitted the same field (review-20260914-3c36 F2).
// This walks the full cross-product rather than a sample, because a drift
// that only shows up in one cell is exactly the kind that ships.
func TestSlack_BothGatesAgreeAcrossTheWholeMatrix(t *testing.T) {
	linkedPrincipal := map[string]*authz.Principal{
		"slack:U1": {UserID: "u1", Role: authz.RoleUser, Projects: []string{"alpha"}},
	}
	for _, allowlist := range []struct {
		name    string
		senders map[string]struct{}
	}{
		{"empty_list", map[string]struct{}{}},
		{"lists_the_speaker", map[string]struct{}{"U1": {}}},
		{"lists_someone_else", map[string]struct{}{"U9": {}}},
	} {
		for _, link := range []struct {
			name   string
			linked map[string]*authz.Principal
		}{
			{"linked", linkedPrincipal},
			{"unlinked", map[string]*authz.Principal{}},
		} {
			for _, unlisted := range []bool{false, true} {
				for _, shim := range []bool{false, true} {
					name := allowlist.name + "/" + link.name
					if unlisted {
						name += "/allow_unlisted"
					}
					if shim {
						name += "/shim"
					}
					t.Run(name, func(t *testing.T) {
						inst := &installation{senders: allowlist.senders, allowUnlisted: unlisted}
						c := &Channel{logger: zerolog.Nop(), installations: []*installation{inst}}
						if shim {
							c.SetIdentityShim(chatauth.New(
								fakeResolver{linked: link.linked},
								chatauth.NewMetrics(prometheus.NewRegistry()),
								zerolog.New(&strings.Builder{}),
							))
						}

						channelWide := c.authorizeSpeaker("U1").Allowed
						_, err := c.resolveSpeakerForInstallation(inst, "U1")
						perInstallation := err == nil

						if channelWide != perInstallation {
							t.Errorf("the two gates disagree: channel-wide=%v, per-installation=%v — "+
								"a speaker authorized by one and refused by the other is dropped at dispatch with no explanation",
								channelWide, perInstallation)
						}
					})
				}
			}
		}
	}
}
