package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/skills"
)

// Companion MCP verbs for the knowledge-skill store
// (LLD 2026-07-07-knowledge-skill-store-design). skill_search/get/list
// require SkillRead; skill_propose requires SkillWrite; skill_approve/
// reject require SkillAdmin (the human gate). skill_feedback is
// deliberately NOT exposed here — usage signals come from the
// executor-trusted path, not a client.

const skillMaxBodyBytes = 65536 // 64 KiB, matches the remember content cap

// --- argument shapes -------------------------------------------------

type skillProposeArgs struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Body        string   `json:"body"`
	Domain      string   `json:"domain"`
	Tags        []string `json:"tags"`
	Roles       []string `json:"roles"`
	RepoScope   string   `json:"repo_scope"`
	Author      string   `json:"author"`
	// Global, when true, proposes a cross-project skill: once approved it
	// injects into EVERY project's roles, not just this key's project.
	Global bool `json:"global"`

	// Dedup-preflight dispositions (LLD §12.2). At most one may be set; both
	// unset is the default and a near-duplicate hit soft-blocks the write.
	//
	// Supersedes names the skill this one replaces. The target is RETIRED and
	// its body preserved — never overwritten, because §6 binds approval to a
	// body hash and an approved artifact must stay recoverable.
	Supersedes string `json:"supersedes"`
	// ConfirmDistinct asserts the skill is genuinely distinct from what the
	// preflight flagged. The value is the REQUIRED justification, stored on
	// the row: without it this degenerates into a reflex bypass in exactly the
	// state duplicates are most likely, and the guard becomes theatre.
	ConfirmDistinct string `json:"confirm_distinct"`
}

type skillSearchArgs struct {
	Query       string `json:"query"`
	RepoScope   string `json:"repo_scope"`
	Domain      string `json:"domain"`
	Role        string `json:"role"`
	Limit       int    `json:"limit"`
	StrictScope bool   `json:"strict_scope"`
}

type skillListArgs struct {
	RepoScope string `json:"repo_scope"`
	Maturity  string `json:"maturity"`
	Domain    string `json:"domain"`
	Limit     int    `json:"limit"`
}

type skillGetArgs struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	RepoScope string `json:"repo_scope"`
}

type skillModerateArgs struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type skillSetGlobalArgs struct {
	ID     string `json:"id"`
	Global bool   `json:"global"`
}

// --- response shapes -------------------------------------------------

