package api

import (
	"fmt"

	"vornik.io/vornik/internal/secrethygiene"
)

// checkConfigSecretHygiene flags two operational hazards in the main
// config.yaml:
//
//  1. Plaintext secret-bearing fields that were NOT loaded from an
//     environment variable. The config loader already runs
//     os.ExpandEnv over string fields, so an operator who uses
//     ${DB_PASSWORD} gets env substitution. But operators frequently
//     paste the real password in during setup and forget to rotate
//     to ${VAR}. We can't distinguish "literal" from "expanded" after
//     the fact — os.ExpandEnv is destructive — so this check
//     fingerprints obvious placeholders and flags anything that looks
//     like a raw secret.
//  2. config.yaml world- or group-readable (mode > 0640). Any process
//     running as another local user can scrape the file.
//
// Complements secrets_permissions (which scans ~/.config/vornik/secrets
// for key material files); this one focuses on vornik's own config.
//
// The evaluation itself lives in internal/secrethygiene (extracted
// 2026-09-13, config-assistant design §7.3) because the configuration
// assistant turns the same finding into a refusal before any model call
// — one heuristic, two verdict surfaces. This method only renders the
// DoctorCheck; every message is the one it has always emitted.
func (h *DoctorHandlers) checkConfigSecretHygiene() DoctorCheck {
	const name = "config_secret_hygiene"
	findings, evaluable := secrethygiene.Evaluate(h.configPath, h.secretFields)
	if !evaluable {
		// Container hasn't wired the snapshot — happens in direct
		// handler tests. Skip quietly rather than emit a false-positive.
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no config snapshot captured, skipping"}
	}

	var items []string
	for _, f := range findings {
		items = append(items, f.Detail)
	}

	if len(items) == 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: "config.yaml permissions tight; sensitive fields sourced from env or empty",
		}
	}
	return DoctorCheck{
		Name:    name,
		Status:  "WARNING",
		Message: fmt.Sprintf("%d config secret-hygiene finding(s)", len(items)),
		Items:   items,
	}
}
