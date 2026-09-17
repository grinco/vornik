package authz

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// memLinkCodes is an in-memory LinkCodeRepository. It deliberately keeps the
// single-use and expiry rules of the real repositories, because the service
// tests below assert behaviour that depends on them.
type memLinkCodes struct {
	codes map[string]*persistence.LinkCode
	now   func() time.Time
	// consumeErr injects a transport-class fault, which must NOT be
	// reported to the redeemer as an invalid code (F5).
	consumeErr error
	// createErr fails the store so the audit ordering can be observed.
	createErr error
}

func newMemLinkCodes(now func() time.Time) *memLinkCodes {
	return &memLinkCodes{codes: map[string]*persistence.LinkCode{}, now: now}
}

func (m *memLinkCodes) CreateLinkCode(_ context.Context, lc *persistence.LinkCode) error {
	if m.createErr != nil {
		return m.createErr
	}
	cp := *lc
	m.codes[lc.CodeHash] = &cp
	return nil
}

func (m *memLinkCodes) GetLinkCode(_ context.Context, hash string) (*persistence.LinkCode, error) {
	lc, ok := m.codes[hash]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *lc
	return &cp, nil
}

func (m *memLinkCodes) ConsumeLinkCode(_ context.Context, hash, channel, externalID string) (*persistence.LinkCode, error) {
	if m.consumeErr != nil {
		return nil, m.consumeErr
	}
	lc, ok := m.codes[hash]
	if !ok || lc.UsedAt != nil || !lc.ExpiresAt.After(m.now()) {
		return nil, persistence.ErrNotFound
	}
	t := m.now()
	lc.UsedAt, lc.UsedByChannel, lc.UsedByExternalID = &t, channel, externalID
	cp := *lc
	return &cp, nil
}

func (m *memLinkCodes) OutstandingLinkCodes(_ context.Context, userID string) ([]*persistence.LinkCode, error) {
	var out []*persistence.LinkCode
	for _, lc := range m.codes {
		if lc.UserID == userID && lc.UsedAt == nil && lc.ExpiresAt.After(m.now()) {
			cp := *lc
			out = append(out, &cp)
		}
	}
	return out, nil
}

// TestMemLinkCodes_SatisfiesMissContract holds this file's in-memory double to
// the same miss contract the real repositories obey.
//
// A double that is stricter or looser than production is the bug class the
// miss-contract design exists to stop: the service tests below assert that a
// spent or unknown code is indistinguishable, and they would pass against a
// double that got that wrong while production got it right — or vice versa.
func TestMemLinkCodes_SatisfiesMissContract(t *testing.T) {
	m := newMemLinkCodes(func() time.Time { return time.Now().UTC() })
	repotest.AssertMissRepo(t, "LinkCodeRepository.GetLinkCode", m.GetLinkCode)
	repotest.AssertMiss(t, "LinkCodeRepository.ConsumeLinkCode", func() (*persistence.LinkCode, error) {
		return m.ConsumeLinkCode(context.Background(), "absent", "telegram", "1")
	})
}

// linkFixture wires the account service with a link-code store and a fixed
// clock, and creates one user to issue codes for.
type linkFixture struct {
	accounts *Accounts
	codes    *memLinkCodes
	repo     *memIdentityRepo
	audit    *memAudit
	user     *persistence.User
	now      time.Time
}

