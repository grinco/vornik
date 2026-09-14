package configassist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/agentloop"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/modelfamily"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrethygiene"
)

// ProposedBy is the reserved proposer identity the assistant stamps
// (registered in controlplane's reserved set so a client cannot forge it).
const ProposedBy = "config-assistant"

// AutoApplyActor is the ledger actor for an auto-applied proposal.
const AutoApplyActor = "config-assistant-auto"

// Actor names who asked (review R2/R5): stable ids only, never a bearer.
type Actor struct {
	Kind         string `json:"kind"` // human | credential | system
	AccountID    string `json:"account_id,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
	SourceID     string `json:"source_id,omitempty"`
	Principal    string `json:"principal"` // rendered for ProposedBy/Approver-style fields
}

// Trace is the machine-readable provenance an agent-door proposal carries
// (design §6.3.2, test 22).
type Trace struct {
	TaskID         string   `json:"task_id,omitempty"`
	StepID         string   `json:"step_id,omitempty"`
	ContextSources []string `json:"context_sources,omitempty"`
}

// Request is one intent (design §3: one-shot, loop inside).
type Request struct {
	ProjectID      string
	Intent         string
	Door           string
	Actor          Actor
	RequestID      string
	IdempotencyKey string
	Trace          *Trace
	// EvidenceRunIDs is the healing initiator's ≥3 run ids (WP9); empty
	// for operator intents.
	EvidenceRunIDs []string
}

// Result is what a request produced: a proposal, or a refusal.
type Result struct {
	RequestID   string                            `json:"request_id"`
	Proposal    *persistence.ControlPlaneProposal `json:"proposal,omitempty"`
	Refusal     *Refusal                          `json:"refusal,omitempty"`
	Class       string                            `json:"class,omitempty"`
	Touches     []Touch                           `json:"touches,omitempty"`
	Verdict     *JudgeVerdict                     `json:"verdict,omitempty"`
	AutoApplied bool                              `json:"auto_applied"`
	Diff        string                            `json:"diff,omitempty"`
	Summary     string                            `json:"summary,omitempty"`
	Permissions []PermissionDiff                  `json:"permission_diff,omitempty"`
	Evidence    string                            `json:"evidence,omitempty"`
	HasEvidence bool                              `json:"has_measurement"`
	Ignored     []string                          `json:"ignored_deletions,omitempty"`
	Usage       Usage                             `json:"usage"`
	Duplicate   bool                              `json:"duplicate,omitempty"` // idempotent replay
	Consult     *ConsultRecord                    `json:"consult,omitempty"`
}

// Usage is the request's spend record.
type Usage struct {
	AssistantModel   string `json:"assistant_model,omitempty"`
	JudgeModel       string `json:"judge_model,omitempty"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	ToolTurns        int    `json:"tool_turns"`
	Placeholders     int    `json:"placeholders"`
}

// ProposalStore is the ledger slice the engine needs. *persistence
// proposal repositories satisfy it once the 2026-09-13 identifier columns
// land (plan §7g).
type ProposalStore interface {
	Create(ctx context.Context, p *persistence.ControlPlaneProposal) error
	GetByID(ctx context.Context, id string) (*persistence.ControlPlaneProposal, error)
	List(ctx context.Context, f persistence.ProposalListFilter) ([]*persistence.ControlPlaneProposal, error)
	SetStatus(ctx context.Context, id, status, actor string) error
	GetByIdempotencyKey(ctx context.Context, key string) (*persistence.ControlPlaneProposal, error)
}

// Applier applies an APPROVED proposal (the shipped engine).
type Applier interface {
	Apply(ctx context.Context, id, actor string, ackDaemon bool) error
}

// UsageRecorder records model spend (assistant + judge) so the request is
// attributable to the project (plan §10: an operator-invoked assistant
// always has a project in scope). Nil = not recorded.
type UsageRecorder func(ctx context.Context, projectID, callSite, model string, promptTokens, completionTokens int)

