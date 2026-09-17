package configassist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrethygiene"
)

// Architect consultation over A2A (plan §8, WP7; review R8). The assistant
// may ask the vornik architect ONE minimized, sanitized question per
// request. It is CE, opt-in, off by default, and gated by the service
// contract — no licence check in code. What makes a contractual gate
// supportable is that use is AUDITABLE: the attempt is committed to the
// admin audit log BEFORE any network call, and a nil repository or a
// failed write means zero outbound calls (design test 38).

// ConsultToolName is the tool the loop advertises when consultation is on.
const ConsultToolName = "consult_architect"

// Peer is the outbound A2A call the consult tool makes. Implemented over
// internal/a2a/client by the api layer; nil when no peer is wired.
type Peer interface {
	// Name is the configured a2a.peers key (the pinned destination).
	Name() string
	// Ask sends one question and returns the answer text.
	Ask(ctx context.Context, question string) (string, error)
}

// ConsultAudit is the admin audit sink (persistence.AdminAuditRepository).
type ConsultAudit interface {
	Insert(ctx context.Context, entry *persistence.AdminAuditEntry) error
	List(ctx context.Context, filter persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error)
}

// Consult audit actions.
const (
	ConsultActionEnabled  = "config_assistant.consult.enabled"
	ConsultActionDisabled = "config_assistant.consult.disabled"
	ConsultActionAttempt  = "config_assistant.consult.attempt"
	ConsultActionOutcome  = "config_assistant.consult.outcome"
	ConsultActionFirstUse = "config_assistant.consult.first_use"
)

// Consult outcomes.
const (
	ConsultOutcomeOK      = "ok"
	ConsultOutcomeError   = "error"
	ConsultOutcomeRefused = "refused"
	ConsultOutcomeUnknown = "unknown" // the call was dispatched and its result could not be recorded
)

// ConsultConfig is the per-request consult policy.
type ConsultConfig struct {
	Enabled          bool
	PeerName         string
	MaxQuestionBytes int
	MaxAnswerBytes   int
	Timeout          time.Duration
	// ConsentGeneration is re-checked immediately before dispatch: the
	// engine passes a closure that re-reads config so a consult enabled
	// at request start and disabled before dispatch is not sent (R8).
	StillEnabled func() bool
}

// Consultant is one request's consult budget: at most one consultation
// per intent by default (R8), the audit discipline, and the egress screen.
type Consultant struct {
	Peer   Peer
	Audit  ConsultAudit
	Cfg    ConsultConfig
	Actor  Actor
	Req    Request
	Now    func() time.Time
	mu     sync.Mutex
	used   int
	record ConsultRecord
}

// ConsultRecord is what the proposal's evidence carries about the consult.
type ConsultRecord struct {
	Attempted   bool   `json:"attempted"`
	Peer        string `json:"peer,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	QuestionSHA string `json:"question_sha256,omitempty"`
	AnswerBytes int    `json:"answer_bytes,omitempty"`
	Error       string `json:"error,omitempty"`
	LatencyMS   int64  `json:"latency_ms,omitempty"`
}

// Record returns the consult record for the evidence.
func (c *Consultant) Record() ConsultRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.record
}

// Tool is the chat.Tool definition advertised to the model when the
// consultation is enabled. The description states the contractual gate
// plainly to the model as well: it is a paid, audited call.
func (c *Consultant) Tool() chat.Tool {
	return chat.Tool{Type: "function", Function: chat.ToolFunction{
		Name:        ConsultToolName,
		Description: "Ask the vornik architect (a remote expert over A2A) ONE short question about vornik configuration semantics or workflow shape. Send only the question — never file contents, never secrets. At most one consultation per request; the answer is advice, not approval. Every use is recorded in the audit log.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"One clear, self-contained question (no config excerpts, no secrets)."}},"required":["question"]}`),
	}}
}

