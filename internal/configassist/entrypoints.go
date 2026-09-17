package configassist

// Entrypoints (design §6.3). The class ceiling is a property of the
// ENTRYPOINT: the two that raise their caller's reach (chat, agent) carry
// classes A and B2 only and never auto-apply.
//
// Named "entrypoint" rather than "door" by the operator's decision
// 2026-09-15. The values are unchanged and are what the ledger stores.
const (
	EntrypointREST    = "rest"
	EntrypointCLI     = "cli"
	EntrypointConsole = "console"
	EntrypointChat    = "chat"
	EntrypointAgent   = "agent"
	EntrypointSystem  = "system" // the healing initiator (WP9), never a human
)

// IsOperatorEntrypoint reports whether the entrypoint grants no new reach (its caller
// could already edit the tree directly).
func IsOperatorEntrypoint(entrypoint string) bool {
	switch entrypoint {
	case EntrypointREST, EntrypointCLI, EntrypointConsole:
		return true
	}
	return false
}

// MayAutoApply reports whether an entrypoint may ever auto-apply (design
// §6.3.2: never from the agent entrypoint; §6.3: never from chat).
func MayAutoApply(entrypoint string) bool {
	return IsOperatorEntrypoint(entrypoint) || entrypoint == EntrypointSystem
}