// Engine is the assistant. Construct with the deployed tree root (the
// `configs/` directory — never its parent, which holds config.yaml and
// secrets/), the store, and the two model providers.
type Engine struct {
	// TreeRoot is the deployed configs directory (design §10a).
	TreeRoot string
	// ApplyPrefix is the path prefix the apply engine expects relative to
	// its ConfigDir (filepath.Dir(config.yaml)) — "configs".
	ApplyPrefix string
	// ConfigPath is the daemon's config.yaml, for the fresh hygiene gate.
	ConfigPath string
	// SecretSnapshot is the doctor's post-expansion secret-field snapshot
	// (dotted key → value); the hygiene gate re-evaluates it FRESH per
	// request (test 33).
	SecretSnapshot func() map[string]string

	Config    func() config.AssistantConfig
	Registry  Registry
	Evidence  Evidence
	Assistant chat.Provider
	Judge     chat.Provider
	Proposals ProposalStore
	Applier   Applier
	Usage     UsageRecorder
	Logger    zerolog.Logger
	Now       func() time.Time
	// Peer and Audit back the opt-in architect consultation (WP7). Both
	// may be nil: the tool is then not advertised (Peer nil) or refuses
	// before any network call (Audit nil — test 38).
	Peer  Peer
	Audit ConsultAudit

	mu       sync.Mutex
	inflight map[string]int
}

