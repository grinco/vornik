// Package configassist is the configuration and troubleshooting assistant
// in the control plane (https://docs.vornik.io
// design.md, GREEN round 6; review amendments R6–R8 normative). It turns a
// natural-language intent into a reviewable, rollbackable proposal against
// the deployed config tree by running a file-editing agent loop IN-PROCESS
// over a sanitized per-request snapshot — never a container, never a
// shell, never the live tree.
//
// The security claim this package makes, in its honest form (design §2.0):
//
//	The eleven helper tools minus the four git tools do not execute a
//	program; the assistant may call only those seven; and nothing may be
//	added to that seven without a test failing.
//
// envelope.go is that claim as code. Everything else in the package sits
// behind it.
package configassist

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/agentloop"
	"vornik.io/vornik/internal/agenttools"
	"vornik.io/vornik/internal/chat"
)

// Verdict is one tool's classification in the envelope.
type Verdict int

const (
	// Out — the tool is refused by the envelope. It is listed so that a
	// refusal is a decision recorded as data, never an omission.
	Out Verdict = iota
	// In — the tool may be called by the assistant's loop.
	In
)

// Classification is one row of the envelope: a tool the agentloop
// dispatcher implements, IN or OUT, with the reason. Every tool
// agentloop.HandlerNames() declares MUST have a row (test 12); a new
// upstream tool is unclassified until someone classifies it, and
// unclassified fails the test rather than widening this surface.
type Classification struct {
	Name    string
	Verdict Verdict
	Reason  string
}

// envelope is the deny-by-default enumeration (design §2.0).
var envelope = []Classification{
	{"file_read", In, "reads one file under the sanitized snapshot root; no execution"},
	{"file_write", In, "writes into the per-request overlay; the walk of §3.1 turns it into create|replace ops"},
	{"file_edit", In, "exact-string edit into the overlay; same boundary as file_write"},
	{"read_many_files", In, "bulk read under the snapshot root; capped by the helper"},
	{"grep", In, "regex search under the snapshot root; no execution (Go implementation, not a subprocess)"},
	{"glob", In, "path enumeration under the snapshot root"},
	{"current_time", In, "clock read; deterministic under test"},
	{"git_status", Out, "spawns git (the one subprocess agentloop permits); config editing needs no history — the apply engine owns git"},
	{"git_diff", Out, "spawns git; the apply engine renders the human diff from the op bundle"},
	{"git_log", Out, "spawns git; out of envelope by the 2026-08-03 no-exec ruling"},
	{"git_show", Out, "spawns git; out of envelope by the 2026-08-03 no-exec ruling"},
}

// Classify returns the classification for a tool name and whether one
// exists. An unknown name is (zero, false) — the caller must treat it as
// OUT.
func Classify(name string) (Classification, bool) {
	for _, c := range envelope {
		if c.Name == name {
			return c, true
		}
	}
	return Classification{}, false
}

// Allowed reports whether the assistant's loop may call the tool.
func Allowed(name string) bool {
	c, ok := Classify(name)
	return ok && c.Verdict == In
}

// AllowedNames returns the IN set, sorted.
func AllowedNames() []string {
	var out []string
	for _, c := range envelope {
		if c.Verdict == In {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Unclassified returns every tool agentloop implements that the envelope
// does not classify. Non-empty is a test failure (test 12): a tool added
// upstream is OUT by default and, more importantly, LOUD.
func Unclassified() []string {
	var out []string
	for _, n := range agentloop.HandlerNames() {
		if _, ok := Classify(n); !ok {
			out = append(out, n)
		}
	}
	return out
}

// ErrToolOutOfEnvelope is the refusal text prefix a tool call outside the
// envelope produces. Text, not a Go error: the loop's contract is that the
// model reads the message (agentloop's Handler contract).
const ErrToolOutOfEnvelope = "ERROR: tool is outside the configuration assistant's envelope"

// Dispatch runs a tool for the assistant's loop. It refuses, by name,
// anything not IN before agentloop sees it — the caller-side allowlist
// design §2.0 describes. env.Workspace is the per-request snapshot root.
func Dispatch(env agentloop.Env, name string, args json.RawMessage) string {
	c, ok := Classify(name)
	if !ok {
		return fmt.Sprintf("%s: %q is not classified (OUT by default)", ErrToolOutOfEnvelope, name)
	}
	if c.Verdict != In {
		return fmt.Sprintf("%s: %q — %s", ErrToolOutOfEnvelope, name, c.Reason)
	}
	return agentloop.Dispatch(env, name, args)
}

// Tools materialises the chat.Tool definitions for the IN set, taking each
// schema from agenttools (the same JSON the container agent sees) and
// rewriting the path wording: the assistant's root is the config snapshot,
// not /app/workspace, and run_shell does not exist here.
func Tools() []chat.Tool {
	names := AllowedNames()
	out := make([]chat.Tool, 0, len(names))
	for _, n := range names {
		t := agenttools.Get(n)
		if t == nil || len(t.Schema) == 0 {
			continue
		}
		var def struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(t.Schema, &def); err != nil {
			continue
		}
		desc := rewritePathWording(def.Function.Description)
		params := rewritePathWording(string(def.Function.Parameters))
		out = append(out, chat.Tool{Type: "function", Function: chat.ToolFunction{
			Name: def.Function.Name, Description: desc, Parameters: json.RawMessage(params),
		}})
	}
	return out
}

// rewritePathWording replaces the container-workspace phrasing in the
// shared tool schemas with the assistant's own root. Purely descriptive —
// the parameters' shape is untouched, so agentloop decodes them unchanged.
func rewritePathWording(s string) string {
	r := strings.NewReplacer(
		"/app/workspace/ (the working directory); the persistent project folder is at project/ (e.g. 'project/src/main.py')", "the configuration root (projects/, swarms/, workflows/, role-library/)",
		"Relative path from /app/workspace/. Use 'project/' prefix for the persistent shared project folder.", "Path relative to the configuration root, e.g. projects/assistant.yaml or workflows/digest.md.",
		"Relative path from /app/workspace/.", "Path relative to the configuration root.",
		"Paths are relative to /app/workspace/ — use 'project/' prefix for the persistent project folder.", "Paths are relative to the configuration root.",
		"/app/workspace/", "the configuration root/",
		"/app/workspace", "the configuration root",
		", run_shell with `head -c 200000` for a bigger window (200KB),", ",",
		"Faster and more token-efficient than run_shell 'grep -r'. ", "",
		"Faster than run_shell 'find'. ", "",
		"Default: workspace root.", "Default: the configuration root.",
	)
	return r.Replace(s)
}
