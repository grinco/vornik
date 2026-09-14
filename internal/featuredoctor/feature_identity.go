package featuredoctor

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/version"
)

// identityFeature declares the CE identity core: user accounts, groups,
// channel→account bindings (`user_identities`), link codes and key→account
// ownership. Community edition by operator decision (2026-09-13 plan §2);
// federated login (OIDC/SSO) stays Enterprise under auth.providers.
//
// Prereq: the identity tables are present on the ACTIVE store — the
// SQLite branch shipped them only with this feature (plan §2, review R1),
// so a daemon whose storage predates the parity work reports the gap by
// name instead of enabling a resolver over tables that do not exist.
func identityFeature() Feature {
	return Feature{
		ID:      "identity",
		Title:   "User accounts and channel/key→account mapping",
		Summary: "One account resolver for every door: web sessions, Telegram and Slack senders, and API keys map to the same user, so revoking access revokes it everywhere and adaptation data carries a person.",
		LLDRef:  "https://docs.vornik.io",
		DocRef:  "docs/public/features/identity.md",
		Edition: version.EditionCommunity,
		Apply:   ReloadHot,
		Gates:   []Gate{{Key: "identity.enabled", EnableTo: true}},
		Prereqs: []Prereq{
			{
				Name:  "identity tables present on the active store",
				Check: checkIdentityTablesPresent,
			},
		},
		Verify: func(ctx context.Context, d Deps) PrereqResult {
			r := checkIdentityTablesPresent(ctx, d)
			if !r.OK {
				return r
			}
			return PrereqResult{OK: true, Detail: "identity core resolves — " + r.Detail}
		},
	}
}

// checkIdentityTablesPresent proves the identity tables exist on the
// active store by running the cheapest read the repository offers
// (ListUsers). A nil repository is "not wired on this backend"; a query
// error is "tables missing or unreadable". Both are unfixable by the
// doctor — the operator upgrades the binary or migrates the store.
func checkIdentityTablesPresent(ctx context.Context, d Deps) PrereqResult {
	if d.Identity == nil {
		return PrereqResult{OK: false, Fixable: false,
			Detail:      "identity repository not wired on the active storage backend",
			Remediation: "upgrade to a build that ships the identity tables on this backend (SQLite parity landed with the 2026-09-13 identity work), then restart"}
	}
	users, err := d.Identity.ListUsers(ctx)
	if err != nil {
		return PrereqResult{OK: false, Fixable: false,
			Detail:      "identity tables unreadable: " + err.Error(),
			Remediation: "run the daemon once so migrations apply (users, groups, group_projects, group_members, user_identities, link_codes), then re-run the doctor"}
	}
	return PrereqResult{OK: true, Detail: fmt.Sprintf("identity tables present (%d account(s))", len(users))}
}
