package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/onboarding"
	"vornik.io/vornik/internal/persistence"
)

// SetupStatus handles GET /api/v1/setup/status. It reports whether the
// install already has a committed onboarding row and, if not, whether the
// conservative heuristic still considers the install fresh.
func (s *Server) SetupStatus(w http.ResponseWriter, r *http.Request) {
	if SessionRoleFromContext(r.Context()) == auth.RoleUser {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED", "admin scope required")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Defense-in-depth: if the detector's Config was not wired (nil),
	// fall back to the server's own config. The UI handler's setupStatus
	// doesn't need this because Detect() already handles nil Config by
	// returning a conservative FreshInstall=true.
	detector := s.setupDetector
	if detector.Config == nil {
		detector.Config = s.config
	}
	status := detector.Detect(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// setupModelLister is the optional capability the concrete chat validator
// implements: list the models a proposed endpoint+key can see, without the
// invocation gate. Kept separate from ChatValidatorInterface so the test
// stubs that only implement Validate keep compiling.
type setupModelLister interface {
	ListModels(ctx context.Context, endpoint, apiKey string) ([]chat.ModelInfo, error)
}

// SetupModels handles POST /api/v1/setup/models. Given a proposed chat
// endpoint + API key it returns the model list the endpoint exposes, so
// the setup form can offer a "Fetch models" dropdown instead of making the
// operator type a model ID from memory. This is a read-only probe: it never
// writes config or session state. Admin-scoped like the rest of setup.
func (s *Server) SetupModels(w http.ResponseWriter, r *http.Request) {
	if SessionRoleFromContext(r.Context()) == auth.RoleUser {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED", "admin scope required")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	lister, ok := s.setupValidator.(setupModelLister)
	if !ok || s.setupValidator == nil {
		respondError(w, http.StatusServiceUnavailable, "SETUP_NOT_CONFIGURED", "model listing not wired")
		return
	}
	proposal, _, err := decodeSetupChatProposal(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "BAD_BODY", err.Error())
		return
	}
	models, err := lister.ListModels(r.Context(), proposal.Endpoint, proposal.APIKey)
	if err != nil {
		// 502: the upstream endpoint/key is the problem, not our request.
		respondError(w, http.StatusBadGateway, "MODELS_UNAVAILABLE", err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"models": models})
}

// SetupDispatcherCommit handles POST /api/v1/setup/dispatcher. It writes
// telegram.dispatcher_project_id to config.yaml once chat/memory are already
// configured and the operator has created a project to pin dispatcher chat
// cost/routing to.
func (s *Server) SetupDispatcherCommit(w http.ResponseWriter, r *http.Request) {
	if SessionRoleFromContext(r.Context()) == auth.RoleUser {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED", "admin scope required")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.setupConfigPath == "" {
		respondError(w, http.StatusServiceUnavailable, "SETUP_NOT_CONFIGURED", "setup config writer not wired")
		return
	}
	var body struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "BAD_BODY", "invalid JSON body")
		return
	}
	projectID := strings.TrimSpace(body.ProjectID)
	if projectID == "" {
		respondError(w, http.StatusBadRequest, "PROJECT_REQUIRED", "project_id required")
		return
	}
	if strings.Contains(projectID, "/") || strings.Contains(projectID, `\`) {
		respondError(w, http.StatusBadRequest, "PROJECT_INVALID", "project_id must be a project id, not a path")
		return
	}
	if s.projectRegistry != nil && s.projectRegistry.GetProject(projectID) == nil {
		respondError(w, http.StatusBadRequest, "PROJECT_UNKNOWN", "project_id is not loaded in the project registry")
		return
	}
	if err := writeDispatcherProjectConfig(s.setupConfigPath, projectID); err != nil {
		var pe *configPatchError
		if errors.As(err, &pe) {
			respondError(w, http.StatusInternalServerError, pe.Code, pe.Msg)
			return
		}
		respondError(w, http.StatusInternalServerError, "CONFIG_PATCH", err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"committed":        true,
		"restart_required": true,
		"project_id":       projectID,
	})
}

// SetupSessionCreate handles POST /api/v1/setup/session. It creates a
// fresh onboarding session (Step 1 of the guide) and returns its JSON.
func (s *Server) SetupSessionCreate(w http.ResponseWriter, r *http.Request) {
	if SessionRoleFromContext(r.Context()) == auth.RoleUser {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED", "admin scope required")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.setupSessions == nil {
		respondError(w, http.StatusServiceUnavailable, "SETUP_NOT_CONFIGURED", "onboarding sessions not wired")
		return
	}
	var body struct {
		SelectedUseCase string `json:"selected_use_case"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "BAD_BODY", "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.SelectedUseCase) == "" {
		body.SelectedUseCase = "generic-assistant"
	}
	operatorID := RequestOperatorIDOrSingleTenant(r, SingleTenantOperatorIDFromConfig(s.config))
	if operatorID == "" {
		respondError(w, http.StatusUnauthorized, "UNAUTHORIZED", "operator identity required")
		return
	}
	sess := &persistence.InstallationOnboardingSession{
		ID:              setupSessionID(),
		OperatorID:      operatorID,
		CurrentStep:     "choose-purpose",
		SelectedUseCase: body.SelectedUseCase,
		Transcript:      []byte("[]"),
	}
	if err := s.setupSessions.Insert(r.Context(), sess); err != nil {
		respondError(w, http.StatusInternalServerError, "SESSION_INSERT", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":                sess.ID,
		"current_step":      sess.CurrentStep,
		"selected_use_case": sess.SelectedUseCase,
	})
}

// SetupSessionValidate handles POST /api/v1/setup/session/{id}/validate.
// It runs the chat validator against the proposed credentials, stores
// the result + proposal on the session, and returns the result for
// inline render.
func (s *Server) SetupSessionValidate(w http.ResponseWriter, r *http.Request) {
	if SessionRoleFromContext(r.Context()) == auth.RoleUser {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED", "admin scope required")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.setupSessions == nil || s.setupValidator == nil {
		respondError(w, http.StatusServiceUnavailable, "SETUP_NOT_CONFIGURED", "setup validate not wired")
		return
	}
	id := setupSessionIDFromPath(r.URL.Path)
	if id == "" {
		respondError(w, http.StatusBadRequest, "BAD_PATH", "session id required")
		return
	}
	sess, err := s.setupSessions.Get(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "SESSION_NOT_FOUND", err.Error())
		return
	}
	if !s.setupSessionOwnedByRequest(r, sess) {
		respondError(w, http.StatusNotFound, "SESSION_NOT_FOUND", "session not found")
		return
	}
	body, _, err := decodeSetupChatProposal(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "BAD_BODY", err.Error())
		return
	}
	result := s.setupValidator.Validate(r.Context(), body)
	sess.CurrentStep = "configure-chat"
	sess.ProposedConfig = setupProposalJSON(body)
	sess.ValidationResults = setupResultJSON(result)
	if err := s.setupSessions.Update(r.Context(), sess); err != nil {
		respondError(w, http.StatusInternalServerError, "SESSION_UPDATE", err.Error())
		return
	}
	if isHTMXRequest(r) {
		respondSetupValidationHTML(w, http.StatusOK, result)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// SetupSessionCommit handles POST /api/v1/setup/session/{id}/commit.
// It re-runs validation (credentials are re-sent, not trusted from a
// prior validate call), hard-gates on PingOK unless force=true, then
// writes the secret + patches config.yaml + advances the session.
// Returns {committed, restart_required}. On a failed gate returns 400
// with the ChatValidationResult. Partial failures are reported honestly
// (see the design doc): a config-patch failure rolls back config.yaml
// but leaves an orphaned chat.env the operator can re-use on retry.
func (s *Server) SetupSessionCommit(w http.ResponseWriter, r *http.Request) {
	if SessionRoleFromContext(r.Context()) == auth.RoleUser {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED", "admin scope required")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.setupSessions == nil || s.setupValidator == nil || s.setupConfigPath == "" || s.setupSecretsDir == "" {
		respondError(w, http.StatusServiceUnavailable, "SETUP_NOT_CONFIGURED", "setup commit not wired")
		return
	}
	id := setupSessionIDFromPath(r.URL.Path)
	if id == "" {
		respondError(w, http.StatusBadRequest, "BAD_PATH", "session id required")
		return
	}
	sess, err := s.setupSessions.Get(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "SESSION_NOT_FOUND", err.Error())
		return
	}
	if !s.setupSessionOwnedByRequest(r, sess) {
		respondError(w, http.StatusNotFound, "SESSION_NOT_FOUND", "session not found")
		return
	}
	proposal, force, err := decodeSetupChatProposal(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "BAD_BODY", err.Error())
		return
	}

	result := s.setupValidator.Validate(r.Context(), proposal)
	if !result.PingOK && !force {
		if isHTMXRequest(r) {
			respondSetupValidationHTML(w, http.StatusOK, result)
			return
		}
		respondJSON(w, http.StatusBadRequest, result)
		return
	}

	// Step 1: write the secret. Abort before touching config if this fails.
	secretPath, err := onboarding.WriteEnvSecret(s.setupSecretsDir, "chat.env", "VORNIK_CHAT_API_KEY", proposal.APIKey)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "SECRET_WRITE", err.Error())
		return
	}

	// Step 2: patch config.yaml via the comment-preserving writer, with
	// backup/restore for rollback on failure.
	if err := writeChatConfig(s.setupConfigPath, secretPath, proposal); err != nil {
		var pe *configPatchError
		if errors.As(err, &pe) {
			respondError(w, http.StatusInternalServerError, pe.Code, pe.Msg)
			return
		}
		respondError(w, http.StatusInternalServerError, "CONFIG_PATCH", fmt.Sprintf("secret written to %s; config patch failed: %v", secretPath, err))
		return
	}

	// Step 3: advance the session. On-disk state is already the source of
	// truth; a session-update failure is reported but not rolled back.
	sess.CurrentStep = "configure-memory"
	sess.ProposedConfig = setupProposalJSON(proposal)
	sess.ValidationResults = setupResultJSON(result)
	if err := s.setupSessions.Update(r.Context(), sess); err != nil {
		respondError(w, http.StatusInternalServerError, "SESSION_UPDATE", fmt.Sprintf("chat config written to %s and %s but session update failed: %v (on-disk state is authoritative; re-commit to reconcile)", s.setupConfigPath, secretPath, err))
		return
	}

	if isHTMXRequest(r) {
		respondSetupCommitHTML(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"committed":        true,
		"restart_required": true,
		"secret_path":      secretPath,
	})
}