func newLinkFixture(t *testing.T) *linkFixture {
	t.Helper()
	f := &linkFixture{now: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	f.repo = newMemRepo()
	f.audit = &memAudit{}
	f.codes = newMemLinkCodes(func() time.Time { return f.now })
	f.accounts = NewAccounts(f.repo, f.audit)
	f.accounts.WithLinkCodes(f.codes)
	f.accounts.now = func() time.Time { return f.now }
	f.user = &persistence.User{ID: "user_link", DisplayName: "Link User"}
	if err := f.repo.CreateUser(context.Background(), f.user); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return f
}

// hashOf is the production hash, not a second copy of it. It carried its own
// ToUpper/TrimSpace implementation until 2026-09-15, which agreed with
// hashLinkCode only by coincidence — and stopped agreeing the moment
// normalisation widened to strip pasted markup. A test helper that
// reimplements the rule it is checking cannot detect the rule changing.
func hashOf(code string) string { return hashLinkCode(code) }

// TestIssueLinkCode_ShapeAndHashOnlyStorage pins §5.2's code contract: the raw
// code is returned to the issuer once, and ONLY its sha256 reaches the store.
// A repository that could hand back a redeemable code would defeat the whole
// point of hashing it.
func TestIssueLinkCode_ShapeAndHashOnlyStorage(t *testing.T) {
	f := newLinkFixture(t)

	code, err := f.accounts.IssueLinkCode(context.Background(), f.user.ID, Actor{Principal: "op", Source: "ui"})
	if err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}
	if len(code) != linkCodeLength {
		t.Errorf("code %q has length %d, want %d", code, len(code), linkCodeLength)
	}
	for _, r := range code {
		if !strings.ContainsRune(linkCodeAlphabet, r) {
			t.Errorf("code %q contains %q, which is outside the unambiguous alphabet", code, r)
		}
	}

	stored, err := f.codes.GetLinkCode(context.Background(), hashOf(code))
	if err != nil {
		t.Fatalf("the issued code must be findable by its hash: %v", err)
	}
	if stored.UserID != f.user.ID {
		t.Errorf("stored.UserID = %q, want %q", stored.UserID, f.user.ID)
	}
	if want := f.now.Add(linkCodeTTL); !stored.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v (TTL %s)", stored.ExpiresAt, want, linkCodeTTL)
	}
	// The raw code must not be recoverable from the store under any key.
	for hash, lc := range f.codes.codes {
		if hash == code || lc.CodeHash == code {
			t.Fatal("the raw code reached the store; only its sha256 may be persisted")
		}
	}
}

// TestIssueLinkCode_CodesAreUnpredictable is a smoke check that codes come from
// a random source rather than a counter: 200 issues must not collide.
func TestIssueLinkCode_CodesAreUnpredictable(t *testing.T) {
	f := newLinkFixture(t)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code, err := f.accounts.IssueLinkCode(context.Background(), f.user.ID, Actor{Principal: "op", Source: "ui"})
		if err != nil {
			t.Fatalf("IssueLinkCode: %v", err)
		}
		if seen[code] {
			t.Fatalf("code %q issued twice in 200 draws — the source is not random enough to be unguessable", code)
		}
		seen[code] = true
	}
}

// TestIssueLinkCode_RefusesWithoutAudit pins the service-wide rule that an
// access-affecting mutation nobody can see is a compliance gap. A link code is
// a pending access grant, so it is covered.
func TestIssueLinkCode_RefusesWithoutAudit(t *testing.T) {
	f := newLinkFixture(t)
	f.accounts.audit = nil
	if _, err := f.accounts.IssueLinkCode(context.Background(), f.user.ID, Actor{Principal: "op"}); !errors.Is(err, ErrUnauditable) {
		t.Fatalf("IssueLinkCode without an audit sink = %v, want ErrUnauditable", err)
	}
	if len(f.codes.codes) != 0 {
		t.Error("no code may be stored when the mutation is refused")
	}
}

// TestRedeemLinkCode_BindsIdentityAndIsSingleUse is the §5.2 consumer: a valid
// code binds (channel, external_id) to the code's user, and the same code
// cannot bind a second identity.
func TestRedeemLinkCode_BindsIdentityAndIsSingleUse(t *testing.T) {
	f := newLinkFixture(t)
	code, err := f.accounts.IssueLinkCode(context.Background(), f.user.ID, Actor{Principal: "op", Source: "ui"})
	if err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}

	got, err := f.accounts.RedeemLinkCode(context.Background(), code, "telegram", "559741208", "Vadim")
	if err != nil {
		t.Fatalf("RedeemLinkCode: %v", err)
	}
	if got.ID != f.user.ID {
		t.Errorf("redeemed to user %q, want %q", got.ID, f.user.ID)
	}
	if f.repo.bindings["telegram:559741208"] != f.user.ID {
		t.Errorf("binding = %q, want %q", f.repo.bindings["telegram:559741208"], f.user.ID)
	}

	if _, err := f.accounts.RedeemLinkCode(context.Background(), code, "slack", "U999", "Other"); !errors.Is(err, ErrLinkCodeInvalid) {
		t.Fatalf("second redemption = %v, want ErrLinkCodeInvalid", err)
	}
	if _, bound := f.repo.bindings["slack:U999"]; bound {
		t.Error("a spent code must not bind a second identity")
	}
}

