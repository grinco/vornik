package configassist

// Doors (design §6.3). The class ceiling is a property of the DOOR: the
// two doors that raise their caller's reach (chat, agent) carry classes A
// and B2 only and never auto-apply.
const (
	DoorREST    = "rest"
	DoorCLI     = "cli"
	DoorConsole = "console"
	DoorChat    = "chat"
	DoorAgent   = "agent"
	DoorSystem  = "system" // the healing initiator (WP9), never a human
)

// IsOperatorDoor reports whether the door grants no new reach (its caller
// could already edit the tree directly).
func IsOperatorDoor(door string) bool {
	switch door {
	case DoorREST, DoorCLI, DoorConsole:
		return true
	}
	return false
}

// MayAutoApply reports whether a door may ever auto-apply (design §6.3.2:
// never from the agent door; §6.3: never from chat).
func MayAutoApply(door string) bool { return IsOperatorDoor(door) || door == DoorSystem }