// configPatchError carries the error code + message from writeChatConfig so
// the commit handler can respondError with the exact partial-failure wording
// the design doc mandates (orphaned chat.env honesty, no silent rollback).
type configPatchError struct {
	Code string
	Msg  string
}

func (e *configPatchError) Error() string { return e.Msg }

// writeChatConfig patches config.yaml with the five chat keys via the
// comment-preserving SetYAMLKey, using backup/restore for rollback on
// failure. It returns a *configPatchError describing the exact
// partial-failure state so the caller can map it to a respondError call
// without re-formatting. The secret at secretPath is never rolled back —
// it is reused on retry — so every error message names it honestly.
func writeChatConfig(configPath, secretPath string, proposal onboarding.ChatConfigProposal) error {
	writer := &featuredoctor.FileConfigWriter{Path: configPath}
	backup, err := writer.Backup()
	if err != nil {
		return &configPatchError{Code: "CONFIG_BACKUP", Msg: fmt.Sprintf("secret written to %s; config backup failed: %v", secretPath, err)}
	}
	content, err := writer.Read()
	if err != nil {
		return &configPatchError{Code: "CONFIG_READ", Msg: fmt.Sprintf("secret written to %s; config read failed: %v (config unchanged)", secretPath, err)}
	}
	patches := []struct {
		key string
		val any
	}{
		{"chat.enabled", true},
		{"chat.provider", "http"},
		{"chat.endpoint", proposal.Endpoint},
		{"chat.model", proposal.Model},
		{"chat.api_key", "${VORNIK_CHAT_API_KEY}"},
	}
	for _, p := range patches {
		content, _, err = config.SetYAMLKey(content, p.key, p.val)
		if err != nil {
			_ = writer.Restore(backup)
			return &configPatchError{Code: "CONFIG_PATCH", Msg: fmt.Sprintf("config patch failed and was rolled back, but the secret file at %s was already written — it is harmless but orphaned; re-trying commit will reuse it: %v", secretPath, err)}
		}
	}
	if err := writer.Write(content); err != nil {
		_ = writer.Restore(backup)
		return &configPatchError{Code: "CONFIG_WRITE", Msg: fmt.Sprintf("config write failed and was rolled back, but the secret file at %s was already written — it is harmless but orphaned; re-trying commit will reuse it: %v", secretPath, err)}
	}
	if err := writer.Validate(); err != nil {
		_ = writer.Restore(backup)
		return &configPatchError{Code: "CONFIG_INVALID", Msg: fmt.Sprintf("written config did not validate and was rolled back; secret at %s is orphaned: %v", secretPath, err)}
	}
	return nil
}

