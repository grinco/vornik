package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestMuxHandler_RoutesByTeamID — two channels for two workspaces;
// each Delivery lands at the matching channel.
func TestMuxHandler_RoutesByTeamID(t *testing.T) {
	now := time.Unix(1700000000, 0)
	chA, _ := New(Config{
		SigningSecret: "s-a",
		TeamID:        "T_A",
		TeamAllowlist: []string{"T_A"},
	})
	chA.clock = func() time.Time { return now }
	recA := &recordingReceiver{}
	bindReceiver(chA, recA)
	chB, _ := New(Config{
		SigningSecret: "s-b",
		TeamID:        "T_B",
		TeamAllowlist: []string{"T_B"},
	})
	chB.clock = func() time.Time { return now }
	recB := &recordingReceiver{}
	bindReceiver(chB, recB)
	mux := NewMuxHandler([]*Channel{chA, chB}, zerolog.Nop())

	payloadA := mustEncode(t, map[string]any{
		"type":     "event_callback",
		"team_id":  "T_A",
		"event_id": "Ev_A1",
		"event": map[string]any{
			"type":         "app_mention",
			"user":         "U_alice",
			"text":         "<@bot> hi",
			"channel":      "C_gen",
			"ts":           "1700000010.000100",
			"channel_type": "channel",
		},
	})
	reqA := signedRequest(t, "s-a", now.Unix(), payloadA)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, reqA)
	if w.Code != http.StatusOK {
		t.Fatalf("A status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := recA.snapshot(); len(got) != 1 {
		t.Errorf("recA count = %d, want 1", len(got))
	}
	if got := recB.snapshot(); len(got) != 0 {
		t.Errorf("recB count = %d, want 0 (different team)", len(got))
	}

	payloadB := mustEncode(t, map[string]any{
		"type":     "event_callback",
		"team_id":  "T_B",
		"event_id": "Ev_B1",
		"event": map[string]any{
			"type":         "app_mention",
			"user":         "U_bob",
			"text":         "<@bot> hi",
			"channel":      "C_team",
			"ts":           "1700000020.000100",
			"channel_type": "channel",
		},
	})
	reqB := signedRequest(t, "s-b", now.Unix(), payloadB)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, reqB)
	if w.Code != http.StatusOK {
		t.Fatalf("B status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := recB.snapshot(); len(got) != 1 {
		t.Errorf("recB count = %d, want 1 after team-B delivery", len(got))
	}
}

// TestMuxHandler_URLVerification — handshakes route to the channel
// whose signing secret verifies the body, since they carry no team_id.
func TestMuxHandler_URLVerification(t *testing.T) {
	now := time.Unix(1700000000, 0)
	chWrong, _ := New(Config{
		SigningSecret: "s-wrong",
		TeamID:        "T_WRONG",
		TeamAllowlist: []string{"T_WRONG"},
	})
	chWrong.clock = func() time.Time { return now }
	chA, _ := New(Config{
		SigningSecret: "s-handshake",
		TeamID:        "T_A",
		TeamAllowlist: []string{"T_A"},
	})
	chA.clock = func() time.Time { return now }
	mux := NewMuxHandler([]*Channel{chWrong, chA}, zerolog.Nop())

	body := mustEncode(t, map[string]any{
		"type":      "url_verification",
		"challenge": "echome",
	})
	req := signedRequest(t, "s-handshake", now.Unix(), body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != "echome" {
		t.Errorf("body = %q, want echome", w.Body.String())
	}
}

// TestMuxHandler_UnknownTeam_DropsWith200 — Slack would retry on
// non-200; the mux preserves the same posture as a single Channel.
func TestMuxHandler_UnknownTeam_DropsWith200(t *testing.T) {
	now := time.Unix(1700000000, 0)
	chA, _ := New(Config{
		SigningSecret: "s-a",
		TeamID:        "T_A",
		TeamAllowlist: []string{"T_A"},
	})
	chA.clock = func() time.Time { return now }
	mux := NewMuxHandler([]*Channel{chA}, zerolog.Nop())

	body := mustEncode(t, map[string]any{
		"type":    "event_callback",
		"team_id": "T_UNRELATED",
		"event":   map[string]any{"type": "app_mention", "user": "U_x"},
	})
	req := signedRequest(t, "s-a", now.Unix(), body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// TestMuxHandler_MalformedJSON_RoutesToFallback — a body that
// doesn't parse can't be routed; mux returns 200 + audit log rather
// than 400 (the per-channel handler that would have served this
// already isn't reachable).
func TestMuxHandler_MalformedJSON_RoutesToFallback(t *testing.T) {
	now := time.Unix(1700000000, 0)
	chA, _ := New(Config{
		SigningSecret: "s-a",
		TeamID:        "T_A",
		TeamAllowlist: []string{"T_A"},
	})
	chA.clock = func() time.Time { return now }
	mux := NewMuxHandler([]*Channel{chA}, zerolog.Nop())

	req := signedRequest(t, "s-a", now.Unix(), []byte("not-json"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (drop-and-ack)", w.Code)
	}
}

// TestMuxHandler_WrongMethod — GET/PUT/DELETE rejected at the mux
// layer with 405.
func TestMuxHandler_WrongMethod(t *testing.T) {
	mux := NewMuxHandler(nil, zerolog.Nop())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/slack/webhook", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

// TestMuxHandler_OversizedBody — defensive cap.
func TestMuxHandler_OversizedBody(t *testing.T) {
	mux := NewMuxHandler(nil, zerolog.Nop())
	body := strings.Repeat("a", maxWebhookBodyBytes+10)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/slack/webhook", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

// TestMuxHandler_DuplicateTeamID_KeepsFirst — defensive: a manually-
// constructed mux with two channels claiming the same team_id keeps
// the first registered. (Production wiring's buildSlackChannels
// rejects duplicates earlier, but the mux layer's posture is
// "log + take first" rather than panic.)
func TestMuxHandler_DuplicateTeamID_KeepsFirst(t *testing.T) {
	chA, _ := New(Config{SigningSecret: "s-a", TeamID: "T_DUP", TeamAllowlist: []string{"T_DUP"}})
	chB, _ := New(Config{SigningSecret: "s-b", TeamID: "T_DUP", TeamAllowlist: []string{"T_DUP"}})
	mux := NewMuxHandler([]*Channel{chA, chB}, zerolog.Nop())
	if mux.byTeam["T_DUP"] != chA {
		t.Error("duplicate team_id resolution: second channel won, want first")
	}
}

func mustEncode(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
