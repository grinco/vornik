package configassist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"vornik.io/vornik/internal/agentloop"
	"vornik.io/vornik/internal/chat"
)

// LoopConfig bounds one request's editing loop (review R8: deadline,
// tool-turn and byte ceilings; design §8.2 MaxOutputBytes).
type LoopConfig struct {
	MaxToolTurns   int
	MaxOutputBytes int
	Deadline       time.Duration
	// Extra are engine-owned tools outside the agentloop envelope (the
	// consult tool): advertised alongside the seven and dispatched here,
	// never through agentloop. Each is a network call, not a subprocess.
	Extra []ExtraTool
}

// ExtraTool is one engine-owned tool: its definition and handler.
type ExtraTool struct {
	Def    chat.Tool
	Handle func(ctx context.Context, args json.RawMessage) string
}

// ToolCallRecord is one tool call the loop made — the machine-readable
// trace an agent-door proposal carries (design test 22) and the audit of
// every request.
type ToolCallRecord struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

// LoopResult is what the loop produced.
type LoopResult struct {
	Declared         []string
	Summary          string
	Turns            int
	BytesWritten     int
	PromptTokens     int
	CompletionTokens int
	Model            string
	ToolCalls        []ToolCallRecord
	// Transcript is retained ONLY for the judge's exclusion test: the
	// judge is never given it (design test 9).
	transcript []chat.Message
}

var planRe = regexp.MustCompile(`(?m)^\s*PLAN:\s*(\[.*?\])`)

// ParsePlan extracts the declared file list from an assistant message.
func ParsePlan(text string) ([]string, bool) {
	m := planRe.FindStringSubmatch(text)
	if m == nil {
		return nil, false
	}
	var files []string
	if err := json.Unmarshal([]byte(m[1]), &files); err != nil {
		return nil, false
	}
	out := files[:0]
	for _, f := range files {
		f = strings.TrimSpace(strings.TrimPrefix(f, "./"))
		if f != "" {
			out = append(out, f)
		}
	}
	return out, true
}

// RunLoop drives the assistant's model over the envelope's tools against
// the snapshot root until it stops calling tools, hits a ceiling, or the
// deadline passes. It never touches the deployed tree: env.Workspace is
// the snapshot.
//
//nolint:gocognit,funlen // The model/tool loop is one protocol state machine; splitting it would hide ordering.
func RunLoop(ctx context.Context, provider chat.Provider, system, intent string, env agentloop.Env, cfg LoopConfig) (*LoopResult, *Refusal, error) {
	if provider == nil {
		return nil, nil, errors.New("configassist: no assistant model provider")
	}
	if cfg.MaxToolTurns <= 0 {
		cfg.MaxToolTurns = 40
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = 256 * 1024
	}
	if cfg.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Deadline)
		defer cancel()
	}
	ctx = chat.WithCallSite(ctx, "config-assistant")
	res := &LoopResult{}
	msgs := []chat.Message{{Role: "system", Content: system}, {Role: "user", Content: intent}}
	tools := Tools()
	extra := map[string]func(context.Context, json.RawMessage) string{}
	for _, x := range cfg.Extra {
		tools = append(tools, x.Def)
		extra[x.Def.Function.Name] = x.Handle
	}
	declared := map[string]bool{}
	for {
		if ctx.Err() != nil {
			return nil, &Refusal{Code: RefuseBudget, Message: "request deadline exceeded before the assistant finished"}, nil
		}
		resp, err := provider.CompleteWithTools(ctx, msgs, tools)
		if err != nil {
			return nil, nil, fmt.Errorf("configassist: assistant model: %w", err)
		}
		res.PromptTokens += resp.Usage.PromptTokens
		res.CompletionTokens += resp.Usage.CompletionTokens
		if resp.Model != "" {
			res.Model = resp.Model
		}
		if len(resp.Choices) == 0 {
			return nil, &Refusal{Code: RefuseBudget, Message: "the assistant model returned no choices"}, nil
		}
		msg := resp.Choices[0].Message
		if msg.Role == "" {
			msg.Role = "assistant"
		}
		if files, ok := ParsePlan(msg.Content); ok && res.Declared == nil {
			res.Declared = files
			for _, f := range files {
				declared[f] = true
			}
		}
		if len(msg.ToolCalls) == 0 {
			res.Summary = strings.TrimSpace(planRe.ReplaceAllString(msg.Content, ""))
			msgs = append(msgs, msg)
			res.transcript = msgs
			return res, nil, nil
		}
		res.Turns++
		if res.Turns > cfg.MaxToolTurns {
			return nil, &Refusal{Code: RefuseTurnCap, Message: fmt.Sprintf("the assistant exceeded %d tool turns without finishing", cfg.MaxToolTurns)}, nil
		}
		msgs = append(msgs, msg)
		for _, call := range msg.ToolCalls {
			name := call.Function.Name
			args := json.RawMessage(call.Function.Arguments)
			path := argPath(args)
			res.ToolCalls = append(res.ToolCalls, ToolCallRecord{Name: name, Path: path})
			var result string
			switch {
			case extra[name] != nil:
				result = extra[name](ctx, args)
			case (name == "file_write" || name == "file_edit") && res.Declared == nil:
				result = "ERROR: declare your PLAN (the files you will touch) before editing"
			case (name == "file_write" || name == "file_edit") && !declared[strings.TrimPrefix(path, "./")]:
				result = "ERROR: " + path + " is not in your declared PLAN; you may only edit declared files"
			default:
				written := bytesWritten(name, args)
				res.BytesWritten += written
				if res.BytesWritten > cfg.MaxOutputBytes {
					// A completion past the cap is a refusal, never a truncation
					// (design §8.2, test 17).
					return nil, &Refusal{Code: RefuseOutputCap, Message: fmt.Sprintf("the assistant wrote more than %d bytes; refusing rather than truncating", cfg.MaxOutputBytes)}, nil
				}
				result = Dispatch(env, name, args)
			}
			msgs = append(msgs, chat.Message{Role: "tool", ToolCallID: call.ID, Name: name, Content: result})
		}
	}
}

func argPath(args json.RawMessage) string {
	var a struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(args, &a)
	return a.Path
}

func bytesWritten(name string, args json.RawMessage) int {
	switch name {
	case "file_write":
		var a struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(args, &a)
		return len(a.Content)
	case "file_edit":
		var a struct {
			NewString string `json:"new_string"`
		}
		_ = json.Unmarshal(args, &a)
		return len(a.NewString)
	}
	return 0
}
