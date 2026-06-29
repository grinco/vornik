package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/onboarding"
	"vornik.io/vornik/internal/persistence"
)

// memoryEmbeddingKeyEnv is the env var the memory embedding API key is
// externalised to (secrets/memory.env), referenced from config.yaml as
// ${VORNIK_MEMORY_EMBEDDING_API_KEY}. The daemon self-sources secrets/*.env
// at load (internal/config/secrets_env.go), so it resolves on every
// deployment after a restart — same model as chat.env.
const memoryEmbeddingKeyEnv = "VORNIK_MEMORY_EMBEDDING_API_KEY"

// SetupMemoryValidate handles POST /api/v1/setup/session/{id}/memory/validate.
// It probes the proposed embedding endpoint and returns a structured result
// for inline render. A disabled proposal short-circuits to Skipped.
func (s *Server) SetupMemoryValidate(w http.ResponseWriter, r *http.Request) {
	if SessionRoleFromContext(r.Context()) == auth.RoleUser {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED", "admin scope required")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.setupSessions == nil || s.setupMemoryValidator == nil {
		respondError(w, http.StatusServiceUnavailable, "SETUP_NOT_CONFIGURED", "memory validate not wired")
		return
	}
	sess, ok := s.loadOwnedSetupSession(w, r)
	if !ok {
		return
	}
	proposal, _, err := decodeSetupMemoryProposal(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "BAD_BODY", err.Error())
		return
	}
	result := s.setupMemoryValidator.Validate(r.Context(), proposal)
	sess.CurrentStep = "configure-memory"
	sess.ProposedConfig = setupMemoryProposalJSON(proposal)
	sess.ValidationResults = setupMemoryResultJSON(result)
	if err := s.setupSessions.Update(r.Context(), sess); err != nil {
		respondError(w, http.StatusInternalServerError, "SESSION_UPDATE", err.Error())
		return
	}
	respondJSON(w, http.StatusOK, result)
}

// SetupMemoryCommit handles POST /api/v1/setup/session/{id}/memory/commit.
// Re-validates (gate on EmbeddingOK when enabled unless force=true), writes
// the embedding key secret + patches memory.* into config.yaml, advances the
// session to complete. A disabled proposal just writes memory.enabled=false.
func (s *Server) SetupMemoryCommit(w http.ResponseWriter, r *http.Request) {
	if SessionRoleFromContext(r.Context()) == auth.RoleUser {
		respondError(w, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED", "admin scope required")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.setupSessions == nil || s.setupMemoryValidator == nil || s.setupConfigPath == "" || s.setupSecretsDir == "" {
		respondError(w, http.StatusServiceUnavailable, "SETUP_NOT_CONFIGURED", "memory commit not wired")
		return
	}
	sess, ok := s.loadOwnedSetupSession(w, r)
	if !ok {
		return
	}
	proposal, force, err := decodeSetupMemoryProposal(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "BAD_BODY", err.Error())
		return
	}

	result := s.setupMemoryValidator.Validate(r.Context(), proposal)
	if proposal.Enabled && !result.EmbeddingOK && !force {
		respondJSON(w, http.StatusBadRequest, result)
		return
	}
	// Use the model's actual dimension when the operator left it unset.
	if proposal.Enabled && proposal.EmbeddingDimension == 0 && result.ReturnedDimension > 0 {
		proposal.EmbeddingDimension = result.ReturnedDimension
	}

	secretPath := ""
	if proposal.Enabled && strings.TrimSpace(proposal.EmbeddingAPIKey) != "" {
		secretPath, err = onboarding.WriteEnvSecret(s.setupSecretsDir, "memory.env", memoryEmbeddingKeyEnv, proposal.EmbeddingAPIKey)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "SECRET_WRITE", err.Error())
			return
		}
	}

	if err := writeMemoryConfig(s.setupConfigPath, secretPath, proposal); err != nil {
		var pe *configPatchError
		if errors.As(err, &pe) {
			respondError(w, http.StatusInternalServerError, pe.Code, pe.Msg)
			return
		}
		respondError(w, http.StatusInternalServerError, "CONFIG_PATCH", err.Error())
		return
	}

	sess.CurrentStep = "complete"
	sess.ProposedConfig = setupMemoryProposalJSON(proposal)
	sess.ValidationResults = setupMemoryResultJSON(result)
	if err := s.setupSessions.Update(r.Context(), sess); err != nil {
		respondError(w, http.StatusInternalServerError, "SESSION_UPDATE",
			fmt.Sprintf("memory config written to %s but session update failed: %v (on-disk state is authoritative)", s.setupConfigPath, err))
		return
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"committed":        true,
		"restart_required": proposal.Enabled,
		"memory_enabled":   proposal.Enabled,
		"secret_path":      secretPath,
	})
}