// TestRedeemLinkCode_OneErrorForEveryFailure is the oracle rule of §5.2: an
// unknown, expired and already-used code are indistinguishable to the
// redeemer, so no one can probe whether a code ever existed.
func TestRedeemLinkCode_OneErrorForEveryFailure(t *testing.T) {
	f := newLinkFixture(t)
	spent, err := f.accounts.IssueLinkCode(context.Background(), f.user.ID, Actor{Principal: "op", Source: "ui"})
	if err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}
	if _, err := f.accounts.RedeemLinkCode(context.Background(), spent, "telegram", "1", ""); err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	expired, err := f.accounts.IssueLinkCode(context.Background(), f.user.ID, Actor{Principal: "op", Source: "ui"})
	if err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}
	f.now = f.now.Add(linkCodeTTL + time.Second)

	for _, tc := range []struct{ name, code string }{
		{"never existed", "ABCD2345"},
		{"already used", spent},
		{"expired", expired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.accounts.RedeemLinkCode(context.Background(), tc.code, "telegram", "2", "")
			if !errors.Is(err, ErrLinkCodeInvalid) {
				t.Fatalf("err = %v, want the single ErrLinkCodeInvalid for every failure", err)
			}
		})
	}
}

// TestRedeemLinkCode_IsCaseAndDashInsensitive: §5.2 shows codes dashed and
// uppercase for legibility in chat, so redemption must tolerate what a human
// actually types back.
func TestRedeemLinkCode_IsCaseAndDashInsensitive(t *testing.T) {
	f := newLinkFixture(t)
	code, err := f.accounts.IssueLinkCode(context.Background(), f.user.ID, Actor{Principal: "op", Source: "ui"})
	if err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}
	typed := strings.ToLower(code[:4] + "-" + code[4:])
	if _, err := f.accounts.RedeemLinkCode(context.Background(), typed, "telegram", "3", ""); err != nil {
		t.Fatalf("RedeemLinkCode(%q) = %v, want success", typed, err)
	}
}

// TestRedeemLinkCode_RefusesWhenTheSpeakerIsLinkedElsewhere — the speaker is
// already bound to a DIFFERENT account. Repointing them silently would move a
// person's chat history between accounts with neither account seeing it, which
// is the same wrong §5.4 refuses for keys.
//
// This failed as a FALSE SUCCESS before BindIdentity reported the conflict
// (review-20260914-3c36 F11): the bind wrote nothing, returned nil, and
// redemption answered with the code's user — telling the speaker they were
// linked while they resolved to the old account, with the code spent proving
// it.
func TestRedeemLinkCode_RefusesWhenTheSpeakerIsLinkedElsewhere(t *testing.T) {
	f := newLinkFixture(t)
	ctx := context.Background()

	// The speaker already belongs to someone else.
	incumbent := &persistence.User{ID: "user_incumbent", DisplayName: "Incumbent"}
	if err := f.repo.CreateUser(ctx, incumbent); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	bound, err := f.repo.BindIdentity(ctx, &persistence.UserIdentity{
		ID: "uident_incumbent", UserID: incumbent.ID, Channel: "telegram", ExternalID: "42",
	})
	if err != nil || !bound {
		t.Fatalf("seed bind: bound=%v err=%v", bound, err)
	}

	code, err := f.accounts.IssueLinkCode(ctx, f.user.ID, Actor{Principal: "op"})
	if err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}

	user, err := f.accounts.RedeemLinkCode(ctx, code, "telegram", "42", "Speaker")
	if !errors.Is(err, ErrIdentityAlreadyLinked) {
		t.Errorf("RedeemLinkCode over another account's identity = %v, want ErrIdentityAlreadyLinked", err)
	}
	// Distinct from the code errors ON PURPOSE: this says nothing about the
	// code, and the remedy the redeemer needs — unlink this chat from the
	// other account first — is unreachable if they are told their code is bad.
	if errors.Is(err, ErrLinkCodeInvalid) {
		t.Error("the already-linked refusal collapsed into the code's error; the redeemer cannot learn the remedy")
	}
	// And the code MUST survive: it is valid, and the failure is not its
	// fault (review-20260914-a786 F11(b) — the refusal used to spend it).
	if outstanding, oerr := f.accounts.OutstandingLinkCodes(ctx, f.user.ID); oerr != nil || len(outstanding) != 1 {
		t.Errorf("outstanding codes = %d (err %v), want the refused code still redeemable", len(outstanding), oerr)
	}
	if user != nil {
		t.Errorf("a refused redemption returned user %+v; it must return nothing", user)
	}
	// The incumbent keeps the identity. Asserted on the resolver, not on the
	// return value, because the defect was precisely that the return value
	// disagreed with the store.
	rows, err := f.repo.ResolvePrincipalRows(ctx, "telegram", "42")
	if err != nil {
		t.Fatalf("ResolvePrincipalRows: %v", err)
	}
	if len(rows) == 0 || rows[0].UserID != incumbent.ID {
		t.Errorf("the identity now resolves to %+v, want the incumbent %q", rows, incumbent.ID)
	}
}

