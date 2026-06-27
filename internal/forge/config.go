package forge

import (
	"fmt"
	"sort"
	"strings"
)

// Provider discriminator values. Defined here (not scattered as string literals)
// so config, factory, and impls agree on one spelling.
const (
	ProviderGitHub = "github"
	ProviderGitLab = "gitlab"
	ProviderGitea  = "gitea"
)

// Config is the provider-discriminated configuration for a project's forge.
// Exactly one provider's credential block is populated, selected by Provider.
// New future providers add a field here and a Register call in their impl
// package — no change to callers.
type Config struct {
	Provider string
	GitHub   GitHubConfig
	// GitLab GitLabConfig // future sibling
	// Gitea  GiteaConfig  // future sibling
}

// GitHubConfig carries the GitHub App credentials. The private key is read from
// PrivateKeyPath at mint time and never leaves the daemon process.
type GitHubConfig struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPath string
	APIBaseURL     string // GitHub Enterprise override; empty → api.github.com
}

// Constructor builds one provider's ForgeProvider from a Config. Impl packages
// register a Constructor under their provider name in init(), so the core
// package never imports the impls (avoids the core→impl cycle: impls import the
// core for the interface).
type Constructor func(Config) (ForgeProvider, error)

var registry = map[string]Constructor{}

// Register wires a provider constructor under name. Called from an impl package's
// init(); last write wins. Not safe for concurrent calls, which is fine: all
// registration happens at package-init time before New is reachable.
func Register(name string, c Constructor) {
	registry[strings.TrimSpace(name)] = c
}

// New builds the ForgeProvider for cfg.Provider, already bound to its
// credentials. A missing or unknown provider is a clear error, never a nil
// provider — so a typo'd or unwired config fails loudly at wire time.
func New(cfg Config) (ForgeProvider, error) {
	name := strings.TrimSpace(cfg.Provider)
	if name == "" {
		return nil, fmt.Errorf("forge: no provider configured")
	}
	c, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("forge: unknown provider %q (known: %s)", name, strings.Join(known(), ", "))
	}
	return c(cfg)
}

// known returns the registered provider names, sorted, for error messages.
func known() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
