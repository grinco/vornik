package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Link-code contract suite, shared by both backends. The `link_codes` table
// has shipped on Postgres and SQLite since the identity core, with no
// producer and no consumer — Phase 4 (oidc-identity-permissions-design §5.2)
// is what finally reads and writes it.
//
// The rules this pins come from §5.2 and are security properties, not
// storage conveniences:
//   - only the SHA-256 of a code is ever stored, so the store cannot leak a
//     redeemable code even when read;
//   - consumption is single-use and atomic, because two racing redemptions
//     of one code would bind a channel identity twice;
//   - an expired code is indistinguishable from an absent one at this layer,
//     so no caller can build the oracle §5.2 forbids.

// RunLinkCodeSuite exercises the LinkCodeRepository contract. It takes the
// identity repository too because a link code references a user, and a code
// for a user that does not exist is not a case worth defining.
func RunLinkCodeSuite(t *testing.T, repo persistence.LinkCodeRepository, identity persistence.IdentityRepository) {
	t.Helper()
	ctx := context.Background()

	newUser := func(t *testing.T) *persistence.User {
		t.Helper()
		u := &persistence.User{ID: uniqueID("user"), DisplayName: "link suite", CreatedAt: clock()}
		if err := identity.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		return u
	}

	// issue stores a code for u expiring at exp, returning the hash the
	// caller would later redeem with.
	issue := func(t *testing.T, u *persistence.User, exp time.Time) string {
		t.Helper()
		hash := uniqueID("hash")
		lc := &persistence.LinkCode{
			CodeHash:  hash,
			UserID:    u.ID,
			CreatedAt: clock(),
			ExpiresAt: exp,
		}
		if err := repo.CreateLinkCode(ctx, lc); err != nil {
			t.Fatalf("CreateLinkCode: %v", err)
		}
		return hash
	}

	t.Run("round trip", func(t *testing.T) {
		u := newUser(t)
		exp := clock().Add(10 * time.Minute)
		hash := issue(t, u, exp)

		got, err := repo.GetLinkCode(ctx, hash)
		if err != nil {
			t.Fatalf("GetLinkCode: %v", err)
		}
		if got.UserID != u.ID {
			t.Errorf("UserID = %q, want %q", got.UserID, u.ID)
		}
		if !got.ExpiresAt.Equal(exp) {
			t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, exp)
		}
		if got.UsedAt != nil {
			t.Errorf("a fresh code must be unused, got UsedAt=%v", got.UsedAt)
		}
	})

	t.Run("absent code is ErrNotFound", func(t *testing.T) {
		// Driven through the miss-contract so the declared behaviour and the
		// asserted one cannot drift: a row in misscontract.go that no suite
		// exercises is a claim, not a check.
		AssertMissRepo(t, "LinkCodeRepository.GetLinkCode", repo.GetLinkCode)
	})

	t.Run("consuming an absent code is ErrNotFound, not a distinct error", func(t *testing.T) {
		AssertMiss(t, "LinkCodeRepository.ConsumeLinkCode", func() (*persistence.LinkCode, error) {
			return repo.ConsumeLinkCode(ctx, uniqueID("nope"), "telegram", "1")
		})
	})

	t.Run("consume binds once and only once", func(t *testing.T) {
		assertSingleUse(t, repo, newUser(t), issue)
	})

	t.Run("expired code cannot be consumed", func(t *testing.T) {
		u := newUser(t)
		hash := issue(t, u, clock().Add(-time.Second))

		// Indistinguishable from absent, deliberately: §5.2 requires the
		// redeeming caller learn nothing about whether a code ever existed.
		if _, err := repo.ConsumeLinkCode(ctx, hash, "telegram", "43"); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("ConsumeLinkCode(expired) = %v, want ErrNotFound", err)
		}
	})

	t.Run("outstanding lists only live codes for that user", func(t *testing.T) {
		assertOutstandingIsLiveOnly(t, repo, newUser(t), newUser(t), issue)
	})
}

// issueFn stores a code for u expiring at exp and returns its hash.
type issueFn func(t *testing.T, u *persistence.User, exp time.Time) string

// assertSingleUse pins the security property of §5.2: a code redeems exactly
// once. A second successful redemption would bind two channel identities from
// one code, which is an identity-takeover primitive rather than a duplicate
// write — so the second attempt must be refused, and refused as ErrNotFound
// so it cannot be told apart from a code that never existed.
func assertSingleUse(t *testing.T, repo persistence.LinkCodeRepository, u *persistence.User, issue issueFn) {
	t.Helper()
	ctx := context.Background()
	hash := issue(t, u, clock().Add(10*time.Minute))

	got, err := repo.ConsumeLinkCode(ctx, hash, "telegram", "42")
	if err != nil {
		t.Fatalf("ConsumeLinkCode: %v", err)
	}
	if got.UserID != u.ID {
		t.Errorf("consumed code UserID = %q, want %q", got.UserID, u.ID)
	}
	if _, err := repo.ConsumeLinkCode(ctx, hash, "slack", "U999"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("second ConsumeLinkCode = %v, want ErrNotFound", err)
	}

	after, err := repo.GetLinkCode(ctx, hash)
	if err != nil {
		t.Fatalf("GetLinkCode after consume: %v", err)
	}
	if after.UsedAt == nil {
		t.Error("UsedAt must be stamped on consume")
	}
	if after.UsedByChannel != "telegram" || after.UsedByExternalID != "42" {
		t.Errorf("consumer recorded as (%q,%q), want (telegram,42)", after.UsedByChannel, after.UsedByExternalID)
	}
}

// assertOutstandingIsLiveOnly pins what the §5.5 panel polls on: a code that
// has expired or been consumed must not keep the panel waiting, and one
// user's codes must never appear in another's list.
func assertOutstandingIsLiveOnly(t *testing.T, repo persistence.LinkCodeRepository, u, other *persistence.User, issue issueFn) {
	t.Helper()
	ctx := context.Background()
	live := issue(t, u, clock().Add(10*time.Minute))
	expired := issue(t, u, clock().Add(-time.Second))
	consumed := issue(t, u, clock().Add(10*time.Minute))
	if _, err := repo.ConsumeLinkCode(ctx, consumed, "telegram", "44"); err != nil {
		t.Fatalf("ConsumeLinkCode: %v", err)
	}
	issue(t, other, clock().Add(10*time.Minute))

	got, err := repo.OutstandingLinkCodes(ctx, u.ID)
	if err != nil {
		t.Fatalf("OutstandingLinkCodes: %v", err)
	}
	if len(got) != 1 || got[0].CodeHash != live {
		hashes := make([]string, 0, len(got))
		for _, c := range got {
			hashes = append(hashes, c.CodeHash)
		}
		t.Fatalf("outstanding = %v, want exactly [%s] (expired=%s consumed=%s)", hashes, live, expired, consumed)
	}
}