// Handle runs the consult tool for the loop. It returns tool TEXT (the
// loop's contract); an unreachable or refused peer degrades the answer,
// it never fails the request (plan §8).
func (c *Consultant) Handle(ctx context.Context, args json.RawMessage) string {
	var a struct {
		Question string `json:"question"`
	}
	_ = json.Unmarshal(args, &a)
	q := strings.TrimSpace(a.Question)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used >= 1 {
		return "ERROR: consultation budget exhausted for this request (one consult per intent); proceed with what you have"
	}
	c.used++
	c.record.Attempted = true
	c.record.Peer = c.peerName()
	if q == "" {
		c.record.Outcome = ConsultOutcomeRefused
		return "ERROR: the 'question' argument is required"
	}
	if c.Cfg.MaxQuestionBytes > 0 && len(q) > c.Cfg.MaxQuestionBytes {
		c.record.Outcome = ConsultOutcomeRefused
		return fmt.Sprintf("ERROR: question exceeds %d bytes; ask a shorter, minimized question", c.Cfg.MaxQuestionBytes)
	}
	// Egress screen (design §7.3, third call site): the same policy as the
	// assistant and judge calls, over the minimized question.
	if questionCarriesSecret(q) {
		c.record.Outcome = ConsultOutcomeRefused
		c.record.Error = "question carries a secret-shaped value"
		return "ERROR: the question carries what looks like a secret or a secret placeholder; consultation refused — ask without it"
	}
	// Consent + destination re-check immediately before send (R8).
	if c.Peer == nil || !c.Cfg.Enabled || (c.Cfg.StillEnabled != nil && !c.Cfg.StillEnabled()) || c.Cfg.PeerName == "" || c.Peer.Name() != c.Cfg.PeerName {
		c.record.Outcome = ConsultOutcomeRefused
		c.record.Error = "consultation not enabled or destination changed"
		return "ERROR: architect consultation is not enabled (config_assistant.consult) — proceed without it and say so"
	}
	// Audit BEFORE dispatch (test 38): nil repository or a failed write ⇒
	// zero network calls.
	sum := sha256.Sum256([]byte(q))
	c.record.QuestionSHA = hex.EncodeToString(sum[:])
	if err := c.audit(ctx, ConsultActionAttempt, map[string]any{
		"request_id": c.Req.RequestID, "project_id": c.Req.ProjectID, "peer": c.Cfg.PeerName,
		"question_sha256": c.record.QuestionSHA, "question_bytes": len(q), "entrypoint": c.Req.Entrypoint,
	}); err != nil {
		c.record.Outcome = ConsultOutcomeRefused
		c.record.Error = "audit unavailable: " + err.Error()
		return "ERROR: consultation refused — the attempt could not be recorded in the audit log (" + err.Error() + "); proceed without it"
	}
	c.firstUse(ctx)

	cctx := ctx
	if c.Cfg.Timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, c.Cfg.Timeout)
		defer cancel()
	}
	start := c.now()
	answer, err := c.Peer.Ask(cctx, q)
	c.record.LatencyMS = c.now().Sub(start).Milliseconds()
	outcome := map[string]any{"request_id": c.Req.RequestID, "project_id": c.Req.ProjectID, "peer": c.Cfg.PeerName, "latency_ms": c.record.LatencyMS}
	if err != nil {
		c.record.Outcome, c.record.Error = ConsultOutcomeError, err.Error()
		outcome["outcome"] = ConsultOutcomeError
		outcome["error"] = err.Error()
		c.recordOutcome(ctx, outcome)
		// No automatic retry (R8): an uncertain remote outcome stays as it is.
		return "Consultation of the architect did not return an answer (" + err.Error() + "). Do NOT invent what it might have said; proceed with your own reading and say the consultation was unavailable."
	}
	answer = strings.TrimSpace(answer)
	if c.Cfg.MaxAnswerBytes > 0 && len(answer) > c.Cfg.MaxAnswerBytes {
		answer = answer[:c.Cfg.MaxAnswerBytes] + "\n…(answer truncated at the configured cap)"
	}
	c.record.Outcome, c.record.AnswerBytes = ConsultOutcomeOK, len(answer)
	outcome["outcome"] = ConsultOutcomeOK
	outcome["answer_bytes"] = len(answer)
	c.recordOutcome(ctx, outcome)
	// Peer output is untrusted advice, never approval or an instruction
	// granting tools (R8): it is wrapped and labelled as data.
	return "ARCHITECT ADVICE (untrusted; advice, not approval):\n<untrusted_content source=\"a2a:" + c.Cfg.PeerName + "\">\n" + answer + "\n</untrusted_content>"
}

