package email

import (
	"context"
	"strings"
	"testing"
)

func TestNoopSignatureVerifier_AlwaysPasses(t *testing.T) {
	v := NoopSignatureVerifier{}
	if err := v.Verify(context.Background(), ParsedMessage{}); err != nil {
		t.Errorf("Verify returned %v, want nil", err)
	}
	if err := v.Verify(context.Background(), ParsedMessage{From: "anything"}); err != nil {
		t.Errorf("Verify returned %v, want nil", err)
	}
}

// SignatureVerifier interface satisfied at compile time by both
// the no-op and the header verifier — guards against accidental
// rename / signature drift.
func TestSignatureVerifier_InterfaceCompliance(t *testing.T) {
	var _ SignatureVerifier = NoopSignatureVerifier{}
	var _ SignatureVerifier = HeaderAuthVerifier{}
}

// ---- HeaderAuthVerifier ----

func TestHeaderAuth_RelaxedAdmitsUnstampedMail(t *testing.T) {
	// No Authentication-Results / Received-SPF header at all —
	// relaxed policy admits.
	v := HeaderAuthVerifier{Policy: AuthPolicyRelaxed}
	if err := v.Verify(context.Background(), ParsedMessage{From: "a@x.com"}); err != nil {
		t.Errorf("relaxed should admit unstamped mail, got %v", err)
	}
}

func TestHeaderAuth_StrictRejectsUnstampedMail(t *testing.T) {
	v := HeaderAuthVerifier{Policy: AuthPolicyStrict}
	err := v.Verify(context.Background(), ParsedMessage{From: "a@x.com"})
	if err == nil {
		t.Fatal("strict must reject unstamped mail")
	}
	if !strings.Contains(err.Error(), "strict policy") {
		t.Errorf("err = %v, want 'strict policy' phrase", err)
	}
}

func TestHeaderAuth_ExplicitSPFFailAlwaysRejects(t *testing.T) {
	cases := []AuthPolicy{AuthPolicyRelaxed, AuthPolicyStrict}
	for _, pol := range cases {
		v := HeaderAuthVerifier{Policy: pol}
		err := v.Verify(context.Background(), ParsedMessage{
			From:        "spoofer@x.com",
			AuthResults: []string{"relay.example.com; spf=fail smtp.mailfrom=spoofer@x.com"},
		})
		if err == nil {
			t.Errorf("policy=%v: spf=fail must always reject", pol)
		}
	}
}

func TestHeaderAuth_ExplicitDKIMFailAlwaysRejects(t *testing.T) {
	cases := []AuthPolicy{AuthPolicyRelaxed, AuthPolicyStrict}
	for _, pol := range cases {
		v := HeaderAuthVerifier{Policy: pol}
		err := v.Verify(context.Background(), ParsedMessage{
			From:        "spoofer@x.com",
			AuthResults: []string{"relay.example.com; dkim=fail (signature invalid) header.d=x.com"},
		})
		if err == nil {
			t.Errorf("policy=%v: dkim=fail must always reject", pol)
		}
	}
}

func TestHeaderAuth_SPFPassStrictAdmits(t *testing.T) {
	v := HeaderAuthVerifier{Policy: AuthPolicyStrict}
	if err := v.Verify(context.Background(), ParsedMessage{
		From:        "alice@example.com",
		AuthResults: []string{"relay.example.com; spf=pass smtp.mailfrom=alice@example.com"},
	}); err != nil {
		t.Errorf("strict + spf=pass must admit, got %v", err)
	}
}

func TestHeaderAuth_DKIMPassStrictAdmits(t *testing.T) {
	v := HeaderAuthVerifier{Policy: AuthPolicyStrict}
	if err := v.Verify(context.Background(), ParsedMessage{
		From:        "alice@example.com",
		AuthResults: []string{"relay.example.com; dkim=pass header.d=example.com"},
	}); err != nil {
		t.Errorf("strict + dkim=pass must admit, got %v", err)
	}
}

func TestHeaderAuth_StrictNeutralRejects(t *testing.T) {
	// "neutral" is not a pass under strict.
	v := HeaderAuthVerifier{Policy: AuthPolicyStrict}
	err := v.Verify(context.Background(), ParsedMessage{
		From:        "alice@example.com",
		AuthResults: []string{"relay; spf=neutral; dkim=neutral"},
	})
	if err == nil {
		t.Error("strict must reject when only neutral verdicts present")
	}
}

