package authz

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Self-service channel-link codes — oidc-identity-permissions-design §5.2.
//
// A signed-in user generates a code in the UI (or over the API/CLI); they
// then send `/link <code>` from the chat channel they want bound, and the
// dispatcher redeems it. This file is both halves of that flow EXCEPT the
// dispatcher call site: issuing, and the redemption the dispatcher will call.

const (
	// linkCodeLength is the code's character count. Eight characters of the
	// alphabet below is ~41 bits — far beyond guessable inside a 10-minute
	// window against a store that refuses a used code.
	linkCodeLength = 8

	// linkCodeAlphabet deliberately omits the characters people confuse when
	// reading a code off one screen and typing it into another: 0/O, 1/I/L,
	// 5/S, 8/B. A code that is mistyped is a support question; a code that is
	// mistyped into someone ELSE's valid code is an incident, and shrinking
	// the alphabet costs entropy that the TTL already makes irrelevant.
	linkCodeAlphabet = "ACDEFGHJKMNPQRTUVWXYZ2346789"

	// linkCodeTTL is the design's 10 minutes: long enough to alt-tab and
	// paste, short enough that a code seen over someone's shoulder is moot.
	linkCodeTTL = LinkCodeTTL
)

// ErrLinkCodeInvalid is the ONLY error a redemption failure produces.
//
// Unknown, expired and already-used codes all answer this, deliberately
// (§5.2): a redeemer who could tell them apart could probe whether a code
// ever existed. The storage layer collapses the three the same way, so the
// distinction is unavailable rather than merely unused.
var ErrLinkCodeInvalid = errors.New("authz: link code not valid")

// ErrLinkCodesUnavailable is returned when no link-code repository is wired.
var ErrLinkCodesUnavailable = errors.New("authz: link codes not available")

// ErrIdentityAlreadyLinked is the FOURTH redemption outcome, and deliberately
// NOT collapsed into ErrLinkCodeInvalid.
//
// The other three are statements about the CODE, and telling them apart would
// let a redeemer probe whether a code ever existed. This one is a statement
// about the SPEAKER'S OWN binding — something they can already read on their
// account page — so hiding it leaks nothing and costs a great deal: the
// redeemer is told their perfectly good code is bad, and the remedy they
// actually need (unlink this chat from the other account first) is never
// mentioned.
//
// It is also why this check runs BEFORE the code is consumed. Refusing after
// consumption would spend a valid one-time code on a failure that is not the
// code's fault (review-20260914-a786, F11(b)).
var ErrIdentityAlreadyLinked = errors.New("authz: this chat identity is already linked to another account")

// WithLinkCodes attaches the link-code store. Kept separate from NewAccounts
// so an operator shell that never links channels constructs unchanged.
func (a *Accounts) WithLinkCodes(codes persistence.LinkCodeRepository) *Accounts {
	a.linkCodes = codes
	return a
}