// TestRedeemLinkCode_SurfacesARealFault — §5.2's indistinguishability is
// between the three ways a code can be no good (absent, spent, expired), not
// between "your code is bad" and "the database is down". Flattening every
// error into ErrLinkCodeInvalid told a redeemer during an outage that their
// perfectly good code was invalid, and hid the outage (F5).
func TestRedeemLinkCode_SurfacesARealFault(t *testing.T) {
	f := newLinkFixture(t)
	ctx := context.Background()
	code, err := f.accounts.IssueLinkCode(ctx, f.user.ID, Actor{Principal: "op"})
	if err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}

	boom := errors.New("connection reset")
	f.codes.consumeErr = boom

	_, err = f.accounts.RedeemLinkCode(ctx, code, "telegram", "7", "Speaker")
	if errors.Is(err, ErrLinkCodeInvalid) {
		t.Error("a transport fault was reported as an invalid code")
	}
	if !errors.Is(err, boom) {
		t.Errorf("RedeemLinkCode = %v, want the underlying fault wrapped", err)
	}
}

// TestRedeemLinkCode_IsAudited — the §5.7 ledger names redemption among the
// five audited mutations, and it was the one that wrote no row (F7). The
// speaker is named as the actor because there is no linked principal yet.
func TestRedeemLinkCode_IsAudited(t *testing.T) {
	f := newLinkFixture(t)
	ctx := context.Background()
	code, err := f.accounts.IssueLinkCode(ctx, f.user.ID, Actor{Principal: "op"})
	if err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}
	if _, err := f.accounts.RedeemLinkCode(ctx, code, "telegram", "42", "Speaker"); err != nil {
		t.Fatalf("RedeemLinkCode: %v", err)
	}

	var row *persistence.AdminAuditEntry
	for _, e := range f.audit.rows {
		if e.Action == "account.link_code.redeem" {
			row = e
		}
	}
	if row == nil {
		t.Fatal("redemption wrote no audit row")
	}
	if row.Principal != "telegram:42" {
		t.Errorf("audit principal = %q, want the redeeming speaker", row.Principal)
	}
	if row.Target != f.user.ID {
		t.Errorf("audit target = %q, want the account joined %q", row.Target, f.user.ID)
	}
	// The raw code must never reach the audit log: a trail that stores
	// redeemable codes is a credential store.
	if strings.Contains(row.After, code) || strings.Contains(row.After, hashOf(code)) {
		t.Error("the audit row carries the link code or its hash")
	}
}

// stubProfileLinker records the profile half of redemption.
type stubProfileLinker struct {
	gotChannel, gotExternal, gotUser string
	calls                            int
	err                              error
}

func (s *stubProfileLinker) LinkSpeakerToAccount(_ context.Context, channel, externalID, userID string) error {
	s.calls++
	s.gotChannel, s.gotExternal, s.gotUser = channel, externalID, userID
	return s.err
}

// TestRedeemLinkCode_MovesTheProfileToTheAccount — §5.2 says a redemption
// writes the user_identities row AND the profile row, and the §5.7 ledger
// tests both. Only the first was written: a person who linked kept their
// accumulated profile keyed under the channel speaker, so §5.0's "once
// linked, the account binding wins" was false for everything except access.
func TestRedeemLinkCode_MovesTheProfileToTheAccount(t *testing.T) {
	f := newLinkFixture(t)
	linker := &stubProfileLinker{}
	f.accounts.WithProfileLinker(linker)
	ctx := context.Background()

	code, err := f.accounts.IssueLinkCode(ctx, f.user.ID, Actor{Principal: "op"})
	if err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}
	if _, err := f.accounts.RedeemLinkCode(ctx, code, "telegram", "42", "Speaker"); err != nil {
		t.Fatalf("RedeemLinkCode: %v", err)
	}

	if linker.calls != 1 {
		t.Fatalf("the profile linker ran %d times, want 1", linker.calls)
	}
	if linker.gotChannel != "telegram" || linker.gotExternal != "42" || linker.gotUser != f.user.ID {
		t.Errorf("linked (%q,%q)→%q, want (telegram,42)→%q",
			linker.gotChannel, linker.gotExternal, linker.gotUser, f.user.ID)
	}
	if got := auditAfter(t, f, "account.link_code.redeem"); !strings.Contains(got, `"profile_repoint":"merged"`) {
		t.Errorf("audit after = %s, want it to record the profile repoint", got)
	}
}

