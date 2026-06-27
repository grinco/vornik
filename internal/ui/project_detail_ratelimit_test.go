package ui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// stubAPIKeyRepo is the minimal APIKeyRepository fake the panel test
// needs — ListByProject only; other methods are no-ops that panic if
// hit (so a future change can't silently exercise an unmocked path).
type stubAPIKeyRepo struct {
	keys []*persistence.APIKey
	err  error
}

func (s *stubAPIKeyRepo) Create(context.Context, *persistence.APIKey) error {
	panic("unexpected Create call")
}
func (s *stubAPIKeyRepo) LookupActiveByHash(context.Context, string) (*persistence.APIKey, error) {
	panic("unexpected LookupActiveByHash call")
}
func (s *stubAPIKeyRepo) ListByProject(_ context.Context, _ string) ([]*persistence.APIKey, error) {
	return s.keys, s.err
}
func (s *stubAPIKeyRepo) ListCompanionByProject(context.Context, string) ([]*persistence.APIKey, error) {
	panic("unexpected ListCompanionByProject call")
}
func (s *stubAPIKeyRepo) TouchLastUsed(context.Context, string) error { return nil }
func (s *stubAPIKeyRepo) Revoke(context.Context, string) error        { return nil }
func (s *stubAPIKeyRepo) UpdateAllowedWorkflows(context.Context, string, []string) error {
	return nil
}
func (s *stubAPIKeyRepo) UpdateAllowPush(context.Context, string, bool) error { return nil }
func (s *stubAPIKeyRepo) RevokeByName(context.Context, string) error          { return nil }

func intPtr(v int) *int           { return &v }
func tPtr(t time.Time) *time.Time { return &t }

// TestBuildRateLimitPanel_HappyPath — active keys with throttle
// configured map to RateLimitKeyRow with the limits visible; keys
// without limits show zeros (template renders "—" / "unlimited").
func TestBuildRateLimitPanel_HappyPath(t *testing.T) {
	repo := &stubAPIKeyRepo{
		keys: []*persistence.APIKey{
			{
				ID: "k1", Name: "HA loop", KeyPrefix: "sk-vornik-foo.ab12",
				RateLimitRPS: intPtr(5), RateLimitBurst: intPtr(20),
			},
			{
				ID: "k2", Name: "legacy CLI", KeyPrefix: "sk-vornik-foo.cd34",
				// RateLimitRPS + Burst both nil — legacy unlimited key.
			},
		},
	}

	rows := buildRateLimitPanel(context.Background(), repo, "foo", zerolog.Nop())
	require.Len(t, rows, 2)

	assert.Equal(t, "HA loop", rows[0].Name)
	assert.Equal(t, "k1", rows[0].KeyID)
	assert.Equal(t, "sk-vornik-foo.ab12", rows[0].KeyPrefix)
	assert.Equal(t, 5, rows[0].RateLimitRPS)
	assert.Equal(t, 20, rows[0].RateLimitBurst)

	assert.Equal(t, "legacy CLI", rows[1].Name)
	assert.Equal(t, 0, rows[1].RateLimitRPS, "nil rps → 0 so template renders an em-dash")
	assert.Equal(t, 0, rows[1].RateLimitBurst)
}

// TestBuildRateLimitPanel_HidesRevokedAndExpired — the panel is for
// LIVE traffic shaping; revoked or expired keys would mislead the
// operator into thinking the configured limit still applies.
func TestBuildRateLimitPanel_HidesRevokedAndExpired(t *testing.T) {
	now := time.Now()
	repo := &stubAPIKeyRepo{
		keys: []*persistence.APIKey{
			{ID: "active", Name: "active", KeyPrefix: "sk-x", RateLimitRPS: intPtr(2), RateLimitBurst: intPtr(5)},
			{ID: "revoked", Name: "revoked", KeyPrefix: "sk-y", RevokedAt: tPtr(now.Add(-time.Hour))},
			{ID: "expired", Name: "expired", KeyPrefix: "sk-z", ExpiresAt: tPtr(now.Add(-time.Hour))},
			{ID: "future-exp", Name: "future-exp", KeyPrefix: "sk-w", ExpiresAt: tPtr(now.Add(time.Hour))},
		},
	}

	rows := buildRateLimitPanel(context.Background(), repo, "p", zerolog.Nop())
	require.Len(t, rows, 2, "only `active` and `future-exp` should remain")
	assert.Equal(t, "active", rows[0].Name)
	assert.Equal(t, "future-exp", rows[1].Name)
}

// TestBuildRateLimitPanel_RepoErrorDegrades — repo errors must not
// fail the page render; operators get an empty panel and a warn log.
func TestBuildRateLimitPanel_RepoErrorDegrades(t *testing.T) {
	repo := &stubAPIKeyRepo{err: errors.New("db down")}
	rows := buildRateLimitPanel(context.Background(), repo, "p", zerolog.Nop())
	assert.Nil(t, rows, "repo errors degrade to nil slice — template hides the panel")
}

// TestBuildRateLimitPanel_NilSafe — sanity checks for the two
// zero-value early returns.
func TestBuildRateLimitPanel_NilSafe(t *testing.T) {
	assert.Nil(t, buildRateLimitPanel(context.Background(), nil, "p", zerolog.Nop()),
		"nil repo returns nil")

	repo := &stubAPIKeyRepo{}
	assert.Empty(t, buildRateLimitPanel(context.Background(), repo, "", zerolog.Nop()),
		"empty project id returns empty (still safe — no panic)")
}