// writeMemoryConfig patches memory.* into config.yaml via the comment-
// preserving writer with backup/restore on failure. When memory is disabled
// it writes only memory.enabled=false. When enabled it writes endpoint,
// model, dimension, and (when a key was provided) the env-placeholder
// api_key. The secret is never rolled back (reused on retry).
func writeMemoryConfig(configPath, secretPath string, p onboarding.MemoryConfigProposal) error {
	writer := &featuredoctor.FileConfigWriter{Path: configPath}
	backup, err := writer.Backup()
	if err != nil {
		return &configPatchError{Code: "CONFIG_BACKUP", Msg: fmt.Sprintf("config backup failed: %v", err)}
	}
	content, err := writer.Read()
	if err != nil {
		return &configPatchError{Code: "CONFIG_READ", Msg: fmt.Sprintf("config read failed: %v (config unchanged)", err)}
	}

	patches := []struct {
		key string
		val any
	}{{"memory.enabled", p.Enabled}}
	if p.Enabled {
		patches = append(patches,
			struct {
				key string
				val any
			}{"memory.embedding_endpoint", p.EmbeddingEndpoint},
			struct {
				key string
				val any
			}{"memory.embedding_model", p.EmbeddingModel},
			struct {
				key string
				val any
			}{"memory.embedding_dimension", p.EmbeddingDimension},
		)
		if strings.TrimSpace(secretPath) != "" {
			patches = append(patches, struct {
				key string
				val any
			}{"memory.embedding_api_key", "${" + memoryEmbeddingKeyEnv + "}"})
		}
	}

	for _, patch := range patches {
		content, _, err = config.SetYAMLKey(content, patch.key, patch.val)
		if err != nil {
			_ = writer.Restore(backup)
			return &configPatchError{Code: "CONFIG_PATCH", Msg: fmt.Sprintf("config patch failed and was rolled back: %v", err)}
		}
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

// loadOwnedSetupSession resolves the session id from the path, loads it, and
// enforces operator ownership. On any failure it writes the response and
// returns ok=false so the caller returns immediately.
func (s *Server) loadOwnedSetupSession(w http.ResponseWriter, r *http.Request) (*persistence.InstallationOnboardingSession, bool) {
	id := setupSessionIDFromPath(r.URL.Path)
	if id == "" {
		respondError(w, http.StatusBadRequest, "BAD_PATH", "session id required")
		return nil, false
	}
	sess, err := s.setupSessions.Get(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, "SESSION_NOT_FOUND", err.Error())
		return nil, false
	}
	if !s.setupSessionOwnedByRequest(r, sess) {
		respondError(w, http.StatusNotFound, "SESSION_NOT_FOUND", "session not found")
		return nil, false
	}
	return sess, true
}

func decodeSetupMemoryProposal(r *http.Request) (onboarding.MemoryConfigProposal, bool, error) {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" || strings.EqualFold(ct, "application/json") {
		var body struct {
			onboarding.MemoryConfigProposal
			Force bool `json:"force"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return onboarding.MemoryConfigProposal{}, false, fmt.Errorf("invalid JSON body")
		}
		return body.MemoryConfigProposal, body.Force, nil
	}
	if err := r.ParseForm(); err != nil {
		return onboarding.MemoryConfigProposal{}, false, fmt.Errorf("invalid form body")
	}
	enabled, _ := strconv.ParseBool(r.FormValue("enabled"))
	force, _ := strconv.ParseBool(r.FormValue("force"))
	dim, _ := strconv.Atoi(r.FormValue("embedding_dimension"))
	return onboarding.MemoryConfigProposal{
		Enabled:            enabled,
		EmbeddingEndpoint:  r.FormValue("embedding_endpoint"),
		EmbeddingAPIKey:    r.FormValue("embedding_api_key"),
		EmbeddingModel:     r.FormValue("embedding_model"),
		EmbeddingDimension: dim,
	}, force, nil
}

func setupMemoryProposalJSON(p onboarding.MemoryConfigProposal) []byte {
	b, _ := json.Marshal(p)
	return b
}

func setupMemoryResultJSON(r onboarding.MemoryValidationResult) []byte {
	b, _ := json.Marshal(r)
	return b
}
