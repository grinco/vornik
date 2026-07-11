package integrations

import (
	"reflect"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

func TestRegistry_HasFourKinds(t *testing.T) {
	kinds := Registry(DialGuard{})
	if len(kinds) != 4 {
		t.Fatalf("Registry() returned %d kinds, want 4", len(kinds))
	}
	seen := make(map[string]bool)
	for _, k := range kinds {
		seen[k.ID] = true
		if k.Prober == nil {
			t.Errorf("kind %q has a nil Prober", k.ID)
		}
		if k.Prober.Kind() != k.ID {
			t.Errorf("kind %q: Prober.Kind() = %q, mismatch", k.ID, k.Prober.Kind())
		}
		if k.DisplayName == "" {
			t.Errorf("kind %q: DisplayName is empty", k.ID)
		}
		if k.DocURL == "" {
			t.Errorf("kind %q: DocURL is empty", k.ID)
		}
		if len(k.Fields) == 0 {
			t.Errorf("kind %q: Fields is empty", k.ID)
		}
		for _, f := range k.Fields {
			if f.Key == "" {
				t.Errorf("kind %q: a CredentialField has an empty Key", k.ID)
			}
			if f.Secret && !f.SecretFile && f.EnvName == "" {
				t.Errorf("kind %q field %q: Secret fields must carry an EnvName unless SecretFile is set", k.ID, f.Key)
			}
			if f.SecretFile && !f.Secret {
				t.Errorf("kind %q field %q: SecretFile without Secret makes no sense", k.ID, f.Key)
			}
		}
	}
	for _, want := range []string{"telegram", "email", "github_app", "slack"} {
		if !seen[want] {
			t.Errorf("Registry() missing kind %q", want)
		}
	}
	// Regression (2026-07-10 MCP-kind removal): MCP tool servers are managed
	// on the control-plane hub's MCP tab, not the Integrations Hub — the hub
	// kind was an inferior duplicate (direct write bypassing the proposal
	// ledger; see catalog.go's Registry doc). It must not reappear here.
	if seen["mcp"] {
		t.Errorf("Registry() contains kind \"mcp\" — MCP management belongs to the control-plane hub, not the Integrations Hub")
	}
}

func TestRegistry_ScopeAssignment(t *testing.T) {
	wantScope := map[string]Scope{
		"telegram":   ScopeDaemon,
		"email":      ScopeProject,
		"github_app": ScopeProject,
		"slack":      ScopeProject,
	}
	for _, k := range Registry(DialGuard{}) {
		if want, ok := wantScope[k.ID]; ok && k.Scope != want {
			t.Errorf("kind %q: Scope = %q, want %q", k.ID, k.Scope, want)
		}
	}
}

func TestRegistry_DefaultProbeTimeoutIsZero(t *testing.T) {
	// v1 ships ProbeTimeout=0 (use the package default) and
	// MinProbeInterval=0 (no per-kind throttle) for every kind (design §5.1).
	for _, k := range Registry(DialGuard{}) {
		if k.MinProbeInterval != 0 {
			t.Errorf("kind %q: MinProbeInterval = %v, want 0 in v1", k.ID, k.MinProbeInterval)
		}
	}
}

func TestRegistry_TelegramField(t *testing.T) {
	for _, k := range Registry(DialGuard{}) {
		if k.ID != "telegram" {
			continue
		}
		if len(k.Fields) != 1 || k.Fields[0].Key != "bot_token" {
			t.Fatalf("telegram Fields = %+v, want exactly [bot_token]", k.Fields)
		}
		if !k.Fields[0].Secret || k.Fields[0].EnvName != "TELEGRAM_BOT_TOKEN" {
			t.Errorf("telegram bot_token field = %+v, want Secret with EnvName TELEGRAM_BOT_TOKEN", k.Fields[0])
		}
		return
	}
	t.Fatal("telegram kind not found")
}

// realYAMLTopLevelKeys returns the set of top-level yaml keys declared on
// v's struct type (v must be a non-pointer struct value), reading the
// "yaml" struct tag exactly the way registry.Project's fields do.
func realYAMLTopLevelKeys(v any) map[string]bool {
	keys := make(map[string]bool)
	t := reflect.TypeOf(v)
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if tag == "" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}

// TestCatalog_ProjectScopeFieldsResolveAgainstRegistrySchema is task 5.2b's
// TDD anchor for the catalog reconciliation (step 1): for each of the three
// project-scope kinds, every CredentialField.Key must have a SaveTarget
// ScalarKeys (or SecretFilePaths) entry whose dotted config key is scoped
// under the kind's real registry.Project* block AND whose leaf segment is
// an actual yaml-tagged field on that struct. A typo'd or stale Key (like
// 5.1's original "username"/"password"/"private_key" fields) makes this
// test fail.
func TestCatalog_ProjectScopeFieldsResolveAgainstRegistrySchema(t *testing.T) {
	cases := []struct {
		kindID     string
		prefix     string
		structKeys map[string]bool
	}{
		{"email", "email.", realYAMLTopLevelKeys(registry.ProjectEmail{})},
		{"slack", "slack.", realYAMLTopLevelKeys(registry.ProjectSlack{})},
		{"github_app", "github_app.", realYAMLTopLevelKeys(registry.ProjectGitHubApp{})},
	}
	reg := Registry(DialGuard{})
	for _, tc := range cases {
		t.Run(tc.kindID, func(t *testing.T) {
			var kind IntegrationKind
			var found bool
			for _, k := range reg {
				if k.ID == tc.kindID {
					kind, found = k, true
					break
				}
			}
			if !found {
				t.Fatalf("kind %q not found in Registry()", tc.kindID)
			}
			target, ok := SaveTargetForKind(tc.kindID)
			if !ok {
				t.Fatalf("SaveTargetForKind(%q) = false, want true", tc.kindID)
			}
			for _, f := range kind.Fields {
				var dotted string
				switch {
				case f.SecretFile:
					if _, ok := target.SecretFilePaths[f.Key]; !ok {
						t.Errorf("field %q: SecretFile but no SecretFilePaths entry", f.Key)
						continue
					}
					// SecretFile fields still declare a ScalarKeys entry
					// naming the real config key their resolved path is
					// written to (github_app.private_key_path) — verified
					// the same way as any other field below.
					var ok2 bool
					dotted, ok2 = target.ScalarKeys[f.Key]
					if !ok2 {
						t.Errorf("field %q: SecretFile field has no ScalarKeys entry naming its config key", f.Key)
						continue
					}
				default:
					var ok2 bool
					dotted, ok2 = target.ScalarKeys[f.Key]
					if !ok2 {
						t.Errorf("field %q: no ScalarKeys entry in SaveTargetForKind(%q)", f.Key, tc.kindID)
						continue
					}
				}
				if !strings.HasPrefix(dotted, tc.prefix) {
					t.Errorf("field %q maps to %q, want it scoped under %q", f.Key, dotted, tc.prefix)
					continue
				}
				leaf := strings.TrimPrefix(dotted, tc.prefix)
				if !tc.structKeys[leaf] {
					t.Errorf("field %q maps to %q, but %q is not a real yaml key on the registry struct (known keys: %v)", f.Key, dotted, leaf, tc.structKeys)
				}
			}
		})
	}
}

// TestCatalog_EmailFieldsSatisfyEnabled proves the reconciled email Fields
// are SUFFICIENT to make registry.ProjectEmail.Enabled() true once saved —
// not just individually-valid keys, but a set that actually activates the
// channel (5.1's original bug: individually-plausible fields that never
// added up to a working config).
func TestCatalog_EmailFieldsSatisfyEnabled(t *testing.T) {
	p := registry.ProjectEmail{
		IMAPHost:        "imap.example.com",
		IMAPUsername:    "user@example.com",
		IMAPPasswordEnv: "EMAIL_IMAP_PASSWORD_PROJ",
	}
	if !p.Enabled() {
		t.Fatal("sanity: this fixture should satisfy ProjectEmail.Enabled()")
	}
	kind := findKind(t, "email")
	haveKeys := fieldKeySet(kind)
	for _, want := range []string{"imap_host", "imap_username", "imap_password_env"} {
		if !haveKeys[want] {
			t.Errorf("email catalog Fields missing %q, needed for ProjectEmail.Enabled()", want)
		}
	}
}

// TestCatalog_SlackFieldsSatisfyEnabled mirrors the email test for Slack:
// ProjectSlack.Enabled() requires team_id + signing_secret_env, both absent
// from 5.1's original catalog.
func TestCatalog_SlackFieldsSatisfyEnabled(t *testing.T) {
	p := registry.ProjectSlack{TeamID: "T123", SigningSecretEnv: "SLACK_SIGNING_SECRET_PROJ"}
	if !p.Enabled() {
		t.Fatal("sanity: this fixture should satisfy ProjectSlack.Enabled()")
	}
	kind := findKind(t, "slack")
	haveKeys := fieldKeySet(kind)
	for _, want := range []string{"team_id", "signing_secret_env"} {
		if !haveKeys[want] {
			t.Errorf("slack catalog Fields missing %q, needed for ProjectSlack.Enabled()", want)
		}
	}
}

// TestCatalog_GitHubAppFieldsSatisfyEnabled mirrors the above for GitHub
// App: Enabled() requires webhook_secret_env + a non-empty repo_allowlist,
// both absent from 5.1's original catalog; PrivateKeyPath is file-based.
func TestCatalog_GitHubAppFieldsSatisfyEnabled(t *testing.T) {
	p := registry.ProjectGitHubApp{
		WebhookSecretEnv: "GITHUB_APP_WEBHOOK_SECRET_PROJ",
		RepoAllowlist:    []string{"myorg/myrepo"},
	}
	if !p.Enabled() {
		t.Fatal("sanity: this fixture should satisfy ProjectGitHubApp.Enabled()")
	}
	kind := findKind(t, "github_app")
	haveKeys := fieldKeySet(kind)
	for _, want := range []string{"webhook_secret_env", "repo_allowlist", "private_key_path"} {
		if !haveKeys[want] {
			t.Errorf("github_app catalog Fields missing %q", want)
		}
	}
}

func findKind(t *testing.T, id string) IntegrationKind {
	t.Helper()
	for _, k := range Registry(DialGuard{}) {
		if k.ID == id {
			return k
		}
	}
	t.Fatalf("kind %q not found in Registry()", id)
	return IntegrationKind{}
}

func fieldKeySet(k IntegrationKind) map[string]bool {
	m := make(map[string]bool, len(k.Fields))
	for _, f := range k.Fields {
		m[f.Key] = true
	}
	return m
}

func TestRegistry_ProbersAreIndependentInstances(t *testing.T) {
	// Registry is called fresh per invocation (e.g. once per request in the
	// eventual HTTP handler) — verify it doesn't panic or share broken
	// state across two calls.
	a := Registry(DialGuard{})
	b := Registry(DialGuard{AllowedHosts: []string{"internal.example.com"}})
	if len(a) != len(b) {
		t.Fatalf("Registry() call count mismatch: %d vs %d", len(a), len(b))
	}
}
