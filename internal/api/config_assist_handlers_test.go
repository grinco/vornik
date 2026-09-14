package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/configassist"
	"vornik.io/vornik/internal/registry"
)

// The operator REST door (design §6.3 door 1): gated by the CE operator
// capability, 503 when unwired, and a refusal is a 200 with the refusal —
// never an error the caller has to parse out of a 5xx.
func TestOperatorAssist_GateAndUnwired(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Admin = config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-operator"}}
	s := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/operator/assist", strings.NewReader(`{"projectId":"p","intent":"x"}`))
	req = req.WithContext(context.WithValue(context.WithValue(req.Context(), authEnabledKey, true), apiKeyKey, "sk-plain"))
	s.OperatorAssist(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("plain key: want 403, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.OperatorAssist(rec, accountsReq(http.MethodPost, "/api/v1/operator/assist", `{"projectId":"p","intent":"x"}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired: want 503, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestOperatorAssist_RefusalIs200WithRefusal(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Admin = config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-operator"}}
	cfg.ConfigAssistant.Enabled = true
	cfg.ConfigAssistant.Paused = true
	s := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg))
	s.configAssist = &configassist.Engine{
		Config:   func() config.AssistantConfig { return cfg.ConfigAssistant },
		Registry: registry.New(),
	}
	rec := httptest.NewRecorder()
	s.OperatorAssist(rec, accountsReq(http.MethodPost, "/api/v1/operator/assist", `{"projectId":"p","intent":"make it faster"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("paused refusal: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Refusal *configassist.Refusal `json:"refusal"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Refusal == nil || out.Refusal.Code != configassist.RefusePaused {
		t.Fatalf("want ASSISTANT_PAUSED refusal, got %s (%v)", rec.Body.String(), err)
	}

	rec = httptest.NewRecorder()
	s.OperatorAssist(rec, accountsReq(http.MethodPost, "/api/v1/operator/assist", `{"projectId":"","intent":""}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body fields: want 400, got %d", rec.Code)
	}
}

// Review R2/R5: the actor the door stamps is a stable id, never the bearer.
func TestAssistActor_NeverCarriesTheBearer(t *testing.T) {
	req := accountsReq(http.MethodPost, "/api/v1/operator/assist", "")
	a := assistActor(req)
	if strings.Contains(a.CredentialID, "sk-operator") || strings.Contains(a.Principal, "sk-operator") {
		t.Fatalf("actor carries the bearer: %+v", a)
	}
	if !strings.HasPrefix(a.CredentialID, "api_key_sha256:") || a.Kind != "credential" {
		t.Fatalf("actor = %+v", a)
	}
	off := httptest.NewRequest(http.MethodPost, "/", nil)
	off = off.WithContext(context.WithValue(off.Context(), authEnabledKey, false))
	if got := assistActor(off); got.Principal != "auth-disabled" {
		t.Fatalf("auth off actor = %+v", got)
	}
}

func TestOperatorAssistRoute_NotUnderAdminPrefix(t *testing.T) {
	if strings.HasPrefix("/api/v1/operator/assist", adminRoutePrefix) {
		t.Fatal("assist route must not live under the EE admin prefix")
	}
}
