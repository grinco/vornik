package projectdoctor

import (
	"os"

	"vornik.io/vornik/internal/onboarding"
)

// envSecretsFile is the single env file the doctor's inline secret
// fix writes into. Kept separate from setup's chat.env so the
// per-project secrets the operator supplies through the doctor are
// grouped and obvious. The daemon loads *.env from its secrets dir
// at boot; os.Setenv (below) makes a freshly-set value live without
// waiting for a restart.
const envSecretsFile = "project-secrets.env"

// EnvSecrets reads secret presence from the process environment and
// writes new secrets both live (os.Setenv) and durably (env file).
type EnvSecrets struct {
	Dir string // secrets directory, e.g. ~/.config/vornik/secrets
}

// NewEnvSecrets builds an EnvSecrets over the given secrets dir.
func NewEnvSecrets(dir string) EnvSecrets { return EnvSecrets{Dir: dir} }

// Has reports whether the named secret is present (non-empty) in the
// running process environment — the authoritative source, since the
// daemon loads env files at boot and Set updates os.Setenv live.
func (e EnvSecrets) Has(name string) bool {
	return os.Getenv(name) != ""
}

// Set makes the secret live immediately (os.Setenv, so the doctor's
// Has check flips green with no restart) AND persists it to the env
// file for the next boot. Subsystems that captured env at startup
// (e.g. an already-connected MCP server) pick the new value up on
// their next use / re-sync, not retroactively.
func (e EnvSecrets) Set(name, value string) error {
	if _, err := onboarding.WriteEnvSecret(e.Dir, envSecretsFile, name, value); err != nil {
		return err
	}
	return os.Setenv(name, value)
}
