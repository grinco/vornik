// Package agentloop is the Go side of the agent container's loop, rebuilt one
// surface at a time out of images/vornik-agent/entrypoint.sh in the order the
// backlog fixed: tool dispatch first, then prompt assembly, the gates, and the
// loop control flow. Design of record:
// https://docs.vornik.io
//
// The package is a leaf by policy — stdlib plus internal/agenttools only —
// because it is compiled into vornik-agent-helper, which ships inside the
// agent image; daemon code must not ride along. TestImportsAreLeaf pins it.
//
// Dispatch is reached from exec_tool in the entrypoint AFTER the allowlist
// gate; nothing here consults the allowlist, and nothing here should. The one
// refusal this package owns is a name it does not implement.
package agentloop

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"vornik.io/vornik/internal/agenttools"
)

// Env is what a tool may know about where it runs. The entrypoint exports
// WORKSPACE; nothing else is read from the environment, so a tool's behaviour
// is a function of (Env, name, args) and the tests construct Env directly.
type Env struct {
	Workspace string
	// Now is the clock; nil means time.Now. Injected so current_time is
	// deterministic under test.
	Now func() time.Time
}

// Handler implements one tool: it receives the decoded arguments and returns
// what the model sees. A tool error is text ("ERROR: …"), never a Go error —
// the loop's contract is that the model reads the message.
type Handler func(env Env, args json.RawMessage) string

// Handlers is the dispatch table: exactly the tools declared
// agenttools.RuntimeHelper, which contractreg holds equal in both directions.
var Handlers = map[string]Handler{}

// HandlerNames returns the implemented tool names, sorted.
func HandlerNames() []string {
	out := make([]string, 0, len(Handlers))
	for n := range Handlers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Dispatch runs the named tool. A name that is not declared RuntimeHelper is
// refused here as a belt: the bash branch cannot send one, and this cannot
// run one the bash branch did not send.
func Dispatch(env Env, name string, args json.RawMessage) string {
	if !agenttools.RunsInHelper(name) {
		return fmt.Sprintf("ERROR: tool '%s' does not run in the helper", name)
	}
	h, ok := Handlers[name]
	if !ok {
		return fmt.Sprintf("ERROR: tool '%s' is declared for the helper but has no handler", name)
	}
	// An empty workspace would anchor every path at "." and report a valid
	// relative path as an escape (2026-09-05: WORKSPACE was assigned but not
	// exported by the entrypoint). Refuse by name so the failure says what is
	// wrong instead of sending the model to retry a path that was fine.
	if env.Workspace == "" {
		return "ERROR: WORKSPACE is not set in the helper's environment — the entrypoint must export it; refusing to resolve paths against the working directory"
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if !json.Valid(args) {
		return "ERROR: arguments are not valid JSON"
	}
	return h(env, args)
}