// TestRedeemLinkCode_SurvivesAProfileMergeFailureAndSaysSo — the access
// binding is already committed when the profile merge runs. Failing the whole
// redemption would spend the code and refuse the link over the DATA half,
// which is the lesser of the two. Succeeding silently would be a partial
// success nobody can see, so the audit row carries the outcome.
func TestRedeemLinkCode_SurvivesAProfileMergeFailureAndSaysSo(t *testing.T) {
	f := newLinkFixture(t)
	f.accounts.WithProfileLinker(&stubProfileLinker{err: errors.New("profile store down")})
	ctx := context.Background()

	code, err := f.accounts.IssueLinkCode(ctx, f.user.ID, Actor{Principal: "op"})
	if err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}
	user, err := f.accounts.RedeemLinkCode(ctx, code, "telegram", "42", "Speaker")
	if err != nil {
		t.Fatalf("a profile-merge failure must not fail the link: %v", err)
	}
	if user == nil || user.ID != f.user.ID {
		t.Fatalf("redemption returned %+v, want the account", user)
	}
	if got := repoBinding(f, "telegram", "42"); got != f.user.ID {
		t.Errorf("the access binding is %q, want %q — it was committed before the profile step", got, f.user.ID)
	}
	if got := auditAfter(t, f, "account.link_code.redeem"); !strings.Contains(got, `"profile_repoint":"failed"`) {
		t.Errorf("audit after = %s, want the failure recorded rather than hidden", got)
	}
}

// TestRedeemLinkCode_WithoutAProfileStoreSaysSkipped — "skipped" and "failed"
// must be distinguishable: one is a deployment with no profiles, the other is
// a profile that should have moved and did not.
func TestRedeemLinkCode_WithoutAProfileStoreSaysSkipped(t *testing.T) {
	f := newLinkFixture(t)
	ctx := context.Background()
	code, err := f.accounts.IssueLinkCode(ctx, f.user.ID, Actor{Principal: "op"})
	if err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}
	if _, err := f.accounts.RedeemLinkCode(ctx, code, "telegram", "42", "Speaker"); err != nil {
		t.Fatalf("RedeemLinkCode: %v", err)
	}
	if got := auditAfter(t, f, "account.link_code.redeem"); !strings.Contains(got, `"profile_repoint":"no_profile_store"`) {
		t.Errorf("audit after = %s, want no_profile_store", got)
	}
}

func auditAfter(t *testing.T, f *linkFixture, action string) string {
	t.Helper()
	for _, e := range f.audit.rows {
		if e.Action == action {
			return e.After
		}
	}
	t.Fatalf("no audit row for %q", action)
	return ""
}

func repoBinding(f *linkFixture, channel, externalID string) string {
	return f.repo.bindings[channel+":"+externalID]
}

// TestAccountOperatorID_IsItsOwnNamespace — the canonical id must not be
// whichever speaker linked first, or a person's canonical row would depend on
// the channel they happened to use and would point at an identity they may
// later unlink.
func TestAccountOperatorID_IsItsOwnNamespace(t *testing.T) {
	got := AccountOperatorID("user_1")
	if got != "account:user_1" {
		t.Errorf("AccountOperatorID = %q, want account:user_1", got)
	}
	if strings.HasPrefix(got, "telegram:") || strings.HasPrefix(got, "slack:") {
		t.Error("the account canonical id collides with a channel speaker namespace")
	}
}

