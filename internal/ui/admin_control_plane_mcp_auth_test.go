package ui

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/mcpauth"
)

// The hub's Add/Edit writes the `auth:` block itself now (MCP server authentication design step
// 6). Until yamledit could write a nested mapping it had to REFUSE editing a server carrying one,
// because the upsert replaces the whole list item and would have silently deleted the block —
// unauthenticating a working server on an unrelated save.

func mcpAddEditWith(t *testing.T, current string, name string, form url.Values) ([]byte, string, error) {
	t.Helper()
	r := httptest.NewRequest("POST", "/ui/admin/control-plane", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	return s.mcpAddEdit([]byte(current), name, r)
}

// parsedAuth reads back one server's auth block from the produced config.
func parsedAuth(t *testing.T, out []byte, server string) mcpauth.Auth {
	t.Helper()
	var probe struct {
		MCP struct {
			Servers []struct {
				Name string       `yaml:"name"`
				Auth mcpauth.Auth `yaml:"auth"`
			} `yaml:"servers"`
		} `yaml:"mcp"`
	}
	if err := yaml.Unmarshal(out, &probe); err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}
	for _, s := range probe.MCP.Servers {
		if s.Name == server {
			return s.Auth
		}
	}
	t.Fatalf("server %q not in output:\n%s", server, out)
	return mcpauth.Auth{}
}

func TestMcpAddEdit_WritesAStaticAuthBlock(t *testing.T) {
	out, _, err := mcpAddEditWith(t, "mcp:\n  servers: []\n", "n8n", url.Values{
		"transport":         {"streamable-http"},
		"url":               {"https://n8n.example.com/mcp/abc"},
		"auth_mode":         {"static"},
		"auth_value_from":   {"secret://N8N_TOKEN"},
		"auth_value_prefix": {"Bearer "},
	})
	if err != nil {
		t.Fatalf("mcpAddEdit: %v", err)
	}
	got := parsedAuth(t, out, "n8n")
	if got.Mode != mcpauth.ModeStatic || got.ValueFrom != "secret://N8N_TOKEN" {
		t.Errorf("auth = %+v", got)
	}
	if got.ValuePrefix != "Bearer " {
		t.Errorf("value_prefix = %q (the trailing space is load-bearing)", got.ValuePrefix)
	}
	// The whole invariant of the ledger path: config carries references, so the
	// proposal diff can be reviewed without leaking anything.
	if strings.Contains(string(out), "PLACEHOLDER") || !strings.Contains(string(out), "secret://") {
		t.Errorf("config must hold a secret:// reference, never a value:\n%s", out)
	}
}

func TestMcpAddEdit_WritesAnEnvAuthBlock(t *testing.T) {
	out, _, err := mcpAddEditWith(t, "mcp:\n  servers: []\n", "reddit", url.Values{
		"transport":     {"stdio"},
		"command":       {"reddit-mcp"},
		"auth_mode":     {"env"},
		"auth_env_from": {"REDDIT_CLIENT_ID=secret://rid\nREDDIT_CLIENT_SECRET=secret://rsec"},
	})
	if err != nil {
		t.Fatalf("mcpAddEdit: %v", err)
	}
	got := parsedAuth(t, out, "reddit")
	if got.EnvFrom["REDDIT_CLIENT_SECRET"] != "secret://rsec" {
		t.Errorf("env_from = %v", got.EnvFrom)
	}
}