type skillSummary struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Domain      string   `json:"domain,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Roles       []string `json:"roles,omitempty"`
	RepoScope   string   `json:"repo_scope,omitempty"`
	Maturity    string   `json:"maturity"`
	Version     int      `json:"version"`
	IsGlobal    bool     `json:"is_global,omitempty"`
}

func toSkillSummary(s *persistence.Skill) skillSummary {
	return skillSummary{
		ID: s.ID, Name: s.Name, Description: s.Description, Domain: s.Domain,
		Tags: s.Tags, Roles: s.Roles, RepoScope: s.RepoScope,
		Maturity: s.Maturity, Version: s.Version, IsGlobal: s.IsGlobal,
	}
}

func marshalSkill(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// --- handlers --------------------------------------------------------

func (s *Server) companionToolSkillPropose(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if !key.SkillWrite {
		return "", errors.New("this key lacks skill_write; ask the operator for `vornikctl companion grant --skill-write`")
	}
	if s.skillStore == nil {
		return "", errors.New("skill store not wired on this daemon")
	}
	var args skillProposeArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	args.Name = strings.TrimSpace(args.Name)
	args.Description = strings.TrimSpace(args.Description)
	if args.Name == "" || args.Description == "" || strings.TrimSpace(args.Body) == "" {
		return "", errors.New("name, description, and body are required")
	}
	if len(args.Body) > skillMaxBodyBytes {
		return "", fmt.Errorf("body exceeds %d bytes", skillMaxBodyBytes)
	}

	args.Supersedes = strings.TrimSpace(args.Supersedes)
	args.ConfirmDistinct = strings.TrimSpace(args.ConfirmDistinct)
	if args.Supersedes != "" && args.ConfirmDistinct != "" {
		return "", errors.New("pass either supersedes or confirm_distinct, not both: they are opposite answers to the same question")
	}

	sum := sha256.Sum256([]byte(args.Body))
	skill := &persistence.Skill{
		ID:                    persistence.GenerateID("skill"),
		ProjectID:             key.ProjectID,
		RepoScope:             effectiveRepoScope(key, args.RepoScope),
		Name:                  args.Name,
		Description:           args.Description,
		Body:                  args.Body,
		BodySHA256:            hex.EncodeToString(sum[:]),
		Domain:                strings.TrimSpace(args.Domain),
		Tags:                  args.Tags,
		Roles:                 args.Roles,
		Maturity:              persistence.SkillMaturityDraft,
		Version:               1,
		OriginClient:          key.ClientKind,
		OriginTask:            taskIDFromContext(ctx),
		Author:                strings.TrimSpace(args.Author),
		IsGlobal:              args.Global,
		DistinctJustification: args.ConfirmDistinct,
	}

	// Dedup preflight (§12.2). Skipped only when the author has already
	// answered a block with an explicit disposition — the answer IS the
	// acknowledgement, so re-blocking on it would be an infinite loop.
	if args.Supersedes == "" && args.ConfirmDistinct == "" {
		matches, err := s.runSkillDupePreflight(ctx, skill)
		if err != nil {
			return "", fmt.Errorf("preflight failed: %w", err)
		}
		if len(matches) > 0 {
			return marshalSkill(map[string]any{
				"blocked": true,
				"matches": matches,
				"note": "near-duplicate(s) found — nothing was written. Re-send with " +
					"`supersedes: \"<id>\"` to replace one (it is retired, its body kept), " +
					"or `confirm_distinct: \"<why these differ>\"` to author it anyway. " +
					"The justification is required and is recorded.",
			})
		}
	}

	// `supersedes` writes a NEW row and retires the target — it never
	// overwrites the target's body. An approved body is bound to a hash by §6
	// and is the audit trail for what an operator sanctioned; replacing it in
	// place would destroy the only copy.
	if args.Supersedes != "" {
		target, err := s.skillStore.GetByID(ctx, args.Supersedes)
		if err != nil {
			return "", fmt.Errorf("supersedes target %q: %w", args.Supersedes, err)
		}
		if target.ProjectID != key.ProjectID {
			return "", errors.New("cannot supersede a skill from another project")
		}
		// A target sharing the candidate's natural key is not a supersede: it is
		// the SAME skill. Performing both halves — Upsert updates that row in
		// place, then SetMaturity retires it — destroys the skill and leaves the
		// name unwritable, because the retired row still holds the UNIQUE key.
		// That is how an operator-approved `rag-first` was lost on 2026-08-20.
		if sameSkillIdentity(skill, target) {
			return "", fmt.Errorf("supersedes %q names the same skill you are proposing "+
				"(%s/%s): that is a new VERSION, not a replacement. Re-send without "+
				"`supersedes` — an exact-key propose updates in place, bumps the version and "+
				"resets to draft, archiving the prior body",
				target.ID, skill.RepoScope, skill.Name)
		}
		skill.SupersedesID = target.ID
	}

	// Upsert so re-proposing the same (scope,name) edits in place: it
	// bumps the version and resets to draft, requiring fresh approval.
	// NOTE: Upsert preserves is_global on an EDIT (it never clears an
	// already-set flag); global:true only takes effect on a fresh create.
	// Use skill_set_global to change reach on an existing skill.
	stored, err := s.skillStore.Upsert(ctx, skill)
	if err != nil {
		return "", fmt.Errorf("propose failed: %w", err)
	}

	// Retire the superseded row only AFTER the replacement is durably stored,
	// so a failed write can never leave the catalogue with neither active.
	if skill.SupersedesID != "" {
		if err := s.skillStore.SetMaturity(ctx, skill.SupersedesID, persistence.SkillMaturityRetired); err != nil {
			return "", fmt.Errorf("stored %s but failed to retire superseded %s: %w", stored.ID, skill.SupersedesID, err)
		}
	}

	note := "proposed as draft — an operator must approve it before it activates"
	if stored.IsGlobal {
		note = "proposed as a GLOBAL draft (affects ALL projects once approved) — an operator must approve it before it activates"
	}
	out := map[string]any{
		"id":        stored.ID,
		"name":      stored.Name,
		"maturity":  stored.Maturity,
		"version":   stored.Version,
		"is_global": stored.IsGlobal,
		"note":      note,
	}
	if stored.SupersedesID != "" {
		out["supersedes"] = stored.SupersedesID
		out["superseded_note"] = "the superseded skill is retired; its body remains readable by id"
	}
	return marshalSkill(out)
}