// IssueLinkCode mints a code for userID and returns the RAW code, which is
// the only time it exists outside the caller: the store receives its sha256
// and nothing else.
//
// It audits before returning, like every other mutation on this service — a
// link code is a pending access grant, and one nobody can see is the
// compliance gap `ready` exists to prevent.
func (a *Accounts) IssueLinkCode(ctx context.Context, userID string, actor Actor) (string, error) {
	if err := a.ready(); err != nil {
		return "", err
	}
	if a.linkCodes == nil {
		return "", ErrLinkCodesUnavailable
	}
	if _, err := a.Get(ctx, userID); err != nil {
		return "", fmt.Errorf("authz: issue link code: %w", err)
	}
	code, err := newLinkCode()
	if err != nil {
		return "", err
	}
	now := a.now()
	lc := &persistence.LinkCode{
		CodeHash:  hashLinkCode(code),
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(linkCodeTTL),
	}
	// The audit is written BEFORE the code exists, not after. The two stores
	// are separate repositories with no shared transaction, so one of the
	// two orderings has to be wrong on a partial failure — and they are not
	// equally wrong. Auditing first can leave a row for a code that was
	// never created: a trail that over-reports, and grants nothing. Storing
	// first could leave a redeemable pending access grant with no trail at
	// all, which is the gap `ready` exists to prevent (review-20260914-3c36
	// F6). Never the code itself, which would make the audit log a
	// redemption oracle.
	if err := a.record(ctx, actor, "account.link_code.issue", userID, map[string]any{
		"expires_at": lc.ExpiresAt,
		// The row is written BEFORE the code exists, so on its own it is a
		// promise, not a record. An auditor reading `issue` rows with no
		// matching link_codes row could not tell "creation failed" from
		// "created, then consumed or expired and swept"
		// (review-20260914-e581 N3 / review-20260914-a786). The confirmation
		// below is what makes the two distinguishable: an issue row with no
		// confirmation is an attempt, and a reconciliation that counts issues
		// against redemptions counts the confirmed ones.
		"stored": false,
	}); err != nil {
		return "", err
	}
	if err := a.linkCodes.CreateLinkCode(ctx, lc); err != nil {
		return "", fmt.Errorf("authz: issue link code: %w", err)
	}
	// The code now exists. The confirmation is what lets an auditor tell
	// "creation failed" from "created, then consumed or expired and swept".
	//
	// It does NOT close the gap in both directions, and the first version of
	// this comment claimed it did (review-20260915-4eba F3). If THIS write
	// fails, the code exists and is redeemable while the trail shows only an
	// attempt — an under-report, which is the wrong direction. So the honest
	// reading of the rows is:
	//
	//   issue + issued  → a code was created.
	//   issue alone     → INDETERMINATE. The store failed, or the store
	//                     succeeded and this row did not. NOT "no code".
	//
	// A reconciliation counting `issued` against redemptions therefore
	// counts a lower bound on grants, not an exact one. Making it exact
	// needs the two stores in one transaction, which they are not.
	if err := a.record(ctx, actor, "account.link_code.issued", userID, map[string]any{
		"expires_at": lc.ExpiresAt,
		"stored":     true,
	}); err != nil {
		return "", err
	}
	return code, nil
}