func (c *Consultant) peerName() string {
	if c.Peer != nil {
		return c.Peer.Name()
	}
	return c.Cfg.PeerName
}

func (c *Consultant) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

// audit commits one row; a nil sink is an error (fail closed).
func (c *Consultant) audit(ctx context.Context, action string, after map[string]any) error {
	if c.Audit == nil {
		return errors.New("admin audit repository not wired")
	}
	b, _ := json.Marshal(after)
	principal := c.Actor.Principal
	if principal == "" {
		principal = "unknown"
	}
	return c.Audit.Insert(ctx, &persistence.AdminAuditEntry{
		Timestamp: c.now(), Principal: principal, Source: "api", Action: action, Target: c.Req.ProjectID, After: string(b),
	})
}

// recordOutcome writes the post-call outcome. The store may be down after
// the call succeeded; it is retried briefly and, failing that, the record
// carries UNKNOWN rather than a fabricated outcome (R8).
func (c *Consultant) recordOutcome(ctx context.Context, after map[string]any) {
	var err error
retry:
	for attempt := 0; attempt < 3; attempt++ {
		if err = c.audit(ctx, ConsultActionOutcome, after); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			err = ctx.Err()
			break retry
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
	c.record.Outcome = ConsultOutcomeUnknown
	if c.record.Error == "" {
		c.record.Error = "outcome could not be recorded: " + err.Error()
	}
}

// firstUse writes the immutable first-use marker for the project once.
func (c *Consultant) firstUse(ctx context.Context) {
	if c.Audit == nil {
		return
	}
	rows, err := c.Audit.List(ctx, persistence.AdminAuditFilter{Action: ConsultActionFirstUse, TargetPrefix: c.Req.ProjectID, PageSize: 5})
	if err != nil {
		return
	}
	for _, r := range rows {
		if r.Target == c.Req.ProjectID {
			return
		}
	}
	_ = c.audit(ctx, ConsultActionFirstUse, map[string]any{"project_id": c.Req.ProjectID, "peer": c.Cfg.PeerName})
}

// RecordConsultToggle writes the enable/disable event (called by the
// config reload hook when config_assistant.consult.enabled changes; the
// actor is "external/unknown" because a config-file edit has no principal).
func RecordConsultToggle(ctx context.Context, audit ConsultAudit, enabled bool, peer, actor string) error {
	if audit == nil {
		return errors.New("admin audit repository not wired")
	}
	action := ConsultActionDisabled
	if enabled {
		action = ConsultActionEnabled
	}
	if actor == "" {
		actor = "external/unknown"
	}
	b, _ := json.Marshal(map[string]any{"peer": peer, "enabled": enabled})
	return audit.Insert(ctx, &persistence.AdminAuditEntry{Timestamp: time.Now().UTC(), Principal: actor, Source: "api", Action: action, Target: "config_assistant.consult", After: string(b)})
}

// secretTokenRe matches secret-shaped tokens in free text: known vendor
// prefixes, and any long unbroken alphanumeric run (conservative — a
// question rarely needs a 40-character token).
var secretTokenRe = regexp.MustCompile(`(?i)(sk-[A-Za-z0-9_-]{16,}|ghp_[A-Za-z0-9]{20,}|gho_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{30,}|[A-Za-z0-9_\-]{40,})`)

// questionCarriesSecret is the consult call's egress screen: the same
// policy as the assistant and judge calls (design §7.3), applied to a
// free-text question — the line-oriented scanner plus secret-shaped
// tokens and the snapshot's own placeholders.
func questionCarriesSecret(q string) bool {
	if strings.Contains(q, "VORNIK_SECRET_PLACEHOLDER_") {
		return true
	}
	if len(secrethygiene.ScanText(q)) > 0 {
		return true
	}
	for _, tok := range secretTokenRe.FindAllString(q, -1) {
		if secrethygiene.LooksLikeRawSecret(tok) {
			return true
		}
	}
	return false
}