func (s *Server) companionToolSkillSearch(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if !key.SkillRead {
		return "", errors.New("this key lacks skill_read; ask the operator for `vornikctl companion grant --skill-read`")
	}
	if s.skillStore == nil {
		return "", errors.New("skill store not wired on this daemon")
	}
	var args skillSearchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	// Search surfaces usable skills by default (active + trusted).
	skills, err := s.skillStore.List(ctx, key.ProjectID, persistence.SkillListFilter{
		RepoScope:     effectiveRepoScope(key, args.RepoScope),
		StrictScope:   args.StrictScope,
		Maturities:    []string{persistence.SkillMaturityActive, persistence.SkillMaturityTrusted},
		Domain:        strings.TrimSpace(args.Domain),
		Role:          strings.TrimSpace(args.Role),
		Limit:         args.Limit,
		IncludeGlobal: true,
	})
	if err != nil {
		return "", fmt.Errorf("search failed: %w", err)
	}
	q := strings.ToLower(strings.TrimSpace(args.Query))
	out := make([]skillSummary, 0, len(skills))
	for _, sk := range skills {
		if q != "" && !strings.Contains(strings.ToLower(sk.Name), q) &&
			!strings.Contains(strings.ToLower(sk.Description), q) {
			continue
		}
		out = append(out, toSkillSummary(sk))
	}
	return marshalSkill(map[string]any{"skills": out, "count": len(out)})
}

func (s *Server) companionToolSkillList(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if !key.SkillRead {
		return "", errors.New("this key lacks skill_read; ask the operator for `vornikctl companion grant --skill-read`")
	}
	if s.skillStore == nil {
		return "", errors.New("skill store not wired on this daemon")
	}
	var args skillListArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	var maturities []string
	if m := strings.TrimSpace(args.Maturity); m != "" {
		maturities = []string{m}
	}
	skills, err := s.skillStore.List(ctx, key.ProjectID, persistence.SkillListFilter{
		RepoScope:     effectiveRepoScope(key, args.RepoScope),
		Maturities:    maturities,
		Domain:        strings.TrimSpace(args.Domain),
		Limit:         args.Limit,
		IncludeGlobal: true,
	})
	if err != nil {
		return "", fmt.Errorf("list failed: %w", err)
	}
	out := make([]skillSummary, 0, len(skills))
	for _, sk := range skills {
		out = append(out, toSkillSummary(sk))
	}
	return marshalSkill(map[string]any{"skills": out, "count": len(out)})
}

func (s *Server) companionToolSkillGet(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if !key.SkillRead {
		return "", errors.New("this key lacks skill_read; ask the operator for `vornikctl companion grant --skill-read`")
	}
	if s.skillStore == nil {
		return "", errors.New("skill store not wired on this daemon")
	}
	var args skillGetArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	skill, err := s.resolveSkill(ctx, key, args.ID, args.Name, args.RepoScope)
	if err != nil {
		return "", err
	}
	return marshalSkill(map[string]any{
		"id": skill.ID, "name": skill.Name, "description": skill.Description,
		"body": skill.Body, "domain": skill.Domain, "tags": skill.Tags,
		"roles": skill.Roles, "repo_scope": skill.RepoScope,
		"maturity": skill.Maturity, "version": skill.Version,
		"is_global": skill.IsGlobal,
	})
}

func (s *Server) companionToolSkillApprove(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	return s.skillModerate(ctx, key, raw, persistence.SkillMaturityActive)
}

func (s *Server) companionToolSkillReject(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	return s.skillModerate(ctx, key, raw, persistence.SkillMaturityRetired)
}