func writeDispatcherProjectConfig(configPath, projectID string) error {
	writer := &featuredoctor.FileConfigWriter{Path: configPath}
	backup, err := writer.Backup()
	if err != nil {
		return &configPatchError{Code: "CONFIG_BACKUP", Msg: fmt.Sprintf("config backup failed: %v", err)}
	}
	content, err := writer.Read()
	if err != nil {
		return &configPatchError{Code: "CONFIG_READ", Msg: fmt.Sprintf("config read failed: %v", err)}
	}
	content, _, err = config.SetYAMLKey(content, "telegram.dispatcher_project_id", projectID)
	if err != nil {
		_ = writer.Restore(backup)
		return &configPatchError{Code: "CONFIG_PATCH", Msg: fmt.Sprintf("config patch failed and was rolled back: %v", err)}
	}
	if err := writer.Write(content); err != nil {
		_ = writer.Restore(backup)
		return &configPatchError{Code: "CONFIG_WRITE", Msg: fmt.Sprintf("config write failed and was rolled back: %v", err)}
	}
	if err := writer.Validate(); err != nil {
		_ = writer.Restore(backup)
		return &configPatchError{Code: "CONFIG_INVALID", Msg: fmt.Sprintf("written config did not validate and was rolled back: %v", err)}
	}
	return nil
}

