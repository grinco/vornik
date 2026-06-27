package auth

import (
	"context"
	"errors"
	"testing"
)

func TestHMACWebhookBackend(t *testing.T) {
	b := NewHMACWebhookBackend()
	cases := []struct {
		name   string
		cred   Credential
		wantOK bool
	}{
		{"signature, no bearer", Credential{HMACPresent: true, Path: "/api/v1/webhooks/p/s"}, true},
		{"signature AND bearer — bearer wins, no opinion", Credential{BearerToken: "k", HMACPresent: true, Path: "/api/v1/webhooks/p/s"}, false},
		{"no signature", Credential{Path: "/api/v1/webhooks/p/s"}, false},
		{"empty credential", Credential{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := b.Authenticate(context.Background(), tc.cred)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("err = %v, want identity", err)
				}
				if id.Subject != "webhook:/api/v1/webhooks/p/s" {
					t.Errorf("Subject = %q", id.Subject)
				}
				if id.DisplayName != id.Subject {
					t.Errorf("DisplayName = %q, want %q", id.DisplayName, id.Subject)
				}
				if len(id.Projects) != 0 {
					t.Errorf("Projects = %v, want empty", id.Projects)
				}
				if id.BoundProjectID != "" {
					t.Errorf("BoundProjectID = %q, want empty", id.BoundProjectID)
				}
				return
			}
			if !errors.Is(err, ErrNoCredential) {
				t.Errorf("err = %v, want ErrNoCredential", err)
			}
		})
	}
}

// TestHMACWebhookBackend_TrustsMiddlewarePathGating pins the known
// trust delegation: the backend does NOT path-check — a non-webhook
// path with HMACPresent set still mints an identity. The
// middleware's Credential construction (hasWebhookSignatureForPath)
// is the ONLY gate. If this test surprises you, read the SHARP EDGE
// note on HMACWebhookBackend before "fixing" it.
func TestHMACWebhookBackend_TrustsMiddlewarePathGating(t *testing.T) {
	b := NewHMACWebhookBackend()
	id, err := b.Authenticate(context.Background(), Credential{
		HMACPresent: true,
		Path:        "/api/v1/projects/p/tasks", // NOT a webhook route
	})
	if err != nil {
		t.Fatalf("err = %v — the backend must trust the middleware's path gating", err)
	}
	if id.Subject != "webhook:/api/v1/projects/p/tasks" {
		t.Errorf("Subject = %q", id.Subject)
	}
}