func TestMcpAddEdit_WritesAnOAuthBlock(t *testing.T) {
	out, _, err := mcpAddEditWith(t, "mcp:\n  servers: []\n", "atlassian", url.Values{
		"transport":               {"streamable-http"},
		"url":                     {"https://mcp.atlassian.com/v1/mcp/authv2"},
		"auth_mode":               {"oauth"},
		"auth_scopes":             {"read:jira-work, offline_access"},
		"auth_client_id":          {"1234.5678"},
		"auth_client_secret_from": {"secret://ATLASSIAN_SECRET"},
	})
	if err != nil {
		t.Fatalf("mcpAddEdit: %v", err)
	}
	got := parsedAuth(t, out, "atlassian")
	if got.Mode != mcpauth.ModeOAuth {
		t.Fatalf("mode = %q", got.Mode)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "read:jira-work" {
		t.Errorf("scopes = %v", got.Scopes)
	}
	if got.ClientSecretFrom != "secret://ATLASSIAN_SECRET" {
		t.Errorf("client_secret_from = %q", got.ClientSecretFrom)
	}
}

// TestMcpAddEdit_ValidatesAtTheFormNotAtApply — a proposal carrying an invalid block would pass
// review and then fail the daemon's config load on apply, which is the worst place to find out.
func TestMcpAddEdit_ValidatesAtTheFormNotAtApply(t *testing.T) {
	for _, tc := range []struct {
		name string
		form url.Values
	}{
		{"literal instead of a reference", url.Values{
			"transport": {"streamable-http"}, "url": {"https://x/mcp"},
			"auth_mode": {"static"}, "auth_value_from": {"PLACEHOLDER-not-a-secret-ref"},
		}},
		{"static on stdio", url.Values{
			"transport": {"stdio"}, "command": {"x"},
			"auth_mode": {"static"}, "auth_value_from": {"secret://t"},
		}},
		{"env on a remote transport", url.Values{
			"transport": {"streamable-http"}, "url": {"https://x/mcp"},
			"auth_mode": {"env"}, "auth_env_from": {"A=secret://a"},
		}},
		{"protocol-owned header", url.Values{
			"transport": {"streamable-http"}, "url": {"https://x/mcp"},
			"auth_mode": {"static"}, "auth_header": {"Mcp-Session-Id"}, "auth_value_from": {"secret://t"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := mcpAddEditWith(t, "mcp:\n  servers: []\n", "s", tc.form)
			if !errors.Is(err, errMCPBadAuth) {
				t.Fatalf("err = %v, want errMCPBadAuth", err)
			}
			if mcpErrToken(err) != "mcp-bad-auth" {
				t.Errorf("token = %q", mcpErrToken(err))
			}
		})
	}
}

// TestMcpAddEdit_ErrorNeverEchoesTheRejectedLiteral — the redirect token and the surrounding
// message reach an operator's browser and the daemon log.
func TestMcpAddEdit_ErrorNeverEchoesTheRejectedLiteral(t *testing.T) {
	_, _, err := mcpAddEditWith(t, "mcp:\n  servers: []\n", "s", url.Values{
		"transport": {"streamable-http"}, "url": {"https://x/mcp"},
		"auth_mode": {"static"}, "auth_value_from": {"PLACEHOLDER-not-a-secret-ref"},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "PLACEHOLDER-not-a-secret-ref") {
		t.Errorf("error echoed the credential: %v", err)
	}
}

// TestMcpAddEdit_ModeNoneRemovesTheBlock — turning auth off must actually turn it off; a stale
// block left behind would keep presenting a credential the operator just disabled.
func TestMcpAddEdit_ModeNoneRemovesTheBlock(t *testing.T) {
	const current = `mcp:
  servers:
    - name: n8n
      transport: streamable-http
      url: https://n8n.example.com/mcp/abc
      auth:
        mode: static
        value_from: secret://N8N_TOKEN
`
	out, _, err := mcpAddEditWith(t, current, "n8n", url.Values{
		"transport": {"streamable-http"},
		"url":       {"https://n8n.example.com/mcp/abc"},
		"auth_mode": {"none"},
	})
	if err != nil {
		t.Fatalf("mcpAddEdit: %v", err)
	}
	if strings.Contains(string(out), "N8N_TOKEN") {
		t.Errorf("mode none must remove the auth block:\n%s", out)
	}
}

// TestMcpAddEdit_UnaffectedServersKeepTheirAuth — an edit to one server must not disturb another's
// block.
func TestMcpAddEdit_UnaffectedServersKeepTheirAuth(t *testing.T) {
	const current = `mcp:
  servers:
    - name: n8n
      transport: streamable-http
      url: https://n8n.example.com/mcp/abc
      auth:
        mode: static
        value_from: secret://N8N_TOKEN
    - name: scraper
      transport: sse
      url: http://127.0.0.1:9000/sse
`
	out, summary, err := mcpAddEditWith(t, current, "scraper", url.Values{
		"transport": {"sse"}, "url": {"http://127.0.0.1:9100/sse"},
	})
	if err != nil {
		t.Fatalf("mcpAddEdit: %v", err)
	}
	if !strings.Contains(summary, "scraper") {
		t.Errorf("summary = %q", summary)
	}
	if !strings.Contains(string(out), "secret://N8N_TOKEN") {
		t.Errorf("another server's auth block was lost:\n%s", out)
	}
	if !strings.Contains(string(out), "9100") {
		t.Errorf("the edit did not land:\n%s", out)
	}
}

func TestParseEnvFromLines(t *testing.T) {
	got := parseEnvFromLines("A=secret://a\n\n# comment\n B = secret://b \nMALFORMED")
	if got["A"] != "secret://a" || got["B"] != "secret://b" {
		t.Errorf("parsed = %v", got)
	}
	// A malformed line is KEPT with an empty value so Validate rejects it by
	// name, rather than the form silently dropping what the operator typed.
	if _, ok := got["MALFORMED"]; !ok {
		t.Errorf("a malformed line must survive to be rejected by name: %v", got)
	}
	if len(got) != 3 {
		t.Errorf("comments and blanks must be skipped: %v", got)
	}
}
