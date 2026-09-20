package authz

// The account side of the CE credential→session exchange —
// 2026-09-19-ce-human-login-design.md §4, §6.1.

import (
	"context"
	"errors"
)

// ErrExchangeRefused is the ONE refusal the exchange ever returns to a caller.
//
// The exchange is a key-verification oracle by construction — a success says
// "this bearer token is a live key mapped to an account" and a failure says it
// is not. That much is inherent to the door. What is avoidable is a SECOND
// signal: a refusal that distinguished "unknown key" from "unmapped key" from
// "disabled account" would tell a prober which of the three it got wrong, and
// the differences are facts about someone else's account. So they share a
// sentinel, and the operator-facing detail goes to the audit trail.
var ErrExchangeRefused = errors.New("authz: session exchange refused")

// ErrExchangeRateLimited is distinct so a caller can answer 429 rather than a
// flat refusal — and, like the claim path's equivalent, it is raised
// identically for every cause so the bound reveals nothing the refusals do
// not.
var ErrExchangeRateLimited = errors.New("authz: too many session exchange attempts for this credential")

// ExchangeSubject is who a credential may open a session as.
type ExchangeSubject struct {
	UserID string
	Role   string
}

// ResolveExchangeSubject answers whether keyID may mint a browser session, and
// for whom.
//
// THE BOUND IS SHARED WITH THE CLAIM PATH, per §6. Both doors prove possession
// of a key, so they share one attempt bucket keyed by the key id — a per-door
// bucket would let an attacker spend one bound and then the other. The cost is
// stated in the design rather than discovered: an attacker spraying a victim's
// key id on the claim path also closes that key's session door, and the
// recourse is the break-glass CLI.
//
// It does NOT verify the key's secret. The caller has already been
// authenticated by the auth chain, and re-verifying here would be the second
// implementation of one security predicate that this repository has paid for
// three times. What this answers is the different question the chain cannot:
// which ACCOUNT does this credential act as, and may it.
func (a *Accounts) ResolveExchangeSubject(ctx context.Context, keyID string) (*ExchangeSubject, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	if keyID == "" {
		return nil, ErrExchangeRefused
	}
	if !a.allowClaimAttempt(ctx, keyID) {
		return nil, ErrExchangeRateLimited
	}

	// ONE read of the mapping, used for both the check and the subject.
	//
	// It was two until review-20260919-76da F3: the cap resolved the owner,
	// then the subject resolved it again, and a key reassigned between the
	// two would pass the cap for user A and mint a session for user B. The
	// per-request cap would have killed that session on its very next
	// request, so it was never a durable grant — but a session minted for
	// the wrong account is not a thing to leave reachable when closing it
	// costs one variable, and the design says "asked ONCE here at mint
	// time".
	owner, err := a.keyOwner(ctx, keyID)
	if err != nil || owner == "" {
		return nil, ErrExchangeRefused
	}

	// The same liveness-and-mapping question the per-request capping rule
	// asks. Deliberately the SAME function: a session must not be mintable
	// under conditions that would refuse it on its very next request.
	if err := a.CheckSessionCredential(ctx, keyID, owner); err != nil {
		return nil, ErrExchangeRefused
	}
	account, err := a.Get(ctx, owner)
	if err != nil || account == nil || account.Disabled {
		// A disabled account may not open a door, and the caller learns
		// nothing about the account from the refusal that they do not
		// already know.
		return nil, ErrExchangeRefused
	}

	// Role comes from the account view already read above, not from a
	// second resolve: it feeds only the JS-readable nav marker, which is
	// decoration and never authority. The server re-resolves the real
	// principal on every request through the session backend.
	return &ExchangeSubject{UserID: owner, Role: account.Role}, nil
}