// TestIssueLinkCode_DistinguishesAnAttemptFromAnIssuedCode — the audit row is
// written before the code exists, deliberately: an orphan audit row
// over-reports and grants nothing, while an orphan code is a redeemable
// access grant with no trail.
//
// The cost is that the row alone cannot say whether the code was created. Two
// reviewers landed on the same gap independently (e581 N3, a786): an auditor
// reading an `issue` row with no matching link_codes row cannot tell
// "creation failed" from "created, then consumed and swept". The confirmation
// row is what separates them, and a reconciliation counts the confirmed ones.
func TestIssueLinkCode_DistinguishesAnAttemptFromAnIssuedCode(t *testing.T) {
	f := newLinkFixture(t)
	if _, err := f.accounts.IssueLinkCode(context.Background(), f.user.ID, Actor{Principal: "op"}); err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}

	var attempt, issued *persistence.AdminAuditEntry
	for _, e := range f.audit.rows {
		switch e.Action {
		case "account.link_code.issue":
			attempt = e
		case "account.link_code.issued":
			issued = e
		}
	}
	if attempt == nil {
		t.Fatal("no attempt row: a code could be created with no trail at all")
	}
	if issued == nil {
		t.Fatal("no confirmation row: an auditor cannot tell a created code from a failed creation")
	}
	if !strings.Contains(attempt.After, `"stored":false`) {
		t.Errorf("attempt row = %s, want stored:false", attempt.After)
	}
	if !strings.Contains(issued.After, `"stored":true`) {
		t.Errorf("confirmation row = %s, want stored:true", issued.After)
	}
	// Neither may carry the code or its hash: an audit log that stores
	// redeemable codes is a credential store.
	for _, e := range []*persistence.AdminAuditEntry{attempt, issued} {
		if strings.Contains(e.After, "code") && !strings.Contains(e.After, "stored") {
			t.Errorf("audit row leaks something code-shaped: %s", e.After)
		}
	}
}

// TestIssueLinkCode_AFailedStoreLeavesOnlyTheAttempt — the partial failure
// the ordering was chosen for. The trail over-reports (an attempt with no
// code) and grants nothing, which is the safe direction; what must NOT happen
// is a confirmation for a code that was never created.
func TestIssueLinkCode_AFailedStoreLeavesOnlyTheAttempt(t *testing.T) {
	f := newLinkFixture(t)
	f.codes.createErr = errors.New("store down")

	if _, err := f.accounts.IssueLinkCode(context.Background(), f.user.ID, Actor{Principal: "op"}); err == nil {
		t.Fatal("IssueLinkCode reported success with a failed store")
	}
	for _, e := range f.audit.rows {
		if e.Action == "account.link_code.issued" {
			t.Error("a confirmation row was written for a code that was never created")
		}
	}
	var sawAttempt bool
	for _, e := range f.audit.rows {
		if e.Action == "account.link_code.issue" {
			sawAttempt = true
		}
	}
	if !sawAttempt {
		t.Error("the attempt left no trail at all")
	}
}

// TestIssueLinkCode_AFailedConfirmationIsIndeterminateNotAbsent — the
// direction the attempt/issued pair does NOT close, stated so the next reader
// does not over-read the rows (review-20260915-4eba F3).
//
// If the confirmation write fails, the code EXISTS and is redeemable while
// the trail shows only an attempt. That is an under-report, the opposite of
// the over-report the ordering was chosen for. So `issue` alone means
// indeterminate, not "no code was created", and a reconciliation counting
// `issued` rows against redemptions counts a lower bound on grants.
func TestIssueLinkCode_AFailedConfirmationIsIndeterminateNotAbsent(t *testing.T) {
	f := newLinkFixture(t)
	ctx := context.Background()

	// Fail only the SECOND audit write — the confirmation.
	f.audit.failAfter = 1

	_, err := f.accounts.IssueLinkCode(ctx, f.user.ID, Actor{Principal: "op"})
	if err == nil {
		t.Fatal("a failed confirmation reported success")
	}

	// The trail shows an attempt and no confirmation...
	var sawAttempt, sawIssued bool
	for _, e := range f.audit.rows {
		switch e.Action {
		case "account.link_code.issue":
			sawAttempt = true
		case "account.link_code.issued":
			sawIssued = true
		}
	}
	if !sawAttempt || sawIssued {
		t.Errorf("rows: attempt=%v issued=%v, want attempt only", sawAttempt, sawIssued)
	}

	// ...and the code IS redeemable. This is the whole point: an auditor
	// reading `issue` with no `issued` must NOT conclude nothing was created.
	outstanding, oerr := f.accounts.OutstandingLinkCodes(ctx, f.user.ID)
	if oerr != nil {
		t.Fatalf("OutstandingLinkCodes: %v", oerr)
	}
	if len(outstanding) != 1 {
		t.Errorf("outstanding = %d, want 1 — the code exists despite the missing confirmation, "+
			"which is exactly why `issue` alone is indeterminate rather than absent", len(outstanding))
	}
}