// RedeemLinkCode consumes a code and binds (channel, externalID) to the
// user it was issued for, returning that user.
//
// This is the function the chat dispatchers call — Telegram's `/link <code>`
// and Slack's `/vornik link <code>`, both wired. Every failure answers
// ErrLinkCodeInvalid; a real fault does not, so the channel can tell a bad
// code from an outage.
func (a *Accounts) RedeemLinkCode(ctx context.Context, code, channel, externalID, display string) (*persistence.User, error) {
	// ready() gates on the audit sink, and redemption now writes a row (F7).
	// Without this, a deployment with no sink would reach a.record with a nil
	// interface and panic on the chat path.
	if err := a.ready(); err != nil {
		return nil, err
	}
	if a.linkCodes == nil {
		return nil, ErrLinkCodesUnavailable
	}
	if channel == "" || externalID == "" {
		return nil, ErrLinkCodeInvalid
	}
	// Is this speaker already somebody's? Asked BEFORE the code is consumed,
	// because a refusal here is about the speaker and not the code, and
	// spending a valid single-use code on it would be a punishment for a
	// mistake the code did not make.
	//
	// A speaker already linked to the SAME account is refused too. That
	// redemption would be a no-op, so "you are already linked" is both true
	// and the more useful answer than a silent success.
	if rows, rerr := a.repo.ResolvePrincipalRows(ctx, channel, externalID); rerr != nil {
		return nil, fmt.Errorf("authz: redeem link code: %w", rerr)
	} else if len(rows) > 0 {
		return nil, ErrIdentityAlreadyLinked
	}

	lc, err := a.linkCodes.ConsumeLinkCode(ctx, hashLinkCode(code), channel, externalID)
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		// Absent, spent and expired arrive here as one ErrNotFound and leave
		// as one ErrLinkCodeInvalid. THAT is the indistinguishability §5.2
		// asks for — between the three ways a code can be no good.
		return nil, ErrLinkCodeInvalid
	case err != nil:
		// A dropped connection is not one of those three. Collapsing it into
		// "invalid" told the redeemer their code was bad and sent them to ask
		// for another, while hiding an outage from everyone (F5). The oracle
		// this reopens — "the database is unwell" — is not a secret.
		return nil, fmt.Errorf("authz: redeem link code: %w", err)
	}
	view, err := a.Get(ctx, lc.UserID)
	if err != nil {
		return nil, fmt.Errorf("authz: redeem link code: %w", err)
	}
	bound, err := a.repo.BindIdentity(ctx, &persistence.UserIdentity{
		ID:         persistence.GenerateID("uident"),
		UserID:     lc.UserID,
		Channel:    channel,
		ExternalID: externalID,
		Display:    display,
		CreatedAt:  a.now(),
	})
	if err != nil {
		return nil, fmt.Errorf("authz: redeem link code: %w", err)
	}
	if !bound {
		// The pre-check above already refused the ordinary version of this.
		// Reaching here means someone bound this speaker in the window
		// between that read and this write — rare, and the code IS spent
		// when it happens, which is the one case where that is unavoidable
		// without a transaction spanning two repositories.
		//
		// Audited as its own outcome. The principle the pre-check exists for
		// — a valid code is not spent on a failure that is not the code's
		// fault — is VIOLATED here, and a spent-on-race code looks exactly
		// like a spent-and-bound one unless the row says otherwise. An
		// operator seeing this knows to re-issue rather than telling the
		// person to retype a code that no longer exists
		// (review-20260915-4eba F2).
		if rerr := a.record(ctx,
			Actor{Principal: channel + ":" + externalID, Source: "chat"},
			"account.link_code.race_conflict", lc.UserID,
			map[string]any{"channel": channel, "external_id": externalID, "code_spent": true},
		); rerr != nil {
			return nil, rerr
		}
		//
		// Before the bind reported this at all, redemption returned the
		// code's user while the speaker stayed attached to the old account —
		// a "you are linked" that was not true (review-20260914-3c36 F11).
		return nil, ErrIdentityAlreadyLinked
	}
	// §5.2: a redemption writes the user_identities row AND the profile row.
	// The binding above is the access half; this is the data half — the
	// tone, verbosity and timezone this speaker has accumulated, folded onto
	// the account so it follows the person rather than the channel.
	//
	// NOT fatal, and the audit says which happened. The access binding is
	// already committed and is the load-bearing half; failing the whole
	// redemption here would spend the code and refuse the link over a
	// profile merge. Recording the outcome in the audit row is what keeps
	// that from being a silent partial success.
	// "no_profile_store" rather than "skipped": the value names the CAUSE,
	// not the outcome. A reader seeing "skipped" on a deployment that does
	// wire a profile store cannot tell a correct skip from a wiring
	// regression — and those are the same nil here, so only the name can
	// carry the distinction (review-20260915-4eba F4).
	profileRepoint := "no_profile_store"
	if a.profiles != nil {
		if err := a.profiles.LinkSpeakerToAccount(ctx, channel, externalID, lc.UserID); err != nil {
			profileRepoint = "failed"
		} else {
			profileRepoint = "merged"
		}
	}

	// §5.5: every mutation is audited, and the §5.7 ledger names redemption
	// among the five. It was the one that did not write a row (F7).
	//
	// There is no acting USER principal — redemption is initiated by a chat
	// speaker who is, by definition, not yet linked to anyone. The speaker is
	// named instead, which is the honest thing the row can say about who
	// acted, and the target is the account they joined.
	if err := a.record(ctx,
		Actor{Principal: channel + ":" + externalID, Source: "chat"},
		"account.link_code.redeem", lc.UserID,
		map[string]any{"channel": channel, "external_id": externalID, "profile_repoint": profileRepoint},
	); err != nil {
		return nil, err
	}
	return &persistence.User{ID: view.UserID, DisplayName: view.DisplayName, CreatedAt: view.CreatedAt}, nil
}

// OutstandingLinkCodes lists the user's live codes. The §5.5 panel polls this
// while a code it issued is outstanding.
func (a *Accounts) OutstandingLinkCodes(ctx context.Context, userID string) ([]*persistence.LinkCode, error) {
	if a.linkCodes == nil {
		return nil, ErrLinkCodesUnavailable
	}
	return a.linkCodes.OutstandingLinkCodes(ctx, userID)
}