// Propose runs one intent end to end (design §3). It returns a Result
// whose Refusal is set when nothing was filed, and never a Go error for a
// refusal — errors are infrastructure failures.
//
//nolint:gocognit,funlen // The pipeline is deliberately one readable sequence of gates (design §3).
func (e *Engine) Propose(ctx context.Context, req Request) (*Result, error) {
	now := e.now()
	res := &Result{RequestID: req.RequestID}
	if req.RequestID == "" {
		req.RequestID = persistence.GenerateID("careq")
		res.RequestID = req.RequestID
	}
	cfg := e.Config()

	// Idempotency (R8): a client retry must not file a second proposal.
	if req.IdempotencyKey != "" && e.Proposals != nil {
		if p, err := e.Proposals.GetByIdempotencyKey(ctx, req.IdempotencyKey); err == nil && p != nil {
			res.Proposal, res.Duplicate = p, true
			return res, nil
		}
	}

	// Kill switches (design §8.2). LEVEL 1 before any model call (test 16).
	if cfg.Paused {
		return refuse(res, RefusePaused, "the configuration assistant is paused (config_assistant.paused); no model was called"), nil
	}
	if !cfg.Enabled && IsOperatorDoor(req.Door) {
		return refuse(res, RefusePaused, "the configuration assistant is not enabled (config_assistant.enabled)"), nil
	}
	if req.Door == DoorChat && !cfg.ChatDoor {
		return refuse(res, RefuseDoorCeiling, "the chat door is not open (config_assistant.chat_door)"), nil
	}
	if e.Registry == nil {
		return nil, errors.New("configassist: registry not wired")
	}
	project := e.Registry.GetProject(req.ProjectID)
	if project == nil {
		return refuse(res, RefuseSchema, "project "+req.ProjectID+" not found"), nil
	}
	// LEVEL 2: only an explicit false disables (test 16).
	if project.ConfigAssistantEnabled != nil && !*project.ConfigAssistantEnabled {
		return refuse(res, RefuseSubjectDisabled, "project "+req.ProjectID+" opted out (config_assistant_enabled: false)"), nil
	}
	if sw := e.Registry.GetSwarm(project.SwarmID); sw != nil && sw.ConfigAssistantEnabled != nil && !*sw.ConfigAssistantEnabled {
		return refuse(res, RefuseSubjectDisabled, "swarm "+sw.ID+" opted out (config_assistant_enabled: false)"), nil
	}
	if wf := e.Registry.GetWorkflow(project.DefaultWorkflowID); wf != nil && wf.ConfigAssistantEnabled != nil && !*wf.ConfigAssistantEnabled {
		return refuse(res, RefuseSubjectDisabled, "workflow "+wf.ID+" opted out (config_assistant_enabled: false)"), nil
	}

	// Different-family judge on the request path (test 37).
	if e.Judge != nil && modelfamily.SameFamily(e.assistantModelID(cfg), e.judgeModelID(cfg)) {
		return refuse(res, RefuseModelFamily, fmt.Sprintf("assistant %q and judge %q are the same provider family (or unclassifiable); refusing rather than self-judging", e.assistantModelID(cfg), e.judgeModelID(cfg))), nil
	}

	// Admission (R8): per-project concurrency.
	release, ok := e.admit(req.ProjectID, cfg.MaxConcurrentPerProject)
	if !ok {
		return refuse(res, RefuseAdmission, fmt.Sprintf("project %s already has %d assistant request(s) in flight", req.ProjectID, cfg.MaxConcurrentPerProject)), nil
	}
	defer release()

	// Grounding (design §4) — data the daemon already holds.
	g, err := BuildGrounding(ctx, e.Registry, e.Evidence, req.ProjectID)
	if err != nil {
		return nil, err
	}
	res.Evidence = g.RenderEvidence()
	res.HasEvidence = g.HasMeasurement()

	// Hygiene gate: FRESH evaluation, per project, before any model call
	// (design §7.3, tests 32/33). Two halves: config.yaml through the
	// doctor's own evaluator, and the project's files through the text
	// scanner. A finding is a refusal naming the finding and the file.
	if r := e.hygieneGate(g); r != nil {
		res.Refusal = r
		return res, nil
	}

	// Snapshot (design §2.2 / R7): authorized subset, secrets placeholdered.
	snap, err := Build(e.TreeRoot, e.authorizer(g), DefaultLimits)
	if err != nil {
		return nil, fmt.Errorf("configassist: snapshot: %w", err)
	}
	defer snap.Close()
	res.Usage.Placeholders = snap.Placeholders()

	// The loop (+ the consult tool when the operator opted in — WP7).
	system := g.SystemPrompt(ClassTable)
	env := agentloop.Env{Workspace: snap.Root, Now: e.Now}
	lcfg := LoopConfig{MaxToolTurns: cfg.MaxToolTurns, MaxOutputBytes: cfg.MaxOutputBytes, Deadline: cfg.EffectiveRequestTimeout()}
	var consultant *Consultant
	if cfg.Consult.Enabled && e.Peer != nil {
		consultant = &Consultant{Peer: e.Peer, Audit: e.Audit, Actor: req.Actor, Req: req, Now: e.Now, Cfg: ConsultConfig{
			Enabled: true, PeerName: cfg.Consult.Peer, MaxQuestionBytes: cfg.Consult.MaxQuestionBytes, MaxAnswerBytes: cfg.Consult.MaxAnswerBytes,
			Timeout: cfg.Consult.EffectiveTimeout(), StillEnabled: func() bool { c := e.Config(); return c.Consult.Enabled && c.Consult.Peer == cfg.Consult.Peer },
		}}
		lcfg.Extra = append(lcfg.Extra, ExtraTool{Def: consultant.Tool(), Handle: consultant.Handle})
	}
	loop, refusal, err := RunLoop(ctx, e.assistantProvider(cfg), system, req.Intent, env, lcfg)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		res.Refusal = refusal
		return res, nil
	}
	res.Summary = loop.Summary
	if consultant != nil {
		rec := consultant.Record()
		res.Consult = &rec
	}
	res.Usage.AssistantModel, res.Usage.PromptTokens, res.Usage.CompletionTokens, res.Usage.ToolTurns = loop.Model, loop.PromptTokens, loop.CompletionTokens, loop.Turns
	e.record(ctx, req.ProjectID, "config-assistant", loop.Model, loop.PromptTokens, loop.CompletionTokens)

	// Walk + re-materialise (tests 15, 35).
	ops, ignored, err := snap.Walk()
	if err != nil {
		if errors.Is(err, ErrPlaceholderTampered) {
			return refuse(res, RefusePlaceholder, err.Error()), nil
		}
		return nil, err
	}
	res.Ignored = ignored
	if len(ops) == 0 {
		return refuse(res, RefuseNoChange, "the assistant made no change: "+loop.Summary), nil
	}

	// Deterministic gates BEFORE the judge (design §5).
	if r := GateDeclaredScope(loop.Declared, ops, ignored); r != nil {
		res.Refusal = r
		return res, nil
	}
	if r := GateSchemas(ops); r != nil {
		res.Refusal = r
		return res, nil
	}

	// Classes (design §6): every touched file and key; most restrictive wins.
	var changes []Change
	for _, op := range ops {
		before, existed := snap.Files[op.Path]
		if !existed {
			ev := ClassifyFileEvent(op.Path, true)
			changes = append(changes, Change{File: op.Path, Key: "<new file>", After: ev.Reason})
		}
		orig, _ := snap.Rematerialize(op.Path, before)
		changes = append(changes, ChangesForFile(op.Path, []byte(orig), []byte(op.Content))...)
	}
	bc := ClassifyChanges(changes)
	for _, op := range ops {
		if _, existed := snap.Files[op.Path]; !existed {
			ev := ClassifyFileEvent(op.Path, true)
			bc.Touches = append(bc.Touches, ev)
			bc.Class = MostRestrictive(bc.Class, ev.Class)
		}
	}
	res.Class, res.Touches = bc.Class, bc.Touches
	// LEVEL 3 kill switch.
	if cfg.ClassDisabled(bc.Class) {
		return refuse(res, RefuseClassDisabled, "class "+bc.Class+" is disabled (config_assistant.disabled_classes)"), nil
	}
	if r := GateDoorCeiling(req.Door, bc.Class); r != nil {
		res.Refusal = r
		return res, nil
	}

	// Human-readable diff + effective-permission diff (design §6.2).
	var diff strings.Builder
	original := map[string][]byte{}
	for _, op := range ops {
		before, _ := snap.Rematerialize(op.Path, snap.Files[op.Path])
		original[op.Path] = []byte(before)
		diff.WriteString(UnifiedDiff(op.Path, before, op.Content))
	}
	res.Diff = diff.String()
	if bc.Class == ClassE {
		res.Permissions = EffectivePermissionDiff(ops, original)
		if r := e.classECap(ctx, req, cfg, now); r != nil {
			res.Refusal = r
			return res, nil
		}
	}

	// The judge (design §7) — intent, diff, class, evidence; never the
	// author's reasoning (test 9).
	verdict := RunJudge(ctx, e.judgeProvider(cfg), JudgeInput{Intent: req.Intent, Diff: res.Diff, Class: bc.Class, Evidence: res.Evidence})
	res.Verdict = &verdict
	res.Usage.JudgeModel = verdict.Model
	if verdict.Judged {
		e.record(ctx, req.ProjectID, "config-assistant-judge", verdict.Model, verdict.PromptTok, verdict.CompleteTok)
	}

	// File the proposal.
	p, err := e.fileProposal(ctx, req, res, ops, snap, bc, loop, now)
	if err != nil {
		return nil, err
	}
	res.Proposal = p

	// Auto-apply (design §6, §7.1): classes A/B only, opt-in per project,
	// judged PASS, operator doors only. C/D/E never (tests 8, 19); chat and
	// agent doors never (test 22); abstain/unavailable never (tests 6, 7).
	if e.mayAutoApply(req, cfg, bc.Class, verdict) {
		if err := e.autoApply(ctx, p); err != nil {
			e.Logger.Warn().Err(err).Str("proposal", p.ID).Msg("config-assistant: auto-apply failed; proposal stays filed")
		} else {
			res.AutoApplied = true
		}
	}
	return res, nil
}

