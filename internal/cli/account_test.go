package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The CE account verbs hit /api/v1/operator/accounts* with the shapes the
// operator shell serves (2026-09-13 review R1).
func TestAccountVerbs_HitOperatorRoutes(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/operator/accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]any{{"id": "u1", "displayName": "Ada", "role": "user", "projects": []string{"a"}}}, "count": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/operator/accounts":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["displayName"] != "Ada" || body["role"] != "user" {
				t.Errorf("create body = %v", body)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{"id": "u1", "displayName": "Ada", "role": "user"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/operator/accounts/u1/grant":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["role"] != "admin" {
				t.Errorf("grant body = %v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{"id": "u1", "role": "admin"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/operator/accounts/u1/unlink":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["channel"] != "telegram" || body["externalId"] != "7" {
				t.Errorf("unlink body = %v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{"id": "u1"}, "warning": "audit write failed"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/operator/accounts/u1/disable":
			_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{"id": "u1", "disabled": true}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/operator/accounts/u1":
			_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{"id": "u1"}})
		default:
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "OPERATOR_CAPABILITY_REQUIRED", "message": "nope"}})
		}
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)

	if err := runAccountList(nil, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	accountRole, accountProjects = "user", []string{"a"}
	if err := runAccountCreate(nil, []string{"Ada"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	accountRole = "admin"
	if err := runAccountGrant(nil, []string{"u1"}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	accountRole = ""
	if err := runAccountGrant(nil, []string{"u1"}); err == nil {
		t.Fatal("grant without --role must fail locally")
	}
	if err := runAccountUnlink(nil, []string{"u1", "telegram", "7"}); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if err := accountAction("disable")(nil, []string{"u1"}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := runAccountShow(nil, []string{"u1"}); err != nil {
		t.Fatalf("show: %v", err)
	}
	// A refused capability surfaces the API error, not a decode error.
	if err := accountAction("enable")(nil, []string{"u2"}); err == nil {
		t.Fatal("403 must surface as an error")
	}
	want := []string{
		"GET /api/v1/operator/accounts", "POST /api/v1/operator/accounts", "POST /api/v1/operator/accounts/u1/grant",
		"POST /api/v1/operator/accounts/u1/unlink", "POST /api/v1/operator/accounts/u1/disable", "GET /api/v1/operator/accounts/u1",
		"POST /api/v1/operator/accounts/u2/enable",
	}
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
}
