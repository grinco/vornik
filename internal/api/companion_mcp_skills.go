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

	sum := sha256.Sum256([]byte(args.Body))
	skill := &persistence.Skill{
		ID:           persistence.GenerateID("skill"),
		ProjectID:    key.ProjectID,
		RepoScope:    effectiveRepoScope(key, args.RepoScope),
		Name:         args.Name,
		Description:  args.Description,
		Body:         args.Body,
		BodySHA256:   hex.EncodeToString(sum[:]),
		Domain:       strings.TrimSpace(args.Domain),
		Tags:         args.Tags,
		Roles:        args.Roles,
		Maturity:     persistence.SkillMaturityDraft,
		Version:      1,
		OriginClient: key.ClientKind,
		OriginTask:   taskIDFromContext(ctx),
		Author:       strings.TrimSpace(args.Author),
		IsGlobal:     args.Global,
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
	note := "proposed as draft — an operator must approve it before it activates"
	if stored.IsGlobal {
		note = "proposed as a GLOBAL draft (affects ALL projects once approved) — an operator must approve it before it activates"
	}
	return marshalSkill(map[string]any{
		"id":        stored.ID,
		"name":      stored.Name,
		"maturity":  stored.Maturity,
		"version":   stored.Version,
		"is_global": stored.IsGlobal,
		"note":      note,
	})
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
		if skill.ProjectID != key.ProjectID && !skill.IsGlobal {
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

func taskIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(mcp.TaskIDHeaderKey{}).(string); ok {
		return v
	}
	return ""
}
