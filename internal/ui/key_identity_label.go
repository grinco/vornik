package ui

import "context"

// Naming an api_key identity, shared by the two surfaces that render one:
// the operator accounts page (/ui/operator/accounts) and the personal page
// (/ui/account). They resolve the name from different directions — a listing
// the accounts page already holds, a per-key lookup on the personal page —
// but they must agree on WHEN a stored display is worth showing and on what
// the resulting label looks like. Two copies of that judgement is how one
// page came to show "slava/codex" while the other showed the raw id.
//
// oidc-identity-permissions-design.md §5.4a records the rule.

// keyDisplayIsOpaque reports whether a stored `user_identities.display` tells
// a reader anything the row does not already say.
//
// Until 2026-09-16 authz.keyIdentity stamped the KEY ID into that column, so
// most rows in an existing database carry a display that is a verbatim copy
// of external_id. The write is fixed at the seam, but those rows are still
// there and no migration rewrites them — resolving the name at read time is
// what §5.4 asks for anyway, and it is also what makes a renamed key show its
// new name rather than the one it had when it was claimed.
func keyDisplayIsOpaque(display, externalID string) bool {
	return display == "" || display == externalID
}

// keyIdentityLabel renders a key the way the operator who minted it knows it:
// "project / name", the same pairing the attribution picker offers, because
// names are only unique within a project.
func keyIdentityLabel(name, project string, revoked bool) string {
	if name == "" {
		name = "(unnamed)"
	}
	label := name
	if project != "" {
		label = project + " / " + name
	}
	if revoked {
		// A revoked key still holds attribution for spend already incurred,
		// so it stays listed — but it must not be mistaken for a live one.
		label += " · revoked"
	}
	return label
}

// nameKeyIdentity resolves one api_key binding to its operator-facing label,
// returning "" when it cannot be named.
//
// Every failure — no key repository wired (a Community box without one), a key
// deleted since it was claimed, a listing error — returns "" so the caller
// falls back to the id. A binding that cannot be named must still be SHOWN:
// §5.5 makes /ui/account the one place a person sees every identity that
// resolves to them, and silently dropping one there is worse than an ugly row.
func (s *Server) nameKeyIdentity(ctx context.Context, keyID string) string {
	if s == nil || s.apiKeyRepo == nil || keyID == "" {
		return ""
	}
	k, err := s.apiKeyRepo.GetByID(ctx, keyID)
	if err != nil || k == nil {
		return ""
	}
	return keyIdentityLabel(k.Name, k.ProjectID, k.RevokedAt != nil)
}