func refuse(res *Result, code, msg string) *Result {
	res.Refusal = &Refusal{Code: code, Message: msg}
	return res
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

func (e *Engine) assistantModelID(cfg config.AssistantConfig) string {
	if cfg.Model != "" {
		return cfg.Model
	}
	if e.Assistant != nil {
		return e.Assistant.Model()
	}
	return ""
}

func (e *Engine) judgeModelID(cfg config.AssistantConfig) string {
	if cfg.JudgeModel != "" {
		return cfg.JudgeModel
	}
	if e.Judge != nil {
		return e.Judge.Model()
	}
	return ""
}

func (e *Engine) assistantProvider(cfg config.AssistantConfig) chat.Provider {
	return withModel(e.Assistant, cfg.Model)
}

func (e *Engine) judgeProvider(cfg config.AssistantConfig) chat.Provider {
	return withModel(e.Judge, cfg.JudgeModel)
}

func withModel(p chat.Provider, model string) chat.Provider {
	if p == nil || model == "" {
		return p
	}
	if mo, ok := p.(chat.ModelOverridable); ok {
		return mo.WithModel(model)
	}
	return p
}

func (e *Engine) record(ctx context.Context, projectID, site, model string, prompt, completion int) {
	if e.Usage != nil && (prompt > 0 || completion > 0) {
		e.Usage(ctx, projectID, site, model, prompt, completion)
	}
}

// admit is the per-project concurrency gate (R8).
func (e *Engine) admit(projectID string, limit int) (func(), bool) {
	if limit <= 0 {
		limit = 1
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inflight == nil {
		e.inflight = map[string]int{}
	}
	if e.inflight[projectID] >= limit {
		return nil, false
	}
	e.inflight[projectID]++
	return func() {
		e.mu.Lock()
		e.inflight[projectID]--
		e.mu.Unlock()
	}, true
}

// authorizer limits the snapshot to what the caller may read (R6): a
// project-scoped request sees its own project's files, the swarm and
// workflows it references; nothing of other projects.
func (e *Engine) authorizer(g *Grounding) Authorizer {
	allowed := map[string]bool{}
	prefixes := []string{}
	for _, f := range g.ProjectFiles {
		if strings.HasSuffix(f, "/") {
			prefixes = append(prefixes, f)
		} else {
			allowed[f] = true
		}
	}
	// Shared, read-only context every project may read: the role library
	// and pricing (never secrets — excluded by the snapshot itself).
	prefixes = append(prefixes, "role-library/", "project-templates/")
	return func(rel string) bool {
		if allowed[rel] || rel == "pricing.yaml" {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(rel, p) {
				return true
			}
		}
		return false
	}
}

// hygieneGate: a project whose config tree currently trips the secret
// hygiene finding is REFUSED before any model call (design §7.3).
func (e *Engine) hygieneGate(g *Grounding) *Refusal {
	var findings []string
	if e.SecretSnapshot != nil && e.ConfigPath != "" {
		if fs, evaluable := secrethygiene.Evaluate(e.ConfigPath, e.SecretSnapshot()); evaluable {
			for _, f := range fs {
				if f.Kind == secrethygiene.KindRawSecret {
					findings = append(findings, secrethygiene.Refusal([]secrethygiene.Finding{f}))
				}
			}
		}
	}
	for _, rel := range e.projectFilesOnDisk(g.ProjectFiles) {
		data, err := os.ReadFile(filepath.Join(e.TreeRoot, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		for _, f := range secrethygiene.ScanText(string(data)) {
			findings = append(findings, fmt.Sprintf("config_secret_hygiene: %s appears to carry a raw plaintext secret in %s", f.Key, rel))
		}
	}
	if len(findings) == 0 {
		return nil
	}
	sort.Strings(findings)
	return &Refusal{Code: RefuseSecretHygiene, Message: "refusing to run: the configuration currently contains a raw secret, which would leave the box with the assistant's model call. Fix the finding (use ${ENV_VAR}) and retry", Findings: findings}
}

// projectFilesOnDisk expands the project's file list (single files and
// directory prefixes) to the regular files present under the tree root.
func (e *Engine) projectFilesOnDisk(rels []string) []string {
	var out []string
	for _, rel := range rels {
		if !strings.HasSuffix(rel, "/") {
			if st, err := os.Stat(filepath.Join(e.TreeRoot, filepath.FromSlash(rel))); err == nil && st.Mode().IsRegular() {
				out = append(out, rel)
			}
			continue
		}
		dir := filepath.Join(e.TreeRoot, filepath.FromSlash(rel))
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			if r, rerr := filepath.Rel(e.TreeRoot, p); rerr == nil {
				out = append(out, filepath.ToSlash(r))
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

// classECap: class E is capped per credential per UTC day; the counter is
// the ledger itself (persisted, restart-safe, shared by every replica —
// test 36), never process memory.
func (e *Engine) classECap(ctx context.Context, req Request, cfg config.AssistantConfig, now time.Time) *Refusal {
	if e.Proposals == nil {
		return nil
	}
	rows, err := e.Proposals.List(ctx, persistence.ProposalListFilter{Limit: 1000})
	if err != nil {
		return &Refusal{Code: RefuseClassECap, Message: "cannot read the class-E ledger: " + err.Error()}
	}
	day := now.UTC().Format("2006-01-02")
	count := 0
	for _, p := range rows {
		if p.ProposedBy != ProposedBy || p.CreatedAt.UTC().Format("2006-01-02") != day {
			continue
		}
		if p.ActorCredentialID != req.Actor.CredentialID || p.ActorCredentialID == "" {
			continue
		}
		if evidenceClass(p.Evidence) == ClassE {
			count++
		}
	}
	if count >= cfg.ClassEDailyCap {
		return &Refusal{Code: RefuseClassECap, Message: fmt.Sprintf("class-E proposals are capped at %d per credential per day and this credential has filed %d today; refused, not queued (a burst of class-E proposals is what a leaked credential looks like)", cfg.ClassEDailyCap, count)}
	}
	return nil
}

func evidenceClass(evidence string) string {
	var ev struct {
		Class string `json:"class"`
	}
	_ = json.Unmarshal([]byte(evidence), &ev)
	return ev.Class
}

// EvidenceRecord is the proposal's Evidence JSON: everything a later
// reader needs to tell an evidence-driven change from a requested one
// (test 28), plus the apply engine's stale-base inputs.
type EvidenceRecord struct {
	RequestID      string            `json:"request_id"`
	Door           string            `json:"door"`
	Class          string            `json:"class"`
	Touches        []Touch           `json:"touches"`
	Verdict        *JudgeVerdict     `json:"verdict,omitempty"`
	HasMeasurement bool              `json:"has_measurement"`
	Measurements   string            `json:"measurements"`
	Permissions    []PermissionDiff  `json:"permission_diff,omitempty"`
	BaseHash       string            `json:"base_hash,omitempty"` // single-target compat (apply.go parseBaseHash)
	ReadSet        map[string]string `json:"read_set"`            // path → sha256 | "ABSENT" (journal LLD §4)
	Ignored        []string          `json:"ignored_deletions,omitempty"`
	Trace          *Trace            `json:"trace,omitempty"`
	ToolCalls      []ToolCallRecord  `json:"tool_calls,omitempty"`
	Placeholders   int               `json:"placeholders"`
	Actor          Actor             `json:"actor"`
	EvidenceRunIDs []string          `json:"evidence_run_ids,omitempty"`
	Intent         string            `json:"intent"`
	Consult        *ConsultRecord    `json:"consult,omitempty"`
}

//nolint:gocognit,funlen // The evidence/proposal envelope is intentionally assembled in one place so fields cannot drift.
func (e *Engine) fileProposal(ctx context.Context, req Request, res *Result, ops []Op, snap *Snapshot, bc BundleClass, loop *LoopResult, now time.Time) (*persistence.ControlPlaneProposal, error) {
	if e.Proposals == nil {
		return nil, errors.New("configassist: proposal ledger not wired")
	}
	rels := TouchedFiles(ops)
	hashes, err := snap.BaseHashes(rels)
	if err != nil {
		return nil, err
	}
	readSet := map[string]string{}
	for rel, h := range hashes {
		if h == "" {
			readSet[e.applyPath(rel)] = "ABSENT"
		} else {
			readSet[e.applyPath(rel)] = h
		}
	}
	// Read dependencies the assistant grounded on but did not touch.
	for _, rel := range snap.readDependencies(rels) {
		if _, ok := readSet[e.applyPath(rel)]; ok {
			continue
		}
		h, herr := snap.BaseHashes([]string{rel})
		if herr == nil {
			if h[rel] == "" {
				readSet[e.applyPath(rel)] = "ABSENT"
			} else {
				readSet[e.applyPath(rel)] = h[rel]
			}
		}
	}
	applyOps := make([]Op, 0, len(ops))
	for _, op := range ops {
		applyOps = append(applyOps, Op{Op: op.Op, Path: e.applyPath(op.Path), Content: op.Content})
	}
	opsJSON, _ := json.Marshal(applyOps)
	rec := EvidenceRecord{
		RequestID: req.RequestID, Door: req.Door, Class: bc.Class, Touches: bc.Touches, Verdict: res.Verdict,
		HasMeasurement: res.HasEvidence, Measurements: res.Evidence, Permissions: res.Permissions,
		ReadSet: readSet, Ignored: res.Ignored, Trace: req.Trace, ToolCalls: loop.ToolCalls,
		Placeholders: snap.Placeholders(), Actor: req.Actor, EvidenceRunIDs: req.EvidenceRunIDs, Intent: req.Intent, Consult: res.Consult,
	}
	if len(ops) == 1 && hashes[ops[0].Path] != "" {
		rec.BaseHash = hashes[ops[0].Path]
	}
	evJSON, _ := json.Marshal(rec)
	title := strings.TrimSpace(req.Intent)
	if len(title) > 120 {
		title = title[:117] + "…"
	}
	rationale := loop.Summary
	if res.Verdict != nil && res.Verdict.Judged {
		rationale += fmt.Sprintf("\n\nJudge (%s): %s — %s", res.Verdict.Model, res.Verdict.Decision, res.Verdict.Summary)
	} else if res.Verdict != nil {
		rationale += "\n\nNOT JUDGED: " + res.Verdict.Reason
	}
	if !res.HasEvidence {
		rationale += "\n\nNo measurement supports this change; it is a requested change, not an evidence-driven one."
	}
	if len(res.Permissions) > 0 {
		rationale += "\n\nEFFECTIVE PERMISSION DIFF:\n" + RenderPermissionDiff(res.Permissions)
	}
	p := &persistence.ControlPlaneProposal{
		ID:                persistence.GenerateID("cpp"),
		ProjectID:         req.ProjectID,
		Kind:              persistence.ProposalKindConfig,
		BlastRadius:       blastRadius(ops),
		Title:             "[assistant/" + bc.Class + "] " + title,
		Diff:              truncate(res.Diff, persistence.ProposalMaxFieldBytes-256),
		Rationale:         truncate(rationale, persistence.ProposalMaxFieldBytes-256),
		Evidence:          truncate(string(evJSON), persistence.ProposalMaxFieldBytes-256),
		Status:            persistence.ProposalStatusDraft,
		ProposedBy:        ProposedBy,
		ApplyOps:          string(opsJSON),
		CreatedAt:         now,
		RequestID:         req.RequestID,
		IdempotencyKey:    req.IdempotencyKey,
		Door:              req.Door,
		ActorKind:         req.Actor.Kind,
		ActorAccountID:    req.Actor.AccountID,
		ActorCredentialID: req.Actor.CredentialID,
	}
	if err := e.Proposals.Create(ctx, p); err != nil {
		if errors.Is(err, persistence.ErrDuplicateKey) && req.IdempotencyKey != "" {
			if dup, gerr := e.Proposals.GetByIdempotencyKey(ctx, req.IdempotencyKey); gerr == nil {
				res.Duplicate = true
				return dup, nil
			}
		}
		return nil, fmt.Errorf("configassist: file proposal: %w", err)
	}
	return p, nil
}

func (e *Engine) applyPath(rel string) string {
	if e.ApplyPrefix == "" {
		return rel
	}
	return strings.TrimSuffix(e.ApplyPrefix, "/") + "/" + rel
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}

func blastRadius(ops []Op) string {
	scope := persistence.ProposalScopeProject
	for _, op := range ops {
		if strings.HasPrefix(strings.ToLower(op.Path), "swarms/") {
			scope = persistence.ProposalScopeSwarm
		}
	}
	return scope
}

// readDependencies lists the shared files the request may have grounded
// on (the swarm and workflow references the project holds) so the journal
// checks them at apply time (LLD §4).
func (s *Snapshot) readDependencies(touched []string) []string {
	seen := map[string]bool{}
	for _, t := range touched {
		seen[t] = true
	}
	var out []string
	for rel := range s.Files {
		if seen[rel] {
			continue
		}
		if strings.HasPrefix(rel, "swarms/") || strings.HasPrefix(rel, "workflows/") {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// mayAutoApply is the whole auto-apply decision (design §6, §6.3, §7.1).
func (e *Engine) mayAutoApply(req Request, cfg config.AssistantConfig, class string, v JudgeVerdict) bool {
	if e.Applier == nil || !MayAutoApply(req.Door) {
		return false
	}
	switch class {
	case ClassA, ClassB1, ClassB2:
	default:
		return false // C, D, E: never, whatever the opt-in says (tests 8, 19)
	}
	if class == ClassB1 && !IsOperatorDoor(req.Door) {
		return false
	}
	if !cfg.AutoApplyAllows(req.ProjectID, class) {
		return false
	}
	return v.Pass()
}

func (e *Engine) autoApply(ctx context.Context, p *persistence.ControlPlaneProposal) error {
	if err := e.Proposals.SetStatus(ctx, p.ID, persistence.ProposalStatusApproved, AutoApplyActor); err != nil {
		return err
	}
	return e.Applier.Apply(ctx, p.ID, AutoApplyActor, false)
}