// setupSessionRouter dispatches /api/v1/setup/session/{id}/{action}.
func (s *Server) setupSessionRouter(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/memory/validate"):
		s.SetupMemoryValidate(w, r)
	case strings.HasSuffix(r.URL.Path, "/memory/commit"):
		s.SetupMemoryCommit(w, r)
	case strings.HasSuffix(r.URL.Path, "/validate"):
		s.SetupSessionValidate(w, r)
	case strings.HasSuffix(r.URL.Path, "/commit"):
		s.SetupSessionCommit(w, r) // implemented in Task 6
	default:
		respondError(w, http.StatusNotFound, "UNKNOWN_SETUP_ACTION", "unknown setup session action")
	}
}

func (s *Server) setupSessionOwnedByRequest(r *http.Request, sess *persistence.InstallationOnboardingSession) bool {
	if sess == nil {
		return false
	}
	operatorID := RequestOperatorIDOrSingleTenant(r, SingleTenantOperatorIDFromConfig(s.config))
	return operatorID != "" && sess.OperatorID == operatorID
}

func decodeSetupChatProposal(r *http.Request) (onboarding.ChatConfigProposal, bool, error) {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" || strings.EqualFold(ct, "application/json") {
		var body struct {
			onboarding.ChatConfigProposal
			Force bool `json:"force"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return onboarding.ChatConfigProposal{}, false, fmt.Errorf("invalid JSON body")
		}
		return body.ChatConfigProposal, body.Force, nil
	}
	if err := r.ParseForm(); err != nil {
		return onboarding.ChatConfigProposal{}, false, fmt.Errorf("invalid form body")
	}
	force, _ := strconv.ParseBool(r.FormValue("force"))
	return onboarding.ChatConfigProposal{
		Endpoint: r.FormValue("endpoint"),
		APIKey:   r.FormValue("api_key"),
		Model:    r.FormValue("model"),
	}, force, nil
}

func isHTMXRequest(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("HX-Request"), "true")
}

func respondSetupValidationHTML(w http.ResponseWriter, status int, result onboarding.ChatValidationResult) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	stateClass := "border-emerald-500/30 bg-emerald-500/5 text-emerald-200"
	title := "Connection validated"
	if !result.PingOK {
		stateClass = "border-amber-500/30 bg-amber-500/5 text-amber-200"
		title = "Connection needs attention"
	}

	var b strings.Builder
	b.WriteString(`<div class="mt-3 rounded-lg border px-4 py-3 text-xs `)
	b.WriteString(stateClass)
	b.WriteString(`"><div class="font-semibold">`)
	b.WriteString(title)
	b.WriteString(`</div>`)
	if len(result.Failures) > 0 {
		b.WriteString(`<ul class="mt-2 space-y-1">`)
		for _, f := range result.Failures {
			b.WriteString(`<li><span class="font-medium">`)
			b.WriteString(html.EscapeString(f.Name))
			b.WriteString(`:</span> `)
			b.WriteString(html.EscapeString(f.Message))
			if strings.TrimSpace(f.Remediation) != "" {
				b.WriteString(` <span class="text-gray-300">`)
				b.WriteString(html.EscapeString(f.Remediation))
				b.WriteString(`</span>`)
			}
			b.WriteString(`</li>`)
		}
		b.WriteString(`</ul>`)
	} else if result.ModelsListed {
		b.WriteString(`<div class="mt-1 text-gray-300">Model list and invocation probe passed.</div>`)
	}
	b.WriteString(`</div>`)
	_, _ = w.Write([]byte(b.String()))
}

func respondSetupCommitHTML(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<div class="mt-3 rounded-lg border border-emerald-500/30 bg-emerald-500/5 px-4 py-3 text-xs text-emerald-200"><div class="font-semibold">Chat config saved</div><div class="mt-1 text-gray-300">Restart required before the daemon uses the new chat credentials.</div></div>`))
}

// setupSessionID generates a fresh onboarding session ID.
func setupSessionID() string {
	return "onb-" + hexID(12)
}

// setupSessionIDFromPath extracts the session id from
// /api/v1/setup/session/{id}/validate (or /commit).
func setupSessionIDFromPath(path string) string {
	const prefix = "/api/v1/setup/session/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func setupProposalJSON(p onboarding.ChatConfigProposal) []byte {
	b, _ := json.Marshal(p)
	return b
}

func setupResultJSON(r onboarding.ChatValidationResult) []byte {
	b, _ := json.Marshal(r)
	return b
}

// hexID returns n random hex characters. Used for onboarding session IDs.
func hexID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