func TestHeaderAuth_RelaxedNeutralAdmits(t *testing.T) {
	v := HeaderAuthVerifier{Policy: AuthPolicyRelaxed}
	if err := v.Verify(context.Background(), ParsedMessage{
		From:        "alice@example.com",
		AuthResults: []string{"relay; spf=neutral"},
	}); err != nil {
		t.Errorf("relaxed admits neutral-only mail, got %v", err)
	}
}

func TestHeaderAuth_ReceivedSPFPassAdmitsStrict(t *testing.T) {
	// No Authentication-Results, but Received-SPF stamped pass — strict admits.
	v := HeaderAuthVerifier{Policy: AuthPolicyStrict}
	if err := v.Verify(context.Background(), ParsedMessage{
		From:        "alice@example.com",
		ReceivedSPF: []string{"Pass (mailfrom: alice@example.com) client-ip=1.2.3.4"},
	}); err != nil {
		t.Errorf("Received-SPF pass must satisfy strict, got %v", err)
	}
}

func TestHeaderAuth_ReceivedSPFFailRejects(t *testing.T) {
	v := HeaderAuthVerifier{Policy: AuthPolicyRelaxed}
	err := v.Verify(context.Background(), ParsedMessage{
		From:        "spoofer@x.com",
		ReceivedSPF: []string{"Fail (sender not authorized) client-ip=1.2.3.4"},
	})
	if err == nil {
		t.Error("Received-SPF fail must reject under relaxed too")
	}
}

func TestHeaderAuth_MultipleHopsPassOverridesEarlierNeutral(t *testing.T) {
	v := HeaderAuthVerifier{Policy: AuthPolicyStrict}
	// First relay stamped neutral; second relay (closer to the
	// recipient mailbox) stamped pass. summariseAuth ranks pass
	// over neutral so strict admits.
	if err := v.Verify(context.Background(), ParsedMessage{
		From: "alice@example.com",
		AuthResults: []string{
			"hop1.example.com; spf=neutral",
			"hop2.example.com; spf=pass smtp.mailfrom=alice@example.com",
		},
	}); err != nil {
		t.Errorf("pass on later hop must override neutral, got %v", err)
	}
}

func TestHeaderAuth_FailOnAnyHopWinsOverPassOnAnother(t *testing.T) {
	v := HeaderAuthVerifier{Policy: AuthPolicyRelaxed}
	if err := v.Verify(context.Background(), ParsedMessage{
		From: "alice@example.com",
		AuthResults: []string{
			"hop1; spf=fail",
			"hop2; spf=pass",
		},
	}); err == nil {
		t.Error("explicit fail on any hop must reject even when another hop stamped pass")
	}
}

func TestHeaderAuth_ParensAreStrippedFromHeader(t *testing.T) {
	// Verdict surrounded by RFC-5322 CFWS parens. parseAuth must
	// still find the right token.
	v := HeaderAuthVerifier{Policy: AuthPolicyStrict}
	if err := v.Verify(context.Background(), ParsedMessage{
		From: "alice@example.com",
		AuthResults: []string{
			"relay.example.com; (slightly hairy chain) spf=pass (good sender) smtp.mailfrom=alice@example.com",
		},
	}); err != nil {
		t.Errorf("parenthetical comments must not break parsing, got %v", err)
	}
}

// ---- low-level parser ----

func TestParseAuthResultsHeader_MethodResultPairs(t *testing.T) {
	hdr := "relay.example.com; spf=pass smtp.mailfrom=a@b; dkim=fail header.d=b; dmarc=pass"
	authservID, got := parseAuthResultsHeader(hdr)
	if authservID != "relay.example.com" {
		t.Errorf("authserv-id = %q, want relay.example.com", authservID)
	}
	want := map[string]authVerdict{
		"spf":   authPass,
		"dkim":  authFail,
		"dmarc": authPass,
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries, want %d (%v)", len(got), len(want), got)
	}
	for _, m := range got {
		if w, ok := want[m.Method]; !ok {
			t.Errorf("unexpected method %q", m.Method)
		} else if m.Result != w {
			t.Errorf("method=%q got %s, want %s", m.Method, m.Result, w)
		}
	}
}

func TestParseAuthResultsHeader_SkipsAuthservID(t *testing.T) {
	// The first segment of an Authentication-Results header is the
	// authserv-id, not a method=verdict — the parser must skip it
	// even if it contains an '=' (rare but legal).
	hdr := "id=relay.example.com; spf=pass"
	_, got := parseAuthResultsHeader(hdr)
	if len(got) != 1 || got[0].Method != "spf" {
		t.Errorf("authserv-id must be skipped, got %v", got)
	}
}

