package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The §5.5 CLI verbs: link-code, claim-key, grant-key, revoke-key. All hit the
// CE operator shell, which is where identity's operator routes live — NOT
// /api/v1/admin/, whose gate answers 501 EDITION_UNSUPPORTED in Community.
func TestAccountKeyVerbs_HitOperatorRoutes(t *testing.T) {
	var seen []string
	var claimBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/operator/accounts/u1/link-code":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "ACDE2345", "expiresIn": 600})
		case "/api/v1/operator/accounts/u1/claim-key":
			_ = json.NewDecoder(r.Body).Decode(&claimBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{"id": "u1"}})
		case "/api/v1/operator/accounts/u1/assign-key":
			_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{"id": "u1"}})
		case "/api/v1/operator/accounts/u1/unassign-key":
			_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{"id": "u1"}})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "NOT_FOUND", "message": "no"}})
		}
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)

	if err := runAccountLinkCode(nil, []string{"u1"}); err != nil {
		t.Fatalf("link-code: %v", err)
	}
	t.Setenv(claimSecretEnv, "sk-vornik-from-env")
	if err := runAccountClaimKey(nil, []string{"u1", "akey_1"}); err != nil {
		t.Fatalf("claim-key: %v", err)
	}
	if err := runAccountGrantKey(nil, []string{"u1", "akey_1"}); err != nil {
		t.Fatalf("grant-key: %v", err)
	}
	if err := runAccountRevokeKey(nil, []string{"u1", "akey_1"}); err != nil {
		t.Fatalf("revoke-key: %v", err)
	}

	want := []string{
		"POST /api/v1/operator/accounts/u1/link-code",
		"POST /api/v1/operator/accounts/u1/claim-key",
		"POST /api/v1/operator/accounts/u1/assign-key",
		"POST /api/v1/operator/accounts/u1/unassign-key",
	}
	for i, w := range want {
		if i >= len(seen) || seen[i] != w {
			t.Fatalf("call %d = %q, want %q (all of: %v)", i, safeAt(seen, i), w, seen)
		}
	}
	if claimBody["keyId"] != "akey_1" || claimBody["keySecret"] != "sk-vornik-from-env" {
		t.Errorf("claim body = %v", claimBody)
	}
}

// TestClaimKey_SecretIsNeverACommandLineArgument is a security property the
// design does not spell out but the shell forces on us: an argument lands in
// shell history and in `ps` output for every user on the box. The secret is
// read from the environment (or stdin), never from argv, and the verb takes
// exactly two positional arguments so there is nowhere to put it.
func TestClaimKey_SecretIsNeverACommandLineArgument(t *testing.T) {
	if got := accountClaimKeyCmd.Args; got == nil {
		t.Fatal("claim-key must pin its argument count")
	}
	// Two args: account id and key id. A third would be the secret.
	if err := accountClaimKeyCmd.Args(accountClaimKeyCmd, []string{"u1", "akey", "sk-secret"}); err == nil {
		t.Error("claim-key accepted a third positional argument; a secret in argv reaches shell history and ps")
	}
	if err := accountClaimKeyCmd.Args(accountClaimKeyCmd, []string{"u1", "akey"}); err != nil {
		t.Errorf("claim-key must accept exactly <account-id> <key-id>: %v", err)
	}
}

// TestClaimKey_RefusesWithoutASecret: a claim with no secret is not an
// anonymous claim, it is a mistake, and it must not reach the API as an empty
// proof of possession.
func TestClaimKey_RefusesWithoutASecret(t *testing.T) {
	_ = os.Unsetenv(claimSecretEnv)
	err := runAccountClaimKey(nil, []string{"u1", "akey_1"})
	if err == nil {
		t.Fatal("claim-key without a secret must fail locally, before any request")
	}
	if !strings.Contains(err.Error(), claimSecretEnv) {
		t.Errorf("the error should name %s so the operator knows where to put the secret: %v", claimSecretEnv, err)
	}
}

func safeAt(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "<missing>"
}

// TestAccountHelp_NamesTheChatCommandThatExists — the account command's help
// told the operator to link with `/account link <code>`, which is not a
// command on any channel. §5.0 settled the collision by REUSING `/link` with
// two code types rather than inventing a verb, and the help text kept naming
// the verb that was never built.
//
// A documented behaviour nothing implements is worse than an absent one: it
// stops the next person looking. Help text is the most-read documentation in
// the product, so it gets an assertion rather than a proofread.
func TestAccountHelp_NamesTheChatCommandThatExists(t *testing.T) {
	help := accountCmd.Long + "\n" + accountCmd.Short
	if strings.Contains(help, "/account link") {
		t.Error("the help names `/account link`, which is not a command on any channel")
	}
	if !strings.Contains(help, "/link <code>") {
		t.Error("the help does not name Telegram's /link; a person reading it cannot redeem the code it tells them to issue")
	}
}

// TestLinkCodeHelp_DoesNotHardcodeTheSlackCommand — slack.slash_command is
// configurable, so printing a literal "/vornik link" tells anyone who changed
// it to type something their workspace does not answer. Same defect as naming
// `/account link`, which this file already guards against.
func TestLinkCodeHelp_DoesNotHardcodeTheSlackCommand(t *testing.T) {
	help := accountLinkCodeCmd.Long + "\n" + accountLinkCodeCmd.Short
	if strings.Contains(help, "/vornik link") {
		t.Error("the help hardcodes /vornik as the Slack command; it is configurable per deployment")
	}
	if !strings.Contains(help, "Slack slash command") {
		t.Error("the help does not tell the reader the Slack command is theirs to substitute")
	}
	// And it must say which verb this is: it posts to the operator route.
	if !strings.Contains(help, "OPERATOR verb") {
		t.Error("the help does not say this needs operator capability, so a non-operator will try it and get 403")
	}
}
