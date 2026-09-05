package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// StepPromptPart names one of the three parts of a step's initial model input.
type StepPromptPart string

const (
	// StepPromptSystem is the system prompt as sent: role prompt plus every
	// daemon-injected block plus the entrypoint's own additions.
	StepPromptSystem StepPromptPart = "system"
	// StepPromptUser is the user message as sent: task context, prior-step outputs.
	StepPromptUser StepPromptPart = "user"
	// StepPromptTools is the tools array as sent, after the fail-closed filter
	// and MCP expansion.
	StepPromptTools StepPromptPart = "tools"
	// StepPromptInput is the bytes the executor handed the container as
	// task.json (step-I/O persistence design §3) — the step's boundary on the
	// way in. Redacted at the seam like every part, so a credential the
	// project config passed inline is a marker here.
	StepPromptInput StepPromptPart = "input"
	// StepPromptResult is the bytes the daemon read back as result.json,
	// after the result_json secrets checkpoint — the boundary on the way out,
	// kept whole so a file the daemon could not parse is still evidence.
	StepPromptResult StepPromptPart = "result"
)

// StepPrompt is one content-addressed part.
type StepPrompt struct {
	Hash string         `json:"hash"`
	Part StepPromptPart `json:"part"`
	Body string         `json:"body"`
}

// HashStepPrompt is the content address: sha256 hex over the UTF-8 bytes.
func HashStepPrompt(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// StepPromptHashes are the five hashes an ExecutionStepOutcome row carries:
// the three prompt parts (migration 175) and the two boundary files
// (migration 178). Empty = not recorded (a container image that predates the
// contract, a step that never reached its first request, a daemon predating
// step-I/O persistence, or a part over the executor's ceiling).
type StepPromptHashes struct {
	System string `json:"system,omitempty"`
	User   string `json:"user,omitempty"`
	Tools  string `json:"tools,omitempty"`
	Input  string `json:"input,omitempty"`
	Result string `json:"result,omitempty"`
}

// StepPromptRepository is the content-addressed store of what a workflow step
// was TOLD at its first model request (step-prompt persistence design §4).
//
// Model-visible means persisted: anything reaching a model request must be
// reconstructable from what the daemon stored. Until 2026-09 the workflow path
// kept a token count; only the chat path kept prompts. This store holds the
// step's initial input in three parts so the near-identical ones (system,
// tools) dedup to one row per version and the unique one (user) is one row per
// step. Bodies arrive REDACTED (the repository the executor holds is
// auditredact's decorator) and the hash names the stored bytes.
type StepPromptRepository interface {
	// Save stores a part and returns the sha256 hex of the STORED bytes. The
	// repository computes the hash rather than trusting one: the decorated
	// repository redacts first, so the hash names what is in the table and a
	// Get can verify what it returns. Idempotent — a second Save of the same
	// bytes is a no-op returning the same hash.
	Save(ctx context.Context, part StepPromptPart, body string) (hash string, err error)
	// Get fetches one part by hash. ErrNotFound when absent.
	Get(ctx context.Context, hash string) (*StepPrompt, error)
	// PruneUnreferenced deletes every row no surviving execution_step_outcomes
	// row references through any of its five hash columns, and reports how
	// many it removed. Content-addressed rows live exactly as long as something
	// points at them, so the prompt horizon IS the outcome horizon.
	PruneUnreferenced(ctx context.Context) (int64, error)
}