func TestParseAuthResultsHeader_EmptyReturnsNil(t *testing.T) {
	if _, got := parseAuthResultsHeader(""); got != nil {
		t.Errorf("empty header should yield nil, got %v", got)
	}
}

func TestStripParenComments(t *testing.T) {
	cases := map[string]string{
		"a (b) c":           "a  c",
		"a (b (c)) d":       "a  d",
		"no parens here":    "no parens here",
		"unbalanced (open":  "unbalanced ",
		"unbalanced close)": "unbalanced close)",
	}
	for in, want := range cases {
		if got := stripParenComments(in); got != want {
			t.Errorf("stripParenComments(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstToken(t *testing.T) {
	cases := map[string]authVerdict{
		"pass":            authPass,
		"PASS  extras":    authPass,
		"fail (reason)":   authFail,
		"softfail":        authSoftFail,
		"neutral, extras": authNeutral,
		"temperror":       authTempFail,
		"permerror":       authPermFail,
		"policy":          authPolicy,
		"none":            authNone,
		"":                authNone,
		"unknown_verdict": authNone,
	}
	for in, want := range cases {
		if got := firstToken(in); got != want {
			t.Errorf("firstToken(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSummariseAuth_RankingPrecedence(t *testing.T) {
	// pass > fail > softfail > neutral > temperror/permerror > none
	cases := []struct {
		name string
		msg  ParsedMessage
		spf  authVerdict
		dkim authVerdict
	}{
		{
			name: "empty",
			msg:  ParsedMessage{},
			spf:  authNone,
			dkim: authNone,
		},
		{
			name: "only ReceivedSPF",
			msg:  ParsedMessage{ReceivedSPF: []string{"Pass extras"}},
			spf:  authPass,
		},
		{
			name: "AuthResults with both methods",
			msg: ParsedMessage{
				AuthResults: []string{"relay; spf=pass; dkim=pass"},
			},
			spf:  authPass,
			dkim: authPass,
		},
		{
			name: "soft then hard pass merges to pass",
			msg: ParsedMessage{
				AuthResults: []string{"r1; spf=softfail", "r2; spf=pass"},
			},
			spf: authPass,
		},
	}
	for _, c := range cases {
		// No trusted-server filtering configured — legacy summarise
		// behaviour (consults every A-R / Received-SPF header).
		spf, dkim := HeaderAuthVerifier{}.summariseAuth(c.msg)
		if spf != c.spf {
			t.Errorf("%s: spf = %s, want %s", c.name, spf, c.spf)
		}
		if c.dkim != "" && dkim != c.dkim {
			t.Errorf("%s: dkim = %s, want %s", c.name, dkim, c.dkim)
		}
	}
}

func TestReasonOrEmpty(t *testing.T) {
	if got := reasonOrEmpty(ParsedMessage{From: "a@x"}); got != "from=a@x" {
		t.Errorf("got %q", got)
	}
	if got := reasonOrEmpty(ParsedMessage{}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// End-to-end: ParseRFC5322 must surface multi-hop AuthResults +
// Received-SPF headers on the resulting ParsedMessage so
// HeaderAuthVerifier can see them.
func TestParseRFC5322_CollectsAuthHeaders(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"From: Alice <alice@example.com>",
		"To: ops@vornik.io",
		"Subject: test",
		"Message-ID: <m1@example.com>",
		"Authentication-Results: hop1.example.com; spf=pass smtp.mailfrom=alice@example.com",
		"Authentication-Results: hop2.example.com; dkim=pass header.d=example.com",
		"Received-SPF: Pass (sender SPF authorized) client-ip=1.2.3.4",
		"Content-Type: text/plain",
		"",
		"hi",
	}, "\r\n"))
	got, err := ParseRFC5322(raw)
	if err != nil {
		t.Fatalf("ParseRFC5322: %v", err)
	}
	if len(got.AuthResults) != 2 {
		t.Fatalf("AuthResults: got %d entries, want 2 (%v)", len(got.AuthResults), got.AuthResults)
	}
	if len(got.ReceivedSPF) != 1 {
		t.Fatalf("ReceivedSPF: got %d entries, want 1", len(got.ReceivedSPF))
	}

	// Plug into HeaderAuthVerifier to confirm the wiring is end-to-end.
	v := HeaderAuthVerifier{Policy: AuthPolicyStrict}
	if err := v.Verify(context.Background(), got); err != nil {
		t.Errorf("strict policy with both pass headers must admit, got %v", err)
	}
}