// TestRedeemLinkCode_ARaceSpendIsAuditedAsItsOwn — the pre-check's principle
// (a valid code is not spent on a failure that is not the code's fault) is
// violated by the race the pre-check cannot close without a transaction
// spanning two repositories. A spent-on-race code otherwise looks exactly
// like a spent-and-bound one (review-20260915-4eba F2).
func TestRedeemLinkCode_ARaceSpendIsAuditedAsItsOwn(t *testing.T) {
	f := newLinkFixture(t)
	ctx := context.Background()
	incumbent := &persistence.User{ID: "user_incumbent", DisplayName: "Incumbent"}
	if err := f.repo.CreateUser(ctx, incumbent); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	code, err := f.accounts.IssueLinkCode(ctx, f.user.ID, Actor{Principal: "op"})
	if err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}

	// Land the competing bind AFTER the pre-check read and BEFORE the write —
	// the window the pre-check cannot cover. The seam makes the interleaving
	// deterministic rather than relying on a stochastic reproduction.
	f.repo.beforeBind = func() {
		f.repo.bindings["telegram:42"] = incumbent.ID
		f.repo.beforeBind = nil
	}

	if _, err := f.accounts.RedeemLinkCode(ctx, code, "telegram", "42", "Speaker"); !errors.Is(err, ErrIdentityAlreadyLinked) {
		t.Fatalf("RedeemLinkCode = %v, want ErrIdentityAlreadyLinked", err)
	}
	var raced *persistence.AdminAuditEntry
	for _, e := range f.audit.rows {
		if e.Action == "account.link_code.race_conflict" {
			raced = e
		}
	}
	if raced == nil {
		t.Fatal("the race spent a valid code and left no row saying so; an operator cannot tell " +
			"it from a clean redemption, and will tell the person to retype a code that is gone")
	}
	if !strings.Contains(raced.After, `"code_spent":true`) {
		t.Errorf("race row = %s, want it to record that the code was spent", raced.After)
	}

	// And code_spent must be TRUE, not merely written. The row is only honest
	// if the consume really did happen before the bind — if consumption were
	// gated on a successful bind, the code would still be live and the audit
	// would record a falsehood, which is worse than the under-report the row
	// was added to fix (review-20260915-1ea8 F3).
	if outstanding, oerr := f.accounts.OutstandingLinkCodes(ctx, f.user.ID); oerr != nil || len(outstanding) != 0 {
		t.Errorf("outstanding = %d (err %v) after a race spend; the audit row says code_spent:true "+
			"and the code is still live", len(outstanding), oerr)
	}
	// Proven from the redeemer's side too: a second attempt with the same code
	// is refused as invalid, because it is gone.
	f.repo.bindings = map[string]string{} // clear the incumbent so only the CODE can refuse
	if _, err := f.accounts.RedeemLinkCode(ctx, code, "telegram", "43", "Speaker"); !errors.Is(err, ErrLinkCodeInvalid) {
		t.Errorf("re-redeeming a race-spent code = %v, want ErrLinkCodeInvalid — the code was not spent", err)
	}
}

// TestLinkCodeID_MatchesTheStoredRow — the observation contract's load-bearing
// and previously unasserted assumption.
//
// The panel gets its handle from LinkCodeID(code) at issue time; the poll
// compares it against LinkCodeIDOf(row) from the store. If those two ever
// derived different bytes from the same code, the watcher would never see its
// id leave the list — so a redeemed code would be reported as EXPIRED, ten
// minutes later, with no error anywhere (review-20260915-c6a0 F2).
//
// Two functions, one value: nothing but this connects them.
func TestLinkCodeID_MatchesTheStoredRow(t *testing.T) {
	f := newLinkFixture(t)
	ctx := context.Background()

	code, err := f.accounts.IssueLinkCode(ctx, f.user.ID, Actor{Principal: "op"})
	if err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}
	outstanding, err := f.accounts.OutstandingLinkCodes(ctx, f.user.ID)
	if err != nil || len(outstanding) != 1 {
		t.Fatalf("OutstandingLinkCodes = %d (err %v), want 1", len(outstanding), err)
	}

	fromCode := LinkCodeID(code)
	fromRow := LinkCodeIDOf(outstanding[0])
	if fromCode != fromRow {
		t.Fatalf("LinkCodeID(code)=%q but LinkCodeIDOf(row)=%q — the panel's handle never matches "+
			"the list, so a redeemed code reads as expired ten minutes later", fromCode, fromRow)
	}
	if len(fromCode) != 16 {
		t.Errorf("id length = %d, want 16", len(fromCode))
	}
	// And the handle must not be the code, in either direction.
	if strings.Contains(fromCode, code) || strings.Contains(code, fromCode) {
		t.Error("the handle contains the code; it travels in a polling context and the code must not")
	}
}

