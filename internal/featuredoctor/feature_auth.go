package featuredoctor

import (
	"context"
	"os"
	"path/filepath"

	"vornik.io/vornik/internal/version"
)

func authFeature() Feature {
	return Feature{
		ID:      "auth",
		Title:   "API authentication",
		Summary: "Per-project API keys + admin-key gating on control-plane routes.",
		LLDRef:  "https://docs.vornik.io",
		DocRef:  "docs/public/features/auth.md",
		Edition: version.EditionCommunity,
		Apply:   RestartRequired,
		Gates:   []Gate{{Key: "api.auth_enabled", EnableTo: true}},
		Prereqs: []Prereq{
			{
				Name: "admin key present",
				Check: func(ctx context.Context, d Deps) PrereqResult {
					path := filepath.Join(d.SecretsDir, "admin-key.txt")
					if _, err := os.Stat(path); err == nil {
						return PrereqResult{OK: true, Detail: "admin-key.txt present"}
					}
					return PrereqResult{OK: false, Fixable: false,
						Detail:      "admin-key.txt missing",
						Remediation: "create " + path + " with a VORNIK_ADMIN_KEY= line before enabling auth, or you will lock yourself out of admin routes"}
				},
			},
		},
		Verify: func(ctx context.Context, d Deps) PrereqResult {
			// Design note: a fuller verify (a key-authed request succeeds /
			// admin lists non-empty) is wired when the API client is
			// available to the daemon-side check; until then, confirm the
			// gate+secret are coherent.
			path := filepath.Join(d.SecretsDir, "admin-key.txt")
			if _, err := os.Stat(path); err != nil {
				return PrereqResult{OK: false, Detail: "auth on but admin-key.txt missing — admin routes unreachable"}
			}
			return PrereqResult{OK: true, Detail: "auth gate + admin key coherent"}
		},
	}
}