// newLinkCode draws linkCodeLength characters from the unambiguous alphabet
// using crypto/rand. Rejection is unnecessary because the alphabet length
// divides evenly into the int63 space via big.Int's uniform Int.
func newLinkCode() (string, error) {
	var sb strings.Builder
	sb.Grow(linkCodeLength)
	limit := big.NewInt(int64(len(linkCodeAlphabet)))
	for i := 0; i < linkCodeLength; i++ {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("authz: link code entropy: %w", err)
		}
		sb.WriteByte(linkCodeAlphabet[n.Int64()])
	}
	return sb.String(), nil
}

// hashLinkCode normalises then hashes. Normalisation is why a human can type
// back what they read: §5.2 renders codes uppercase and may dash them for
// legibility, so the redeemer tolerates lowercase, surrounding space and any
// dashes the operator inserted or dropped.
//
// It also tolerates PASTED FORMATTING, which is not the same problem as a
// mistype and was found the hard way on 2026-09-15: the "My account" panel
// rendered the instruction inside a <code> element, Slack's composer kept that
// formatting on paste, and Slack serialised it back as literal backticks — so
// the daemon received the code wrapped in literal backticks and refused a
// perfectly good code. The
// operator got it in only after stripping the formatting by hand.
//
// The rule is "keep [A-Z0-9], drop everything else". That is safe rather than
// merely convenient, and the safety is a property of the generator:
// linkCodeAlphabet is pure uppercase alphanumeric, so the filter alters no
// legitimate code and cannot normalise two distinct codes onto one hash.
// TestHashLinkCode_NormalisationCannotCollideTwoCodes pins that dependency, so
// widening the alphabet fails loudly instead of creating a collision.
func hashLinkCode(code string) string {
	var b strings.Builder
	b.Grow(len(code))
	for _, r := range strings.ToUpper(code) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// ProfileLinker folds a chat speaker's operator profile onto an account's
// canonical id — §5.2's "a redemption writes the user_identities row AND the
// profile row".
//
// Declared HERE, by the consumer, and implemented outside: the profile store
// lives behind the dispatcher's merge logic, and an account service that
// imported the dispatcher to reach it would invert the dependency this
// package exists to keep clean.
type ProfileLinker interface {
	LinkSpeakerToAccount(ctx context.Context, channel, externalID, userID string) error
}

// WithProfileLinker wires the profile half of redemption. Nil — the default —
// leaves redemption writing the access binding alone, which is the correct
// behaviour on a deployment with no operator-profile store: there is no
// profile to move.
func (a *Accounts) WithProfileLinker(l ProfileLinker) *Accounts {
	a.profiles = l
	return a
}

// AccountOperatorID is the canonical operator id derived from an account.
//
// A namespace of its own, rather than reusing whichever speaker id happened
// to link first. Reusing a speaker id would make the canonical row for a
// person depend on which channel they linked from, and would leave it
// pointing at a channel identity they may later unlink.
func AccountOperatorID(userID string) string { return "account:" + userID }

// LinkCodeTTL is how long a code stays redeemable, exported because the HTTP
// and UI layers both have to tell a human how long they have.
//
// It was mirrored as a second constant in internal/api, which is one copy of
// a number that can drift from the one the service enforces — and the drift
// would show as a page promising ten minutes for a code the service expired
// in five. One source, read by everyone who quotes it.
const LinkCodeTTL = 10 * time.Minute

// LinkCodeID is the panel's handle on a code it issued: the first 16 hex
// characters of the code's sha256.
//
// §5.5's observation contract needs the panel to recognise ITS code among the
// caller's outstanding ones, and the design's own caveat rules out the
// obvious alternative: "a new identity appeared" must not be read as "my code
// redeemed", because a second outstanding code or a concurrent admin
// key-assignment both add an identity the poller did not cause.
//
// A hash prefix rather than the code: it is not reversible, it is already
// what the store holds, and it can travel in a polling URL — which the raw
// code never may.
func LinkCodeID(code string) string { return hashLinkCode(code)[:16] }

// LinkCodeIDOf is the same handle derived from a stored row.
func LinkCodeIDOf(lc *persistence.LinkCode) string {
	if lc == nil || len(lc.CodeHash) < 16 {
		return ""
	}
	return lc.CodeHash[:16]
}