// TestRedeemLinkCode_TolerantOfPastedFormatting — 2026-09-15: the "My account"
// panel rendered the instruction inside a <code> element, so copying it into
// Slack's rich-text composer carried the code formatting along, and Slack
// serialised that back as literal backticks in the slash payload. The operator
// redeemed successfully only after stripping the formatting by hand.
//
// Normalisation keeps [A-Z0-9] and drops everything else. That is safe rather
// than merely convenient: linkCodeAlphabet is pure uppercase alphanumeric, so
// no legitimate code is altered by the filter and no two distinct codes can be
// normalised onto each other.
func TestRedeemLinkCode_TolerantOfPastedFormatting(t *testing.T) {
	for i, decorate := range []func(string) string{
		func(c string) string { return "`" + c + "`" }, // Slack/markdown code span
		func(c string) string { return "*" + c + "*" }, // bold
		func(c string) string { return "_" + c + "_" }, // italic
		func(c string) string { return "~" + c + "~" }, // strikethrough
		func(c string) string { return "“" + c + "”" }, // smart quotes
		func(c string) string { return " " + c + " " }, // NBSP from a web copy
		// NOT covered, deliberately: literal HTML tag text, e.g.
		// "<code>ACDE2345</code>". The filter keeps [A-Z0-9], so that
		// normalises to "CODEACDE2345CODE" and is refused. Supporting it
		// would need tag stripping, and a browser copy yields the element's
		// TEXT, never its markup — the shape only arises from copying page
		// source, which nobody does. It fails closed, which is the right
		// direction for an unrecognised shape.
	} {
		f := newLinkFixture(t)
		code, err := f.accounts.IssueLinkCode(context.Background(), f.user.ID, Actor{Principal: "op", Source: "ui"})
		if err != nil {
			t.Fatalf("IssueLinkCode: %v", err)
		}
		typed := decorate(code)
		if _, err := f.accounts.RedeemLinkCode(context.Background(), typed, "slack", "U1", ""); err != nil {
			t.Errorf("case %d: RedeemLinkCode(%q) = %v, want success", i, typed, err)
		}
	}
}

// TestHashLinkCode_NormalisationCannotCollideTwoCodes pins the property that
// makes the filter above safe. If the generator alphabet ever grew a character
// the filter strips, two distinct codes could hash identically — one person
// redeeming another's code.
func TestHashLinkCode_NormalisationCannotCollideTwoCodes(t *testing.T) {
	for _, r := range linkCodeAlphabet {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", r) {
			t.Errorf("linkCodeAlphabet contains %q, which hashLinkCode's normalisation strips: "+
				"two distinct codes could normalise onto one hash", r)
		}
	}
}

// TestRedeemLinkCode_RefusesLiteralHtmlTagText pins the fail-closed direction
// of the normalisation, which was previously only incidental.
//
// "<code>ACDE2345</code>" normalises to "CODEACDE2345CODE" and is refused.
// That is correct — a browser copy yields the element's text, never its
// markup, so the shape arises only from copying page source. But nothing
// asserted it, so a later "be more tolerant" change could have started
// accepting markup-shaped input with no test failing
// (review-20260915-3d5b, finding 4). The deliberate non-coverage is now a
// pinned invariant rather than an omission.
func TestRedeemLinkCode_RefusesLiteralHtmlTagText(t *testing.T) {
	f := newLinkFixture(t)
	code, err := f.accounts.IssueLinkCode(context.Background(), f.user.ID, Actor{Principal: "op", Source: "ui"})
	if err != nil {
		t.Fatalf("IssueLinkCode: %v", err)
	}
	typed := "<code>" + code + "</code>"
	if _, err := f.accounts.RedeemLinkCode(context.Background(), typed, "slack", "U1", ""); !errors.Is(err, ErrLinkCodeInvalid) {
		t.Fatalf("RedeemLinkCode(%q) = %v, want ErrLinkCodeInvalid: tag text must fail closed", typed, err)
	}
	// And the code is NOT spent by the refusal — it never matched a row.
	if _, err := f.accounts.RedeemLinkCode(context.Background(), code, "slack", "U1", ""); err != nil {
		t.Errorf("the clean code no longer redeems after a tag-text attempt: %v", err)
	}
}
