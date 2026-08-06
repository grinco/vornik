package mcp

import "encoding/json"

// daemonSuppliedArg names an argument the DAEMON fills in on every tool call,
// overriding whatever the model sent.
//
// project_id is the caller's identity. An MCP server uses it for context
// isolation, concurrency accounting and per-project browser profiles (the
// scraper does all three), which makes it exactly the kind of value a model
// must not be able to choose: a model that named another project's id would be
// charged against that project's limits and could reach its profile. The daemon
// knows the real one authoritatively at call time.
//
// Leaving it in the advertised schema was also a plain usability defect. It is
// declared `required`, and nothing tells a chat model what to put there, so a
// chat-initiated fetch failed validation outright — observed 2026-08-05 on a
// "review this PR" request, which came back as
// `path: ["project_id"] Required` and looked to the assistant like its own
// mistake. Agent roles only worked because their swarm prompts spell the value
// out by hand, which is a workaround for this, not a design.
const daemonSuppliedArg = "project_id"

// hideDaemonSuppliedArgs removes the daemon-supplied argument from an advertised
// input schema — from `properties` and from `required` — so the model is neither
// asked for a value it cannot know nor able to offer one. Returns ok=false when
// the schema does not mention it, so the common case keeps the server's exact
// bytes rather than a re-marshalled copy.
//
// Deliberately shallow: it does not descend into anyOf/allOf/$defs. A server
// that hides project_id inside a composed sub-schema keeps it, which fails
// visibly at call time rather than silently dropping a constraint we did not
// understand.
func hideDaemonSuppliedArgs(schema json.RawMessage) (json.RawMessage, bool) {
	var doc map[string]any
	if err := json.Unmarshal(schema, &doc); err != nil {
		return schema, false // not an object schema; leave it alone
	}
	touched := false

	if props, ok := doc["properties"].(map[string]any); ok {
		if _, present := props[daemonSuppliedArg]; present {
			delete(props, daemonSuppliedArg)
			touched = true
		}
	}
	if req, ok := doc["required"].([]any); ok {
		kept := make([]any, 0, len(req))
		for _, r := range req {
			if s, isStr := r.(string); isStr && s == daemonSuppliedArg {
				touched = true
				continue
			}
			kept = append(kept, r)
		}
		if touched {
			if len(kept) == 0 {
				delete(doc, "required")
			} else {
				doc["required"] = kept
			}
		}
	}
	if !touched {
		return schema, false
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return schema, false
	}
	return out, true
}

// injectDaemonSuppliedArgs sets the daemon-supplied argument on an outgoing
// call, overwriting any value already present.
//
// Applied to EVERY call rather than only to tools whose schema declares the
// argument: a server that ignores an unknown key is harmless, whereas deciding
// per-tool would mean re-reading the schema on the hot path and would silently
// skip a server whose tools/list we failed to parse. Overwriting rather than
// defaulting is the security-relevant half — a model that supplies another
// project's id must not be believed.
func injectDaemonSuppliedArgs(argsJSON, projectID string) string {
	if projectID == "" {
		return argsJSON
	}
	var args map[string]any
	if argsJSON == "" {
		args = map[string]any{}
	} else if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args == nil {
		// Not a JSON object (a server taking a bare array or a malformed
		// argument string). Pass it through untouched and let the server
		// reject it — rewriting an argument shape we do not understand would
		// turn a legible validation error into a confusing one.
		return argsJSON
	}
	args[daemonSuppliedArg] = projectID
	out, err := json.Marshal(args)
	if err != nil {
		return argsJSON
	}
	return string(out)
}