func (s *Server) skillModerate(ctx context.Context, key *persistence.APIKey, raw json.RawMessage, target string) (string, error) {
	if !key.SkillAdmin {
		return "", errors.New("this key lacks skill_admin; approving/rejecting skills requires `vornikctl companion grant --skill-admin`")
	}
	if s.skillStore == nil {
		return "", errors.New("skill store not wired on this daemon")
	}
	var args skillModerateArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.ID) == "" {
		return "", errors.New("id is required")
	}
	// Confirm the skill exists in this key's project before mutating,
	// so a caller can't flip another project's skill by guessing an id.
	skill, err := s.skillStore.GetByID(ctx, args.ID)
	if err != nil {
		return "", fmt.Errorf("skill not found: %w", err)
	}
	if skill.ProjectID != key.ProjectID {
		return "", errors.New("skill not found in this project")
	}
	// Apply the decision through the shared channel-neutral path so the
	// MCP, Telegram, and Slack surfaces can't diverge (the "corrected"
	// credit on rejecting an active/trusted skill lives there).
	decision := skills.Approve
	if target == persistence.SkillMaturityRetired {
		decision = skills.Reject
	}
	outcome, err := skills.ApplyDecision(ctx, s.skillStore, args.ID, decision)
	if err != nil {
		return "", fmt.Errorf("set maturity failed: %w", err)
	}
	return marshalSkill(map[string]any{
		"id": skill.ID, "name": skill.Name, "maturity": outcome,
		"body_sha256": skill.BodySHA256, // approval binds to the exact reviewed body
		"reason":      strings.TrimSpace(args.Reason),
	})
}

// companionToolSkillSetGlobal flips a skill's cross-project reach. It is
// SkillAdmin-gated (same human gate as approve/reject — marking a skill
// global fires it in EVERY project) and project-checked so a caller can
// only widen its OWN project's skills. Does not change maturity.
func (s *Server) companionToolSkillSetGlobal(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if !key.SkillAdmin {
		return "", errors.New("this key lacks skill_admin; changing a skill's global reach requires `vornikctl companion grant --skill-admin`")
	}
	if s.skillStore == nil {
		return "", errors.New("skill store not wired on this daemon")
	}
	var args skillSetGlobalArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.ID) == "" {
		return "", errors.New("id is required")
	}
	// Project-check before mutating: a caller can only flip a skill in
	// its own project (the home-project invariant — is_global widens
	// injection, never ownership).
	skill, err := s.skillStore.GetByID(ctx, args.ID)
	if err != nil {
		return "", fmt.Errorf("skill not found: %w", err)
	}
	if skill.ProjectID != key.ProjectID {
		return "", errors.New("skill not found in this project")
	}
	if err := s.skillStore.SetGlobal(ctx, args.ID, args.Global); err != nil {
		return "", fmt.Errorf("set global failed: %w", err)
	}
	note := "skill is now project-only"
	if args.Global {
		note = "skill is now GLOBAL — it injects into ALL projects' roles"
	}
	return marshalSkill(map[string]any{
		"id": skill.ID, "name": skill.Name, "is_global": args.Global, "note": note,
	})
}

// resolveSkill fetches a skill by id (scope-agnostic, project-checked)
// or by its scope-qualified natural key.
func (s *Server) resolveSkill(ctx context.Context, key *persistence.APIKey, id, name, repoScope string) (*persistence.Skill, error) {
	if strings.TrimSpace(id) != "" {
		skill, err := s.skillStore.GetByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("skill not found: %w", err)
		}
		if !skillReadableByKey(skill, key, repoScope) {
			return nil, errors.New("skill not found in this project")
		}
		return skill, nil
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("either id or name is required")
	}
	list, err := s.skillStore.List(ctx, key.ProjectID, persistence.SkillListFilter{
		RepoScope:     effectiveRepoScope(key, repoScope),
		StrictScope:   true,
		Maturities:    []string{persistence.SkillMaturityActive, persistence.SkillMaturityTrusted},
		IncludeGlobal: true,
	})
	if err != nil {
		return nil, fmt.Errorf("skill not found: %w", err)
	}
	for _, skill := range list {
		if skill.Name == strings.TrimSpace(name) {
			return skill, nil
		}
	}
	return nil, errors.New("skill not found")
}

func skillReadableByKey(skill *persistence.Skill, key *persistence.APIKey, repoScope string) bool {
	if skill == nil || key == nil {
		return false
	}
	if skill.ProjectID != key.ProjectID && !skill.IsGlobal {
		return false
	}
	if skill.Maturity != persistence.SkillMaturityActive && skill.Maturity != persistence.SkillMaturityTrusted {
		return false
	}
	scope := effectiveRepoScope(key, repoScope)
	if scope == "" {
		return true
	}
	return skill.RepoScope == scope || skill.RepoScope == "*"
}

func taskIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(mcp.TaskIDHeaderKey{}).(string); ok {
		return v
	}
	return ""
}
